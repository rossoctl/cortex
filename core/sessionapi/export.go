package sessionapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
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
// A WALK THAT FAILS PAST ITS FIRST RECORD RESETS THE CONNECTION. The status line is long
// gone by then, so the client cannot be told in the response — and it must not be left
// believing the export is whole: returning normally would write the terminating chunk and
// hand it a COMPLETE 200 off a dead walk, the #1543 bug class. So the handler logs (Warn:
// that log line is the server's only trace of a truncated export) and panics with
// http.ErrAbortHandler. NDJSON keeps every complete line already flushed valid —
// Writer's stated design property — so a consumer keeps what arrived and reads an error
// at the end rather than a clean EOF. A failure before any record — an unknown session,
// a bad since, a segment that cannot be OPENED OR READ — still answers a status.
//
// A segment whose CONTENT is damaged is a different case, and it is tolerated rather
// than answered: readSegment serves the readable prefix and reports the rest as
// truncated, which Page discards, so the walk sees no error. Such a segment contributes
// its readable events and nothing more — the same behaviour the paging endpoints have.
// A segment damaged before its first event therefore does read as an empty one here;
// surfacing that as an error needs the archive to hand `truncated` up, which is an
// archive change rather than this endpoint's: #1855.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if s.archive == nil {
		jsonError(w, `{"error":"session archive not enabled"}`, http.StatusNotFound)
		return
	}
	// ParseQuery rather than URL.Query(), which discards the pairs it cannot decode and
	// returns the rest: a mistyped escape in a FILTER then reads as an absent filter, so
	// `?session=%GG` exported the whole archive with a 200 instead of refusing. Worse for
	// a repeated parameter — `?since=1h&since=%ZZ` dropped both values, so even the
	// repeated-since refusal below never saw them. A malformed query is a caller bug, and
	// the one thing this endpoint must not do is answer it with more data than was asked
	// for. agentop never gets here (url.Values.Encode escapes properly); curl does.
	q, qerr := url.ParseQuery(r.URL.RawQuery)
	if qerr != nil {
		jsonError(w, `{"error":"malformed query"}`, http.StatusBadRequest)
		return
	}
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
			jsonError(w, `{"error":"since must appear at most once"}`, http.StatusBadRequest)
			return
		}
		if raw := raws[0]; raw != "" {
			d, err := parseSince(raw)
			if err != nil {
				jsonError(w, `{"error":"since must be a duration or Nd, e.g. 168h or 7d"}`, http.StatusBadRequest)
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
			jsonError(w, `{"error":`+string(msg)+`}`, http.StatusBadRequest)
			return
		}
		if !cw.wrote {
			// The walk died before its first record, so the status line is still ours to
			// answer: a segment that cannot be opened or read must not read as an empty
			// archive. A segment whose content is merely damaged never reaches here —
			// see the note on this function for why that case is tolerated instead.
			// search.go's shape, for the same class of failure one endpoint over.
			jsonError(w, `{"error":"archive read failed"}`, http.StatusInternalServerError)
			return
		}
		// Past the first record the status line is gone, so the only honest signal left is
		// a broken connection. Returning normally here would make net/http write the
		// terminating chunk and hand the client a COMPLETE 200 off a walk that died —
		// io.Copy returns nil, a CLI prints its success summary, and a short export is
		// indistinguishable from a whole one (#1543, the same bug the forward proxy's
		// stream relay fixed the same way: see classifyBrokenStream's note on abort).
		// ErrAbortHandler is the standard library's own abort signal — deferred
		// finalization still runs and net/http logs no stack trace.
		//
		// The Warn stays: it is the server's trace of the failure, and the panic only
		// tells the CLIENT. Every complete NDJSON line already flushed stays valid, so a
		// consumer keeps what arrived — it just cannot mistake it for the whole archive.
		slog.Warn("sessionapi: export truncated", "error", err)
		panic(http.ErrAbortHandler)
	}
}

// jsonError is http.Error for a body that is already JSON.
//
// http.Error sets `text/plain; charset=utf-8` unconditionally, so every refusal on this
// route went out labelled plain text over a `{"error":...}` body — and on this route
// specifically it OVERWROTE the `application/x-ndjson` set for the success path, so the
// two surfaces of one endpoint disagreed about a header they both send. Nothing consumes
// it today (agentop parses the body regardless), but docs/session-api.md documents the
// route's content type, and a client negotiating on it would be misled.
//
// Scoped to this file rather than offered as a package-wide helper: the other handlers
// have the same wart, and fixing them is a repo-wide change with its own blast radius
// (see the #1830 review thread). This keeps the endpoint the PR introduces honest without
// quietly changing the responses of endpoints it does not own.
func jsonError(w http.ResponseWriter, body string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body+"\n")
}

// parseSince reads a since value: a Go duration ("168h", "30m") or a whole-day count
// ("7d", "30d"). Negative and zero values are errors — a floor at or ahead of now asks
// for nothing, and a caller who types them is telling this server about a bug upstream
// of it. So is a day count past 106751: time.Duration cannot hold it, and the overflow
// would wrap negative and floor ahead of now, which reads as an empty export rather than
// the refusal it is.
//
// A LEADING SIGN IS REFUSED IN THE DAY-COUNT BRANCH, because strconv.Atoi accepts one and
// the grammar this endpoint documents ("a duration or Nd") does not: "+7d" would parse to
// 168h while being a form no client was told it could send. The duration branch keeps
// whatever time.ParseDuration accepts — a sign is part of ITS documented grammar, and "-7h"
// is still refused below for being non-positive.
func parseSince(s string) (time.Duration, error) {
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(rest)
		if err != nil || strings.HasPrefix(rest, "+") || days <= 0 ||
			int64(days) > math.MaxInt64/int64(24*time.Hour) {
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
// runs, rather than at the end of a response that can be a month of traffic; the walk
// takes long enough on a large archive that a consumer tailing one live should not wait
// for the buffer (how long exactly is unbenchmarked — see exportTimeout's note). The wrote flag is how the handler tells an error it may still answer
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
