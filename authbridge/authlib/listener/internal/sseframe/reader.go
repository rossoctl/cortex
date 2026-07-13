// Package sseframe reads complete Server-Sent Event frames off a
// streaming HTTP response body. The reader emits one slice per
// "data:"-bearing event, with all data lines of that event
// concatenated and delivered together — exactly the shape MCP's
// Streamable HTTP transport, A2A's message/stream, and OpenAI-
// compatible chat-completions streaming use to carry one JSON-RPC
// message (or one chunk) per event.
//
// This is a deliberately narrow subset of the SSE spec
// (https://html.spec.whatwg.org/multipage/server-sent-events.html):
// we surface only the data payload of each event. Comment lines
// (starting with ":"), event-type lines ("event:"), id, and retry
// fields are skipped. The framing this code cares about is:
//
//	data: { ... json-rpc message ... }\n
//	\n
//	data: { ... another message ... }\n
//	\n
//
// Multi-line data within a single event is folded with "\n"
// separators per the spec; an empty line ends the event. CR/LF and
// CRLF terminators are accepted.
package sseframe

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// ErrFrameTooLarge is returned by Reader.ReadFrame when a single SSE
// event's accumulated data exceeds the configured per-frame cap.
// Streaming responses bound memory to one frame at a time, so this
// signals an upstream that produced a single oversized JSON-RPC
// message — the proxy gives up rather than buffering it.
var ErrFrameTooLarge = errors.New("sseframe: frame exceeds per-frame size cap")

// Reader scans an io.Reader for complete SSE events and returns the
// concatenated data lines of each. Construct with NewReader; call
// ReadFrame repeatedly until io.EOF.
type Reader struct {
	br      *bufio.Reader
	maxSize int
	// scratch holds the in-progress event's accumulated data lines.
	// Reused across ReadFrame calls to avoid per-frame allocation in
	// the common steady-state case.
	scratch []byte
	// lastEvent holds the "event:" field value of the frame most
	// recently returned by ReadFrame (empty if the frame named no
	// event type). Reused across calls; valid until the next ReadFrame.
	// Preserved so a proxy re-framing the stream can reproduce the
	// upstream's SSE event type — clients like the Anthropic SDK type
	// each event from the "event:" line, not the data payload.
	lastEvent []byte
}

// NewReader wraps r with the given per-frame size cap (bytes).
// A non-positive maxSize falls back to the package default
// (DefaultMaxFrameSize).
func NewReader(r io.Reader, maxSize int) *Reader {
	if maxSize <= 0 {
		maxSize = DefaultMaxFrameSize
	}
	return &Reader{
		br:      bufio.NewReader(r),
		maxSize: maxSize,
	}
}

// DefaultMaxFrameSize bounds a single SSE event's data payload at
// 1 MiB — same as the buffered-path cap so the streaming path
// preserves the per-message memory ceiling without bounding the
// total stream length.
const DefaultMaxFrameSize = 1 << 20

// ReadFrame reads from the underlying reader until a complete SSE
// event arrives (i.e. a blank line terminator after one or more
// "data:" lines), then returns the concatenated data payload.
// Empty events (no "data:" lines, or only comment/event-type lines)
// are skipped silently — the returned slice is always non-empty
// unless the stream ends.
//
// At end-of-stream the function returns (nil, io.EOF). If the stream
// ends after a partial event with data already accumulated, that
// trailing event is delivered before EOF (per the spec, an SSE
// stream may end without a final blank-line terminator).
//
// The returned slice is owned by the Reader and reused on the next
// call; callers that need to retain bytes across ReadFrame calls
// must copy.
func (r *Reader) ReadFrame() ([]byte, error) {
	r.scratch = r.scratch[:0]
	r.lastEvent = r.lastEvent[:0]
	hasData := false

	for {
		line, err := r.readLine()
		if err == io.EOF {
			// Stream ended. If we accumulated data without a final
			// blank-line terminator, deliver it before EOF — per spec
			// "if the user agent has reached the end of the file, then
			// dispatch the event."
			if hasData {
				return r.scratch, nil
			}
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}

		// Blank line — event terminator. Dispatch what we have, or
		// keep scanning if this event was empty (comment-only event).
		if len(line) == 0 {
			if hasData {
				return r.scratch, nil
			}
			continue
		}

		// Comment line — silently skipped per spec ("If the line
		// starts with a U+003A COLON character, ignore the line").
		if line[0] == ':' {
			continue
		}

		// Field-bearing line. Split at the first colon; everything
		// after it (with one optional leading space stripped) is the
		// value. Lines without a colon name a field with an empty
		// value, which we don't care about either way.
		field, value := splitField(line)
		if field == "event" {
			// Capture the event type so a re-framing proxy can reproduce
			// the upstream "event:" line. Copied because value points into
			// readLine's buffer, which is reused on the next line read.
			r.lastEvent = append(r.lastEvent[:0], value...)
			continue
		}
		if field != "data" {
			// We deliberately ignore "id", "retry", and any unknown
			// field — the consumer only needs the data payload (and the
			// event type captured above).
			continue
		}

		// Append "\n" before the next data line per spec ("If the
		// data buffer's last character is a U+000A LINE FEED (LF)
		// character, then remove the last character from the data
		// buffer." — equivalently, separator between data lines is
		// LF, and trailing LF is stripped at dispatch).
		if hasData {
			if err := r.appendByte('\n'); err != nil {
				return nil, err
			}
		}
		hasData = true
		if err := r.appendBytes(value); err != nil {
			return nil, err
		}
	}
}

// LastEvent returns the "event:" field value of the frame most recently
// returned by ReadFrame, or an empty slice if that frame named no event
// type. The returned slice is owned by the Reader and reused on the next
// ReadFrame call; copy to retain. A re-framing proxy uses this to emit an
// "event: <type>" line so downstream SSE consumers that type events from
// the event field (e.g. the Anthropic SDK) see the upstream's framing.
func (r *Reader) LastEvent() []byte { return r.lastEvent }

// readLine reads one logical SSE line off the underlying buffered
// reader. Line terminators per the SSE spec are LF, CR, or CRLF —
// any of the three ends a line. The returned slice excludes the
// terminator and is valid only until the next call. EOF without a
// final newline is reported as io.EOF only when the line is empty;
// otherwise the unterminated tail is returned.
//
// Per-line bytes are bounded by r.maxSize so a pathological non-data
// line (huge comment, malformed unknown field) cannot grow memory
// past the per-frame cap.
func (r *Reader) readLine() ([]byte, error) {
	var line []byte
	for {
		b, err := r.br.ReadByte()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		switch b {
		case '\n':
			return line, nil
		case '\r':
			// Coalesce CRLF as a single terminator. If the next byte
			// isn't LF, leave it for the next ReadByte; a bare CR is
			// itself a valid line terminator.
			if next, perr := r.br.Peek(1); perr == nil && len(next) == 1 && next[0] == '\n' {
				_, _ = r.br.ReadByte()
			}
			return line, nil
		default:
			if len(line)+1 > r.maxSize {
				return nil, ErrFrameTooLarge
			}
			line = append(line, b)
		}
	}
}

// appendByte / appendBytes grow the scratch buffer while enforcing
// the per-frame cap. ErrFrameTooLarge fires the moment we'd exceed
// the cap, before any further reading — bounding memory to maxSize
// regardless of what the upstream sends.
func (r *Reader) appendByte(b byte) error {
	if len(r.scratch)+1 > r.maxSize {
		return ErrFrameTooLarge
	}
	r.scratch = append(r.scratch, b)
	return nil
}

func (r *Reader) appendBytes(b []byte) error {
	if len(r.scratch)+len(b) > r.maxSize {
		return ErrFrameTooLarge
	}
	r.scratch = append(r.scratch, b...)
	return nil
}

// splitField parses an SSE field line "name:value" or "name:". A
// single optional leading space after the colon is stripped per spec.
// Lines with no colon are treated as a field name with empty value.
func splitField(line []byte) (string, []byte) {
	idx := bytes.IndexByte(line, ':')
	if idx < 0 {
		return string(line), nil
	}
	name := string(line[:idx])
	value := line[idx+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return name, value
}
