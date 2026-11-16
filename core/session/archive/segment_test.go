package archive

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// rawLines decodes a segment's zstd stream without the package reader, so the writer is tested
// against the format rather than against its own reader. A stream still open for writing ends
// mid-frame; every complete line before that point is returned.
func rawLines(t *testing.T, path string) []record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	br := bufio.NewReader(dec)
	var out []record
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var rec record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				t.Fatalf("undecodable line %q: %v", line, jerr)
			}
			out = append(out, rec)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("decode: %v", err)
			}
			return out
		}
	}
}

func appendAll(t *testing.T, w *segmentWriter, at time.Time, n int) {
	t.Helper()
	for _, e := range synthSession(7, n, 2048) {
		if err := w.append(e, at); err != nil {
			t.Fatal(err)
		}
	}
}

// A process never appends to an existing segment, so two segments opened in the same second
// must get two names rather than share a file.
func TestCreateSegment_NamesByLocalOpenTimeAndNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	opened := time.Date(2026, 10, 1, 17, 23, 54, 0, time.Local)
	a, err := createSegment(dir, opened)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	b, err := createSegment(dir, opened)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	if a.info().File != "20261001-172354.jsonl.zst" || b.info().File != "20261001-172354-1.jsonl.zst" {
		t.Fatalf("names = %q, %q", a.info().File, b.info().File)
	}
}

// Segments hold raw prompts and completions: readable by the user only.
func TestCreateSegment_ModesAre0600And0700(t *testing.T) {
	skipWithoutUnixModes(t)
	dir := filepath.Join(t.TempDir(), "sess")
	w, err := createSegment(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, w.info().File))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("modes: dir %o, file %o", di.Mode().Perm(), fi.Mode().Perm())
	}
}

// Durability between flushes and close is the whole point of flushing: a process killed with
// the segment open must leave every flushed event readable.
func TestSegmentWriter_AFlushedEventIsReadableWithoutClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	w, err := createSegment(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	appendAll(t, w, time.Now(), 4) // 12 events
	if err := w.flush(); err != nil {
		t.Fatal(err)
	}
	events := 0
	for _, rec := range rawLines(t, filepath.Join(dir, w.info().File)) {
		if rec.K == kindEvent {
			events++
		}
	}
	if events != 12 {
		t.Fatalf("%d events readable after flush, want 12", events)
	}
}

// Any prefix of a segment must decode on its own, which needs every string defined before the
// first event that refers to it.
func TestSegmentWriter_StringRecordsPrecedeTheirEvent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	w, err := createSegment(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, w, time.Now(), 5)
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	refsSeen := 0
	for _, rec := range rawLines(t, filepath.Join(dir, w.info().File)) {
		if rec.K == kindString {
			defined[rec.H] = true
			continue
		}
		if rec.R == nil {
			continue
		}
		var all []*string
		all = append(all, rec.R.M...)
		all = append(all, rec.R.C)
		all = append(all, rec.R.TD...)
		all = append(all, rec.R.TP...)
		all = append(all, rec.R.AP...)
		for _, h := range all {
			if h == nil {
				continue
			}
			refsSeen++
			if !defined[*h] {
				t.Fatalf("event seq %d refers to %s before it is defined", rec.E.Seq, *h)
			}
		}
	}
	if refsSeen == 0 {
		t.Fatal("fixture produced no references")
	}
}

func TestSegmentWriter_InfoTracksSeqRangeEventsBytesAndLastWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	w, err := createSegment(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
	appendAll(t, w, at, 3) // seqs 1..9
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	info := w.info()
	fi, err := os.Stat(filepath.Join(dir, info.File))
	if err != nil {
		t.Fatal(err)
	}
	if info.FirstSeq != 1 || info.LastSeq != 9 || info.Events != 9 || !info.LastWrite.Equal(at) || info.Bytes != fi.Size() {
		t.Fatalf("info = %+v, file is %d bytes", info, fi.Size())
	}
}
