package archive

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
)

const (
	// zstdWindow is the encoder's window. 1 MiB, with the default level and lower-memory mode,
	// measured 146x on the dedup stream at 3.0 MB of heap per open writer. A larger window buys
	// little once the long repeats are already references, and costs heap per open session.
	zstdWindow = 1 << 20

	// segmentExt and segmentLayout name a segment by the local time it was opened, so a human
	// listing a session's directory reads when each segment began.
	segmentExt    = ".jsonl.zst"
	segmentLayout = "20060102-150405"

	// dirMode and fileMode are the cost ledger's, for its reason and a stronger one: these files
	// hold raw prompts, completions and tool results.
	dirMode  = 0o700
	fileMode = 0o600

	// maxNameTries bounds the suffix search when segments open in the same second.
	maxNameTries = 100
)

// SegmentInfo is one segment's entry in session.json: enough to choose the segments a page
// overlaps, and to prune by age and size, without opening any of them.
type SegmentInfo struct {
	File      string    `json:"file"`
	FirstSeq  uint64    `json:"firstSeq"`
	LastSeq   uint64    `json:"lastSeq"`
	Events    int       `json:"events"`
	Bytes     int64     `json:"bytes"`
	LastWrite time.Time `json:"lastWrite"`
}

// segmentWriter appends events to one segment: a single zstd stream of JSON lines.
//
// A SEGMENT IS NEVER APPENDED TO BY A SECOND WRITER. createSegment opens with O_EXCL, and a
// process starts a new segment instead of reopening one, so a torn tail can only ever sit at the
// end of a segment nobody will write again. That is what lets a compressed stream survive a
// crash: there is no safe way to append after a torn zstd block, and this never has to.
//
// EACH SEGMENT IS SELF-CONTAINED. seen and blocks start empty, so every string and block the
// segment refers to is defined in it, and deleting one segment never breaks another.
//
// Not safe for concurrent use: one goroutine owns every writer.
type segmentWriter struct {
	f    *os.File
	out  countingWriter
	enc  *zstd.Encoder
	seen map[string]struct{}
	// blocks is the segment's written capture blocks, apart from seen; see splitBody.
	blocks map[string]struct{}
	inf    SegmentInfo
	dirty  bool
}

// countingWriter counts the compressed bytes the encoder has handed the file.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// createSegment creates a new segment in sessionDir, creating the directory if needed. It never
// opens an existing file: a name already taken in the same second gets a numeric suffix.
func createSegment(sessionDir string, opened time.Time) (*segmentWriter, error) {
	if err := os.MkdirAll(sessionDir, dirMode); err != nil {
		return nil, err
	}
	base := opened.Format(segmentLayout)
	var f *os.File
	var name string
	for i := range maxNameTries {
		name = base + segmentExt
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", base, i, segmentExt)
		}
		var err error
		f, err = os.OpenFile(filepath.Join(sessionDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	if f == nil {
		return nil, fmt.Errorf("archive: no free segment name for %s in %s after %d tries", base, sessionDir, maxNameTries)
	}
	// The new name must survive a crash, or every event flushed into the file is unreachable.
	if err := syncDir(sessionDir); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	w := &segmentWriter{f: f, seen: map[string]struct{}{}, blocks: map[string]struct{}{}, inf: SegmentInfo{File: name}}
	w.out.w = f
	enc, err := zstd.NewWriter(&w.out,
		zstd.WithWindowSize(zstdWindow),
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithLowerEncoderMem(true),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	w.enc = enc
	return w, nil
}

// append writes e — first the strings it refers to that this segment has not defined, then the
// event — and stamps now as the segment's last write. Nothing is durable until flush.
func (w *segmentWriter) append(e pipeline.SessionEvent, now time.Time) error {
	return w.appendCapture(e, nil, now)
}

// appendCapture is append for an event that carries its request's capture c, which may be nil:
// the blocks of c this segment has not written go before the event too.
//
// ONE WRITE PER EVENT, so a string or block record and the event referring to it reach the
// stream together. If the event cannot be encoded the strings and blocks are forgotten again:
// recording them as defined without writing them would make every later reference to them
// dangle.
func (w *segmentWriter) appendCapture(e pipeline.SessionEvent, c *pipeline.RequestCapture, now time.Time) error {
	stripped, r, fresh := split(e, w.seen)
	f, blocks := splitCapture(c, w.blocks)
	forget := func() {
		for _, s := range fresh {
			delete(w.seen, s.h)
		}
		for _, b := range blocks {
			delete(w.blocks, b.h)
		}
	}
	var buf []byte
	for _, s := range fresh {
		line, err := stringLine(s)
		if err != nil {
			forget()
			return err
		}
		buf = append(buf, line...)
	}
	for _, b := range blocks {
		line, err := blockLine(b)
		if err != nil {
			forget()
			return err
		}
		buf = append(buf, line...)
	}
	line, err := eventLine(stripped, r, f)
	if err != nil {
		forget()
		return err
	}
	buf = append(buf, line...)
	if _, err := w.enc.Write(buf); err != nil {
		return err
	}
	if w.inf.Events == 0 {
		w.inf.FirstSeq = e.Seq
	}
	w.inf.LastSeq = e.Seq
	w.inf.Events++
	w.inf.LastWrite = now
	w.dirty = true
	return nil
}

// flush makes every appended event durable: it ends the current zstd block and fsyncs. A
// decoder reads everything up to the last flush of a stream that never closed.
func (w *segmentWriter) flush() error {
	if !w.dirty {
		return nil
	}
	if err := w.enc.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	w.dirty = false
	return nil
}

// close finishes the frame, fsyncs and closes the file. The writer is unusable afterwards.
func (w *segmentWriter) close() error {
	eerr := w.enc.Close()
	serr := w.f.Sync()
	cerr := w.f.Close()
	return errors.Join(eerr, serr, cerr)
}

// info is the segment's session.json entry. Bytes counts what the encoder has handed the file,
// which after flush or close is the file's size.
func (w *segmentWriter) info() SegmentInfo {
	inf := w.inf
	inf.Bytes = w.out.n
	return inf
}

// syncDir fsyncs a directory, so a file just created in it survives a crash under its name.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	cerr := d.Close()
	return errors.Join(serr, cerr)
}
