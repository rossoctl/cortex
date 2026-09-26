// Package bootstrap holds process-level helpers shared by the authbridge
// binaries (authbridge-proxy, authbridge-cpex, authbridge-envoy). Each binary
// has its own main() orchestration and listener wiring; only the byte-identical
// plumbing — logging setup, the SIGUSR1 log-level toggle, the health and stats
// servers, and the HTTP-listener helpers — lives here so a fix has to be made
// once rather than three times.
package bootstrap

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/reverseproxy"
	"github.com/rossoctl/cortex/core/listener/transparentproxy"
	"github.com/rossoctl/cortex/core/observe"
	"github.com/rossoctl/cortex/core/pipeline"
)

// logLevel is the process-wide slog level, mutated live by StartSignalToggle.
var logLevel = new(slog.LevelVar)

// LogLevel returns the current process-wide slog level, for binaries that log
// their configured level at startup.
func LogLevel() slog.Level { return logLevel.Level() }

// InitLogging sets the process log level from the LOG_LEVEL env var (debug /
// warn / error, default info) and installs a slog text handler on stderr. The
// binaryName is attached to every record as the "binary" attribute so logs from
// co-located sidecars stay distinguishable.
func InitLogging(binaryName string) {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(h).With("binary", binaryName))
	// slog.SetDefault also routes the standard log package through this handler,
	// and it does so at Info unless told otherwise. Every fatal startup error in
	// the binaries goes through log.Fatalf, so without this a port clash — the
	// most common local failure — prints as INFO and the process then exits,
	// leaving an operator scanning an apparently clean log for a cause. Fatals
	// are the only std-log users in these binaries, so raising the bridge to
	// Error labels them correctly rather than mislabelling anything else.
	slog.SetLogLoggerLevel(slog.LevelError)
}

// StartSignalToggle installs a SIGUSR1 handler that toggles the process log
// level between DEBUG and INFO, so operators can flip verbose logging on a
// running pod without a restart.
func StartSignalToggle() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	go func() {
		for range sigCh {
			if logLevel.Level() == slog.LevelDebug {
				logLevel.Set(slog.LevelInfo)
				slog.Info("log level toggled to INFO (send SIGUSR1 to switch back to DEBUG)")
			} else {
				logLevel.Set(slog.LevelDebug)
				slog.Info("log level toggled to DEBUG (send SIGUSR1 to switch back to INFO)")
			}
		}
	}()
}

// StartHealthServer binds addr, serves liveness (/healthz) and readiness
// (/readyz) in a goroutine, and returns the server for graceful shutdown.
// Readiness reports 503 while any inbound or outbound plugin is still waiting on
// a dependency (e.g. a credential file that hasn't landed yet). A bind failure
// is returned so the caller can decide how to handle it; a serve-time failure
// after bind is logged.
func StartHealthServer(inboundH, outboundH *pipeline.Holder, addr string) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if name := inboundH.NotReadyPlugin(); name != "" {
			http.Error(w, "inbound plugin not ready: "+name, http.StatusServiceUnavailable)
			return
		}
		if name := outboundH.NotReadyPlugin(); name != "" {
			http.Error(w, "outbound plugin not ready: "+name, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		slog.Info("health server listening", "addr", listener.Addr().String())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("health server failed", "error", err)
		}
	}()
	return srv, nil
}

// StartStatServer binds addr for the stats/config-inspection server, serves it
// in a goroutine, and returns the server for graceful shutdown. A bind failure
// is returned so the caller can decide how to handle it (the mains log.Fatalf);
// a serve-time failure after bind is logged.
func StartStatServer(cfg *config.Config, cfgProvider observe.ConfigProvider, statsProvider observe.StatsProvider, reloadStatus, pricingTable http.Handler, addr string) (*observe.StatServer, error) {
	srv := observe.NewStatServer(addr, cfgProvider, statsProvider,
		observe.WithReloadStatus(reloadStatus),
		observe.WithPricingTable(pricingTable))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		slog.Info("stat server listening", "addr", listener.Addr().String())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("stat server failed", "error", err)
		}
	}()
	return srv, nil
}

// StartHTTPServer binds addr, serves handler in a goroutine, and returns the
// server for graceful shutdown. It logs the concrete bound address (resolving
// an ephemeral ":0" to the OS-assigned port). A bind failure is returned so the
// caller can decide how to handle it; a serve-time failure after bind is logged.
func StartHTTPServer(name string, handler http.Handler, addr string) (*http.Server, error) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		slog.Info("HTTP server listening", "name", name, "addr", listener.Addr().String())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "name", name, "error", err)
		}
	}()
	return srv, nil
}

// StartReverseProxyServer mirrors StartHTTPServer but uses the
// reverseproxy.Server's Listen() method so the byte-peek TLS-sniffing
// listener is wired in when mTLS is enabled. With mTLS off, Listen
// returns a plain net.Listen and behavior matches StartHTTPServer.
//
// Logged "mtls" attribute makes the listener mode visible at startup;
// operators expecting a separate :8443 port for TLS get a clear hint
// that this is the same :8080 with byte-peek detection.
func StartReverseProxyServer(name string, rp *reverseproxy.Server, addr string) (*http.Server, error) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           rp.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := rp.Listen(addr)
	if err != nil {
		return nil, err
	}
	go func() {
		slog.Info("Reverse server listening", "name", name, "addr", listener.Addr().String(), "mtls", rp.MTLSEnabled())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("Reverse server failed", "name", name, "error", err)
		}
	}()
	return srv, nil
}

// StartTransparentInboundServer binds the inbound transparent listener and
// serves the reverse proxy's handler over it, resolving each request's
// forwarding target from the destination the client actually addressed
// (recovered via SO_ORIGINAL_DST) rather than from a fixed backend URL.
//
// Pair with proxy-init's INBOUND_TRANSPARENT_PORT: the port here MUST match the
// PREROUTING REDIRECT target, or inbound traffic is redirected to a dead port.
// Callers should treat a returned error as fatal for that reason.
func StartTransparentInboundServer(name string, rp *reverseproxy.Server, addr string) (*http.Server, error) {
	if addr == "" {
		return nil, fmt.Errorf("%s: transparent_inbound_addr is empty but inbound_interception is transparent", name)
	}
	// Bind a *net.TCPListener directly rather than via rp.Listen: SO_ORIGINAL_DST
	// must be read off the raw TCP connection before any bytes are consumed. The
	// mTLS posture is then applied with the same WrapListener the fixed-backend
	// reverse proxy uses, so permissive/strict behavior cannot drift between the
	// two inbound shapes.
	la, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%s: resolve addr %q: %w", name, addr, err)
	}
	tcpLn, err := net.ListenTCP("tcp", la)
	if err != nil {
		return nil, fmt.Errorf("%s: listen on %q: %w", name, addr, err)
	}
	listener := rp.WrapListener(transparentproxy.NewInboundListener(tcpLn))
	srv := &http.Server{
		Addr:              addr,
		Handler:           rp.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Carries each connection's recovered original destination into the
		// request context, unwrapping through tlssniff / tls.Conn as needed.
		ConnContext: transparentproxy.ConnContextHook,
	}
	go func() {
		slog.Info("Transparent inbound server listening",
			"name", name, "addr", listener.Addr().String(), "mtls", rp.MTLSEnabled())
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("Transparent inbound server failed", "name", name, "error", err)
		}
	}()
	return srv, nil
}
