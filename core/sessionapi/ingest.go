package sessionapi

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rossoctl/cortex/core/attribution"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/parse/inference"
	"github.com/rossoctl/cortex/core/redact"
)

// POST /v1/ingest takes the rows that cortex-gateway records in praxis mode, as NDJSON, one
// line per row (line format v1, docs/session-api.md). Cortex files each row, settles its cost
// and keeps its capture as a listener does. Registered only when WithIngest has a token.

const (
	// ingestLineVersion is the line format version, the "v" key.
	ingestLineVersion = 1
	// ingestLineCap is the largest line read. A longer line is refused as line_cap.
	ingestLineCap = 64 << 20
	// requestCaptureTTL is how long a kept request capture waits for its response line. Longer
	// than any stream: a 60 s window would drop the response capture of every long stream.
	requestCaptureTTL = time.Hour
)

// The refusal reasons of ingest_refused_total.
const (
	refusedBadJSON = "bad_json"
	refusedVersion = "version"
	refusedSchema  = "schema"
	refusedOp      = "op"
	refusedLineCap = "line_cap"
	refusedErased  = "erased"
)

var refusalReasons = []string{refusedBadJSON, refusedVersion, refusedSchema, refusedOp, refusedLineCap, refusedErased}

// Ingest turns on POST /v1/ingest and GET /v1/ingest/stats.
type Ingest struct {
	// Token is the bearer token a producer must send. Empty leaves ingestion off.
	Token string
	// Rates prices ingested rows. nil reports them unpriced.
	Rates pricing.Resolver
}

// WithIngest turns on the ingestion endpoint (praxis mode). Without it, or with an empty
// token, both routes answer 404 and no erase mark is kept.
func WithIngest(in Ingest) Option {
	return func(s *Server) { s.ingestCfg = in }
}

// IngestStats is GET /v1/ingest/stats.
type IngestStats struct {
	Lines           uint64            `json:"ingest_lines_total"`
	Refused         map[string]uint64 `json:"ingest_refused_total"`
	CapturesDropped uint64            `json:"ingest_captures_dropped_total"`
}

// ingester is the state of the ingestion endpoint.
type ingester struct {
	cfg   Ingest
	marks *eraseMarks

	lines, capturesDropped atomic.Uint64
	refusedMu              sync.Mutex
	refused                map[string]uint64

	// reqCaps holds the request ids of kept request captures, with when each was kept, until
	// the response line arrives. A response capture is kept only for an id here.
	reqCapsMu sync.Mutex
	reqCaps   map[string]time.Time

	// streams are the open ingest streams, so Shutdown can end them.
	streamsMu sync.Mutex
	streams   map[*http.ResponseController]struct{}
}

func newIngester(cfg Ingest) *ingester {
	return &ingester{cfg: cfg, marks: newEraseMarks(), refused: map[string]uint64{},
		reqCaps: map[string]time.Time{}, streams: map[*http.ResponseController]struct{}{}}
}

func (in *ingester) refuse(reason string) {
	in.refusedMu.Lock()
	in.refused[reason]++
	in.refusedMu.Unlock()
}

// endStreams sets a read deadline of now on each open stream, so a blocked read returns and
// its handler ends. Restarts stay quick: queued lines are lost, and the producer counts them.
func (in *ingester) endStreams() {
	in.streamsMu.Lock()
	defer in.streamsMu.Unlock()
	for rc := range in.streams {
		_ = rc.SetReadDeadline(time.Now())
	}
}

// ingestLine is one line, format v1.
type ingestLine struct {
	V             int                `json:"v"`
	SchemaVersion int                `json:"schemaVersion"`
	Op            string             `json:"op"`
	RequestID     string             `json:"requestId"`
	StartedAt     time.Time          `json:"startedAt"`
	Session       string             `json:"session"`
	Event         model.SessionEvent `json:"event"`
	Capture       *ingestCapture     `json:"capture"`
	Cost          *ingestCost        `json:"cost"`
}

// ingestCost holds a response row's cost-relevant headers.
type ingestCost struct {
	Headers map[string]string `json:"headers"`
}

// ingestCapture is the wire form of model.RequestCapture, which has no JSON tags.
type ingestCapture struct {
	Original  []byte          `json:"original"`
	Forwarded []byte          `json:"forwarded"`
	Header    http.Header     `json:"header"`
	Query     string          `json:"query"`
	Response  *ingestResponse `json:"response"`
	Dropped   bool            `json:"dropped"`
}

type ingestResponse struct {
	Status     int         `json:"status"`
	Header     http.Header `json:"header"`
	Body       []byte      `json:"body"`
	Decoded    bool        `json:"decoded"`
	Truncated  bool        `json:"truncated"`
	Ended      bool        `json:"ended"`
	Incomplete bool        `json:"incomplete"`
}

// handleIngest is registered behind httpx.LoopbackOnly (see New), also when the rest of the
// API is not.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	in := s.ingest
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+in.cfg.Token)) != 1 {
		http.Error(w, "ingest needs the gateway's bearer token", http.StatusUnauthorized)
		return
	}
	rc := http.NewResponseController(w)
	in.streamsMu.Lock()
	in.streams[rc] = struct{}{}
	in.streamsMu.Unlock()
	defer func() {
		in.streamsMu.Lock()
		delete(in.streams, rc)
		in.streamsMu.Unlock()
	}()

	br := bufio.NewReaderSize(r.Body, 64<<10)
	for {
		b, over, err := readIngestLine(br)
		switch {
		case over:
			in.refuse(refusedLineCap)
		case len(bytes.TrimSpace(b)) > 0:
			s.ingestOne(b)
		}
		if errors.Is(err, io.EOF) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err != nil {
			slog.Debug("sessionapi: ingest stream ended", "error", err)
			return
		}
	}
}

// readIngestLine reads one line without its '\n'. A line over ingestLineCap is read to its
// end and discarded, and over is set.
func readIngestLine(br *bufio.Reader) (line []byte, over bool, err error) {
	for {
		frag, e := br.ReadSlice('\n')
		if !over {
			if len(line)+len(frag) > ingestLineCap+1 {
				over, line = true, nil
			} else {
				line = append(line, frag...)
			}
		}
		if errors.Is(e, bufio.ErrBufferFull) {
			continue
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		if len(line) > ingestLineCap {
			over, line = true, nil
		}
		return line, over, e
	}
}

// ingestOne files one line, or counts why it was refused.
func (s *Server) ingestOne(b []byte) {
	in := s.ingest
	var l ingestLine
	switch err := json.Unmarshal(b, &l); {
	case err != nil:
		in.refuse(refusedBadJSON)
		return
	case l.V != ingestLineVersion:
		in.refuse(refusedVersion)
		return
	case l.SchemaVersion != SchemaVersion:
		in.refuse(refusedSchema)
		return
	case l.Op != "append":
		in.refuse(refusedOp)
		return
	}
	sid := attribution.FiledSession(l.Session, s.store.ActiveSession)
	// #1722 releases the line's tunnel join here.
	if in.marks.erasedAfter(sid, l.StartedAt) {
		in.refuse(refusedErased)
		return
	}

	ev := l.Event
	// Cortex does not trust a producer's cost record, nor its numbering or time.
	delete(ev.Plugins, event.Key)
	delete(ev.Plugins, event.PluginName)
	ev.Seq, ev.SessionID, ev.At = 0, "", time.Now()

	if ev.Phase == model.SessionResponse {
		s.settleIngested(&ev, l.Cost)
	}
	s.store.AppendCapture(sid, ev, in.capture(l, &ev))
	in.lines.Add(1)
}

// settleIngested prices a response row from its cost headers, with the publish gate of
// inferenceparser's settleCost.
func (s *Server) settleIngested(ev *model.SessionEvent, c *ingestCost) {
	hdr := http.Header{}
	if c != nil {
		for k, v := range c.Headers {
			hdr.Set(k, v)
		}
	}
	inference.SettleServedModel(ev.Inference, hdr)
	st := settle.SettleInput(settle.Input{Host: ev.Host, Path: ev.HTTPPath, Inference: ev.Inference, ResponseHeaders: hdr}, s.ingest.cfg.Rates)
	if !st.Priced && !st.HasPrompt && st.RejectedReason == "" {
		return
	}
	rec, err := json.Marshal(settle.NewRecord(st, nil))
	if err != nil {
		slog.Warn("sessionapi: ingest cost record", "error", err)
		return
	}
	if ev.Plugins == nil {
		ev.Plugins = map[string]json.RawMessage{}
	}
	ev.Plugins[event.Key], ev.Plugins[event.PluginName] = rec, rec
}

// capture is the capture to keep for l, or nil. Only an inference row keeps one (a body of
// any other request can be a credential), credentials are removed again, and a response
// capture is kept only after its request's.
func (in *ingester) capture(l ingestLine, ev *model.SessionEvent) *model.RequestCapture {
	c := l.Capture
	switch {
	case c == nil:
		return nil
	case c.Dropped:
		in.capturesDropped.Add(1)
		return nil
	case ev.Inference == nil:
		return nil
	}
	in.reqCapsMu.Lock()
	defer in.reqCapsMu.Unlock()
	if c.Response == nil {
		in.reqCaps[l.RequestID] = time.Now()
		return &model.RequestCapture{Original: c.Original, Forwarded: c.Forwarded,
			Header: redact.Headers(c.Header), Query: redact.Query(c.Query)}
	}
	now := time.Now()
	for id, at := range in.reqCaps {
		if now.Sub(at) > requestCaptureTTL {
			delete(in.reqCaps, id)
		}
	}
	if _, ok := in.reqCaps[l.RequestID]; !ok {
		return nil
	}
	delete(in.reqCaps, l.RequestID)
	res := c.Response
	return &model.RequestCapture{Response: &model.ResponseCapture{Status: res.Status, Header: redact.Headers(res.Header),
		Body: res.Body, Decoded: res.Decoded, Truncated: res.Truncated, Ended: res.Ended, Incomplete: res.Incomplete}}
}

func (s *Server) handleIngestStats(w http.ResponseWriter, r *http.Request) {
	in := s.ingest
	st := IngestStats{Lines: in.lines.Load(), CapturesDropped: in.capturesDropped.Load(), Refused: map[string]uint64{}}
	in.refusedMu.Lock()
	for _, reason := range refusalReasons {
		st.Refused[reason] = in.refused[reason]
	}
	in.refusedMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
