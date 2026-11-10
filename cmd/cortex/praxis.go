package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/rossoctl/cortex/core/bootstrap"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/sessionapi"
)

var _ sessionapi.HopJoins = (*forwardproxy.Hop)(nil)

// praxisProbeEvery is how often the hop probes the gateway until one probe passes (Decision 3).
const praxisProbeEvery = time.Second

// outboundEntries is the outbound plugin list to build: none in praxis mode, and it logs the
// skipped names at Warn: no policy plugin runs in praxis mode, because the gateway does not
// run them and a WebSocket upgrade does not go through the gateway. The config keeps its list.
func outboundEntries(c *config.Config, log *slog.Logger) []config.PluginEntry {
	if !c.PraxisEnabled() {
		return c.Pipeline.Outbound.Plugins
	}
	if len(c.Pipeline.Outbound.Plugins) > 0 {
		names := make([]string, len(c.Pipeline.Outbound.Plugins))
		for i, e := range c.Pipeline.Outbound.Plugins {
			names[i] = e.Name
		}
		log.Warn("praxis mode: outbound plugins are off; cortex-gateway does not run them, and WebSocket upgrades go direct", "plugins", names)
	}
	return nil
}

// startPraxis sets fpSrv.Hop and decline mode, serves the egress listener on ln with upstream,
// starts the probe, and returns the hop for Ingest.Joins and the egress server for shutdown.
// Call it before the forward proxy serves: nothing locks the fields it sets.
func startPraxis(ctx context.Context, cfg *config.Config, fpSrv *forwardproxy.Server, token string, upstream http.RoundTripper, ln net.Listener) (*forwardproxy.Hop, *http.Server) {
	hop := forwardproxy.NewHop(cfg.Praxis.ListenAddr(), token)
	fpSrv.Hop = hop
	// The gateway does not carry a Responses API WebSocket, so Codex must use HTTPS.
	fpSrv.ResponsesWebSockets = config.ResponsesWebSocketsDecline
	srv := bootstrap.ServeHTTPServer("praxis-egress", forwardproxy.NewEgressHandler(token, upstream), ln)
	go hop.Probe(ctx, praxisProbeEvery)
	return hop, srv
}
