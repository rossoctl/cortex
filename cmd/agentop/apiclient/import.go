package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/rossoctl/cortex/core/session/importer"
)

// ErrImportRunning is a 409 from POST /v1/import: the proxy runs an import already.
var ErrImportRunning = errors.New("an import is already running")

// Import asks the proxy to import agent transcripts, and returns its report. A 404 returns
// ErrNotFound: the proxy has no import route (archive off, or not loopback-only). A 400 returns
// an error that wraps ErrBadRequest and holds the server's reason.
//
// The caller sets the deadline. The proxy sends the header before it starts the work, so the
// transport's HeaderTimeout does not limit a long import.
func (c *Client) Import(ctx context.Context, req importer.Request) (importer.Report, error) {
	const path = "/v1/import"
	body, err := json.Marshal(req)
	if err != nil {
		return importer.Report{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return importer.Report{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	// Never follow a redirect, as sendDelete: a 307 sends the POST again to wherever Location points.
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(hreq)
	if err != nil {
		return importer.Report{}, err
	}
	defer resp.Body.Close()
	// Bounded, as every body this client reads. A report is one short line per agent.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return importer.Report{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusConflict:
		return importer.Report{}, ErrImportRunning
	case http.StatusNotFound:
		return importer.Report{}, fmt.Errorf("%s: %w", path, ErrNotFound)
	case http.StatusBadRequest:
		if detail := badRequestDetail(data); detail != "" {
			return importer.Report{}, fmt.Errorf("%s: %w: %s", path, ErrBadRequest, detail)
		}
		return importer.Report{}, fmt.Errorf("%s: %w", path, ErrBadRequest)
	default:
		return importer.Report{}, fmt.Errorf("%s: unexpected status %d", path, resp.StatusCode)
	}
	var rep importer.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return importer.Report{}, fmt.Errorf("%s: decode: %w", path, err)
	}
	// The caller prints these strings to a terminal.
	for i := range rep.Agents {
		a := &rep.Agents[i]
		a.Agent, a.Stopped, a.Error = serverText(a.Agent), serverText(a.Stopped), serverText(a.Error)
	}
	return rep, nil
}
