package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kagenti/kagenti-extensions/AuthBridge/AuthProxy/go-processor/internal/resolver"
)

// Configuration for token exchange
type Config struct {
	ClientID       string
	ClientSecret   string
	TokenURL       string
	TargetAudience string
	TargetScopes   string
	SpireEnabled   bool   // Whether to use SPIFFE federated auth
	JWTSvidPath    string // Path to JWT-SVID file (reloaded on each token exchange)
	mu             sync.RWMutex
}

var globalConfig = &Config{}

type processor struct {
	v3.UnimplementedExternalProcessorServer
}

type tokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

const defaultRoutesConfigPath = "/etc/authproxy/routes.yaml"

var globalResolver resolver.TargetResolver

// defaultBypassInboundPaths are paths that skip inbound JWT validation by default.
// These cover common public endpoints: Agent Card discovery, health/readiness probes.
var defaultBypassInboundPaths = []string{"/.well-known/*", "/healthz", "/readyz", "/livez"}

// bypassInboundPaths holds path patterns that skip inbound JWT validation.
// Defaults to defaultBypassInboundPaths; override via BYPASS_INBOUND_PATHS env var.
// Patterns use Go's path.Match syntax (e.g., "/.well-known/*" matches "/.well-known/agent.json").
var bypassInboundPaths = defaultBypassInboundPaths

// matchBypassPath checks if the given request path matches any configured bypass pattern.
// Query strings are stripped and the path is normalized before matching.
func matchBypassPath(requestPath string) bool {
	// Strip query string if present
	if idx := strings.IndexByte(requestPath, '?'); idx >= 0 {
		requestPath = requestPath[:idx]
	}
	// Normalize to prevent bypass via non-canonical forms (e.g., //healthz, /./healthz)
	requestPath = path.Clean(requestPath)
	for _, pattern := range bypassInboundPaths {
		matched, err := path.Match(pattern, requestPath)
		if err != nil {
			log.Printf("[Inbound] Invalid bypass pattern %q: %v", pattern, err)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

// readFileContent reads the content of a file, trimming whitespace
func readFileContent(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}

// loadConfig loads configuration from environment variables or files.
// For dynamic credentials from client-registration, it reads from /shared/ files.
// All configuration is loaded regardless of SPIRE enablement for simplicity.
func loadConfig() {
	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()

	// Static configuration from environment variables
	globalConfig.TokenURL = os.Getenv("TOKEN_URL")
	globalConfig.TargetAudience = os.Getenv("TARGET_AUDIENCE")
	globalConfig.TargetScopes = os.Getenv("TARGET_SCOPES")

	// SPIRE/SPIFFE configuration
	spireEnabled := os.Getenv("SPIRE_ENABLED")
	globalConfig.SpireEnabled = (spireEnabled == "true")

	// File paths for dynamic credentials
	clientIDFile := os.Getenv("CLIENT_ID_FILE")
	if clientIDFile == "" {
		clientIDFile = "/shared/client-id.txt"
	}
	clientSecretFile := os.Getenv("CLIENT_SECRET_FILE")
	if clientSecretFile == "" {
		clientSecretFile = "/shared/client-secret.txt"
	}
	jwtSvidPath := os.Getenv("JWT_SVID_PATH")
	if jwtSvidPath == "" {
		jwtSvidPath = "/opt/jwt_svid.token"
	}

	// Load CLIENT_ID from file or environment
	if clientID, err := readFileContent(clientIDFile); err == nil && clientID != "" {
		globalConfig.ClientID = clientID
		log.Printf("[Config] Loaded CLIENT_ID from file: %s", clientIDFile)
	} else if envClientID := os.Getenv("CLIENT_ID"); envClientID != "" {
		globalConfig.ClientID = envClientID
		log.Printf("[Config] Using CLIENT_ID from environment variable")
	}

	// Load CLIENT_SECRET from file or environment (always load, regardless of SPIRE)
	if clientSecret, err := readFileContent(clientSecretFile); err == nil && clientSecret != "" {
		globalConfig.ClientSecret = clientSecret
		log.Printf("[Config] Loaded CLIENT_SECRET from file: %s", clientSecretFile)
	} else if envClientSecret := os.Getenv("CLIENT_SECRET"); envClientSecret != "" {
		globalConfig.ClientSecret = envClientSecret
		log.Printf("[Config] Using CLIENT_SECRET from environment variable")
	}

	// Store JWT-SVID path (will be reloaded on each token exchange)
	globalConfig.JWTSvidPath = jwtSvidPath
	if jwtSvid, err := readFileContent(jwtSvidPath); err == nil && jwtSvid != "" {
		log.Printf("[Config] JWT-SVID available at: %s (%d bytes)", jwtSvidPath, len(jwtSvid))
	} else if err != nil {
		log.Printf("[Config] JWT-SVID not available at %s: %v", jwtSvidPath, err)
	}

	// Log configuration summary
	log.Printf("[Config] Configuration loaded:")
	log.Printf("[Config]   CLIENT_ID: %s", globalConfig.ClientID)
	log.Printf("[Config]   CLIENT_SECRET: [REDACTED, length=%d]", len(globalConfig.ClientSecret))
	log.Printf("[Config]   SPIRE_ENABLED: %v", globalConfig.SpireEnabled)
	log.Printf("[Config]   JWT_SVID_PATH: %s", globalConfig.JWTSvidPath)
	log.Printf("[Config]   TOKEN_URL: %s", globalConfig.TokenURL)
	log.Printf("[Config]   TARGET_AUDIENCE: %s", globalConfig.TargetAudience)
	log.Printf("[Config]   TARGET_SCOPES: %s", globalConfig.TargetScopes)
}

// waitForCredentials waits for credential files to be available
// This handles the case where client-registration hasn't finished yet
func waitForCredentials(maxWait time.Duration) bool {
	clientIDFile := os.Getenv("CLIENT_ID_FILE")
	if clientIDFile == "" {
		clientIDFile = "/shared/client-id.txt"
	}
	clientSecretFile := os.Getenv("CLIENT_SECRET_FILE")
	if clientSecretFile == "" {
		clientSecretFile = "/shared/client-secret.txt"
	}

	log.Printf("[Config] Waiting for credential files (max %v)...", maxWait)
	deadline := time.Now().Add(maxWait)

	for time.Now().Before(deadline) {
		// Check if both files exist and have content
		clientID, err1 := readFileContent(clientIDFile)
		clientSecret, err2 := readFileContent(clientSecretFile)

		if err1 == nil && err2 == nil && clientID != "" && clientSecret != "" {
			log.Printf("[Config] Credential files are ready")
			return true
		}

		log.Printf("[Config] Credentials not ready yet, waiting...")
		time.Sleep(2 * time.Second)
	}

	log.Printf("[Config] Timeout waiting for credentials, will use environment variables if available")
	return false
}

// getConfig returns the current configuration
func getConfig() (clientID, clientSecret, tokenURL, targetAudience, targetScopes string, spireEnabled bool, jwtSvidPath string) {
	globalConfig.mu.RLock()
	defer globalConfig.mu.RUnlock()
	return globalConfig.ClientID, globalConfig.ClientSecret, globalConfig.TokenURL, globalConfig.TargetAudience, globalConfig.TargetScopes, globalConfig.SpireEnabled, globalConfig.JWTSvidPath
}

var (
	jwksCache        *jwk.Cache
	inboundJWKSURL   string
	inboundIssuer    string
	expectedAudience string
)

// deriveJWKSURL derives the JWKS URL from the token endpoint URL.
// e.g. ".../protocol/openid-connect/token" -> ".../protocol/openid-connect/certs"
func deriveJWKSURL(tokenURL string) string {
	return strings.TrimSuffix(tokenURL, "/token") + "/certs"
}

// initJWKSCache initializes the JWKS cache for inbound token validation.
// The cache uses a default refresh window of 15 minutes. This means JWKS keys
// are automatically refreshed in the background, helping to prevent validation
// failures due to stale keys (e.g., after key rotation).
func initJWKSCache(jwksURL string) {
	ctx := context.Background()
	jwksCache = jwk.NewCache(ctx)
	if err := jwksCache.Register(jwksURL); err != nil {
		log.Printf("[Inbound] Failed to register JWKS URL %s: %v", jwksURL, err)
		return
	}
	log.Printf("[Inbound] JWKS cache initialized with URL: %s", jwksURL)
}

// validateInboundJWT validates a JWT token for inbound requests.
func validateInboundJWT(tokenString, jwksURL, expectedIssuer string) error {
	if jwksCache == nil {
		return fmt.Errorf("JWKS cache not initialized")
	}

	ctx := context.Background()
	keySet, err := jwksCache.Get(ctx, jwksURL)
	if err != nil {
		return fmt.Errorf("failed to fetch JWKS: %w", err)
	}

	token, err := jwt.Parse([]byte(tokenString), jwt.WithKeySet(keySet), jwt.WithValidate(true))
	if err != nil {
		return fmt.Errorf("failed to parse/validate token: %w", err)
	}

	if token.Issuer() != expectedIssuer {
		return fmt.Errorf("invalid issuer: expected %s, got %s", expectedIssuer, token.Issuer())
	}

	// Validate audience if EXPECTED_AUDIENCE is configured.
	// This is optional to support flexible deployment scenarios:
	// - Set EXPECTED_AUDIENCE for strict zero-trust validation
	// - Leave unset if audience validation is handled elsewhere (e.g., downstream service)
	// - In service mesh scenarios, the audience might vary based on routing
	if expectedAudience != "" {
		audiences := token.Audience()
		audienceValid := false
		for _, aud := range audiences {
			if aud == expectedAudience {
				audienceValid = true
				break
			}
		}
		if !audienceValid {
			return fmt.Errorf("invalid audience: expected %s, got %v", expectedAudience, audiences)
		}
	}

	log.Printf("[Inbound] Token validated - issuer: %s, audience: %v", token.Issuer(), token.Audience())
	return nil
}

// denyRequest returns a ProcessingResponse that sends a 401 Unauthorized to the client.
func denyRequest(message string) *v3.ProcessingResponse {
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &v3.ImmediateResponse{
				Status: &typev3.HttpStatus{
					Code: typev3.StatusCode_Unauthorized,
				},
				Body:    []byte(fmt.Sprintf(`{"error":"unauthorized","message":"%s"}`, message)),
				Details: "jwt_validation_failed",
			},
		},
	}
}

// denyOutboundRequest returns a 503 Service Unavailable when outbound token
// acquisition fails, preventing unauthenticated requests from reaching downstream.
func denyOutboundRequest(message string) *v3.ProcessingResponse {
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &v3.ImmediateResponse{
				Status: &typev3.HttpStatus{
					Code: typev3.StatusCode_ServiceUnavailable,
				},
				Body: []byte(fmt.Sprintf(`{"error":"token_acquisition_failed","message":"%s"}`, message)),
			},
		},
	}
}

// getHostFromHeaders extracts host from :authority (HTTP/2) or Host header
func getHostFromHeaders(headers []*core.HeaderValue) string {
	if host := getHeaderValue(headers, ":authority"); host != "" {
		return host
	}
	return getHeaderValue(headers, "host")
}

// exchangeToken performs OAuth 2.0 Token Exchange (RFC 8693).
// Exchanges the subject token for a new token with the specified audience.
//
// Two authentication modes are supported:
// 1. Traditional: Uses client_secret for client authentication
// 2. SPIFFE Federated: Uses JWT-SVID (reloaded from file on each call) as client_assertion
//
// When spireEnabled=true:
// - Reloads JWT-SVID from the jwtSvidPath parameter on each token exchange
// - Uses client_assertion_type: "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe"
// - Uses client_assertion: <JWT-SVID> instead of client_secret
// - Requires Keycloak with federated-jwt client authenticator and SPIFFE identity provider
//
// When spireEnabled=false:
// - Uses traditional client_secret authentication
// - Requires the exchanging client to be in the subject token's audience
func exchangeToken(clientID, clientSecret, tokenURL, subjectToken, audience, scopes string, spireEnabled bool, jwtSvidPath string) (string, error) {
	log.Printf("[Token Exchange] Starting token exchange")
	log.Printf("[Token Exchange] Token URL: %s", tokenURL)
	log.Printf("[Token Exchange] Client ID: %s", clientID)
	log.Printf("[Token Exchange] Audience: %s", audience)
	log.Printf("[Token Exchange] Scopes: %s", scopes)
	log.Printf("[Token Exchange] SPIRE Enabled: %v", spireEnabled)

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	data.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("subject_token", subjectToken)
	data.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("audience", audience)
	data.Set("scope", scopes)

	// Choose authentication method based on SPIRE enablement
	if spireEnabled {
		// SPIFFE Federated Authentication
		log.Printf("[Token Exchange] Using SPIFFE federated authentication")

		// Reload JWT-SVID from file on each token exchange to ensure it's fresh
		// spiffe-helper continuously updates this file (~every 2.5 minutes)
		jwtSvid, err := readFileContent(jwtSvidPath)
		if err != nil || jwtSvid == "" {
			log.Printf("[Token Exchange] Failed to load JWT-SVID from %s: %v", jwtSvidPath, err)
			return "", fmt.Errorf("JWT-SVID unavailable at %s (SPIRE enabled but file not readable)", jwtSvidPath)
		}

		log.Printf("[Token Exchange] Loaded fresh JWT-SVID from %s (%d bytes)", jwtSvidPath, len(jwtSvid))

		// Use jwt-spiffe assertion type (required by Keycloak's SPIFFE provider)
		data.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-spiffe")
		data.Set("client_assertion", jwtSvid)
	} else {
		// Traditional client_secret authentication
		log.Printf("[Token Exchange] Using traditional client_secret authentication")
		data.Set("client_secret", clientSecret)
	}

	resp, err := http.PostForm(tokenURL, data)
	if err != nil {
		log.Printf("[Token Exchange] Failed to make request: %v", err)
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[Token Exchange] Failed to read response: %v", err)
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("[Token Exchange] Failed with status %d: %s", resp.StatusCode, string(body))
		return "", status.Errorf(codes.Internal, "token exchange failed: %s", string(body))
	}

	var tokenResp tokenExchangeResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		log.Printf("[Token Exchange] Failed to parse response: %v", err)
		return "", err
	}

	log.Printf("[Token Exchange] Successfully exchanged token")
	return tokenResp.AccessToken, nil
}

// clientCredentialsGrant performs an OAuth 2.0 Client Credentials grant.
// Used as a fallback when no Authorization header is present on outbound requests.
// The agent's identity (client-id/client-secret from client-registration) is used
// to obtain a token scoped to the target audience.
func clientCredentialsGrant(clientID, clientSecret, tokenURL, audience, scopes string) (string, error) {
	log.Printf("[Client Credentials] Starting client credentials grant")
	log.Printf("[Client Credentials] Token URL: %s", tokenURL)
	log.Printf("[Client Credentials] Client ID: %s", clientID)
	log.Printf("[Client Credentials] Audience: %s", audience)
	log.Printf("[Client Credentials] Scopes: %s", scopes)

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("grant_type", "client_credentials")
	data.Set("audience", audience)
	data.Set("scope", scopes)

	resp, err := http.PostForm(tokenURL, data)
	if err != nil {
		log.Printf("[Client Credentials] Failed to make request: %v", err)
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[Client Credentials] Failed to read response: %v", err)
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("[Client Credentials] Failed with status %d: %s", resp.StatusCode, string(body))
		return "", status.Errorf(codes.Internal, "client credentials grant failed: %s", string(body))
	}

	var tokenResp tokenExchangeResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		log.Printf("[Client Credentials] Failed to parse response: %v", err)
		return "", err
	}

	log.Printf("[Client Credentials] Successfully obtained token")
	return tokenResp.AccessToken, nil
}

func getHeaderValue(headers []*core.HeaderValue, key string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Key, key) {
			return string(header.RawValue)
		}
	}
	return ""
}

// handleInbound processes inbound traffic by validating the JWT token.
func (p *processor) handleInbound(headers *core.HeaderMap) *v3.ProcessingResponse {
	log.Println("=== Inbound Request Headers ===")
	if headers != nil {
		for _, header := range headers.Headers {
			if !strings.EqualFold(header.Key, "authorization") &&
				!strings.EqualFold(header.Key, "x-client-secret") {
				log.Printf("%s: %s", header.Key, string(header.RawValue))
			}
		}
	}

	if jwksCache == nil || inboundIssuer == "" {
		log.Println("[Inbound] Inbound validation not configured (ISSUER or TOKEN_URL missing), skipping")
		return &v3.ProcessingResponse{
			Response: &v3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &v3.HeadersResponse{},
			},
		}
	}

	// Check if the request path matches a bypass pattern
	if requestPath := getHeaderValue(headers.Headers, ":path"); requestPath != "" && matchBypassPath(requestPath) {
		log.Printf("[Inbound] Path %q matches bypass pattern, skipping JWT validation", requestPath)
		return &v3.ProcessingResponse{
			Response: &v3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &v3.HeadersResponse{
					Response: &v3.CommonResponse{
						HeaderMutation: &v3.HeaderMutation{
							RemoveHeaders: []string{"x-authbridge-direction"},
						},
					},
				},
			},
		}
	}

	authHeader := getHeaderValue(headers.Headers, "authorization")
	if authHeader == "" {
		log.Println("[Inbound] Missing Authorization header")
		return denyRequest("missing Authorization header")
	}

	tokenString := strings.TrimPrefix(authHeader, "Bearer ")
	tokenString = strings.TrimPrefix(tokenString, "bearer ")
	if tokenString == authHeader {
		log.Println("[Inbound] Invalid Authorization header format")
		return denyRequest("invalid Authorization header format")
	}

	if err := validateInboundJWT(tokenString, inboundJWKSURL, inboundIssuer); err != nil {
		log.Printf("[Inbound] JWT validation failed: %v", err)
		return denyRequest(fmt.Sprintf("token validation failed: %v", err))
	}

	log.Println("[Inbound] JWT validation succeeded, forwarding request")
	// Remove the x-authbridge-direction header so the app never sees it
	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &v3.HeadersResponse{
				Response: &v3.CommonResponse{
					HeaderMutation: &v3.HeaderMutation{
						RemoveHeaders: []string{"x-authbridge-direction"},
					},
				},
			},
		},
	}
}

// handleOutbound processes outbound traffic by performing token exchange.
// It uses the resolver to get per-host configuration for audience/scopes/tokenURL.
func (p *processor) handleOutbound(ctx context.Context, headers *core.HeaderMap) *v3.ProcessingResponse {
	log.Println("=== Outbound Request Headers ===")
	if headers != nil {
		for _, header := range headers.Headers {
			if !strings.EqualFold(header.Key, "authorization") &&
				!strings.EqualFold(header.Key, "x-client-secret") {
				log.Printf("%s: %s", header.Key, string(header.RawValue))
			}
		}
	}

	// Extract host and resolve target configuration
	requestHost := getHostFromHeaders(headers.Headers)
	targetConfig, err := globalResolver.Resolve(ctx, requestHost)
	if err != nil {
		log.Printf("[Resolver] Error resolving host %q: %v", requestHost, err)
	}

	// Handle passthrough routes - skip token exchange
	if targetConfig != nil && targetConfig.Passthrough {
		log.Printf("[Resolver] Passthrough enabled for host %q, skipping token exchange", requestHost)
		return &v3.ProcessingResponse{
			Response: &v3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &v3.HeadersResponse{},
			},
		}
	}

	// Get global configuration (from files or env vars)
	clientID, clientSecret, tokenURL, targetAudience, targetScopes, spireEnabled, jwtSvidPath := getConfig()

	// Apply target-specific overrides if available
	if targetConfig != nil {
		log.Printf("[Resolver] Applying target config for host %q", requestHost)
		if targetConfig.Audience != "" {
			targetAudience = targetConfig.Audience
			log.Printf("[Resolver] Using target audience: %s", targetAudience)
		}
		if targetConfig.Scopes != "" {
			targetScopes = targetConfig.Scopes
			log.Printf("[Resolver] Using target scopes: %s", targetScopes)
		}
		if targetConfig.TokenEndpoint != "" {
			tokenURL = targetConfig.TokenEndpoint
			log.Printf("[Resolver] Using target token_url: %s", tokenURL)
		}
	}

	// Check if we have required config (clientSecret not needed when SPIRE is enabled)
	hasCredentials := (spireEnabled && jwtSvidPath != "") || clientSecret != ""
	if clientID != "" && hasCredentials && tokenURL != "" && targetAudience != "" && targetScopes != "" {
		log.Println("[Token Exchange] Configuration loaded, attempting token exchange")
		log.Printf("[Token Exchange] Client ID: %s", clientID)
		log.Printf("[Token Exchange] Target Audience: %s", targetAudience)
		log.Printf("[Token Exchange] Target Scopes: %s", targetScopes)

		authHeader := getHeaderValue(headers.Headers, "authorization")
		if authHeader != "" {
			subjectToken := strings.TrimPrefix(authHeader, "Bearer ")
			subjectToken = strings.TrimPrefix(subjectToken, "bearer ")

			if subjectToken != authHeader {
				newToken, err := exchangeToken(clientID, clientSecret, tokenURL, subjectToken, targetAudience, targetScopes, spireEnabled, jwtSvidPath)
				if err == nil {
					log.Printf("[Token Exchange] Successfully exchanged token, replacing Authorization header")
					return &v3.ProcessingResponse{
						Response: &v3.ProcessingResponse_RequestHeaders{
							RequestHeaders: &v3.HeadersResponse{
								Response: &v3.CommonResponse{
									HeaderMutation: &v3.HeaderMutation{
										SetHeaders: []*core.HeaderValueOption{
											{
												Header: &core.HeaderValue{
													Key:      "authorization",
													RawValue: []byte("Bearer " + newToken),
												},
											},
										},
									},
								},
							},
						},
					}
				}
				log.Printf("[Token Exchange] Failed to exchange token: %v", err)
				return denyOutboundRequest("token exchange failed")
			} else {
				log.Printf("[Token Exchange] Invalid Authorization header format")
				return denyOutboundRequest("invalid Authorization header format")
			}
		} else {
			// No Authorization header on outbound — fall back to client_credentials.
			// This handles agent frameworks that don't propagate the inbound token.
			// The token uses the agent's identity rather than the end user's.
			log.Printf("[Client Credentials] No Authorization header on outbound, falling back to client_credentials grant")
			newToken, err := clientCredentialsGrant(clientID, clientSecret, tokenURL, targetAudience, targetScopes)
			if err == nil {
				log.Printf("[Client Credentials] Injecting token into outbound request")
				return &v3.ProcessingResponse{
					Response: &v3.ProcessingResponse_RequestHeaders{
						RequestHeaders: &v3.HeadersResponse{
							Response: &v3.CommonResponse{
								HeaderMutation: &v3.HeaderMutation{
									SetHeaders: []*core.HeaderValueOption{
										{
											Header: &core.HeaderValue{
												Key:      "authorization",
												RawValue: []byte("Bearer " + newToken),
											},
										},
									},
								},
							},
						},
					},
				}
			}
			log.Printf("[Client Credentials] Failed to obtain token: %v", err)
			return denyOutboundRequest("client credentials token acquisition failed")
		}
	} else {
		log.Println("[Token Exchange] Missing configuration, skipping token exchange")
		log.Printf("[Token Exchange] CLIENT_ID present: %v, CLIENT_SECRET present: %v, TOKEN_URL present: %v",
			clientID != "", clientSecret != "", tokenURL != "")
		log.Printf("[Token Exchange] TARGET_AUDIENCE present: %v, TARGET_SCOPES present: %v",
			targetAudience != "", targetScopes != "")
	}

	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &v3.HeadersResponse{},
		},
	}
}

func (p *processor) Process(stream v3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := stream.Recv()
		if err != nil {
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		resp := &v3.ProcessingResponse{}

		switch r := req.Request.(type) {
		case *v3.ProcessingRequest_RequestHeaders:
			headers := r.RequestHeaders.Headers
			direction := getHeaderValue(headers.Headers, "x-authbridge-direction")

			if direction == "inbound" {
				resp = p.handleInbound(headers)
			} else {
				resp = p.handleOutbound(ctx, headers)
			}

		case *v3.ProcessingRequest_ResponseHeaders:
			log.Println("=== Response Headers ===")
			headers := r.ResponseHeaders.Headers
			if headers != nil {
				for _, header := range headers.Headers {
					log.Printf("%s: %s", header.Key, string(header.RawValue))
				}
			}
			resp = &v3.ProcessingResponse{
				Response: &v3.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &v3.HeadersResponse{},
				},
			}

		default:
			log.Printf("Unknown request type: %T\n", r)
		}

		if err := stream.Send(resp); err != nil {
			return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
		}
	}
}

func main() {
	log.Println("=== Go External Processor Starting ===")

	// Wait for credential files from client-registration (up to 60 seconds)
	// This handles the startup race condition with client-registration container
	waitForCredentials(60 * time.Second)

	// Load configuration from files (or environment variables as fallback)
	loadConfig()

	// Initialize inbound JWT validation
	_, _, tokenURL, _, _, _, _ := getConfig() // clientID, clientSecret, tokenURL, targetAudience, targetScopes, spireEnabled, jwtSvid
	inboundIssuer = os.Getenv("ISSUER")
	expectedAudience = os.Getenv("EXPECTED_AUDIENCE")
	if tokenURL != "" && inboundIssuer != "" {
		inboundJWKSURL = deriveJWKSURL(tokenURL)
		initJWKSCache(inboundJWKSURL)
		log.Printf("[Inbound] Issuer: %s", inboundIssuer)
		if expectedAudience != "" {
			log.Printf("[Inbound] Expected audience: %s", expectedAudience)
		} else {
			log.Printf("[Inbound] Audience validation disabled (EXPECTED_AUDIENCE not set)")
		}
	} else {
		if tokenURL == "" {
			log.Println("[Inbound] TOKEN_URL not configured, inbound JWT validation disabled")
		}
		if inboundIssuer == "" {
			log.Println("[Inbound] ISSUER not configured, inbound JWT validation disabled")
		}
	}

	// Initialize inbound bypass paths (override defaults if env var is set)
	if bypassEnv, ok := os.LookupEnv("BYPASS_INBOUND_PATHS"); ok {
		bypassInboundPaths = nil
		for _, p := range strings.Split(bypassEnv, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, err := path.Match(p, "/"); err != nil {
				log.Printf("[Inbound] Ignoring invalid bypass path pattern %q: %v", p, err)
				continue
			}
			bypassInboundPaths = append(bypassInboundPaths, p)
		}
	}
	log.Printf("[Inbound] Bypass paths: %v", bypassInboundPaths)

	// Initialize the target resolver
	configPath := os.Getenv("ROUTES_CONFIG_PATH")
	if configPath == "" {
		configPath = defaultRoutesConfigPath
	}
	var err error
	globalResolver, err = resolver.NewStaticResolver(configPath)
	if err != nil {
		log.Fatalf("failed to load routes config: %v", err)
	}

	// Start gRPC server
	port := ":9090"
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	v3.RegisterExternalProcessorServer(grpcServer, &processor{})

	log.Printf("Starting Go external processor on %s", port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
