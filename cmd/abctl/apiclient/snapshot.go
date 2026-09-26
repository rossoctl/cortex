package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// GetSessionPage fetches up to limit events ending just BEFORE the given Seq — the page
// older than a cursor the caller already holds. before == 0 means "from the newest", so
// the first request and every later page are the same call.
//
// The response is decoded one event at a time rather than as a document, and repeated
// strings are collapsed as they arrive. Both matter more here than anywhere else in this
// client: a snapshot of a real session is hundreds of megabytes of JSON, and abctl was
// holding more memory than the proxy it was watching — 1.64GB against 1.03GB, and the
// only one of the two still climbing. See decodeSessionView.
func (c *Client) GetSessionPage(ctx context.Context, id string, before uint64, limit int) (*pipeline.SessionView, error) {
	// view=summary on every timeline fetch, tail and page alike: the events table
	// renders no message body, and carrying them made opening a session a
	// multi-second wait — 99.5 MiB and 1.32s on the wire for one 500-event
	// response, against roughly half a megabyte projected. The full event is
	// fetched per row by GetEvent when the operator opens the detail pane.
	//
	// An older proxy ignores the parameter and returns full events, so this stays
	// correct against one, just as slow as before. view.View says which happened.
	path := fmt.Sprintf("/v1/sessions/%s?limit=%d&view=%s", url.PathEscape(id), limit, SummaryView)
	if before > 0 {
		path += fmt.Sprintf("&before=%d", before)
	}
	body, err := c.getBody(ctx, path)
	if err != nil {
		return nil, err
	}
	defer body.Close() //nolint:errcheck // read-only body

	view, err := decodeSessionView(body)
	if err != nil {
		return nil, fmt.Errorf("%s: decode: %w", path, err)
	}
	return view, nil
}

// GetEvent fetches one event in full, payloads included.
//
// The counterpart to view=summary: the timeline no longer carries message bodies, so
// the detail pane fetches the row under the cursor. This is the one thing the
// projection makes slower — a round trip where it used to read from memory — and it
// buys a session that opens in milliseconds instead of seconds.
//
// Not decoded through decodeSessionView's interning path: that exists to stop a
// hundred-megabyte document being held whole, and this is one event.
func (c *Client) GetEvent(ctx context.Context, id string, seq uint64) (*pipeline.SessionEvent, error) {
	path := fmt.Sprintf("/v1/sessions/%s/events/%d", url.PathEscape(id), seq)
	var ev pipeline.SessionEvent
	if err := c.getJSON(ctx, path, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// decodeSessionView reads a session snapshot event by event, interning as it goes.
//
// WHY NOT Decode into a SessionView: json.Decoder only bounds its buffer between VALUES,
// and a whole snapshot is one value — so decoding the document whole means holding all of
// it in memory at once, on top of the structs it produces. Walking the object and decoding
// each element of "events" separately lets the decoder slide consumed bytes out of its
// buffer (encoding/json's refill compacts at scanp), so the buffer stays proportional to
// the largest single event instead of to the response. This is the client-side half of
// what writeSessionView does on the server; BenchmarkDecodeSessionViewHeap measures it
// rather than trusting the reasoning.
//
// The interner is the other half, and it is not an alternative to streaming: streaming
// bounds the bytes in flight, interning bounds what is KEPT. An LLM request re-sends the
// whole conversation, so consecutive events repeat almost all of their message text, and
// the server's own encoder expanded every one of those shares back into its own bytes on
// the way out. One Interner per response, fed events in order, collapses them again.
//
// Sharing stops at the response boundary: a client paging backward gets a fresh table per
// page, so identical messages in two different pages are held twice. Within-page
// duplication is the dominant term — the same reason the store's own table only spans one
// event — and a table outliving its page would pin content whose page the caller dropped.
func decodeSessionView(r io.Reader) (*pipeline.SessionView, error) {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}

	view := &pipeline.SessionView{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key, got %v", tok)
		}
		switch key {
		case "events":
			if err := decodeEvents(dec, view); err != nil {
				return nil, err
			}
		case "id":
			if err := dec.Decode(&view.ID); err != nil {
				return nil, err
			}
		case "totalEvents":
			if err := dec.Decode(&view.TotalEvents); err != nil {
				return nil, err
			}
		case "oldestSeq":
			if err := dec.Decode(&view.OldestSeq); err != nil {
				return nil, err
			}
		case "view":
			// Read, not skipped: this is how abctl learns whether the projection it
			// asked for was actually applied. Absent means the proxy predates it.
			if err := dec.Decode(&view.View); err != nil {
				return nil, err
			}
		default:
			// Skipped, not rejected: a newer proxy may send fields this abctl does not
			// know, and the decode has to consume the value either way to stay in sync
			// with the token stream.
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
	}
	// The closing brace. Reading it keeps the decoder's position honest for anything that
	// later wants to read past this value.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return view, nil
}

// decodeEvents fills view.Events from the array (or null) positioned at the decoder.
func decodeEvents(dec *json.Decoder, view *pipeline.SessionView) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	// "events": null is what the server sends for a session with no events, and it is not
	// an error — SessionView's own encoder emits it for a nil slice.
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf(`expected "events" to be an array or null, got %v`, tok)
	}

	// Non-nil before the loop, so an empty array decodes to an empty slice and only null
	// decodes to nil — which is what encoding/json does, and the difference survives a
	// re-encode as "events":[] against "events":null.
	if view.Events == nil {
		view.Events = []pipeline.SessionEvent{}
	}

	var in session.Interner
	for dec.More() {
		var ev pipeline.SessionEvent
		if err := dec.Decode(&ev); err != nil {
			return err
		}
		// In arrival order, which is what the rolling table needs: each event interns
		// against the one before it.
		in.InternEvent(&ev)
		view.Events = append(view.Events, ev)
	}
	_, err = dec.Token() // ']'
	return err
}

// expectDelim consumes one token and requires it to be the given delimiter.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, got %v", want, tok)
	}
	return nil
}
