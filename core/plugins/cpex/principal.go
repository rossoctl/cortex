package cpex

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// codePrincipalUnavailable is the violation code for a tool call that
// needs the Cortex-held principal when its token file cannot be read.
const codePrincipalUnavailable = "principal_unavailable"

// codeDelegationUnavailable is the violation code for an allowed result
// whose delegate() step minted a credential the host cannot read.
const codeDelegationUnavailable = "delegation_unavailable"

// principalSource holds the caller token principal injection presents to
// CPEX identity resolution. The token lives only here and in the identity
// payload handed to CPEX; it is never written to pctx.
type principalSource struct {
	path   string
	header string // lowercase, as the identity payload keys headers

	mu    sync.Mutex
	token string
	// modTime and size describe the file token was read from, so a rotated
	// file is read again.
	modTime time.Time
	size    int64
}

// newPrincipalSource reads the token file. A missing or empty file is
// returned as an error alongside a usable source: requests that need the
// token are denied, and the file is read again on each of them until it
// appears.
func newPrincipalSource(cfg principalConfig) (*principalSource, error) {
	if cfg.TokenFile == "" {
		return nil, nil
	}
	p := &principalSource{path: cfg.TokenFile, header: strings.ToLower(strings.TrimSpace(cfg.Header))}
	_, err := p.load()
	return p, err
}

// Token returns the token, reading the file again when it has changed.
func (p *principalSource) Token() (string, bool) {
	tok, err := p.load()
	return tok, err == nil
}

// load stats the file on every call and reads it when its modification
// time or size differs from the cached token's. A failed or empty read
// keeps the cached token, so a rotation caught mid-write is retried on the
// next call rather than denying requests.
func (p *principalSource) load() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fi, err := os.Stat(p.path)
	if err == nil && p.token != "" && fi.ModTime().Equal(p.modTime) && fi.Size() == p.size {
		return p.token, nil
	}
	tok, rerr := p.read(err)
	if rerr != nil {
		if p.token != "" {
			return p.token, nil
		}
		return "", rerr
	}
	p.token, p.modTime, p.size = tok, fi.ModTime(), fi.Size()
	return tok, nil
}

func (p *principalSource) read(statErr error) (string, error) {
	if statErr != nil {
		return "", fmt.Errorf("read principal token_file %q: %w", p.path, statErr)
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return "", fmt.Errorf("read principal token_file %q: %w", p.path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("principal token_file %q is empty", p.path)
	}
	return tok, nil
}

// headerName is the principal header, or "" when injection is off.
func (p *principalSource) headerName() string {
	if p == nil {
		return ""
	}
	return p.header
}

var errPrincipalUnavailable = errors.New("principal token unavailable")

// injectsPrincipal reports whether a request identified as entityType gets
// the Cortex-held principal: only an outbound request, which the monitored
// agent sends, presented as a tool call or an inference request, so a
// taint on an llm route has a subject. An inbound caller is never asserted
// as the principal.
func injectsPrincipal(pctx *pipeline.Context, entityType string) bool {
	if pctx.Direction != pipeline.Outbound {
		return false
	}
	return entityType == "tool" || entityType == entityTypeLLM
}

// identityHeaders builds the header map handed to CPEX identity
// resolution. With principal injection configured the caller's own copy
// of the principal header is dropped, and with inject set the
// Cortex-held token takes its place; errPrincipalUnavailable means that
// token cannot be read and the request must be denied.
func identityHeaders(h http.Header, inject bool, p *principalSource) (map[string]string, error) {
	out := lowerHeaders(h)
	if p == nil {
		return out, nil
	}
	delete(out, p.header)
	if !inject {
		return out, nil
	}
	tok, ok := p.Token()
	if !ok {
		return nil, errPrincipalUnavailable
	}
	if out == nil {
		out = map[string]string{}
	}
	out[p.header] = tok
	return out, nil
}

// withoutHeader returns m without the header name (case-insensitive),
// leaving m itself unchanged.
func withoutHeader(m map[string]string, name string) map[string]string {
	if name == "" || len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if !strings.EqualFold(k, name) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lowerHeaders flattens http.Header into the lowercase-keyed single-value
// map the identity resolvers expect (they look their configured header up
// case-folded). Unlike flattenHeaders this keeps Authorization /
// X-User-Token — the jwt resolvers need the raw tokens — and does not
// strip the audit-sensitive set, because the IdentityPayload header map
// is consumed only by the in-process resolvers, never logged.
func lowerHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) == 0 {
			continue
		}
		if len(vs) == 1 {
			out[strings.ToLower(k)] = vs[0]
		} else {
			out[strings.ToLower(k)] = strings.Join(vs, ", ")
		}
	}
	return out
}
