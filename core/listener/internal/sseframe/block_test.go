package sseframe

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// readAllBlocks drains r, copying each block because the Reader reuses its buffers.
func readAllBlocks(t *testing.T, r *Reader) []Block {
	t.Helper()
	var out []Block
	for {
		b, err := r.ReadBlock()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("ReadBlock: %v", err)
		}
		out = append(out, Block{
			Raw:     append([]byte(nil), b.Raw...),
			Data:    append([]byte(nil), b.Data...),
			HasData: b.HasData,
		})
	}
}

// The relay's whole promise: concatenating every block's Raw reproduces the
// upstream byte for byte, whatever fields and terminators it used.
func TestReadBlock_RawIsExactWireBytes(t *testing.T) {
	body := ": keepalive\n\n" +
		"event: turn\nid: 7\ndata: {\"type\":\"turn\"}\n\n" +
		"retry: 3000\n\n" +
		"event: text\r\nid: 8\r\ndata: line one\r\ndata: line two\r\n\r\n" +
		"id: 9\rdata: {\"type\":\"done\"}\r\r"
	blocks := readAllBlocks(t, NewReader(strings.NewReader(body), 0))

	var joined []byte
	for _, b := range blocks {
		joined = append(joined, b.Raw...)
	}
	if string(joined) != body {
		t.Fatalf("joined Raw differs from upstream\n got %q\nwant %q", joined, body)
	}

	type want struct {
		hasData bool
		data    string
	}
	wants := []want{
		{false, ""},
		{true, `{"type":"turn"}`},
		{false, ""},
		{true, "line one\nline two"},
		{true, `{"type":"done"}`},
	}
	if len(blocks) != len(wants) {
		t.Fatalf("got %d blocks, want %d: %+v", len(blocks), len(wants), blocks)
	}
	for i, w := range wants {
		if blocks[i].HasData != w.hasData || string(blocks[i].Data) != w.data {
			t.Errorf("block %d = {HasData:%v Data:%q}, want {HasData:%v Data:%q}",
				i, blocks[i].HasData, blocks[i].Data, w.hasData, w.data)
		}
	}
}

// A heartbeat must come back at its own blank line, not be held until the
// next data event arrives — the upstream here sends nothing after it.
func TestReadBlock_HeartbeatReturnedAtItsBlankLine(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = pw.Write([]byte(": ping\n\n")) }()

	r := NewReader(pr, 0)
	got := make(chan Block, 1)
	go func() {
		b, err := r.ReadBlock()
		if err != nil {
			t.Errorf("ReadBlock: %v", err)
		}
		got <- Block{Raw: append([]byte(nil), b.Raw...), HasData: b.HasData}
	}()
	b := <-got
	if string(b.Raw) != ": ping\n\n" || b.HasData {
		t.Errorf("block = {Raw:%q HasData:%v}, want {Raw:\": ping\\n\\n\" HasData:false}", b.Raw, b.HasData)
	}
}

// An unterminated final block is dispatched (ReadFrame's EOF rule) and its
// Raw carries no blank line the upstream did not send.
func TestReadBlock_UnterminatedFinalBlock(t *testing.T) {
	blocks := readAllBlocks(t, NewReader(strings.NewReader("data: a\n\ndata: tail"), 0))
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(blocks), blocks)
	}
	last := blocks[1]
	if string(last.Raw) != "data: tail" || string(last.Data) != "tail" || !last.HasData {
		t.Errorf("final block = {Raw:%q Data:%q HasData:%v}", last.Raw, last.Data, last.HasData)
	}
}

// The BOM is stripped for parsing (so the first event keeps its type) but
// kept in Raw, so the client sees the stream as sent.
func TestReadBlock_LeadingBOMKeptInRaw(t *testing.T) {
	body := "\xEF\xBB\xBFevent: turn\ndata: x\n\n"
	r := NewReader(strings.NewReader(body), 0)
	b, err := r.ReadBlock()
	if err != nil {
		t.Fatalf("ReadBlock: %v", err)
	}
	if string(b.Raw) != body {
		t.Errorf("Raw = %q, want %q", b.Raw, body)
	}
	if string(b.Data) != "x" || string(r.LastEvent()) != "turn" {
		t.Errorf("Data = %q, LastEvent = %q; want \"x\", \"turn\"", b.Data, r.LastEvent())
	}
}

// An oversized block yields no bytes at all, so a relay cannot forward half of it.
func TestReadBlock_TooLargeReturnsNoBlock(t *testing.T) {
	body := "data: " + strings.Repeat("x", 64) + "\n\n"
	b, err := NewReader(strings.NewReader(body), 16).ReadBlock()
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if len(b.Raw) != 0 || len(b.Data) != 0 {
		t.Errorf("block = {Raw:%q Data:%q}, want empty", b.Raw, b.Data)
	}
}

// Non-data lines have a cap of their own: a stream of comments with no
// data line in sight cannot grow one block without bound.
func TestReadBlock_RawCapBoundsCommentFlood(t *testing.T) {
	var body bytes.Buffer
	for i := 0; i < 100; i++ {
		body.WriteString(": aaaaaaaaaa\n") // 13 bytes per line, no blank line
	}
	_, err := NewReader(&body, 64).ReadBlock() // non-data cap = 64
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

// Short data lines make Raw several times larger than Data ("data: a\n" is
// 8 bytes for 1). An event under the data cap must still pass, as it did
// before ReadBlock existed.
func TestReadBlock_ManyShortDataLinesUnderDataCap(t *testing.T) {
	body := strings.Repeat("data: a\n", 20) + "\n" // Data 39 bytes, Raw 161
	data, err := NewReader(strings.NewReader(body), 64).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v (Data is under the 64-byte cap)", err)
	}
	if len(data) != 39 {
		t.Errorf("len(Data) = %d, want 39", len(data))
	}
}
