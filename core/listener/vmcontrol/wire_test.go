//go:build unix

package vmcontrol

import (
	"bytes"
	"encoding/hex"
	"io"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// The golden bytes below were produced by moca's own proto.Marshal
// (egressv1.ControlRequest / ControlReply, remote-worker @ c13d556), so what
// this codec emits is pinned byte for byte against the far end of the wire —
// which Cortex cannot generate at test time, since moca's gen module is
// unpublished and the sandbox has no protoc.

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

func TestGoldenMarshal(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want string
	}{
		{
			"register(vm-a, sid-a)",
			marshalControlRequest(&ControlRequest{Register: &Register{VmID: "vm-a", Sid: "sid-a"}}),
			"0a0d0a04766d2d6112057369642d61",
		},
		{
			"register(vm-g, sid-a, grant)",
			marshalControlRequest(&ControlRequest{Register: &Register{VmID: "vm-g", Sid: "sid-a", Grant: []byte("a-grant")}}),
			"0a160a04766d2d6712057369642d611a07612d6772616e74",
		},
		{
			// a set oneof case is emitted even when every field is empty
			"register(empty)",
			marshalControlRequest(&ControlRequest{Register: &Register{}}),
			"0a00",
		},
		{
			"unregister(vm-a)",
			marshalControlRequest(&ControlRequest{Unregister: &Unregister{VmID: "vm-a"}}),
			"12060a04766d2d61",
		},
		{
			"empty request",
			marshalControlRequest(&ControlRequest{}),
			"",
		},
		{
			"reply ok",
			marshalControlReply(&ControlReply{Ok: true}),
			"0801",
		},
		{
			"reply refused(bad_request, register carries no fd)",
			marshalControlReply(&ControlReply{Code: "bad_request", Message: "register carries no fd"}),
			"120b6261645f726571756573741a1672656769737465722063617272696573206e6f206664",
		},
	}
	for _, tc := range cases {
		if !bytes.Equal(tc.got, hexBytes(t, tc.want)) {
			t.Errorf("%s: got %x, want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestGoldenUnmarshal(t *testing.T) {
	req, err := decodeControlRequest(hexBytes(t, "0a0d0a04766d2d6112057369642d61"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Register == nil || req.Register.VmID != "vm-a" || req.Register.Sid != "sid-a" || req.Unregister != nil {
		t.Errorf("register decode: %+v", req)
	}
	req, err = decodeControlRequest(hexBytes(t, "0a160a04766d2d6712057369642d611a07612d6772616e74"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Register == nil || string(req.Register.Grant) != "a-grant" {
		t.Errorf("grant decode: %+v", req)
	}
	req, err = decodeControlRequest(hexBytes(t, "12060a04766d2d61"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Unregister == nil || req.Unregister.VmID != "vm-a" || req.Register != nil {
		t.Errorf("unregister decode: %+v", req)
	}
	rep, err := decodeControlReply(hexBytes(t, "120b6261645f726571756573741a1672656769737465722063617272696573206e6f206664"))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ok || rep.Code != "bad_request" || rep.Message != "register carries no fd" {
		t.Errorf("refusal decode: %+v", rep)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		req  ControlRequest
	}{
		{"register", ControlRequest{Register: &Register{VmID: "vm-7", Sid: "run-9", Grant: []byte{0, 1, 2}}}},
		{"register empty", ControlRequest{Register: &Register{}}},
		{"unregister", ControlRequest{Unregister: &Unregister{VmID: "vm-7"}}},
		{"empty", ControlRequest{}},
	}
	for _, tc := range cases {
		got, err := decodeControlRequest(marshalControlRequest(&tc.req))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := tc.req
		switch {
		case want.Register == nil && got.Register != nil, want.Register != nil && got.Register == nil,
			want.Unregister == nil && got.Unregister != nil, want.Unregister != nil && got.Unregister == nil:
			t.Errorf("%s: oneof shape changed: %+v", tc.name, got)
		case want.Register != nil:
			if got.Register.VmID != want.Register.VmID || got.Register.Sid != want.Register.Sid ||
				!bytes.Equal(got.Register.Grant, want.Register.Grant) {
				t.Errorf("%s: got %+v want %+v", tc.name, *got.Register, *want.Register)
			}
		case want.Unregister != nil:
			if got.Unregister.VmID != want.Unregister.VmID {
				t.Errorf("%s: got %+v want %+v", tc.name, *got.Unregister, *want.Unregister)
			}
		}
	}
}

// Unknown fields must be skipped, not fatal: S2's Rebind rides field 3 of
// ControlRequest, and future revisions may add more. Within the oneof, the
// last case seen wins, as proto3 unmarshal does.
func TestDecodeSkipsUnknownFields(t *testing.T) {
	base := hexBytes(t, "0a0d0a04766d2d6112057369642d61") // register(vm-a, sid-a)
	suffixes := map[string][]byte{
		"rebind-shaped (field 3, empty)": {0x1a, 0x00},
		"unknown varint (field 4)":       {0x20, 0x2a},
		"unknown 64-bit (field 5)":       {0x29, 1, 2, 3, 4, 5, 6, 7, 8},
		"unknown LEN (field 6)":          {0x32, 0x03, 'a', 'b', 'c'},
		"unknown 32-bit (field 7)":       {0x35, 1, 2, 3, 4},
	}
	for name, suffix := range suffixes {
		req, err := decodeControlRequest(append(append([]byte{}, base...), suffix...))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if req.Register == nil || req.Register.VmID != "vm-a" || req.Register.Sid != "sid-a" {
			t.Errorf("%s: payload lost: %+v", name, req)
		}
	}
	// The same skipping inside a submessage: Register with an unknown field 4.
	req, err := decodeControlRequest([]byte{0x0a, 0x07, 0x0a, 0x03, 'v', 'm', 'x', 0x20, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	if req.Register == nil || req.Register.VmID != "vmx" {
		t.Errorf("submessage unknown field: %+v", req)
	}
}

func TestDecodeRejectsGroupsAndTruncation(t *testing.T) {
	if _, err := decodeControlRequest([]byte{0x0b}); err == nil { // field 1, group start
		t.Error("group wire type accepted")
	}
	// A submessage whose length runs past the body: length 5, one byte present.
	if _, err := decodeControlRequest([]byte{0x0a, 0x05, 0x0a}); err == nil {
		t.Error("truncated submessage accepted")
	}
	// An unterminated varint (a run of continuation bytes) is refused, not spun on.
	if _, err := decodeControlRequest(bytes.Repeat([]byte{0xff}, 15)); err == nil {
		t.Error("unterminated varint accepted")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()
	defer b.Close()

	body := []byte("a control frame body")
	if err := writeFrame(a, body, -1); err != nil {
		t.Fatal(err)
	}
	got, fds, err := readFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) || len(fds) != 0 {
		t.Errorf("got %q fds %v", got, fds)
	}
}

func TestFrameOversize(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()
	defer b.Close()

	if err := writeFrame(a, make([]byte, maxFrame+1), -1); err != errFrameTooLarge {
		t.Fatalf("write: got %v, want errFrameTooLarge", err)
	}
	// A length header lying about it is caught on read: the connection dies
	// rather than the reader allocating to a hostile size.
	hdr := make([]byte, 4)
	hdr[0] = 0xff // > maxFrame in the first byte alone
	if _, err := a.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFrame(b); err != errFrameTooLarge {
		t.Fatalf("read: got %v, want errFrameTooLarge", err)
	}
}

func TestFrameTruncatedEOF(t *testing.T) {
	a, b := socketPair(t)
	defer a.Close()

	hdr := []byte{0, 0, 0, 8}
	if _, err := a.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("four")); err != nil {
		t.Fatal(err)
	}
	a.Close() // half a frame, then the stream ends
	if _, _, err := readFrame(b); err != io.ErrUnexpectedEOF {
		t.Fatalf("got %v, want ErrUnexpectedEOF", err)
	}
}

// socketPair is a connected Unix socketpair, the transport readFrame and
// writeFrame speak over.
func socketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fileConn(t, fds[0]), fileConn(t, fds[1])
}

func fileConn(t *testing.T, fd int) *net.UnixConn {
	t.Helper()
	f := os.NewFile(uintptr(fd), "test-sock")
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close() // FileConn dups; the original must not hold the socket open
	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Fatalf("file conn is %T", c)
	}
	return uc
}
