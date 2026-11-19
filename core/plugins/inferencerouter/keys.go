package inferencerouter

import (
	"cmp"
	"crypto/subtle"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/rossoctl/cortex/core/auth"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// serverKey is one server's name and key, for keyOwner.
type serverKey struct{ name, key string }

// buildKeys gives the keys keyOwner compares, sorted by server name, and the
// canonical names of the headers that a server's auth_header names, other than
// Authorization and X-Api-Key, which keyOwner reads always. A key that two or more
// servers share is not in the list, so a request that sends it selects no server.
// For each such key, buildKeys logs one WARN that names the servers and not the key.
func buildKeys(servers map[string]routerconfig.Server) (keys []serverKey, headers []string) {
	byKey := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		if s.Key != "" {
			byKey[s.Key] = append(byKey[s.Key], name)
		}
		switch h := http.CanonicalHeaderKey(s.AuthHeader); h {
		case "", "Authorization", "X-Api-Key":
		default:
			if !slices.Contains(headers, h) {
				headers = append(headers, h)
			}
		}
	}
	for key, names := range byKey {
		if len(names) > 1 {
			slog.Warn("inference-router: servers share one key, so a request that sends it selects no server",
				"servers", strings.Join(names, ","))
			continue
		}
		keys = append(keys, serverKey{name: names[0], key: key})
	}
	slices.SortFunc(keys, func(a, b serverKey) int { return strings.Compare(a.name, b.name) })
	slices.Sort(headers)
	return keys, headers
}

// keyOwner is the one server whose key h sends, "" when h sends no server's key or
// the keys of two or more servers. It reads the bearer of each Authorization value,
// each X-Api-Key value, and each value of the headers in p.keyHeaders. It compares
// each value with each key in constant time, with no early exit.
func (p *Router) keyOwner(h http.Header) string {
	var values []string
	for _, v := range h.Values("Authorization") {
		values = append(values, auth.ExtractBearer(v))
	}
	values = append(values, h.Values("X-Api-Key")...)
	for _, name := range p.keyHeaders {
		values = append(values, h.Values(name)...)
	}
	owner, many := "", false
	for _, v := range values {
		if v == "" {
			continue
		}
		for _, k := range p.keys {
			if subtle.ConstantTimeCompare([]byte(v), []byte(k.key)) == 1 {
				if owner != "" && owner != k.name {
					many = true
				}
				owner = k.name
			}
		}
	}
	if many {
		return ""
	}
	return owner
}

// choiceFor is the server for a request of agent in a session with no pin, "" for
// not routed: the agents: entry, else the key_routes: entry of keyOf, else keyOf, the
// server whose key the request sent. A request with no agent is not routed by its key.
func (p *Router) choiceFor(agent, keyOf string) string {
	if agent == "" {
		return ""
	}
	if s := p.agents[agent]; s != "" {
		return s
	}
	if keyOf == "" {
		return ""
	}
	return cmp.Or(p.keyRoutes[keyOf], keyOf)
}
