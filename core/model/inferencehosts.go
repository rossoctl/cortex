package model

// InferenceHostSeen is where one agent's eligible inference requests in a session went.
type InferenceHostSeen struct {
	Agent        string `json:"agent"`                  // model.AgentName(e.Client.Label()), as serverOf compares it
	Host         string `json:"host"`                   // e.Host as recorded: where the bytes went. Raw, not routerconfig.Key
	Server       string `json:"server,omitempty"`       // RoutedServer of the request: the router's server name, "" when not routed
	Last         uint64 `json:"last"`                   // Seq of the newest eligible request
	LastUnrouted uint64 `json:"lastUnrouted,omitempty"` // Seq of the newest one CapturePath takes with RequestedHost == ""
}

// InferenceHosts is one session's table: one InferenceHostSeen for each (agent, host, server).
type InferenceHosts []InferenceHostSeen

// LastServer is serverOf's answer over the table: the newest evidence for agent.
// serverAt is the reader's host→server map. A routed host counts at Last.
// Any other host counts at LastUnrouted, and answers ("", true).
// Each seq must be in one entry only, as one event has one agent and one host: on a tie the first entry wins.
func (h InferenceHosts) LastServer(agent string, serverAt func(host string) (string, bool)) (string, bool) {
	var best uint64
	var server string
	for _, s := range h {
		if s.Agent != agent {
			continue
		}
		if srv, ok := serverAt(s.Host); ok {
			if s.Last > best {
				best, server = s.Last, srv
			}
		} else if s.LastUnrouted > best {
			best, server = s.LastUnrouted, ""
		}
	}
	return server, best > 0
}

// routerPlugin must be equal to inferencerouter.Name. core/model cannot import the router.
const routerPlugin = "inference-router"

// RoutedServer is the server name that the inference-router recorded for a request: the
// "server" detail of the last outbound request-phase modify/routed invocation. It is "" for a
// nil in or no such invocation. Other invocations that carry a server detail (would_route,
// pinned_server_removed and others) do not count: they did not send the request to that server.
func RoutedServer(in *Invocations) string {
	if in == nil {
		return ""
	}
	for i := len(in.Outbound) - 1; i >= 0; i-- {
		inv := &in.Outbound[i]
		if inv.Plugin == routerPlugin && inv.Action == ActionModify && inv.Reason == "routed" &&
			inv.Phase == InvocationPhaseRequest {
			return inv.Details["server"]
		}
	}
	return ""
}

// RoutedKeyOf is the name of the server whose key a request sent: the "key_of" detail of the last
// outbound request-phase inference-router invocation that has one. The router records key_of on
// every reason, so the reason does not count. It is "" for a nil in or no such invocation.
func RoutedKeyOf(in *Invocations) string {
	if in == nil {
		return ""
	}
	for i := len(in.Outbound) - 1; i >= 0; i-- {
		inv := &in.Outbound[i]
		if inv.Plugin == routerPlugin && inv.Phase == InvocationPhaseRequest && inv.Details["key_of"] != "" {
			return inv.Details["key_of"]
		}
	}
	return ""
}

// LastNamedServer is LastServer for a config where servers can share a host. exists tells if
// a server name is in the config. serverAt gives the host's server: known is true when the
// host has at least one server, and server is "" when it has more than one.
// An entry with a server name that exists counts at Last with that name. Else an entry on a
// host with one server counts at Last with that server. Else an entry on a host with several
// servers is no evidence. Else the entry counts at LastUnrouted, and answers "".
// The newest evidence wins. On a tie the first entry wins, as in LastServer.
func (h InferenceHosts) LastNamedServer(agent string, exists func(server string) bool, serverAt func(host string) (server string, known bool)) (string, bool) {
	var best uint64
	var server string
	for _, s := range h {
		if s.Agent != agent {
			continue
		}
		at, name := s.LastUnrouted, ""
		if s.Server != "" && exists(s.Server) {
			at, name = s.Last, s.Server
		} else if srv, known := serverAt(s.Host); known && srv != "" {
			at, name = s.Last, srv
		} else if known {
			continue
		}
		if at > best {
			best, server = at, name
		}
	}
	return server, best > 0
}
