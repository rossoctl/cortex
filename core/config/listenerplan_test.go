package config

import (
	"slices"
	"strings"
	"testing"
)

// The plan must name exactly the sockets the proxy serves, under the same conditions: a socket
// reserved and never served accepts connections nothing answers, and one served without a
// reservation is bound after the data it was meant to guard. take and claimed catch either
// at boot; this pins each deployment shape's list so neither boot is the first to find out.
func TestListenerPlan_NamesWhatEachShapeServes(t *testing.T) {
	off := false
	listener := ListenerConfig{
		ReverseProxyAddr:       "127.0.0.1:1",
		TransparentInboundAddr: "127.0.0.1:2",
		ForwardProxyAddr:       "127.0.0.1:3",
		TransparentProxyAddr:   "127.0.0.1:4",
		SessionAPIAddr:         "127.0.0.1:5",
		HealthAddr:             "127.0.0.1:6",
	}
	cfgWith := func(edit func(*Config)) *Config {
		c := &Config{Listener: listener, Stats: StatsConfig{StatsAddress: "127.0.0.1:7"}}
		edit(c)
		return c
	}
	cases := []struct {
		name  string
		cfg   *Config
		local bool
		want  []string
	}{
		{
			name:  "laptop: forward only, no iptables to feed a transparent listener",
			cfg:   cfgWith(func(c *Config) { c.Listener.Roles = []string{RoleForward} }),
			local: true,
			want:  []string{"forward-proxy", "stats", "session-api", "health"},
		},
		{
			name: "pod: both roles, reverse-proxy inbound, enforce-redirect egress",
			cfg:  cfgWith(func(*Config) {}),
			want: []string{"reverse-proxy", "forward-proxy", "transparent-proxy", "stats", "session-api", "health"},
		},
		{
			name: "pod: transparent inbound",
			cfg: cfgWith(func(c *Config) {
				c.Listener.InboundInterception = InboundInterceptionTransparent
			}),
			want: []string{"transparent-inbound", "forward-proxy", "transparent-proxy", "stats", "session-api", "health"},
		},
		{
			name: "local install never starts transparent inbound either",
			cfg: cfgWith(func(c *Config) {
				c.Listener.InboundInterception = InboundInterceptionTransparent
			}),
			local: true,
			want:  []string{"forward-proxy", "stats", "session-api", "health"},
		},
		{
			name: "no transparent_proxy_addr means no transparent proxy",
			cfg:  cfgWith(func(c *Config) { c.Listener.TransparentProxyAddr = "" }),
			want: []string{"reverse-proxy", "forward-proxy", "stats", "session-api", "health"},
		},
		{
			name: "sessions off means no session API",
			cfg:  cfgWith(func(c *Config) { c.Session.Enabled = &off }),
			want: []string{"reverse-proxy", "forward-proxy", "transparent-proxy", "stats", "health"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := ListenerPlan(tc.cfg, tc.local)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(plan))
			for i, p := range plan {
				got[i] = p.Name
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("plan = %v, want %v", got, tc.want)
			}
		})
	}
}

// An empty transparent_inbound_addr was an error from the bind; reserving must keep it one
// rather than binding "" — which net.Listen takes as a random port on every interface.
func TestListenerPlan_EmptyTransparentInboundIsAnError(t *testing.T) {
	cfg := &Config{Listener: ListenerConfig{
		InboundInterception: InboundInterceptionTransparent,
	}}
	if _, err := ListenerPlan(cfg, false); err == nil || !strings.Contains(err.Error(), "transparent_inbound_addr is empty") {
		t.Fatalf("err = %v, want the empty-address error", err)
	}
}
