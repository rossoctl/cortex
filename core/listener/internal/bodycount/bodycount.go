// Package bodycount counts the bytes a listener reads off an upstream response
// body, so every listener reports the same figure for the same response (#1309).
//
// It exists because the proxies read a response body through four different arms
// — buffered whole, relayed SSE block by block, relayed chunk by chunk, or copied
// straight through — and a per-arm count is easy to get subtly wrong: the SSE arm
// handles parsed payloads alongside the bytes it forwards, and a buffered read
// stops at the listener's cap. extproc has no such ambiguity (Envoy hands it the raw chunks),
// so counting at the one place where the raw bytes enter the proxy is what makes
// the three listeners agree. Wrap once, as early as the body is in hand, and let
// every arm downstream read through the wrapper.
package bodycount

import "io"

// Wrap returns rc with the length of every successful read added to *n. The
// pointer is a pipeline.Context field at both call sites: the arms that consume
// the body are several frames below the wrap, and the record site is above it
// again, so there is nowhere to thread a return value through.
//
// Not safe for concurrent reads, which is not a constraint in practice: a
// response body is read by the one goroutine serving its request, and that is the
// goroutine that records the row.
func Wrap(rc io.ReadCloser, n *int64) io.ReadCloser {
	return &countingBody{rc: rc, n: n}
}

// WrapCapture is Wrap that also keeps the first max bytes read in *keep, for the session
// archive's capture of the response, sets *truncated once more than max arrived, and sets *ended
// when the body was read to its end.
func WrapCapture(rc io.ReadCloser, n *int64, keep *[]byte, truncated, ended *bool, max int) io.ReadCloser {
	return &countingBody{rc: rc, n: n, keep: keep, truncated: truncated, ended: ended, max: max}
}

type countingBody struct {
	rc io.ReadCloser
	n  *int64

	keep             *[]byte
	truncated, ended *bool
	max              int
}

// Read counts what it hands back, including bytes returned alongside an error:
// io.Reader may do both, and those bytes did arrive. A body that breaks off
// mid-read therefore reports what was received before it broke rather than zero,
// which is the figure the 502 it produces is worth reading next to.
func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	*c.n += int64(n)
	if c.keep != nil && n > 0 {
		room := c.max - len(*c.keep)
		if n > room {
			*c.truncated = true
		}
		*c.keep = append(*c.keep, p[:min(n, max(room, 0))]...)
	}
	if c.ended != nil && err == io.EOF {
		*c.ended = true
	}
	return n, err
}

func (c *countingBody) Close() error { return c.rc.Close() }
