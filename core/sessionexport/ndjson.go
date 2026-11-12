package sessionexport

import (
	"encoding/json"
	"io"

	"github.com/rossoctl/cortex/core/model"
)

// Writer encodes records as newline-delimited JSON: one record per line, no enclosing array.
//
// NDJSON RATHER THAN A JSON ARRAY, because the consumer is a pipeline and the producer is a
// walk. A reader can process a month of traffic one line at a time without holding the file,
// every line-oriented tool already works on it, and a writer that is interrupted leaves a
// file whose every complete line is still valid — where a truncated array is unparseable in
// its entirety. The session archive makes the same bet, for the same reason.
type Writer struct {
	enc *json.Encoder
	n   int64
}

// NewWriter returns a Writer encoding to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{enc: json.NewEncoder(w)}
}

// Write encodes one record, which is always exactly one line. json.Encoder appends the
// newline, and encoding/json escapes a literal newline — and every other control character —
// inside a string, so the property is structural rather than a consequence of what Record
// happens to carry today. A field added here later that holds prose cannot break it.
func (w *Writer) Write(r Record) error {
	if err := w.enc.Encode(r); err != nil {
		return err
	}
	w.n++
	return nil
}

// WriteEvent projects e and writes it. A NIL EVENT WRITES NOTHING and is not counted,
// returning nil: an archive walk hands this whatever it holds, and a nil element is the
// walk's gap rather than an event that happened.
//
// SKIPPED HERE RATHER THAN HANDLED IN FromEvent, which stays total and returns a usable
// zero Record for a nil event. The two answer different questions: a projection of nothing
// is harmless, but a LINE for nothing is not — Record.At has no omitempty (an event always
// has a time), so the row read `{"schema":1,"sessionId":"","at":"0001-01-01T00:00:00Z"}`,
// which a consumer counting rows per session or bucketing by At takes for a real event in
// year 1 with no session. It also dropped the sessionID the caller passed, since FromEvent's
// nil branch returns before the fallback. One line is the wrong place to learn that an
// archive had a hole in it.
func (w *Writer) WriteEvent(e *model.SessionEvent, sessionID string) error {
	if e == nil {
		return nil
	}
	return w.Write(FromEvent(e, sessionID))
}

// Count is how many records have been written.
func (w *Writer) Count() int64 { return w.n }
