package wsframe

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func readOne(t *testing.T, raw []byte, limit int64) *Frame {
	t.Helper()
	f, err := NewReader(bytes.NewReader(raw), limit, 1<<20).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	return f
}

// RFC 6455 §5.7's own examples, so the reader agrees with the spec rather than only
// with this package's encoder.
func TestReadFrame_RFC6455Examples(t *testing.T) {
	cases := []struct {
		name   string
		raw    []byte
		opcode byte
		fin    bool
		want   string
	}{
		{"unmasked text", []byte{0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}, OpText, true, "Hello"},
		{"masked text", []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}, OpText, true, "Hello"},
		{"first fragment", []byte{0x01, 0x03, 0x48, 0x65, 0x6c}, OpText, false, "Hel"},
		{"last fragment", []byte{0x80, 0x02, 0x6c, 0x6f}, OpContinuation, true, "lo"},
		{"unmasked ping", []byte{0x89, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}, OpPing, true, "Hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := readOne(t, tc.raw, 1024)
			if f.Opcode != tc.opcode || f.Fin != tc.fin || string(f.Payload) != tc.want {
				t.Errorf("got opcode=%x fin=%v payload=%q", f.Opcode, f.Fin, f.Payload)
			}
			// The relay forwards Raw, so it must be the bytes exactly as sent.
			if !bytes.Equal(f.Raw, tc.raw) {
				t.Errorf("Raw = % x, want the input % x", f.Raw, tc.raw)
			}
		})
	}
}

// The 16- and 64-bit length forms, both directions of masking.
func TestEncodeReadRoundTrip_AllLengthForms(t *testing.T) {
	for _, size := range []int{0, 125, 126, 65535, 65536, 200_000} {
		for _, masked := range []bool{false, true} {
			payload := bytes.Repeat([]byte("x"), size)
			raw := Encode(OpText, payload, masked)
			f := readOne(t, raw, 1<<20)
			if !bytes.Equal(f.Payload, payload) || f.Masked != masked || !bytes.Equal(f.Raw, raw) {
				t.Errorf("size %d masked %v: round trip failed (len %d)", size, masked, len(f.Payload))
			}
		}
	}
}

// A frame over the reading limit is still read whole — the relay needs its bytes — but
// its payload is not kept.
func TestReadFrame_OverLimitIsRelayedNotRead(t *testing.T) {
	raw := Encode(OpText, bytes.Repeat([]byte("y"), 300), true)
	f := readOne(t, raw, 100)
	if !f.Truncated || f.Payload != nil {
		t.Errorf("Truncated=%v Payload len=%d, want truncated with no payload", f.Truncated, len(f.Payload))
	}
	if !bytes.Equal(f.Raw, raw) {
		t.Error("Raw must still carry the whole frame")
	}
}

func TestReadFrame_RefusesWhatNoEndpointSends(t *testing.T) {
	cases := map[string][]byte{
		"RSV1 (permessage-deflate)": {0xC1, 0x00},
		"unknown opcode":            {0x83, 0x00},
		"fragmented ping":           {0x09, 0x00},
		"ping over 125 bytes":       append([]byte{0x89, 126, 0x00, 126}, bytes.Repeat([]byte("p"), 126)...),
		"64-bit length high bit":    {0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewReader(bytes.NewReader(raw), 1024, 1<<20).ReadFrame()
			if !errors.Is(err, ErrProtocol) {
				t.Errorf("err = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestReadFrame_RefusesAFrameOverTheRelayLimit(t *testing.T) {
	raw := Encode(OpBinary, make([]byte, 5000), false)
	_, err := NewReader(bytes.NewReader(raw), 1024, 4096).ReadFrame()
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("err = %v, want ErrProtocol", err)
	}
}

// A clean close between frames is EOF; one partway through a frame is not, so the
// caller can tell a finished connection from a torn one.
func TestReadFrame_EOFShapes(t *testing.T) {
	if _, err := NewReader(bytes.NewReader(nil), 10, 1<<20).ReadFrame(); err != io.EOF {
		t.Errorf("empty stream: err = %v, want io.EOF", err)
	}
	torn := Encode(OpText, []byte("hello"), true)[:5]
	if _, err := NewReader(bytes.NewReader(torn), 10, 1<<20).ReadFrame(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("torn frame: err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func messageFrames(t *testing.T, raws ...[]byte) []*Frame {
	t.Helper()
	r := NewReader(bytes.NewReader(bytes.Join(raws, nil)), 1<<20, 1<<20)
	var out []*Frame
	for {
		f, err := r.ReadFrame()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
}

func frag(op byte, fin bool, payload string) []byte {
	b := Encode(op, []byte(payload), true)
	if !fin {
		b[0] &^= 0x80
	}
	return b
}

// A fragmented message reassembles across its continuations, and a ping between two
// fragments does not interrupt it (RFC 6455 §5.4).
func TestMessage_ReassemblesAroundInterleavedControlFrames(t *testing.T) {
	m := NewMessage(1024)
	frames := messageFrames(t,
		frag(OpText, false, `{"type":"response.`),
		Encode(OpPing, []byte("hi"), true),
		frag(OpContinuation, false, `create","model":`),
		frag(OpContinuation, true, `"gpt"}`),
	)
	var got []string
	for _, f := range frames {
		op, payload, readable, ok, err := m.Add(f)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if op != OpText || !readable {
				t.Fatalf("op=%x readable=%v", op, readable)
			}
			got = append(got, string(payload))
		}
	}
	if len(got) != 1 || got[0] != `{"type":"response.create","model":"gpt"}` {
		t.Errorf("messages = %q", got)
	}
}

// A message over the limit completes but is not readable, and the next one starts
// fresh rather than inheriting the lost state.
func TestMessage_OverLimitIsCountedNotRead(t *testing.T) {
	m := NewMessage(8)
	frames := messageFrames(t,
		frag(OpText, false, "12345"),
		frag(OpContinuation, true, "67890"),
		frag(OpText, true, "ok"),
	)
	var results []string
	for _, f := range frames {
		_, payload, readable, ok, err := m.Add(f)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if readable {
				results = append(results, string(payload))
			} else {
				results = append(results, "<unreadable>")
			}
		}
	}
	if strings.Join(results, ",") != "<unreadable>,ok" {
		t.Errorf("results = %v", results)
	}
}

func TestMessage_RefusesOutOfOrderFragments(t *testing.T) {
	m := NewMessage(1024)
	if _, _, _, _, err := m.Add(messageFrames(t, frag(OpContinuation, true, "x"))[0]); !errors.Is(err, ErrProtocol) {
		t.Errorf("stray continuation: err = %v", err)
	}
	m = NewMessage(1024)
	fs := messageFrames(t, frag(OpText, false, "a"), frag(OpText, true, "b"))
	_, _, _, _, _ = m.Add(fs[0])
	if _, _, _, _, err := m.Add(fs[1]); !errors.Is(err, ErrProtocol) {
		t.Errorf("new message mid-message: err = %v", err)
	}
}

func TestClosePayload_FitsAControlFrame(t *testing.T) {
	p := ClosePayload(ClosePolicyViolation, strings.Repeat("r", 500))
	if len(p) > maxControlPayload {
		t.Errorf("len = %d, over %d", len(p), maxControlPayload)
	}
	if p[0] != 0x03 || p[1] != 0xF0 {
		t.Errorf("code bytes = % x, want 03 f0 (1008)", p[:2])
	}
}
