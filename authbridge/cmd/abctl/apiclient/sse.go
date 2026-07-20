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

	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
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

	attempt := 0
	for {
		err := c.streamOnce(ctx, filter, out)
		if ctx.Err() != nil {
			// Caller cancelled — terminal state is the channel close on defer.
			return
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
// Returns nil only if the server closed cleanly.
func (c *Client) streamOnce(ctx context.Context, filter string, out chan<- StreamEvent) error {
	u := c.endpoint + "/v1/events"
	if filter != "" {
		u += "?session=" + filter
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	// Reuse the shared no-timeout client so the Transport's idle-connection
	// pool survives across reconnects.
	resp, err := c.httpStream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream: unexpected status %d", resp.StatusCode)
	}
	sendStatus(out, StreamStatus{Phase: "open"})

	return parseSSE(ctx, resp.Body, out)
}

// parseSSE reads SSE frames line-by-line. A frame is a run of lines ending
// in a blank line. Lines starting with ':' are comments (our heartbeats).
// We only care about frames with a `data:` line containing a JSON-encoded
// SessionEvent; other line types are tolerated but ignored.
func parseSSE(ctx context.Context, r io.Reader, out chan<- StreamEvent) error {
	scanner := bufio.NewScanner(r)
	// Some events (e.g. full LLM completion bodies) can be many KB. Raise the
	// line buffer well above the default 64 KB to match the server's cap.
	scanner.Buffer(make([]byte, 0, 16*1024), 4*1024*1024)

	var dataBuf strings.Builder

	flush := func() {
		defer dataBuf.Reset()
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

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// Comment / heartbeat — ignore.
		case strings.HasPrefix(line, "data:"):
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
	// Drain trailing frame if the stream ended mid-record (rare; usual case
	// is a clean "\n\n" before EOF).
	flush()
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return io.EOF
}

func sendStatus(out chan<- StreamEvent, s StreamStatus) {
	// Non-blocking best-effort — the TUI reads promptly; losing a status
	// update during catastrophe is acceptable.
	select {
	case out <- StreamEvent{Status: s}:
	default:
	}
}
