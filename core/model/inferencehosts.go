package model

// InferenceHostSeen is where one agent's eligible inference requests in a session went.
type InferenceHostSeen struct {
	Agent        string `json:"agent"`                  // model.AgentName(e.Client.Label()), as serverOf compares it
	Host         string `json:"host"`                   // e.Host as recorded: where the bytes went. Raw, not routerconfig.Key
	Last         uint64 `json:"last"`                   // Seq of the newest eligible request
	LastUnrouted uint64 `json:"lastUnrouted,omitempty"` // Seq of the newest one CapturePath takes with RequestedHost == ""
}

// InferenceHosts is one session's table: one InferenceHostSeen for each (agent, host) pair.
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
