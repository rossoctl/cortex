package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
)

func outboundCfg(praxis bool) *config.Config {
	cfg := praxisCfg(praxis)
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "inference-parser"}, {Name: "tool-prune"}}
	return cfg
}

func TestOutboundEntries_PraxisSkipsPlugins(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := outboundCfg(true)
	if got := outboundEntries(cfg, log); len(got) != 0 {
		t.Fatalf("praxis mode built %v, want none", got)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("log %q is not at Warn", buf.String())
	}
	for _, name := range []string{"inference-parser", "tool-prune"} {
		if !strings.Contains(buf.String(), name) {
			t.Errorf("log %q does not name %s", buf.String(), name)
		}
	}
	if len(cfg.Pipeline.Outbound.Plugins) != 2 {
		t.Errorf("the config's list changed: %v", cfg.Pipeline.Outbound.Plugins)
	}

	buf.Reset()
	got := outboundEntries(outboundCfg(false), log)
	if len(got) != 2 || !slices.Equal([]string{got[0].Name, got[1].Name}, []string{"inference-parser", "tool-prune"}) {
		t.Fatalf("direct mode got %v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("direct mode logged %q", buf.String())
	}
}

func TestOutboundEntries_PraxisEmptyListLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	if got := outboundEntries(praxisCfg(true), slog.New(slog.NewTextHandler(&buf, nil))); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if buf.Len() != 0 {
		t.Errorf("logged %q", buf.String())
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestStartPraxis_DeclinesResponsesWebSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fp := &forwardproxy.Server{ResponsesWebSockets: config.ResponsesWebSocketsParse}
	hop, srv := startPraxis(ctx, praxisCfg(true), fp, "tok", http.DefaultTransport, listen(t))
	defer srv.Close()
	if fp.ResponsesWebSockets != config.ResponsesWebSocketsDecline {
		t.Errorf("ResponsesWebSockets = %q, want decline", fp.ResponsesWebSockets)
	}
	if fp.Hop == nil || fp.Hop != hop {
		t.Errorf("fp.Hop = %v, want the returned hop", fp.Hop)
	}
}

func TestStartPraxis_EgressServesProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := listen(t)
	// A fake gateway: it forwards everything, headers and all, to the egress listener.
	gw := httptest.NewServer(httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: ln.Addr().String()}))
	defer gw.Close()
	cfg := praxisCfg(true)
	cfg.Praxis.Listen = gw.Listener.Addr().String()
	hop, srv := startPraxis(ctx, cfg, &forwardproxy.Server{}, "tok", http.DefaultTransport, ln)
	defer srv.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !hop.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("the hop is not ready after 3s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestOutboundEntries_WarnsDeprecatedCpexOnLocalInstall(t *testing.T) {
	old := localInstall
	t.Cleanup(func() { localInstall = old })

	cfg := praxisCfg(false)
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "cpex"}}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	localInstall = true
	if got := outboundEntries(cfg, log); len(got) != 1 {
		t.Fatalf("local install built %v, want the cpex entry", got)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("local install logged %d Warn lines, want 1: %q", n, buf.String())
	}
	if !strings.Contains(buf.String(), "the laptop cpex build is deprecated") {
		t.Errorf("log %q does not name the deprecation", buf.String())
	}

	buf.Reset()
	localInstall = false
	outboundEntries(cfg, log)
	if buf.Len() != 0 {
		t.Errorf("a sidecar logged %q", buf.String())
	}

	buf.Reset()
	localInstall = true
	if got := outboundEntries(praxisCfg(true), log); len(got) != 0 {
		t.Fatalf("praxis mode built %v, want none", got)
	}
	if strings.Contains(buf.String(), "deprecated") {
		t.Errorf("praxis mode logged the deprecation: %q", buf.String())
	}
}
