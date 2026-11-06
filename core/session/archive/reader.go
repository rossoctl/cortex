package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
)

// maxDecoderWindow caps the window a segment may declare. The writer uses 1 MiB; the cap sits
// above it so a later writer can grow, and well below the decoder's default so a damaged or
// crafted file cannot choose how much memory a read allocates.
const maxDecoderWindow = 8 << 20

// blockPrefix is how every block line the writer produces begins.
var blockPrefix = []byte(`{"k":"b",`)

// readSegment calls fn for each event in the segment at path, in file order, with every
// reference restored, until fn returns false.
//
// truncated reports that the stream stopped before a clean end: a torn tail, an undecodable
// line, or a reference the segment never defined. Every event before that point was delivered,
// and none after it — a reader never serves a blanked field as if it were the original. A
// segment still being written reads as truncated too, since its frame has not ended; that is
// expected, and a caller who knows the segment is open can ignore it.
//
// err is for what is not a property of the data: the file could not be opened, or it declares
// a window past maxDecoderWindow.
func readSegment(path string, fn func(e *pipeline.SessionEvent) bool) (truncated bool, err error) {
	return readRecords(path, nil, nil, func(e *pipeline.SessionEvent, _ json.RawMessage) bool { return fn(e) })
}

// readRecords is readSegment, handing fn each event's capture record too, undecoded, nil when it
// has none. It keeps the block records whose hash is in want in blocks. A nil want keeps none,
// and does not even decode them: a read that does not rebuild a body holds no blocks.
func readRecords(path string, want map[string]struct{}, blocks map[string]string,
	fn func(e *pipeline.SessionEvent, c json.RawMessage) bool) (truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	dec, err := zstd.NewReader(f,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(maxDecoderWindow),
		zstd.WithDecoderLowmem(true))
	if err != nil {
		return false, err
	}
	defer dec.Close()

	// The segment's strings, by hash. Every event this read delivers points into these, so the
	// events share one copy of each string — the dedup the segment stored survives the read.
	table := map[string]string{}
	br := bufio.NewReaderSize(dec, 64<<10)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				return true, nil // a partial last line: the tail was torn mid-record
			}
			// The writer's json.Marshal puts the kind first, so a block nobody wants is skipped
			// before it is decoded. A block written any other way is decoded and dropped below.
			if want == nil && bytes.HasPrefix(line, blockPrefix) {
				continue
			}
			var rec record
			if json.Unmarshal(line, &rec) != nil {
				return true, nil
			}
			switch rec.K {
			case kindString:
				table[rec.H] = rec.V
			case kindBlock:
				if _, ok := want[rec.H]; ok {
					if rec.X != nil {
						blocks[rec.H] = string(rec.X)
					} else {
						blocks[rec.H] = rec.V
					}
				}
			case kindEvent:
				if rec.E == nil || !join(rec.E, rec.R, table) {
					return true, nil
				}
				if !fn(rec.E, rec.F) {
					return false, nil
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return false, nil
			}
			if errors.Is(rerr, zstd.ErrWindowSizeExceeded) {
				return false, rerr
			}
			return true, nil
		}
	}
}
