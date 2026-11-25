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
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
)

func outboundCfg(praxis bool) *config.Config {
	cfg := praxisCfg(praxis)
	cfg.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "inference-parser"}, {Name: "sparc"}}
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
	for _, name := range []string{"inference-parser", "sparc"} {
		if !strings.Contains(buf.String(), name) {
			t.Errorf("log %q does not name %s", buf.String(), name)
		}
	}
	if len(cfg.Pipeline.Outbound.Plugins) != 2 {
		t.Errorf("the config's list changed: %v", cfg.Pipeline.Outbound.Plugins)
	}

	buf.Reset()
	got := outboundEntries(outboundCfg(false), log)
	if len(got) != 2 || !slices.Equal([]string{got[0].Name, got[1].Name}, []string{"inference-parser", "sparc"}) {
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
	ready := hopReady(t, cfg.Praxis.ListenAddr())
	hop, srv := startPraxis(ctx, cfg, &forwardproxy.Server{}, "tok", http.DefaultTransport, ln)
	defer srv.Close()
	<-ready
	if !hop.Ready() {
		t.Fatal("the hop logged that it is ready, but Ready() is false")
	}
}

// hopReadyMsg is what a hop logs once its probe passes, after Ready() turns true.
const hopReadyMsg = "praxis hop: cortex-gateway is ready"

// hopReady returns a channel that closes when the hop to gateway logs hopReadyMsg. The
// default logger writes text to stderr through it until the test ends. It wraps a handler
// of its own: the initial default handler writes through the log package, which
// SetDefault points back at slog.
func hopReady(t *testing.T, gateway string) <-chan struct{} {
	t.Helper()
	ready := make(chan struct{})
	prev := slog.Default()
	h := slog.NewTextHandler(os.Stderr, nil)
	slog.SetDefault(slog.New(&readyHandler{Handler: h, gateway: gateway, once: &sync.Once{}, ready: ready}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return ready
}

// readyHandler closes ready on the hop's ready line for gateway, and passes every record
// the wrapped handler takes on to it.
type readyHandler struct {
	slog.Handler
	gateway string
	once    *sync.Once
	ready   chan struct{}
}

func (h *readyHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *readyHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == hopReadyMsg {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "gateway" && a.Value.String() == h.gateway {
				h.once.Do(func() { close(h.ready) })
				return false
			}
			return true
		})
	}
	if !h.Handler.Enabled(ctx, r.Level) {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h *readyHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.Handler = h.Handler.WithAttrs(as)
	return &c
}

func (h *readyHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.Handler = h.Handler.WithGroup(name)
	return &c
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

func TestWarnDeprecatedToolPrune(t *testing.T) {
	old := localInstall
	t.Cleanup(func() { localInstall = old })
	withPrune := praxisCfg(false)
	withPrune.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "tool-prune"}, {Name: "context-guru"}}
	withoutPrune := praxisCfg(false)
	withoutPrune.Pipeline.Outbound.Plugins = []config.PluginEntry{{Name: "context-guru"}}
	praxis := praxisCfg(true)
	praxis.Pipeline.Outbound.Plugins = withPrune.Pipeline.Outbound.Plugins

	cg := []string{"tool-prune", "context-guru"}
	for _, tc := range []struct {
		name       string
		cfg        *config.Config
		registered []string
		local      bool
		want       int
	}{
		{"tool-prune listed, context-guru registered", withPrune, cg, true, 1},
		{"no tool-prune in the list", withoutPrune, cg, true, 0},
		{"context-guru not registered", withPrune, []string{"tool-prune"}, true, 0},
		{"praxis mode", praxis, cg, true, 0},
		{"a sidecar", withPrune, cg, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localInstall = tc.local
			var buf bytes.Buffer
			warnDeprecatedToolPrune(tc.cfg, tc.registered, slog.New(slog.NewTextHandler(&buf, nil)))
			if n := strings.Count(buf.String(), deprecatedToolPruneWarning); n != tc.want {
				t.Errorf("logged the warning %d times, want %d: %q", n, tc.want, buf.String())
			}
		})
	}
}
