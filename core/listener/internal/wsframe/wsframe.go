// Package wsframe reads and writes RFC 6455 WebSocket frames for a proxy that relays a
// connection it also needs to read.
//
// RELAY FIRST, READ SECOND. A Frame keeps the exact bytes it arrived as (Raw), so the
// caller forwards those and never re-encodes what it relays: a re-encode could only
// change what the peer receives. Payload is a separate, unmasked copy for reading.
//
// What it does not do is deliberate. It negotiates no extensions — the caller strips
// Sec-WebSocket-Extensions from the handshake, so a frame with RSV1 set is a protocol
// error here rather than something to inflate — and it does not enforce RFC 6455's
// close handshake, which is the two endpoints' business; it only reads close frames so
// the caller can see one go by.
package wsframe

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Opcodes (RFC 6455 §5.2).
const (
	OpContinuation byte = 0x0
	OpText         byte = 0x1
	OpBinary       byte = 0x2
	OpClose        byte = 0x8
	OpPing         byte = 0x9
	OpPong         byte = 0xA
)

// Close codes this package writes (RFC 6455 §7.4.1).
const (
	ClosePolicyViolation = 1008
	CloseInternalError   = 1011
)

// maxControlPayload is RFC 6455 §5.5's cap on a control frame's payload.
const maxControlPayload = 125

// ErrProtocol is a frame no conforming endpoint sends. The caller should stop relaying:
// the two ends no longer agree on where frames begin.
var ErrProtocol = errors.New("wsframe: protocol error")

// Frame is one frame as read off the wire.
type Frame struct {
	Fin    bool
	Opcode byte
	Masked bool
	// Raw is the frame exactly as it arrived, header and (still masked) payload.
	// Forward this, not a re-encoding.
	Raw []byte
	// Payload is the unmasked payload, or nil when the frame is over the reader's
	// limit: such a frame is still relayed whole, but not read.
	Payload []byte
	// Truncated reports that Payload is nil because the frame exceeded the limit.
	Truncated bool
}

// IsControl reports whether f is a close, ping or pong frame.
func (f *Frame) IsControl() bool { return f.Opcode&0x8 != 0 }

// Reader reads frames from one direction of a connection.
type Reader struct {
	r io.Reader
	// limit is the largest payload kept for reading. A larger frame is read and
	// relayed in full but its Payload is dropped, so one huge message costs the
	// proxy no more than limit bytes of reading memory... beyond the Raw copy the
	// relay needs anyway.
	limit int64
	// maxFrame bounds Raw: a frame over it is refused, since relaying it would mean
	// buffering it whole first.
	maxFrame int64
}

// NewReader reads frames from r, keeping payloads up to limit bytes for reading and
// refusing any single frame over maxFrame bytes.
func NewReader(r io.Reader, limit, maxFrame int64) *Reader {
	return &Reader{r: r, limit: limit, maxFrame: maxFrame}
}

// ReadFrame reads the next frame. It returns io.EOF only when the connection closed
// cleanly between frames; a close mid-frame is io.ErrUnexpectedEOF.
func (r *Reader) ReadFrame() (*Frame, error) {
	var hdr [14]byte
	if _, err := io.ReadFull(r.r, hdr[:2]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	b0, b1 := hdr[0], hdr[1]
	f := &Frame{Fin: b0&0x80 != 0, Opcode: b0 & 0x0F, Masked: b1&0x80 != 0}
	if b0&0x70 != 0 {
		// RSV1-3. RSV1 is permessage-deflate's, which the handshake strip should have
		// kept off; the other two have no meaning without an extension either.
		return nil, fmt.Errorf("%w: reserved bits set (0x%02x) — an extension the proxy did not negotiate", ErrProtocol, b0&0x70)
	}
	switch f.Opcode {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
	default:
		return nil, fmt.Errorf("%w: unknown opcode 0x%x", ErrProtocol, f.Opcode)
	}

	n := 2
	length := int64(b1 & 0x7F)
	switch length {
	case 126:
		if _, err := io.ReadFull(r.r, hdr[n:n+2]); err != nil {
			return nil, unexpected(err)
		}
		length = int64(binary.BigEndian.Uint16(hdr[n : n+2]))
		n += 2
	case 127:
		if _, err := io.ReadFull(r.r, hdr[n:n+8]); err != nil {
			return nil, unexpected(err)
		}
		u := binary.BigEndian.Uint64(hdr[n : n+8])
		if u>>63 != 0 {
			return nil, fmt.Errorf("%w: 64-bit length with the high bit set", ErrProtocol)
		}
		length = int64(u)
		n += 8
	}
	if f.IsControl() && (length > maxControlPayload || !f.Fin) {
		return nil, fmt.Errorf("%w: control frame fragmented or over %d bytes", ErrProtocol, maxControlPayload)
	}
	var key [4]byte
	if f.Masked {
		if _, err := io.ReadFull(r.r, hdr[n:n+4]); err != nil {
			return nil, unexpected(err)
		}
		copy(key[:], hdr[n:n+4])
		n += 4
	}
	if length > r.maxFrame-int64(n) {
		return nil, fmt.Errorf("%w: a %d-byte frame is over the relay's %d-byte limit", ErrProtocol, length, r.maxFrame)
	}

	// Raw grows as the payload arrives rather than being sized from the length the
	// header claims: a peer that sends a 14-byte header claiming 32 MiB and then
	// stalls would otherwise hold that much memory for as long as it stalls.
	var raw bytes.Buffer
	raw.Grow(n + int(min(length, rawPrealloc)))
	raw.Write(hdr[:n])
	if got, err := io.CopyN(&raw, r.r, length); err != nil || got != length {
		return nil, unexpected(orEOF(err))
	}
	f.Raw = raw.Bytes()
	if length > r.limit {
		f.Truncated = true
		return f, nil
	}
	// Payload aliases Raw only when there is no mask to undo; otherwise it is the one
	// copy reading needs.
	if !f.Masked {
		f.Payload = f.Raw[n:]
		return f, nil
	}
	f.Payload = append([]byte(nil), f.Raw[n:]...)
	maskBytes(f.Payload, key)
	return f, nil
}

// rawPrealloc is how much of a frame's claimed length is reserved up front: enough
// for the common frame in one allocation, and no more than that before the bytes
// arrive.
const rawPrealloc = 64 << 10

// orEOF is err, or io.EOF when a copy stopped short without one.
func orEOF(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// maskBytes XORs b with key in place — masking and unmasking are the same operation.
func maskBytes(b []byte, key [4]byte) {
	for i := range b {
		b[i] ^= key[i&3]
	}
}

// Encode builds one unfragmented frame. A client must mask what it sends and a server
// must not (RFC 6455 §5.1), so the caller says which side it is writing as.
func Encode(opcode byte, payload []byte, masked bool) []byte {
	length := len(payload)
	hdr := make([]byte, 0, 14+length)
	hdr = append(hdr, 0x80|opcode)
	var maskBit byte
	if masked {
		maskBit = 0x80
	}
	switch {
	case length <= 125:
		hdr = append(hdr, maskBit|byte(length))
	case length <= 0xFFFF:
		hdr = append(hdr, maskBit|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(length))
	default:
		hdr = append(hdr, maskBit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(length))
	}
	if !masked {
		return append(hdr, payload...)
	}
	var key [4]byte
	_, _ = rand.Read(key[:])
	hdr = append(hdr, key[:]...)
	start := len(hdr)
	hdr = append(hdr, payload...)
	maskBytes(hdr[start:], key)
	return hdr
}

// ClosePayload is a close frame's payload: the two-byte code, then the reason. The
// reason is cut to fit a control frame.
func ClosePayload(code uint16, reason string) []byte {
	if len(reason) > maxControlPayload-2 {
		// RFC 6455 §5.5.1 has the receiver fail the connection on a reason that is
		// not valid UTF-8, so the cut backs up to a character boundary.
		cut := maxControlPayload - 2
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	b := binary.BigEndian.AppendUint16(nil, code)
	return append(b, reason...)
}

// closeCodeNames are the RFC 6455 §7.4.1 codes a close frame commonly carries.
var closeCodeNames = map[uint16]string{
	1000: "normal closure",
	1001: "going away",
	1002: "protocol error",
	1003: "unsupported data",
	1007: "invalid payload",
	1008: "policy violation",
	1009: "message too big",
	1010: "missing extension",
	1011: "internal error",
	1012: "service restart",
	1013: "try again later",
}

// DescribeClose renders a close frame's payload for a log or a recorded row: its code,
// the code's RFC 6455 name when it has one, and the sender's reason. "close without a
// code" for an empty payload, which RFC 6455 allows. The reason is the sender's own
// text, so it is kept to printable ASCII and cut to fit a short line.
func DescribeClose(payload []byte) string {
	if len(payload) < 2 {
		return "close without a code"
	}
	code := binary.BigEndian.Uint16(payload[:2])
	out := fmt.Sprintf("close code %d", code)
	if name, ok := closeCodeNames[code]; ok {
		out += " " + name
	}
	var reason []byte
	for _, b := range payload[2:] {
		if len(reason) >= 80 {
			break
		}
		if b >= 0x20 && b < 0x7f {
			reason = append(reason, b)
		}
	}
	if len(reason) > 0 {
		out += ": " + string(reason)
	}
	return out
}

// ErrMessageTooBig is a data message over its limit. The caller should end the socket
// (close code 1009, message too big), as the HTTPS path answers a body over its cap
// with a 413: a message too big to read is also one whose fragments the relay would
// otherwise have to hold without bound, waiting for an end that may never come.
var ErrMessageTooBig = errors.New("wsframe: message over the size limit")

// CloseMessageTooBig is RFC 6455 §7.4.1's code for ErrMessageTooBig.
const CloseMessageTooBig = 1009

// CloseTryAgainLater asks the peer to reconnect (RFC 6455 §7.4.1, registered by IANA).
const CloseTryAgainLater = 1013

// maxFragments bounds the frames one message may arrive in. Each fragment is a
// separate allocation the relay holds until the message ends, so a flood of empty
// continuations — six bytes each on the wire, and never over the byte limit — would
// cost memory without bound. Codex sends each message as one frame.
const maxFragments = 1024

// Message reassembles a data message from its frames.
//
// It sees control frames go by without letting them interrupt a message, as RFC 6455
// §5.4 allows a peer to interleave them between fragments. It reports a message whole
// once its final fragment arrives. A message over limit — in payload, in wire bytes, or
// in fragments — is ErrMessageTooBig, which ends the socket: it is never relayed
// unread, because a turn too big to read is one no plugin would see.
type Message struct {
	limit     int64
	opcode    byte
	buf       []byte
	wire      int64 // wire bytes of the message so far, header and all
	fragments int
	inFlight  bool
}

// NewMessage reassembles messages of up to limit bytes.
func NewMessage(limit int64) *Message { return &Message{limit: limit} }

// Add takes the next data frame and, when it completes a message, returns that
// message's opcode and payload with ok true. It returns ErrMessageTooBig once the
// message passes its limit, and ErrProtocol for fragments out of order.
//
// readable is false only for a binary message, which is relayed and counted but not
// parsed. The payload is the caller's to keep: the next message does not reuse it.
func (m *Message) Add(f *Frame) (opcode byte, payload []byte, readable, ok bool, err error) {
	if f.IsControl() {
		return 0, nil, false, false, nil
	}
	switch {
	case f.Opcode == OpContinuation && !m.inFlight:
		return 0, nil, false, false, fmt.Errorf("%w: continuation frame with no message in progress", ErrProtocol)
	case f.Opcode != OpContinuation && m.inFlight:
		return 0, nil, false, false, fmt.Errorf("%w: a new message began before the last one finished", ErrProtocol)
	case f.Opcode != OpContinuation:
		m.opcode, m.buf, m.wire, m.fragments, m.inFlight = f.Opcode, nil, 0, 0, true
	}
	m.wire += int64(len(f.Raw))
	m.fragments++
	if f.Truncated || int64(len(m.buf))+int64(len(f.Payload)) > m.limit || m.wire > m.limit+maxFragments*14 || m.fragments > maxFragments {
		m.buf, m.inFlight = nil, false
		return 0, nil, false, false, fmt.Errorf("%w: over %d bytes or %d fragments", ErrMessageTooBig, m.limit, maxFragments)
	}
	if f.Fin && m.fragments == 1 {
		// The common case, one frame: hand the frame's own payload over rather than
		// copying it into buf and out again.
		m.inFlight = false
		return m.opcode, f.Payload, m.opcode == OpText, true, nil
	}
	m.buf = append(m.buf, f.Payload...)
	if !f.Fin {
		return 0, nil, false, false, nil
	}
	m.inFlight = false
	payload, m.buf = m.buf, nil
	return m.opcode, payload, m.opcode == OpText, true, nil
}
