package apiclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStream_ParsesEvents(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			t.Errorf("wrong path: %q", r.URL.Path)
		}
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, ": ok\n\n")
		flusher.Flush()
		fmt.Fprint(w, "event: session-event\nid: 1\ndata: {\"sessionId\":\"s1\",\"at\":\"2026-01-01T00:00:00Z\",\"direction\":\"inbound\",\"phase\":\"request\",\"a2a\":{\"method\":\"message/send\"}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "event: session-event\nid: 2\ndata: {\"sessionId\":\"s2\",\"at\":\"2026-01-01T00:00:01Z\",\"direction\":\"outbound\",\"phase\":\"response\"}\n\n")
		flusher.Flush()
		// Hold the connection open briefly so the test can observe the events
		// before reconnect-land kicks in.
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch := c.Stream(ctx, "")

	var gotEvents []string
	deadline := time.After(time.Second)
	for len(gotEvents) < 2 {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed before receiving 2 events; got %d", len(gotEvents))
			}
			if msg.Event != nil {
				gotEvents = append(gotEvents, msg.Event.SessionID)
			}
			// status messages (open, reconnecting) are ignored here.
		case <-deadline:
			t.Fatalf("timeout waiting for events; got %v", gotEvents)
		}
	}
	if gotEvents[0] != "s1" || gotEvents[1] != "s2" {
		t.Errorf("got %v, want [s1 s2]", gotEvents)
	}
}

func TestStream_IgnoresHeartbeats(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		// Three heartbeats followed by one event.
		fmt.Fprint(w, ": ok\n\n: heartbeat\n\n: heartbeat\n\n")
		fmt.Fprint(w, "event: session-event\ndata: {\"sessionId\":\"after-heartbeats\",\"at\":\"2026-01-01T00:00:00Z\",\"direction\":\"inbound\",\"phase\":\"request\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch := c.Stream(ctx, "")
	deadline := time.After(time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatal("closed without event")
			}
			if msg.Event != nil {
				if msg.Event.SessionID != "after-heartbeats" {
					t.Errorf("unexpected sessionID %q", msg.Event.SessionID)
				}
				return
			}
		case <-deadline:
			t.Fatal("timeout")
		}
	}
}

func TestStream_PassesSessionFilter(t *testing.T) {
	var got atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = c.Stream(ctx, "keep-this")
	// Give the goroutine a moment to hit the server before we cancel.
	time.Sleep(50 * time.Millisecond)
	if q := got.Load(); q == nil || q.(string) != "session=keep-this" {
		t.Errorf("query = %q, want session=keep-this", q)
	}
}

func TestStream_ReconnectsAfterServerClose(t *testing.T) {
	var connects int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connects, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		// First connection closes immediately; second stays open.
		if n == 1 {
			return
		}
		<-r.Context().Done()
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := c.Stream(ctx, "")

	var sawReconnecting, sawSecondOpen bool
	seenOpens := 0
	deadline := time.After(3 * time.Second)
	for !sawReconnecting || !sawSecondOpen {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed; reconnecting=%v secondOpen=%v", sawReconnecting, sawSecondOpen)
			}
			switch msg.Status.Phase {
			case "open":
				seenOpens++
				if seenOpens >= 2 {
					sawSecondOpen = true
				}
			case "reconnecting":
				sawReconnecting = true
			}
		case <-deadline:
			t.Fatalf("timeout; reconnecting=%v secondOpen=%v connects=%d",
				sawReconnecting, sawSecondOpen, atomic.LoadInt32(&connects))
		}
	}
	if atomic.LoadInt32(&connects) < 2 {
		t.Errorf("expected >=2 connects, got %d", atomic.LoadInt32(&connects))
	}
}

func TestStream_CancelClosesChannel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithCancel(context.Background())
	ch := c.Stream(ctx, "")

	// Let the stream open, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.After(time.Second)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return // channel closed as expected
				}
			case <-deadline:
				t.Error("channel not closed within 1s of cancel")
				return
			}
		}
	}()
	wg.Wait()
}

func TestBackoffSchedule_CapsAt30s(t *testing.T) {
	cases := map[int]time.Duration{
		1:  time.Second,
		2:  2 * time.Second,
		3:  4 * time.Second,
		4:  8 * time.Second,
		5:  16 * time.Second,
		6:  30 * time.Second,
		7:  30 * time.Second,
		99: 30 * time.Second,
	}
	for attempt, want := range cases {
		if got := backoffSchedule(attempt); got != want {
			t.Errorf("backoffSchedule(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// Backoff counts consecutive FAILED connects, not a lifetime of disconnects
// (#1568): once a connection reaches "open", the counter resets, so five
// disconnects spread over a day each wait 1s instead of the capped 30s.
func TestStream_BackoffResetsAfterOpen(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		// Every connection opens, then closes — each is a successful open
		// followed by a disconnect.
		<-time.After(50 * time.Millisecond)
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := c.Stream(ctx, "")

	reconnects := 0
	deadline := time.After(5 * time.Second)
	for reconnects < 3 {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d reconnects", reconnects)
			}
			if msg.Status.Phase == "reconnecting" {
				reconnects++
				if msg.Status.Attempt != 1 {
					t.Fatalf("reconnect #%d carried attempt %d, want 1 — backoff did not reset after a successful open",
						reconnects, msg.Status.Attempt)
				}
			}
		case <-deadline:
			t.Fatalf("timeout after %d reconnects", reconnects)
		}
	}
}

// A frame larger than the reader's cap must cost one frame, not the
// connection (#1568): the old bufio.Scanner died with ErrTooLong past 4 MiB
// — a size real events reach, since the server marshals full inference
// bodies — and everything published during the reconnect gap was lost.
// Here the oversized frame is 5 MiB (over the old limit, under the new cap)
// and the events on either side of it must both arrive.
func TestStream_SkipsOversizedFrame(t *testing.T) {
	valid := "event: session-event\ndata: {\"sessionId\":\"s1\",\"at\":\"2026-01-01T00:00:00Z\",\"direction\":\"inbound\",\"phase\":\"request\"}\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, valid)
		flusher.Flush()
		fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", 5*1024*1024))
		flusher.Flush()
		fmt.Fprint(w, valid)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	c := New(ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := c.Stream(ctx, "")

	var gotEvents int
	deadline := time.After(3 * time.Second)
	for gotEvents < 2 {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d events — the oversized frame killed the connection", gotEvents)
			}
			if msg.Event != nil {
				gotEvents++
			}
		case <-deadline:
			t.Fatalf("timeout after %d events; the oversized frame killed the connection", gotEvents)
		}
	}
}

// lineScan's contract, at a cap small enough to exercise directly: a line
// within the cap passes through untouched, a longer one comes back
// truncated with the reader positioned at the NEXT line.
func TestLineScan_TruncatesInsteadOfFailing(t *testing.T) {
	ls := newLineScan(strings.NewReader("short\n"+strings.Repeat("y", 64)+"\nalso short\n"), 16)

	line, trunc, err := ls.next()
	if err != nil || trunc || line != "short" {
		t.Fatalf("first line = %q trunc=%v err=%v, want \"short\" untouched", line, trunc, err)
	}
	line, trunc, err = ls.next()
	if err != nil {
		t.Fatalf("oversized line errored: %v — the reader must survive it", err)
	}
	if !trunc {
		t.Fatal("64-byte line at a 16-byte cap must report truncated")
	}
	if len(line) != 16 {
		t.Fatalf("truncated line kept %d bytes, want the 16-byte cap", len(line))
	}
	line, trunc, err = ls.next()
	if err != nil || trunc || line != "also short" {
		t.Fatalf("line after the oversized one = %q trunc=%v err=%v, want \"also short\" — reader position was lost", line, trunc, err)
	}
	if _, _, err = ls.next(); err != io.EOF {
		t.Fatalf("exhausted reader returned %v, want io.EOF", err)
	}
}
