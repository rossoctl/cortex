package sessionapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/sessionexport"
)

// handleExport answers GET /v1/export with the session archive as content-free records:
// one NDJSON line per archived event, the shape core/sessionexport projects and nothing
// else — no message content, no completions, no tool arguments, which is what lets this
// leave the machine where the full events must not (see Record's own docs for the field
// list and its reading rules).
//
// THE DURABLE ARCHIVE ONLY, NOT THE STORE. This is a backfill of what past processes
// flushed to disk, at most a tick behind the open segments; a session with nothing
// flushed yet does not appear. Merging memory in is what the paging endpoints are for.
//
// QUERY, ALL PARSED BEFORE THE FIRST BYTE so a bad one is a real 400 rather than a
// truncated stream: `session=<id>` repeatable carries only the named sessions (an
// unknown id is a 400 naming the archived ones), `since=<duration>` carries only events
// newer than that far back ("168h" or "7d" — a Go duration or a day count, since
// time.ParseDuration rejects "7d" and usage.ParseWindow's bucket-width and window-cap
// rules are wrong here). A repeated since is a 400 rather than a silent first-wins: two
// values are a caller bug, and the first one looking valid must not hide the second.
// Absent parameters carry everything.
//
// A WALK THAT FAILS PAST ITS FIRST RECORD TRUNCATES RATHER THAN CORRUPTS. The status
// line is long gone by then, and NDJSON keeps every complete line valid in a truncated
// file — Writer's stated design property — so the handler logs (Warn: that log line is
// the only trace a truncated export leaves) and stops. A failure before any record — an
// unknown session, a bad since, a segment that will not decode — still answers a status.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if s.archive == nil {
		http.Error(w, `{"error":"session archive not enabled"}`, http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	f := sessionexport.Filter{}
	if ids := q["session"]; len(ids) > 0 {
		f.Sessions = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			// An empty value reads as absent, as an empty since does: ?session= alone is a
			// trailing separator, not a request for a session named "".
			if id != "" {
				f.Sessions[id] = struct{}{}
			}
		}
	}
	if raws := q["since"]; len(raws) > 0 {
		if len(raws) > 1 {
			http.Error(w, `{"error":"since must appear at most once"}`, http.StatusBadRequest)
			return
		}
		if raw := raws[0]; raw != "" {
			d, err := parseSince(raw)
			if err != nil {
				http.Error(w, `{"error":"since must be a duration or Nd, e.g. 168h or 7d"}`, http.StatusBadRequest)
				return
			}
			f.Since = s.clock().Add(-d)
		}
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	cw := &flushWriter{ResponseWriter: w}
	if _, err := sessionexport.Walk(s.archive, sessionexport.NewWriter(cw), f); err != nil {
		var unknown *sessionexport.UnknownSessionsError
		if errors.As(err, &unknown) && !cw.wrote {
			// json.Marshal rather than %q: %q is Go quoting, and a session id with a
			// control rune in it would escape in a form no JSON parser accepts.
			msg, _ := json.Marshal(err.Error())
			http.Error(w, `{"error":`+string(msg)+`}`, http.StatusBadRequest)
			return
		}
		if !cw.wrote {
			// The walk died before its first record, so the status line is still ours to
			// answer: a segment that will not decode must not read as an empty archive.
			// search.go's shape, for the same class of failure one endpoint over.
			http.Error(w, `{"error":"archive read failed"}`, http.StatusInternalServerError)
			return
		}
		// Past the first record there is nothing left to answer with: the file the client
		// holds ends at a line boundary, which is the best a stream can do.
		slog.Warn("sessionapi: export truncated", "error", err)
	}
}

// parseSince reads a since value: a Go duration ("168h", "30m") or a whole-day count
// ("7d", "30d"). Negative and zero values are errors — a floor at or ahead of now asks
// for nothing, and a caller who types them is telling this server about a bug upstream
// of it. So is a day count past 106751: time.Duration cannot hold it, and the overflow
// would wrap negative and floor ahead of now, which reads as an empty export rather than
// the refusal it is.
func parseSince(s string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(rest)
		if err != nil || days <= 0 || int64(days) > math.MaxInt64/int64(24*time.Hour) {
			return 0, fmt.Errorf("day count %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("duration %q", s)
	}
	return d, nil
}

// flushWriter flushes after every write and remembers whether anything was written.
//
// Flushing per record keeps a consumer piping this through jq or wc current as the walk
// runs, rather than at the end of a response that can be a month of traffic; a 30-day
// archive walks in about ten seconds, but a consumer tailing one live should not wait
// for the buffer. The wrote flag is how the handler tells an error it may still answer
// with a status from one that can only truncate.
type flushWriter struct {
	http.ResponseWriter
	wrote bool
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.ResponseWriter.Write(p)
	f.wrote = true
	if flusher, ok := f.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}
