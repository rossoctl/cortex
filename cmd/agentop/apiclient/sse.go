package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// StreamEvent is what Stream publishes. One of Event or Status is set per
// message — never both. The TUI treats Status as UI-only state (banner,
// footer); Event as authoritative session data.
type StreamEvent struct {
	Event  *pipeline.SessionEvent
	Status StreamStatus
}

// StreamStatus is a non-data transition on the stream. Terminal states are
// signalled by the channel closing; StreamStatus is only for live transitions
// the UI wants to render as a banner (open, reconnecting-with-backoff).
type StreamStatus struct {
	Phase   string // "open" | "reconnecting"
	Attempt int    // reconnect attempt count (0 for open)
	Err     error
	// Wait is how long until the next connect attempt, for the "reconnecting"
	// phase. Zero for other phases.
	Wait time.Duration
}

// maxBackoff caps the reconnect wait. Tune here to change the upper bound
// on how long the TUI sits waiting for a flapping server.
const maxBackoff = 30 * time.Second

// backoffSchedule returns the wait for the Nth reconnect attempt (1-based).
// Schedule: 1s, 2s, 4s, 8s, 16s, then maxBackoff forever.
func backoffSchedule(attempt int) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	d := time.Second << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

// Stream opens an SSE connection to /v1/events and publishes SessionEvents
// to the returned channel until ctx is cancelled. On transport errors it
// auto-reconnects with exponential backoff (1s..30s, capped, indefinite).
// Heartbeat comments are discarded silently.
//
// The caller owns ctx — cancelling it both stops reconnect attempts and
// closes the returned channel. Status transitions (open, reconnecting,
// closed) are published on the same channel so the UI can render a banner
// without a side-channel.
//
// Optional sessionFilter narrows the stream to one session server-side.
func (c *Client) Stream(ctx context.Context, sessionFilter string) <-chan StreamEvent {
	out := make(chan StreamEvent, 256)
	go c.streamLoop(ctx, sessionFilter, out)
	return out
}

func (c *Client) streamLoop(ctx context.Context, filter string, out chan<- StreamEvent) {
	defer close(out)

	// attempt is consecutive FAILED connects, not a lifetime counter: it
	// resets once a connection reaches "open", so five disconnects spread
	// over a day (laptop sleep, proxy restart) each wait 1s, not the capped
	// 30s — and the banner's attempt number describes the current outage
	// rather than the process's age (#1568).
	attempt := 0
	for {
		opened, err := c.streamOnce(ctx, filter, out)
		if ctx.Err() != nil {
			// Caller cancelled — terminal state is the channel close on defer.
			return
		}
		if opened {
			attempt = 0
		}
		attempt++
		wait := backoffSchedule(attempt)
		sendStatus(out, StreamStatus{Phase: "reconnecting", Attempt: attempt, Wait: wait, Err: err})
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// streamOnce opens a single SSE connection and pumps events to out until
// either the server closes, ctx is cancelled, or a transport error occurs.
// opened reports whether the connection got as far as the "open" status —
// the caller resets its backoff on it. Returns a nil error only on a clean
// server close.
func (c *Client) streamOnce(ctx context.Context, filter string, out chan<- StreamEvent) (opened bool, err error) {
	u := c.endpoint + "/v1/events"
	if filter != "" {
		u += "?session=" + filter
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")

	// Reuse the shared no-timeout client so the Transport's idle-connection
	// pool survives across reconnects.
	resp, err := c.httpStream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("stream: unexpected status %d", resp.StatusCode)
	}
	sendStatus(out, StreamStatus{Phase: "open"})

	return true, parseSSE(ctx, resp.Body, out)
}

// maxSSELine bounds how much of one SSE line parseSSE keeps. NOT a wire
// limit: the server has no cap on event size (full inference bodies,
// against a proxy that accepts 32 MiB request bodies), so no fixed bound
// covers every legitimate event. A line longer than this marks its frame
// as oversized, and the frame is skipped — the connection survives.
const maxSSELine = 32 * 1024 * 1024

// parseSSE reads SSE frames line-by-line. A frame is a run of lines ending
// in a blank line. Lines starting with ':' are comments (our heartbeats).
// We only care about frames with a `data:` line containing a JSON-encoded
// SessionEvent; other line types are tolerated but ignored.
func parseSSE(ctx context.Context, r io.Reader, out chan<- StreamEvent) error {
	lines := newLineScan(r, maxSSELine)

	var dataBuf strings.Builder
	// oversized marks the frame in progress as unparseable — one of its
	// lines was truncated at the cap. The frame is dropped at its blank
	// line and the stream keeps reading; before this, a bufio.Scanner
	// ErrTooLong killed the connection, and every event published during
	// the reconnect gap was lost (#1568).
	oversized := false

	flush := func() {
		defer dataBuf.Reset()
		bad := oversized
		oversized = false
		if bad {
			slog.Debug("apiclient: skipping oversized SSE frame", "bytes", dataBuf.Len())
			return
		}
		if dataBuf.Len() == 0 {
			return
		}
		var ev pipeline.SessionEvent
		raw := dataBuf.String()
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			// Corrupt frame — the TUI doesn't need to distinguish malformed
			// frames from missed ones, but log at debug level so a
			// server/client schema drift is diagnosable during a live
			// incident rather than events silently vanishing.
			preview := raw
			if len(preview) > 200 {
				preview = preview[:200] + "…"
			}
			slog.Debug("apiclient: dropping malformed SSE frame", "err", err, "preview", preview)
			return
		}
		select {
		case out <- StreamEvent{Event: &ev}:
		case <-ctx.Done():
		}
	}

	for {
		line, truncated, err := lines.next()
		if err != nil {
			// Drain trailing frame if the stream ended mid-record (rare;
			// usual case is a clean "\n\n" before EOF).
			flush()
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return err
		}
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// Comment / heartbeat — ignore.
		case strings.HasPrefix(line, "data:"):
			if truncated {
				oversized = true
			}
			val := strings.TrimPrefix(line, "data:")
			val = strings.TrimPrefix(val, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(val)
		default:
			// `event:` / `id:` / `retry:` — not needed by this client.
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// lineScan is bufio.Scanner's line loop with one difference that matters
// here: a line longer than max does not fail the reader. The line comes
// back truncated (at most max bytes, truncated=true) and the reader is
// positioned at the next line, so one huge frame costs one frame instead
// of the connection.
type lineScan struct {
	br  *bufio.Reader
	max int
}

func newLineScan(r io.Reader, max int) *lineScan {
	return &lineScan{br: bufio.NewReaderSize(r, 16*1024), max: max}
}

// next returns the next line without its terminator. truncated reports
// that the line exceeded the cap and was cut. A terminal error is returned
// with whatever remained buffered; io.EOF means clean end of stream.
func (s *lineScan) next() (line string, truncated bool, err error) {
	var buf []byte
	for {
		frag, rerr := s.br.ReadSlice('\n')
		switch rerr {
		case nil:
			frag = frag[:len(frag)-1] // drop the '\n'
		case bufio.ErrBufferFull:
			// Line longer than the read buffer: keep accumulating if within
			// the cap, otherwise discard the overflow and remember it.
			if len(buf)+len(frag) > s.max {
				if len(buf) < s.max {
					buf = append(buf, frag[:s.max-len(buf)]...)
				}
				truncated = true
			} else {
				buf = append(buf, frag...)
			}
			continue
		case io.EOF:
			if len(frag) == 0 && len(buf) == 0 {
				return "", false, io.EOF
			}
			// Final line without a trailing newline.
		default:
			return string(buf), truncated, rerr
		}
		if len(buf)+len(frag) > s.max {
			if len(buf) < s.max {
				buf = append(buf, frag[:s.max-len(buf)]...)
			}
			truncated = true
		} else {
			buf = append(buf, frag...)
		}
		return string(buf), truncated, nil
	}
}

func sendStatus(out chan<- StreamEvent, s StreamStatus) {
	// Non-blocking best-effort — the TUI reads promptly; losing a status
	// update during catastrophe is acceptable.
	select {
	case out <- StreamEvent{Status: s}:
	default:
	}
}
