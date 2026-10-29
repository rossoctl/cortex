package wsframe

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// A message over the limit is ErrMessageTooBig as soon as it passes it, not relayed
// unread: a turn too big to read is one no plugin sees, and its fragments would
// otherwise be held until a final one that may never come.
func TestMessage_OverLimitIsRefused(t *testing.T) {
	m := NewMessage(8)
	frames := messageFrames(t,
		frag(OpText, false, "12345"),
		frag(OpContinuation, true, "67890"),
	)
	if _, _, _, ok, err := m.Add(frames[0]); err != nil || ok {
		t.Fatalf("first fragment: ok=%v err=%v, want neither", ok, err)
	}
	if _, _, _, _, err := m.Add(frames[1]); !errors.Is(err, ErrMessageTooBig) {
		t.Errorf("over the limit: err = %v, want ErrMessageTooBig", err)
	}
}

// A frame the reader did not keep, being over its limit, ends the message the same way.
func TestMessage_TruncatedFragmentIsRefused(t *testing.T) {
	m := NewMessage(1 << 20)
	first := &Frame{Opcode: OpText, Fin: false, Payload: []byte("A"), Raw: []byte{0x01, 0x01, 'A'}}
	big := &Frame{Opcode: OpContinuation, Fin: false, Truncated: true, Raw: make([]byte, 16)}
	if _, _, _, _, err := m.Add(first); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := m.Add(big); !errors.Is(err, ErrMessageTooBig) {
		t.Errorf("a truncated middle fragment: err = %v, want ErrMessageTooBig — it must not be spliced out", err)
	}
}

// Empty continuations cost a frame each and add no payload, so the payload limit alone
// would never stop them. The fragment cap does.
func TestMessage_FragmentFloodIsRefused(t *testing.T) {
	m := NewMessage(1 << 20)
	if _, _, _, _, err := m.Add(&Frame{Opcode: OpText, Raw: []byte{0x01, 0x00}}); err != nil {
		t.Fatal(err)
	}
	var err error
	for i := 0; i < maxFragments+1 && err == nil; i++ {
		_, _, _, _, err = m.Add(&Frame{Opcode: OpContinuation, Raw: []byte{0x00, 0x00}})
	}
	if !errors.Is(err, ErrMessageTooBig) {
		t.Errorf("after %d empty continuations: err = %v, want ErrMessageTooBig", maxFragments+1, err)
	}
}

// After a refused message the next one starts fresh.
func TestMessage_NextMessageAfterARefusalIsRead(t *testing.T) {
	m := NewMessage(4)
	_, _, _, _, _ = m.Add(messageFrames(t, frag(OpText, true, "too long"))[0])
	_, payload, readable, ok, err := m.Add(messageFrames(t, frag(OpText, true, "ok"))[0])
	if err != nil || !ok || !readable || string(payload) != "ok" {
		t.Errorf("next message: payload=%q readable=%v ok=%v err=%v", payload, readable, ok, err)
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

func TestDescribeClose(t *testing.T) {
	cases := map[string]struct {
		payload []byte
		want    string
	}{
		"no code":          {nil, "close without a code"},
		"normal":           {ClosePayload(1000, ""), "close code 1000 normal closure"},
		"internal, reason": {ClosePayload(1011, "boom"), "close code 1011 internal error: boom"},
		"unnamed code":     {ClosePayload(4000, ""), "close code 4000"},
		"escape in reason": {ClosePayload(1008, "a\x1b[2Jb"), "close code 1008 policy violation: a[2Jb"},
	}
	for name, tc := range cases {
		if got := DescribeClose(tc.payload); got != tc.want {
			t.Errorf("%s: DescribeClose = %q, want %q", name, got, tc.want)
		}
	}
}

// RFC 6455 §5.7's longer examples: a 256-byte and a 65536-byte unmasked binary frame,
// in the 16-bit and 64-bit length forms. Both read back with the right length, and
// both directions agree on byte order (a little-endian length would misframe them).
func TestReadFrame_RFC6455LongLengthExamples(t *testing.T) {
	for _, tc := range []struct {
		name string
		hdr  []byte
		n    int
	}{
		{"256 bytes, 16-bit length", []byte{0x82, 0x7E, 0x01, 0x00}, 256},
		{"64 KiB, 64-bit length", []byte{0x82, 0x7F, 0, 0, 0, 0, 0, 0x01, 0x00, 0x00}, 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := append(append([]byte(nil), tc.hdr...), bytes.Repeat([]byte{'x'}, tc.n)...)
			f, err := NewReader(bytes.NewReader(raw), 1<<20, 1<<20).ReadFrame()
			if err != nil || len(f.Payload) != tc.n || !bytes.Equal(f.Raw, raw) {
				t.Fatalf("payload=%d err=%v rawEqual=%v", len(f.Payload), err, f != nil && bytes.Equal(f.Raw, raw))
			}
			if enc := Encode(OpBinary, f.Payload, false); !bytes.Equal(enc[:len(tc.hdr)], tc.hdr) {
				t.Errorf("Encode header = % x, want % x", enc[:len(tc.hdr)], tc.hdr)
			}
		})
	}
}

// The relay limit is checked against the header's claim before any payload is read, so
// a header claiming more than the limit is refused without waiting for, or reserving
// room for, a body that never comes.
func TestReadFrame_RefusesAnOversizedClaimBeforeReading(t *testing.T) {
	claim := []byte{0x82, 0x7F, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF} // no body follows
	if _, err := NewReader(bytes.NewReader(claim), 1024, 4096).ReadFrame(); !errors.Is(err, ErrProtocol) {
		t.Errorf("err = %v, want ErrProtocol", err)
	}
	// At the limit exactly: header plus payload is maxFrame, which is allowed.
	exact := append([]byte{0x82, 6}, "123456"...)
	if _, err := NewReader(bytes.NewReader(exact), 1024, 8).ReadFrame(); err != nil {
		t.Errorf("a frame exactly at the limit: err = %v", err)
	}
	if _, err := NewReader(bytes.NewReader(exact), 1024, 7).ReadFrame(); !errors.Is(err, ErrProtocol) {
		t.Errorf("a frame one byte over: err = %v, want ErrProtocol", err)
	}
}

// A header claiming a large payload and then stalling holds memory for what arrived, not
// for what it claimed.
func TestReadFrame_ReservesForWhatArrivesNotWhatIsClaimed(t *testing.T) {
	hdr := []byte{0x82, 0x7F, 0, 0, 0, 0, 0x01, 0, 0, 0} // 16 MiB claimed
	_, err := NewReader(io.MultiReader(bytes.NewReader(hdr), bytes.NewReader([]byte("abc"))), 1<<30, 1<<30).ReadFrame()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrUnexpectedEOF for a body that stopped short", err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write(hdr); _, _ = pw.Write([]byte("abc")) }()
	done := make(chan struct{})
	go func() { _, _ = NewReader(pr, 1<<30, 1<<30).ReadFrame(); close(done) }()
	time.Sleep(50 * time.Millisecond) // the reader is now stalled mid-payload
	runtime.ReadMemStats(&after)
	_ = pw.Close()
	<-done
	if grew := int64(after.HeapAlloc) - int64(before.HeapAlloc); grew > 4<<20 {
		t.Errorf("a stalled 16 MiB claim grew the heap by %d bytes; want it bounded by what arrived", grew)
	}
}

// A close reason is cut at a character boundary, so the payload stays valid UTF-8.
func TestClosePayload_CutsAtACharacter(t *testing.T) {
	p := ClosePayload(1000, strings.Repeat("é", 100))
	if len(p) > maxControlPayload || !utf8.Valid(p[2:]) {
		t.Errorf("payload %d bytes, valid UTF-8 %v", len(p), utf8.Valid(p[2:]))
	}
}
