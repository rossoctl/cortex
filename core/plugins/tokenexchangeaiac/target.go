package tokenexchangeaiac

import (
	"fmt"
	"net"
	"strings"
)

// Reasons a host is not offered to the IdP. Each is recorded as the reason of a
// skip invocation, so they are machine-stable strings.
const (
	reasonHostNotInCluster = "host_not_in_cluster"
	reasonNamespaceUnknown = "namespace_unknown"
)

// Template placeholders. Anything else between braces is refused at Configure.
const (
	placeholderService   = "{service}"
	placeholderNamespace = "{namespace}"
)

// target is an outbound destination this plugin will ask the IdP about.
type target struct {
	service   string
	namespace string // "" when the host did not name one and none is configured
	audience  string
	// foreign: the host names a namespace other than this workload's, and the
	// audience does not carry the namespace. The audience is then a claim on a
	// target elsewhere, which the host cannot back; see targetResolver.
	foreign bool
}

// targetResolver turns a Host header into a target, or into the reason it is
// not one.
//
// The point of this type is the hosts it REFUSES. The audience is derived from
// the host, and the host is chosen by the workload — which may be following a
// prompt it should not have. If every host were eligible, a request to
// github-tool.evil.example would ask the IdP for the github-tool audience,
// receive a real token, and send it to evil.example. So only names that
// Kubernetes DNS resolves inside the cluster are eligible:
//
//	github-tool                            short name, resolved by the pod's
//	                                       search path to its own namespace
//	github-tool.team1.svc                  service.namespace.svc
//	github-tool.team1.svc.cluster.local    the FQDN, with the configured
//	                                       cluster domain
//
// Deliberately NOT eligible: "github-tool.team1". Kubernetes resolves it, but
// it has the same shape as "example.com", and a rule that cannot tell a
// namespace from a TLD is the hole described above. IP literals are refused
// too: they name no service.
//
// The same hole opens one level down when the template has no {namespace}.
// The audience then cannot tell github-tool.team1.svc from
// github-tool.attacker.svc, so a tenant who creates a Service named github-tool
// in their own namespace would be handed a real github-tool token. A caller
// that dials the short name trusts its own namespace's DNS, not every
// namespace's, so under such a template a qualified host in any other
// namespace — or in any namespace at all when this workload's is not known — is
// resolved as foreign. The plugin never exchanges for a foreign target: it asks
// the IdP whether the audience names a client, passes the request through when
// it does not, and denies it when it does, since a Service elsewhere is then
// claiming a target's name. A template with {namespace} puts the namespace into
// the audience and needs no such rule.
//
// Within the namespaces it accepts, anyone who can create a Service can still
// choose a name that matches an IdP client. No host-derived scheme can do
// better; one that reads the peer's SPIFFE ID from an mTLS handshake could.
type targetResolver struct {
	template      string
	clusterSuffix string // ".svc." + cluster domain
	namespace     string // used for short names; the only one a namespace-blind template accepts
}

func newTargetResolver(template, clusterDomain, namespace string) (*targetResolver, error) {
	if err := validateTemplate(template); err != nil {
		return nil, err
	}
	clusterDomain = strings.Trim(strings.ToLower(clusterDomain), ".")
	if clusterDomain == "" {
		return nil, fmt.Errorf("cluster_domain must not be empty")
	}
	return &targetResolver{
		template:      template,
		clusterSuffix: ".svc." + clusterDomain,
		namespace:     namespace,
	}, nil
}

// validateTemplate refuses a template with no {service} — every request would
// map to one audience, which is a static credential, not discovery — and any
// brace-delimited name other than the two placeholders, which is almost
// certainly a typo that would otherwise reach the IdP verbatim.
func validateTemplate(t string) error {
	if !strings.Contains(t, placeholderService) {
		return fmt.Errorf("audience_template %q must contain %s", t, placeholderService)
	}
	rest := strings.NewReplacer(placeholderService, "", placeholderNamespace, "").Replace(t)
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("audience_template %q: only %s and %s are supported placeholders",
			t, placeholderService, placeholderNamespace)
	}
	return nil
}

func (r *targetResolver) usesNamespace() bool {
	return strings.Contains(r.template, placeholderNamespace)
}

// resolve returns the target for host, or a non-empty reason it has none.
func (r *targetResolver) resolve(host string) (target, string) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || net.ParseIP(host) != nil {
		return target{}, reasonHostNotInCluster
	}

	var t target
	qualified := true
	switch labels := strings.Split(host, "."); {
	case len(labels) == 1:
		t = target{service: labels[0], namespace: r.namespace}
		qualified = false
	case len(labels) == 3 && labels[2] == "svc":
		t = target{service: labels[0], namespace: labels[1]}
	case strings.HasSuffix(host, r.clusterSuffix) &&
		strings.Count(strings.TrimSuffix(host, r.clusterSuffix), ".") == 1:
		t = target{service: labels[0], namespace: labels[1]}
	default:
		return target{}, reasonHostNotInCluster
	}
	if !validLabel(t.service) || (t.namespace != "" && !validLabel(t.namespace)) {
		return target{}, reasonHostNotInCluster
	}
	if r.usesNamespace() {
		if t.namespace == "" {
			return target{}, reasonNamespaceUnknown
		}
	} else if qualified && t.namespace != r.namespace {
		// The audience drops the namespace, so only the one a short name
		// would resolve to may be named. r.namespace == "" lands here too.
		t.foreign = true
	}
	t.audience = strings.NewReplacer(
		placeholderService, t.service,
		placeholderNamespace, t.namespace,
	).Replace(r.template)
	return t, ""
}

// validLabel reports whether s is an RFC 1123 DNS label, the form Kubernetes
// requires of Service and namespace names. Checking it here keeps characters
// that mean something in a URL or a SPIFFE ID out of the audience.
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}
