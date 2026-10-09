// Package routerconfig is the inference-router plugin's configuration: its shape,
// its validation, and the host rules the plugin routes by.
//
// It is a package of its own, importing only the standard library, so that agentop
// can check a config before writing it without linking the plugin. Linking the
// plugin would pull the plugin registry, and SPIFFE and gRPC with it, into a
// terminal UI. One copy of these rules is what keeps `agentop server` from writing a
// config the proxy then refuses on reload.
package routerconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// NoAgent is the agent name of a request that carried no User-Agent. It is
// pipeline.UnknownClientLabel, copied rather than imported to keep this package
// free of the pipeline; a test in the plugin package holds the two equal.
//
// It cannot be routed: requests under it come from no one agent, so routing it
// would send every unlabelled request — a health probe, a curl — to a server.
const NoAgent = "unknown"

// Config is the plugin's config: block.
type Config struct {
	Servers map[string]Server `json:"servers" required:"true" description:"Inference servers, by name."`
	Agents  map[string]string `json:"agents" description:"Agents to route, by agent name, to a server name. Unlisted agents are not routed."`
}

// Server is one inference server.
type Server struct {
	URL    string `json:"url" required:"true" description:"scheme://host[:port] of the server; no path."`
	Key    string `json:"key" required:"true" description:"API key sent to this server in place of the client's."`
	Main   string `json:"main" required:"true" description:"A word naming this server's model for an agent's main work, a request with tools, sent when the server refuses the model the agent asked for. The newest listed model whose name contains it."`
	Helper string `json:"helper" required:"true" description:"A word naming this server's model for an agent's helper calls, a request without tools. Resolved as main is."`
}

// CheckWord reports whether word can name a server's main or helper model. A word is
// matched against a model's name with everything up to the last "/" dropped, so one
// containing a "/" could never match; whitespace never appears in a model's name.
func CheckWord(word string) error {
	switch {
	case word == "":
		return errors.New("is empty: give a word of the model's name, such as opus or glm")
	case strings.Contains(word, "/"):
		return fmt.Errorf("%q has a /: give a word of the name after the provider's prefix", word)
	case strings.IndexFunc(word, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0:
		return fmt.Errorf("%q has whitespace or a control character", word)
	}
	return nil
}

// replacedFields are a server's fields from before main and helper, refused by name
// so a config written for them says what replaced them.
var replacedFields = []string{"opus", "sonnet", "haiku"}

// refuseReplaced reports a server that still names its models by Claude family.
// raw is read leniently here; Decode's strict pass would only say "unknown field".
func refuseReplaced(raw json.RawMessage) error {
	var probe struct {
		Servers map[string]map[string]json.RawMessage `json:"servers"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil // the strict pass reports it
	}
	for _, name := range slices.Sorted(maps.Keys(probe.Servers)) {
		for _, f := range replacedFields {
			if _, ok := probe.Servers[name][f]; ok {
				return fmt.Errorf("servers.%s.%s: opus, sonnet and haiku were replaced by main and helper, "+
					"each a word of one of the server's own model names", name, f)
			}
		}
	}
	return nil
}

// Decode reads raw, the entry's config: block as JSON, refusing unknown fields, and
// validates it. Unknown fields are refused because the likeliest one is a guess at
// a feature this design leaves out on purpose — a `default:` server — and a
// silently ignored guess would route nothing while looking configured.
func Decode(raw json.RawMessage) (Config, error) {
	var c Config
	if len(raw) > 0 {
		if err := refuseReplaced(raw); err != nil {
			return Config{}, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return Config{}, err
		}
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.Agents == nil {
		c.Agents = map[string]string{}
	}
}

// Validate checks every rule the plugin routes by. Servers and agents are checked
// in name order, so the same config always reports the same first error.
func (c Config) Validate() error {
	if len(c.Servers) == 0 {
		return errors.New("servers: at least one server is required")
	}
	byHost := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(c.Servers)) {
		s := c.Servers[name]
		if err := CheckName(name); err != nil {
			return fmt.Errorf("servers: %w", err)
		}
		ep, err := ParseURL(s.URL)
		if err != nil {
			return fmt.Errorf("servers.%s.url: %w", name, err)
		}
		// Port-blind on purpose. agentop names a session's server from the host its
		// requests went to, and two servers on one host would make that ambiguous.
		if other, ok := byHost[ep.Hostname]; ok {
			return fmt.Errorf("servers %q and %q are both on %s: each server needs a host of its own, "+
				"because a session's server is named from the host its requests went to", other, name, ep.Hostname)
		}
		byHost[ep.Hostname] = name
		if err := CheckKey(s.Key); err != nil {
			return fmt.Errorf("servers.%s.key: %w", name, err)
		}
		if err := CheckWord(s.Main); err != nil {
			return fmt.Errorf("servers.%s.main: %w", name, err)
		}
		if err := CheckWord(s.Helper); err != nil {
			return fmt.Errorf("servers.%s.helper: %w", name, err)
		}
	}
	for _, agent := range slices.Sorted(maps.Keys(c.Agents)) {
		if err := CheckAgent(agent); err != nil {
			return fmt.Errorf("agents: %w", err)
		}
		server := c.Agents[agent]
		if _, ok := c.Servers[server]; !ok {
			return fmt.Errorf("agents.%s: %q is not a server listed under servers", agent, server)
		}
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// CheckName reports whether name can name a server.
func CheckName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%q is not a server name: use lowercase letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

// CheckAgent reports whether name can be routed. Agent names are the ones
// pipeline.AgentName produces and agentop shows, such as claude-code and opencode.
func CheckAgent(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%q is not an agent name: agent names are lowercase, as agentop shows them (claude-code, opencode)", name)
	}
	if name == NoAgent {
		return fmt.Errorf("%q cannot be routed: it is the name for requests that carry no User-Agent, which belong to no one agent", name)
	}
	return nil
}

// CheckKey reports whether key can be sent as a header value. The error never
// repeats the key.
func CheckKey(key string) error {
	if key == "" {
		return errors.New("the key is empty")
	}
	for i := 0; i < len(key); i++ {
		if key[i] <= ' ' || key[i] > '~' {
			return errors.New("the key must be printable ASCII with no spaces: it is sent as a header value")
		}
	}
	return nil
}

// Endpoint is a server URL reduced to what routing compares.
type Endpoint struct {
	// Scheme is "http" or "https".
	Scheme string
	// Host is the lowercased host, with the port only when it is not the scheme's
	// default. It is the redirect target's host.
	Host string
	// Hostname is the lowercased host without any port: what a request is matched
	// to a server by, and what makes two servers the same host.
	Hostname string
}

// URL is the endpoint as scheme://host, the form agentop writes.
func (e Endpoint) URL() string { return e.Scheme + "://" + e.Host }

// PlaintextRemote reports an http endpoint on another machine. A request routed
// there leaves the proxy decrypted, with its key and its prompt, across the network.
func (e Endpoint) PlaintextRemote() bool {
	if e.Scheme != "http" || e.Hostname == "localhost" {
		return false
	}
	ip := net.ParseIP(e.Hostname)
	return ip == nil || !ip.IsLoopback()
}

// ParseURL validates a server URL and returns its Endpoint. It accepts what
// pipeline.Context.Redirect accepts — http or https, a host, no user info, no path
// beyond "/", no query or fragment — and two things more: a port must be a number
// from 1 to 65535, and the scheme's default port is dropped, so a server at
// https://x:443 is the host a request for x names, and the redirect a routed request
// for x still gets records no requested host.
//
// Its errors are fixed text that names the problem and quotes no part of raw, not
// even redacted. They reach the logs and the unauthenticated /reload/status, and a
// key pasted into a mistyped URL can land anywhere in it: user info, query,
// fragment, a path segment, or the path or port url.Parse files it under when the
// "//" is wrong. Redacting shape by shape kept missing one.
func ParseURL(raw string) (Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// Not even url.Parse's inner error, which quotes a bad port or escape.
		return Endpoint{}, errors.New("not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return Endpoint{}, errors.New("the URL's scheme must be http or https")
	case u.Opaque != "" || u.Hostname() == "":
		return Endpoint{}, errors.New("the URL has no host")
	case u.User != nil:
		return Endpoint{}, errors.New("the URL carries user info; the key is given separately, never in the URL")
	case u.Path != "" && u.Path != "/":
		return Endpoint{}, errors.New("the URL has a path; give scheme://host[:port] only, since a routed request keeps its own path")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return Endpoint{}, errors.New("the URL has a query or fragment")
	}
	port := ""
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Endpoint{}, errors.New("the URL's port must be a number from 1 to 65535")
		}
		port = strconv.Itoa(n)
	}
	hostname := strings.ToLower(u.Hostname())
	return Endpoint{Scheme: u.Scheme, Host: joinHost(hostname, withoutDefault(u.Scheme, port)), Hostname: hostname}, nil
}

// Hostname is a request's host[:port], lowercased and without the port: what the
// request is matched to a server by.
func Hostname(hostport string) string {
	h, _ := splitHost(hostport)
	return h
}

func splitHost(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h), p
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")), ""
}

func withoutDefault(scheme, port string) string {
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return ""
	}
	return port
}

func joinHost(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
