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
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

	f.Raw = make([]byte, n+int(length))
	copy(f.Raw, hdr[:n])
	if _, err := io.ReadFull(r.r, f.Raw[n:]); err != nil {
		return nil, unexpected(err)
	}
	if length > r.limit {
		f.Truncated = true
		return f, nil
	}
	f.Payload = append([]byte(nil), f.Raw[n:]...)
	if f.Masked {
		maskBytes(f.Payload, key)
	}
	return f, nil
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
		reason = reason[:maxControlPayload-2]
	}
	b := binary.BigEndian.AppendUint16(nil, code)
	return append(b, reason...)
}

// Message reassembles a data message from its frames.
//
// It sees control frames go by without letting them interrupt a message, as RFC 6455
// §5.4 allows a peer to interleave them between fragments. It reports a message whole
// once its final fragment arrives, and only if every fragment was readable and the total
// stayed within limit; otherwise the message is still relayed, just not read.
type Message struct {
	limit    int64
	opcode   byte
	buf      []byte
	inFlight bool
	// lost is set when a fragment of the current message was over the reader's
	// limit or the total grew past this one's.
	lost bool
}

// NewMessage reassembles messages of up to limit bytes.
func NewMessage(limit int64) *Message { return &Message{limit: limit} }

// Add takes the next data frame and, when it completes a message, returns that
// message's opcode and payload with ok true. A message that could not be read returns
// ok true and a nil payload with readable false, so the caller can still count it.
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
		m.opcode, m.buf, m.lost, m.inFlight = f.Opcode, m.buf[:0], false, true
	}
	if f.Truncated || int64(len(m.buf))+int64(len(f.Payload)) > m.limit {
		m.lost = true
	} else if !m.lost {
		m.buf = append(m.buf, f.Payload...)
	}
	if !f.Fin {
		return 0, nil, false, false, nil
	}
	m.inFlight = false
	if m.lost {
		return m.opcode, nil, false, true, nil
	}
	return m.opcode, append([]byte(nil), m.buf...), true, true, nil
}
