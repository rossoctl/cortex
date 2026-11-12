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

// WriteEvent projects e and writes it.
func (w *Writer) WriteEvent(e *model.SessionEvent, sessionID string) error {
	return w.Write(FromEvent(e, sessionID))
}

// Count is how many records have been written.
func (w *Writer) Count() int64 { return w.n }
