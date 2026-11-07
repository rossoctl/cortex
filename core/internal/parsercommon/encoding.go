package parsercommon

import (
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// MaxDecodedRequestSize caps how large a zstd-decompressed request body is
// allowed to grow to, independent of the wire-size cap the listener already
// enforces (32 MiB as of this writing). zstd.NewReader's own default
// (WithDecoderMaxMemory) is 64 GiB — a highly compressible body well within
// the wire cap could otherwise decompress far past it. 64 MiB is generous
// relative to any real request seen so far (a live Codex sample decompressed
// to ~65 KB) while still bounding the worst case.
const MaxDecodedRequestSize = 64 << 20

// requestZstd is a single decoder shared across requests rather than one
// constructed per call. klauspost/compress/zstd documents DecodeAll as safe
// to call concurrently on one Decoder — the size limit below applies to each
// call independently, not to total memory across concurrent requests.
var requestZstd, _ = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxDecodedRequestSize))

// DecodeRequestBody returns body decompressed per the request's
// Content-Encoding header, or body unchanged when the header names an
// encoding this function doesn't handle, or isn't present at all.
//
// zstd only, for now — the one encoding seen on live Codex traffic to the
// Responses endpoint. Nothing upstream of the parsers decompresses a request
// body (every listener hands plugins the raw bytes it read off the wire), so
// inference-parser decodes with this before parsing, and any plugin that
// re-reads a parsed body must decode it the same way to see the same bytes.
//
// A failed decompression (truncated body, a header that lied, or a body
// that would exceed MaxDecodedRequestSize) returns the original compressed
// bytes rather than erroring: the caller's subsequent JSON decode then
// fails too, and the request is treated the same as any other body it
// cannot read.
func DecodeRequestBody(headers http.Header, body []byte) []byte {
	if headers == nil || !strings.EqualFold(headers.Get("Content-Encoding"), "zstd") {
		return body
	}
	out, err := requestZstd.DecodeAll(body, nil)
	if err != nil {
		return body
	}
	return out
}
