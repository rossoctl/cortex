package routerconfig

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Resolve is the model word names among ids, a server's model list, and "" when it
// names none. Of the ids whose name — everything up to the last "/" dropped —
// contains word, case ignored, it is the one with the highest version, and of those
// the shortest name, so a model listed under several spellings resolves the same
// way every time.
//
// A version is the numbers in the name, read left to right, a trailing date left
// out: claude-opus-5-5 beats claude-opus-5, which beats claude-opus-4-8. Only
// numbers standing alone count, so a size such as the 550B in
// NVIDIA-Nemotron-3-Ultra-550B is not taken for one.
//
// The newest is what a server's main and helper words resolve to, so that a server
// adding a newer model needs no edit to be used. The router and agentop both resolve
// with this, so what agentop shows is what the router sends.
func Resolve(word string, ids []string) string {
	word = strings.ToLower(word)
	best := ""
	var bestVersion []int
	for _, id := range ids {
		name := baseName(id)
		if word == "" || !strings.Contains(strings.ToLower(name), word) {
			continue
		}
		v := version(name)
		if best == "" {
			best, bestVersion = id, v
			continue
		}
		switch c := slices.Compare(v, bestVersion); {
		case c > 0, c == 0 && cmp.Or(cmp.Compare(len(id), len(best)), strings.Compare(id, best)) < 0:
			best, bestVersion = id, v
		}
	}
	return best
}

// baseName is id with everything up to its last "/" dropped: the model's own name
// without the provider's prefix, aws/ or rits/zai-org/.
func baseName(id string) string { return id[strings.LastIndex(id, "/")+1:] }

// trailingDate is a date a provider appends to a model's name: -20251001 or
// -2025-08-07.
var trailingDate = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// version is the numbers standing alone in name, a trailing date left out.
func version(name string) []int {
	name = trailingDate.ReplaceAllString(name, "")
	var v []int
	for _, tok := range strings.FieldsFunc(name, func(r rune) bool {
		return !('0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z')
	}) {
		if n, err := strconv.Atoi(tok); err == nil {
			v = append(v, n)
		}
	}
	return v
}

// maxModelList bounds the model list FetchModels reads.
const maxModelList = 4 << 20

// FetchModels is the model list a server serves to key: the ids its
// GET /v1/models answers with. Its errors quote no part of the URL or the key.
//
// The list is a lower bound. A LiteLLM server also serves names it does not list,
// aliases of listed models, so it is good for resolving a main or helper word and
// for showing what a server offers, and not for judging whether a name the agent
// asked for is served. The router lets the server's own answer judge that.
func FetchModels(ctx context.Context, client *http.Client, ep Endpoint, key string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL()+"/v1/models", nil)
	if err != nil {
		return nil, errors.New("could not build the request for the server's model list")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // its text quotes the URL
		}
		return nil, fmt.Errorf("could not reach the server: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered %d to its model list", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelList)).Decode(&list); err != nil {
		return nil, errors.New("the server's model list is not an OpenAI-style list of models")
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// NewModelsClient is the client FetchModels is meant to be given: the system's
// roots, as the TLS bridge's upstream client trusts, and no proxy, so a proxy's own
// fetch never goes back through itself.
func NewModelsClient() *http.Client {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
