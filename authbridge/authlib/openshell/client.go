// Package openshell is a Go client for the OpenShell gateway's credential
// surface. It replicates how the sandbox supervisor authenticates and fetches
// its resolved provider environment, so AuthBridge — running as a sidecar in
// the sandbox pod — can do the same:
//
//  1. read the pod's projected SA token (audience openshell-gateway);
//  2. dial the gateway over mTLS;
//  3. IssueSandboxToken (Bearer <sa-token>) → a gateway-minted sandbox JWT;
//  4. GetSandboxProviderEnvironment(sandbox_id) (Bearer <jwt>) → the resolved
//     real credential values keyed by env-var name.
//
// The gateway restricts these RPCs to the sandbox's own identity, so this
// client only succeeds when it runs in the sandbox pod (shared SA + the
// openshell.io/sandbox-id annotation).
package openshell

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/config"
	osv1 "github.com/kagenti/kagenti-extensions/authbridge/authlib/openshell/genproto/openshellv1"
)

// rpcTimeout bounds each gateway RPC so a connected-but-unresponsive gateway
// cannot block a caller (e.g. the resolver's background refresh loop) forever.
const rpcTimeout = 10 * time.Second

// Config locates the gateway and the credentials needed to talk to it. The
// paths mirror what the OpenShell k8s driver injects into the sandbox pod.
type Config struct {
	// Endpoint is the gateway gRPC endpoint (OPENSHELL_ENDPOINT). An
	// https:// scheme selects mTLS; http:// is plaintext.
	Endpoint string
	// MTLSCert, MTLSKey, MTLSCA are the client cert, key, and CA file paths for
	// mTLS — independent paths mirroring OpenShell's OPENSHELL_TLS_CERT /
	// OPENSHELL_TLS_KEY / OPENSHELL_TLS_CA (the driver splits the CA into a
	// separate secret/dir from the client keypair). All three are required when
	// Endpoint is https://.
	MTLSCert string
	MTLSKey  string
	MTLSCA   string
	// SATokenPath is the projected SA token file (OPENSHELL_K8S_SA_TOKEN_FILE,
	// e.g. /var/run/secrets/openshell/token).
	SATokenPath string
	// SandboxID is this sandbox's id (OPENSHELL_SANDBOX_ID); it must equal
	// the id the gateway binds into the minted JWT, or the gateway rejects
	// the provider-environment fetch as cross-sandbox access.
	SandboxID string
	// Insecure permits a plaintext (non-TLS) connection to a non-loopback
	// gateway. Plaintext sends the SA token and minted JWT as cleartext gRPC
	// metadata, so it is refused by default for non-loopback targets; this
	// flag is an explicit opt-in (logged with a warning). Loopback targets are
	// always allowed plaintext.
	Insecure bool
}

func (c Config) validate() error {
	if c.Endpoint == "" {
		return fmt.Errorf("openshell: endpoint is required")
	}
	if c.SATokenPath == "" {
		return fmt.Errorf("openshell: sa_token_path is required")
	}
	if c.SandboxID == "" {
		return fmt.Errorf("openshell: sandbox_id is required")
	}
	if strings.HasPrefix(c.Endpoint, "https://") && (c.MTLSCert == "" || c.MTLSKey == "" || c.MTLSCA == "") {
		return fmt.Errorf("openshell: mtls_cert, mtls_key, and mtls_ca are required for an https endpoint")
	}
	return nil
}

// Environment is the resolved provider environment for a sandbox.
type Environment struct {
	// Values maps env-var name (e.g. ANTHROPIC_AUTH_TOKEN) to the real
	// resolved credential value.
	Values map[string]string
	// Revision is a fingerprint of the inputs that produced Values.
	Revision uint64
	// ExpiresAtMs is the per-key expiry (epoch ms); absent/0 means no expiry.
	ExpiresAtMs map[string]int64
}

// Client talks to the OpenShell gateway. It is safe for concurrent use.
type Client struct {
	cfg  Config
	conn *grpc.ClientConn
	rpc  osv1.OpenShellClient

	mu       sync.RWMutex
	token    string    // current gateway JWT
	tokenExp time.Time // zero = unknown / non-expiring
}

// Dial connects to the gateway. It does not contact the server until the
// first RPC, so a successful Dial does not imply reachability.
func Dial(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	target := cfg.Endpoint
	var creds credentials.TransportCredentials
	plaintext := false
	switch {
	case strings.HasPrefix(cfg.Endpoint, "https://"):
		target = strings.TrimPrefix(cfg.Endpoint, "https://")
		tlsCfg, err := mtlsConfig(cfg.MTLSCert, cfg.MTLSKey, cfg.MTLSCA, hostOnly(target))
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(tlsCfg)
	case strings.HasPrefix(cfg.Endpoint, "http://"):
		target = strings.TrimPrefix(cfg.Endpoint, "http://")
		plaintext = true
	default:
		// No scheme: mTLS when client cert material is configured, else plaintext.
		if cfg.MTLSCert != "" {
			tlsCfg, err := mtlsConfig(cfg.MTLSCert, cfg.MTLSKey, cfg.MTLSCA, hostOnly(target))
			if err != nil {
				return nil, err
			}
			creds = credentials.NewTLS(tlsCfg)
		} else {
			plaintext = true
		}
	}

	if plaintext {
		// Fail closed: a plaintext transport leaks the SA token and the minted
		// gateway JWT as cleartext metadata. Only allow it for loopback, or
		// behind the explicit Insecure opt-in (with a warning).
		if !cfg.Insecure && !isLoopbackHost(target) {
			return nil, fmt.Errorf("openshell: refusing plaintext gRPC to non-loopback %q — the SA token and gateway JWT would travel in cleartext; use an https:// endpoint with mtls_cert_dir, or set insecure: true to override", target)
		}
		if !isLoopbackHost(target) {
			slog.Warn("openshell: insecure plaintext gRPC to the gateway — SA token and minted JWT travel unencrypted", "endpoint", cfg.Endpoint)
		}
		creds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("openshell: dial %q: %w", target, err)
	}
	return &Client{cfg: cfg, conn: conn, rpc: osv1.NewOpenShellClient(conn)}, nil
}

// FetchEnvironment returns the sandbox's resolved provider environment,
// minting (or re-minting) the gateway JWT as needed.
func (c *Client) FetchEnvironment(ctx context.Context) (*Environment, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	env, err := c.fetchWithToken(ctx, tok)
	if status.Code(err) == codes.Unauthenticated {
		// JWT rejected (expired or rotated under us) — re-bootstrap from the
		// (possibly rotated) SA token and retry once.
		if mErr := c.mint(ctx); mErr != nil {
			return nil, mErr
		}
		tok, _ = c.getToken()
		env, err = c.fetchWithToken(ctx, tok)
	}
	return env, err
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *Client) fetchWithToken(ctx context.Context, tok string) (*Environment, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	md := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	resp, err := c.rpc.GetSandboxProviderEnvironment(md, &osv1.GetSandboxProviderEnvironmentRequest{
		SandboxId: c.cfg.SandboxID,
	})
	if err != nil {
		return nil, fmt.Errorf("openshell: GetSandboxProviderEnvironment: %w", err)
	}
	return &Environment{
		Values:      resp.GetEnvironment(),
		Revision:    resp.GetProviderEnvRevision(),
		ExpiresAtMs: resp.GetCredentialExpiresAtMs(),
	}, nil
}

// ensureToken returns a non-expired gateway JWT, minting one if absent or
// expired.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	if tok, exp := c.getToken(); tok != "" && (exp.IsZero() || time.Now().Before(exp)) {
		return tok, nil
	}
	if err := c.mint(ctx); err != nil {
		return "", err
	}
	tok, _ := c.getToken()
	return tok, nil
}

// mint exchanges the projected SA token for a gateway JWT via IssueSandboxToken.
func (c *Client) mint(ctx context.Context) error {
	sa, err := config.ReadCredentialFile(c.cfg.SATokenPath)
	if err != nil {
		return fmt.Errorf("openshell: read SA token %q: %w", c.cfg.SATokenPath, err)
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	md := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+sa)
	resp, err := c.rpc.IssueSandboxToken(md, &osv1.IssueSandboxTokenRequest{})
	if err != nil {
		return fmt.Errorf("openshell: IssueSandboxToken: %w", err)
	}
	c.setToken(resp.GetToken(), resp.GetExpiresAtMs())
	return nil
}

func (c *Client) setToken(tok string, expMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = tok
	if expMs > 0 {
		c.tokenExp = time.UnixMilli(expMs)
	} else {
		c.tokenExp = time.Time{}
	}
}

func (c *Client) getToken() (string, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token, c.tokenExp
}

// mtlsConfig builds a client mTLS config from independent cert, key, and CA
// file paths (mirroring OpenShell's OPENSHELL_TLS_CERT/KEY/CA — the driver puts
// the CA in a different secret/dir than the client keypair). The CA pool extends
// the system roots (mirrors authlib/tlsbridge/upstream.go).
func mtlsConfig(certPath, keyPath, caPath, serverName string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("openshell: load client cert (%q, %q): %w", certPath, keyPath, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("openshell: read ca %q: %w", caPath, err)
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("openshell: ca %q is not valid PEM", caPath)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// hostOnly strips a :port suffix, leaving the host for ServerName.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// isLoopbackHost reports whether the host part of hostport is localhost or a
// loopback IP — the only targets for which plaintext gRPC is allowed without
// the explicit Insecure opt-in.
func isLoopbackHost(hostport string) bool {
	h := hostOnly(hostport)
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
