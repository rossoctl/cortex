package apiclient

import (
	"bufio"
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
//
// A non-nil onProgress asks for progress, and gets each Progress line. A proxy without
// progress lines sends only the report, and onProgress is never called.
func (c *Client) Import(ctx context.Context, req importer.Request, onProgress func(importer.Progress)) (importer.Report, error) {
	const path = "/v1/import"
	req.Progress = onProgress != nil
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
	if resp.StatusCode == http.StatusOK {
		return readImportStream(resp.Body, path, onProgress)
	}
	// Bounded, as every body this client reads.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return importer.Report{}, err
	}
	switch resp.StatusCode {
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
}

// readImportStream reads the Progress lines and then the report of a 200. Each line is bounded,
// as every body this client reads: a report is one short line per agent.
func readImportStream(body io.Reader, path string, onProgress func(importer.Progress)) (importer.Report, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	var rep importer.Report
	found := false
	for sc.Scan() {
		var line struct {
			Progress *importer.Progress `json:"progress"`
			importer.Report
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return importer.Report{}, fmt.Errorf("%s: decode: %w", path, err)
		}
		if line.Progress == nil {
			rep, found = line.Report, true
			continue
		}
		if onProgress != nil {
			p := *line.Progress
			p.Agent = serverText(p.Agent) // printed to a terminal
			onProgress(p)
		}
	}
	if err := sc.Err(); err != nil {
		return importer.Report{}, fmt.Errorf("%s: %w", path, err)
	}
	if !found {
		return importer.Report{}, fmt.Errorf("%s: decode: no report", path)
	}
	// The caller prints these strings to a terminal.
	for i := range rep.Agents {
		a := &rep.Agents[i]
		a.Agent, a.Stopped, a.Error = serverText(a.Agent), serverText(a.Stopped), serverText(a.Error)
	}
	return rep, nil
}
