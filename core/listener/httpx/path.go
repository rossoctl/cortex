package httpx

import (
	"net/url"
	"strings"
)

// PathOnly extracts the URL path from a raw request target. The Envoy-fed
// listener (ext_proc's :path pseudo-header) receives the full request target,
// query string included, but pctx.Path must hold only the path — see
// pipeline.Context.Path. It runs the same parser net/http runs for the
// proxy listeners, so pctx.Path is identical across listener modes
// (percent-decoding included), modulo targets that parser rejects: net/http
// answers those with 400 before any pipeline runs, while the Envoy-fed
// listener falls back to a plain query strip.
func PathOnly(target string) string {
	u, err := url.ParseRequestURI(target)
	if err != nil {
		if i := strings.IndexByte(target, '?'); i >= 0 {
			return target[:i]
		}
		return target
	}
	return u.Path
}
