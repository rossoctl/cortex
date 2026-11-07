package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// ErrNoSearch is the answer of a proxy that has no search route, because it predates it.
var ErrNoSearch = errors.New("apiclient: this Cortex has no details search")

// SearchResult is GET /v1/sessions/{id}/search: the seqs of the session's events whose bodies
// or capture hold the query, ascending, and the newest seq the proxy read.
type SearchResult struct {
	Seqs    []uint64 `json:"seqs"`
	LastSeq uint64   `json:"lastSeq"`
}

// maxSearchBody bounds the answer: one int for each event of a session.
const maxSearchBody = 16 << 20

// SearchSession asks the proxy which of session id's events hold q in what view=summary drops.
// It reads a 404's body itself, which getBody does not do: the route answers an unknown session
// with a JSON body (ErrNotFound), and a proxy without the route with the mux's plain text
// (ErrNoSearch). A long session takes seconds, so the default deadline is the stream's.
func (c *Client) SearchSession(ctx context.Context, id, q string) (SearchResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, streamDefaultTimeout)
		defer cancel()
	}
	path := fmt.Sprintf("/v1/sessions/%s/search?q=%s", url.PathEscape(id), url.QueryEscape(q))
	req, err := http.NewRequestWithContext(ctx, "GET", c.endpoint+path, nil)
	if err != nil {
		return SearchResult{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return SearchResult{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchBody))
	if err != nil {
		return SearchResult{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var out SearchResult
		if err := json.Unmarshal(body, &out); err != nil {
			return SearchResult{}, fmt.Errorf("%s: decode: %w", path, err)
		}
		return out, nil
	case http.StatusNotFound:
		if bytes.Contains(body, []byte(`"session not found"`)) {
			return SearchResult{}, fmt.Errorf("%s: %w", path, ErrNotFound)
		}
		return SearchResult{}, fmt.Errorf("%s: %w", path, ErrNoSearch)
	}
	return SearchResult{}, fmt.Errorf("%s: status %d", path, resp.StatusCode)
}
