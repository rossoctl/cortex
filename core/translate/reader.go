package translate

import (
	"io"

	"github.com/rossoctl/cortex/core/translate/pivot"
)

// readSize is the most that one Read takes from the upstream body.
const readSize = 32 << 10

// replyReader converts an upstream reply body as it is read (rule 10).
type replyReader struct {
	body    io.ReadCloser
	dec     pivot.ReplyDecoder
	enc     pivot.ReplyEncoder
	buf     []byte // upstream bytes of one Read
	pending []byte // converted bytes not read yet
	err     error  // the upstream error, after pending is read
}

// NewReplyReader gives the converted reply as a stream. Each Read takes
// at most 32 KiB from body and returns as soon as it has converted bytes:
// it never waits for the whole stream. At the upstream EOF it ends the
// decoder and the encoder. An upstream read error other than EOF comes
// after the bytes already converted. Close closes body.
func NewReplyReader(body io.ReadCloser, dec pivot.ReplyDecoder, enc pivot.ReplyEncoder) io.ReadCloser {
	return &replyReader{body: body, dec: dec, enc: enc}
}

func (r *replyReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.buf == nil {
			r.buf = make([]byte, readSize)
		}
		n, err := r.body.Read(r.buf)
		r.pending = encodeAll(r.enc, r.pending, r.dec.Feed(r.buf[:n]))
		switch {
		case err == io.EOF:
			r.pending = encodeAll(r.enc, r.pending, r.dec.End())
			r.pending = append(r.pending, r.enc.End()...)
			r.err = io.EOF
		case err != nil:
			r.err = err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *replyReader) Close() error { return r.body.Close() }
