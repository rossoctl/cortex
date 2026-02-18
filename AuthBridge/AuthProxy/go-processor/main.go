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
	"strings"
	"sync"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
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
	mu             sync.RWMutex
}

var globalConfig = &Config{}

type processor struct {
	v3.UnimplementedExternalProcessorServer
	mu          sync.Mutex
	streamSpans map[v3.ExternalProcessor_ProcessServer]*streamSpanState
}

type tokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

const defaultRoutesConfigPath = "/etc/authproxy/routes.yaml"

var globalResolver resolver.TargetResolver

// OTEL agent config
var (
	agentName      string
	agentVersion   string
	agentProvider  string
	serviceName    string
	otelTracer     trace.Tracer
	textPropagator propagation.TextMapPropagator
	otelEnabled    bool
)

// A2A JSON-RPC parsing
type a2aRequest struct {
	Method string `json:"method"`
	Params struct {
		ContextID string `json:"contextId"`
		Message   struct {
			MessageID string `json:"messageId"`
			ContextID string `json:"contextId"`
			Parts     []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"message"`
	} `json:"params"`
}

type a2aResponse struct {
	Result struct {
		Artifacts []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"artifacts"`
	} `json:"result"`
}

// streamSpanState tracks the OTEL span and accumulated response data for a stream.
type streamSpanState struct {
	span           trace.Span
	ctx            context.Context
	responseBody   []byte             // accumulated response chunks for STREAMED mode
	childSpanIndex int                // counter for nested child spans (LLM/tool events)
	taskID         string             // A2A task ID for fire-and-forget result fetching
	hasOutput      bool               // whether output has been set on the root span
	completed      bool               // whether the main stream completed normally (end_of_stream)
	resubCancel    context.CancelFunc // cancel function for the resubscribe goroutine
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
// Retries loading credentials from files if they're not immediately available.
func loadConfig() {
	globalConfig.mu.Lock()
	defer globalConfig.mu.Unlock()

	// Static configuration from environment variables
	globalConfig.TokenURL = os.Getenv("TOKEN_URL")
	globalConfig.TargetAudience = os.Getenv("TARGET_AUDIENCE")
	globalConfig.TargetScopes = os.Getenv("TARGET_SCOPES")

	// For CLIENT_ID and CLIENT_SECRET, prefer files from /shared/ (dynamic credentials)
	// This allows AuthProxy to use the same credentials as the auto-registered client
	clientIDFile := os.Getenv("CLIENT_ID_FILE")
	if clientIDFile == "" {
		clientIDFile = "/shared/client-id.txt"
	}
	clientSecretFile := os.Getenv("CLIENT_SECRET_FILE")
	if clientSecretFile == "" {
		clientSecretFile = "/shared/client-secret.txt"
	}

	// Try to load from files first (preferred for SPIFFE-based dynamic credentials)
	if clientID, err := readFileContent(clientIDFile); err == nil && clientID != "" {
		globalConfig.ClientID = clientID
		log.Printf("[Config] Loaded CLIENT_ID from file: %s", clientIDFile)
	} else if envClientID := os.Getenv("CLIENT_ID"); envClientID != "" {
		// Fall back to environment variable
		globalConfig.ClientID = envClientID
		log.Printf("[Config] Using CLIENT_ID from environment variable")
	}

	if clientSecret, err := readFileContent(clientSecretFile); err == nil && clientSecret != "" {
		globalConfig.ClientSecret = clientSecret
		log.Printf("[Config] Loaded CLIENT_SECRET from file: %s", clientSecretFile)
	} else if envClientSecret := os.Getenv("CLIENT_SECRET"); envClientSecret != "" {
		// Fall back to environment variable
		globalConfig.ClientSecret = envClientSecret
		log.Printf("[Config] Using CLIENT_SECRET from environment variable")
	}

	log.Printf("[Config] Configuration loaded:")
	log.Printf("[Config]   CLIENT_ID: %s", globalConfig.ClientID)
	log.Printf("[Config]   CLIENT_SECRET: [REDACTED, length=%d]", len(globalConfig.ClientSecret))
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
func getConfig() (clientID, clientSecret, tokenURL, targetAudience, targetScopes string) {
	globalConfig.mu.RLock()
	defer globalConfig.mu.RUnlock()
	return globalConfig.ClientID, globalConfig.ClientSecret, globalConfig.TokenURL, globalConfig.TargetAudience, globalConfig.TargetScopes
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

// getHostFromHeaders extracts host from :authority (HTTP/2) or Host header
func getHostFromHeaders(headers []*core.HeaderValue) string {
	if host := getHeaderValue(headers, ":authority"); host != "" {
		return host
	}
	return getHeaderValue(headers, "host")
}

// exchangeToken performs OAuth 2.0 Token Exchange (RFC 8693).
// Exchanges the subject token for a new token with the specified audience.
// Requires the exchanging client to be in the subject token's audience.
// When using dynamic credentials from /shared/, this works because the token's
// audience matches the auto-registered client's SPIFFE ID.
func exchangeToken(clientID, clientSecret, tokenURL, subjectToken, audience, scopes string) (string, error) {
	log.Printf("[Token Exchange] Starting token exchange")
	log.Printf("[Token Exchange] Token URL: %s", tokenURL)
	log.Printf("[Token Exchange] Client ID: %s", clientID)
	log.Printf("[Token Exchange] Audience: %s", audience)
	log.Printf("[Token Exchange] Scopes: %s", scopes)

	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	data.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("subject_token", subjectToken)
	data.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	data.Set("audience", audience)
	data.Set("scope", scopes)

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

func getHeaderValue(headers []*core.HeaderValue, key string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Key, key) {
			return string(header.RawValue)
		}
	}
	return ""
}

// handleInbound processes inbound traffic by validating the JWT token.
// When OTEL is enabled, also creates the root span and injects traceparent header.
func (p *processor) handleInbound(stream v3.ExternalProcessor_ProcessServer, headers *core.HeaderMap) *v3.ProcessingResponse {
	log.Println("=== Inbound Request Headers ===")
	if headers != nil {
		for _, header := range headers.Headers {
			if !strings.EqualFold(header.Key, "authorization") &&
				!strings.EqualFold(header.Key, "x-client-secret") {
				log.Printf("%s: %s", header.Key, string(header.RawValue))
			}
		}
	}

	// JWT validation (if configured)
	if jwksCache != nil && inboundIssuer != "" {
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
	} else {
		log.Println("[Inbound] Inbound validation not configured (ISSUER or TOKEN_URL missing), skipping")
	}

	// Build header mutations
	removeHeaders := []string{"x-authbridge-direction"}
	var setHeaders []*core.HeaderValueOption

	// Skip OTEL span creation for non-API paths (agent card, health)
	reqPath := getHeaderValue(headers.Headers, ":path")
	isAPIRequest := reqPath == "/" || strings.HasPrefix(reqPath, "/?")

	// Create OTEL root span NOW (during header processing) so traceparent
	// is injected BEFORE Envoy forwards headers to the agent.
	// Input/output will be set later during body processing.
	if otelEnabled && otelTracer != nil && isAPIRequest {
		spanName := fmt.Sprintf("invoke_agent %s", agentName)
		ctx, span := otelTracer.Start(context.Background(), spanName,
			trace.WithSpanKind(trace.SpanKindInternal),
		)

		// Set GenAI semantic convention attributes only.
		// MLflow/OpenInference attributes are derived by the OTEL Collector.
		span.SetAttributes(
			attribute.String("gen_ai.operation.name", "invoke_agent"),
			attribute.String("gen_ai.provider.name", agentProvider),
			attribute.String("gen_ai.agent.name", agentName),
			attribute.String("gen_ai.agent.version", agentVersion),
		)

		// Store span for body processing
		p.mu.Lock()
		p.streamSpans[stream] = &streamSpanState{span: span, ctx: ctx}
		p.mu.Unlock()

		// Inject traceparent header — THIS is the critical part.
		// The agent's OTEL SDK will read this header and create child spans
		// under our root span's trace context.
		carrier := propagation.MapCarrier{}
		textPropagator.Inject(ctx, carrier)
		for key, value := range carrier {
			setHeaders = append(setHeaders, &core.HeaderValueOption{
				Header: &core.HeaderValue{Key: key, RawValue: []byte(value)},
			})
		}

		log.Printf("[OTEL] Created root span at HEADER phase: %s (traceparent injected)", spanName)
	}

	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &v3.HeadersResponse{
				Response: &v3.CommonResponse{
					HeaderMutation: &v3.HeaderMutation{
						RemoveHeaders: removeHeaders,
						SetHeaders:    setHeaders,
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
	clientID, clientSecret, tokenURL, targetAudience, targetScopes := getConfig()

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

	if clientID != "" && clientSecret != "" && tokenURL != "" && targetAudience != "" && targetScopes != "" {
		log.Println("[Token Exchange] Configuration loaded, attempting token exchange")
		log.Printf("[Token Exchange] Client ID: %s", clientID)
		log.Printf("[Token Exchange] Target Audience: %s", targetAudience)
		log.Printf("[Token Exchange] Target Scopes: %s", targetScopes)

		authHeader := getHeaderValue(headers.Headers, "authorization")
		if authHeader != "" {
			subjectToken := strings.TrimPrefix(authHeader, "Bearer ")
			subjectToken = strings.TrimPrefix(subjectToken, "bearer ")

			if subjectToken != authHeader {
				newToken, err := exchangeToken(clientID, clientSecret, tokenURL, subjectToken, targetAudience, targetScopes)
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
			} else {
				log.Printf("[Token Exchange] Invalid Authorization header format")
			}
		} else {
			log.Printf("[Token Exchange] No Authorization header found")
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

// handleRequestBody enriches the existing root span with A2A request body data.
// The span was already created during header processing (handleInbound).
func (p *processor) handleRequestBody(stream v3.ExternalProcessor_ProcessServer, body []byte) *v3.ProcessingResponse {
	p.mu.Lock()
	state := p.streamSpans[stream]
	p.mu.Unlock()

	if state == nil || state.span == nil || !otelEnabled {
		return &v3.ProcessingResponse{
			Response: &v3.ProcessingResponse_RequestBody{
				RequestBody: &v3.BodyResponse{},
			},
		}
	}

	// Parse A2A JSON-RPC body to extract input and conversation ID
	var req a2aRequest
	if err := json.Unmarshal(body, &req); err != nil {
		log.Printf("[OTEL] Failed to parse A2A request body: %v", err)
		return &v3.ProcessingResponse{
			Response: &v3.ProcessingResponse_RequestBody{
				RequestBody: &v3.BodyResponse{},
			},
		}
	}

	userInput := ""
	if len(req.Params.Message.Parts) > 0 {
		userInput = req.Params.Message.Parts[0].Text
	}
	conversationID := req.Params.ContextID
	if conversationID == "" {
		conversationID = req.Params.Message.ContextID
	}

	// Enrich the root span with GenAI attributes only.
	// MLflow/OpenInference attributes are derived by the OTEL Collector.
	if userInput != "" {
		state.span.SetAttributes(
			attribute.String("gen_ai.prompt", userInput),
		)
	}
	if conversationID != "" {
		state.span.SetAttributes(
			attribute.String("gen_ai.conversation.id", conversationID),
		)
	}

	log.Printf("[OTEL] Enriched root span with body: input=%d chars, conversation=%s",
		len(userInput), conversationID)

	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_RequestBody{
			RequestBody: &v3.BodyResponse{},
		},
	}
}

// handleResponseBody processes response chunks as they stream through.
// For each SSE chunk, it parses events and creates nested child spans for
// LLM and tool events. On end_of_stream, it sets the output on the root span.
func (p *processor) handleResponseBody(stream v3.ExternalProcessor_ProcessServer, body []byte, endOfStream bool) *v3.ProcessingResponse {
	p.mu.Lock()
	state := p.streamSpans[stream]

	if state != nil {
		// Accumulate response body chunks
		state.responseBody = append(state.responseBody, body...)

		// Parse SSE events from this chunk and create child spans
		if otelEnabled && len(body) > 0 {
			for _, jsonStr := range parseSSEEvents(body) {
				// Extract task ID and context ID from SSE events.
				if state.taskID == "" {
					tid, cid := extractTaskIDAndContext(jsonStr)
					if cid != "" {
						state.span.SetAttributes(
							attribute.String("gen_ai.conversation.id", cid),
						)
						log.Printf("[OTEL] Set conversation ID: %s", cid)
					}
					if tid != "" {
						state.taskID = tid
						log.Printf("[OTEL] Captured task ID: %s", tid)

						// Start background resubscribe immediately while stream is active.
						resubCtx, resubCancel := context.WithCancel(context.Background())
						state.resubCancel = resubCancel
						go func(ctx context.Context, taskID string, span trace.Span, spanCtx context.Context) {
							output, childCount := resubscribeAndCapture(ctx, taskID, span, spanCtx, 0)
							if ctx.Err() != nil {
								return
							}
							if output != "" {
								span.SetAttributes(
									attribute.String("gen_ai.completion", output),
								)
								span.SetStatus(otelcodes.Ok, "")
								log.Printf("[OTEL] resubscribe recovered output (%d chars, %d child spans)", len(output), childCount)
							} else {
								span.SetStatus(otelcodes.Ok, "client disconnected")
								log.Printf("[OTEL] resubscribe: no output (taskID=%s, %d child spans)", taskID, childCount)
							}
							span.End()
							log.Printf("[OTEL] resubscribe: root span ended")
						}(resubCtx, tid, state.span, state.ctx)
					}
				}

				eventType, text := classifySSEEvent(jsonStr)
				switch eventType {
				case "llm", "tool":
					p.createChildSpan(state, eventType, text)
				case "artifact":
					if text != "" {
						state.span.SetAttributes(
							attribute.String("gen_ai.completion", text),
						)
						state.hasOutput = true
						log.Printf("[OTEL] Set output on root span (%d chars)", len(text))
					}
				}
			}
		}
	}

	if !endOfStream {
		p.mu.Unlock()
		return &v3.ProcessingResponse{
			Response: &v3.ProcessingResponse_ResponseBody{
				ResponseBody: &v3.BodyResponse{},
			},
		}
	}

	// End of stream — main stream completed normally
	delete(p.streamSpans, stream)
	p.mu.Unlock()

	if state != nil && state.span != nil {
		state.completed = true

		// Cancel the background resubscribe goroutine — not needed
		if state.resubCancel != nil {
			state.resubCancel()
			log.Printf("[OTEL] Cancelled background resubscribe (main stream completed)")
		}

		fullBody := state.responseBody

		// If no child spans were created from SSE events, try extracting
		// from JSON-RPC result.history (non-streaming response format).
		if state.childSpanIndex == 0 {
			p.extractChildSpansFromHistory(state, fullBody)
		}

		// If output wasn't set from artifact events, try extracting from full body
		if state.span.IsRecording() {
			output := extractA2AOutput(fullBody)
			if output != "" {
				state.span.SetAttributes(
					attribute.String("gen_ai.completion", output),
				)
				log.Printf("[OTEL] Set output on root span from full body (%d chars)", len(output))
			}
		}

		state.span.SetStatus(otelcodes.Ok, "")
		state.span.End()
		log.Printf("[OTEL] Root span ended (accumulated %d bytes, %d child spans)", len(fullBody), state.childSpanIndex)
	}

	return &v3.ProcessingResponse{
		Response: &v3.ProcessingResponse_ResponseBody{
			ResponseBody: &v3.BodyResponse{},
		},
	}
}

// extractChildSpansFromHistory parses a JSON-RPC response body and creates
// child spans from result.history messages. This handles the non-streaming
// response format where the A2A SDK returns a complete task with history
// instead of SSE events.
//
// The A2A response has: history (status-update messages) + artifacts (final answer).
// LangGraph flow: LLM(tool_call) → tool(result) → LLM(final_answer)
// But the 3rd LLM step often isn't in history (gets merged into the artifact).
// When we detect a tool span without a following LLM span, we infer the final
// LLM call and create a "chat" span for it.
func (p *processor) extractChildSpansFromHistory(state *streamSpanState, body []byte) {
	if state == nil || state.span == nil || otelTracer == nil {
		return
	}

	var rpcResp struct {
		Result struct {
			ContextID string `json:"contextId"`
			History   []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"history"`
			Artifacts []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"artifacts"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"result"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return
	}

	// Set conversation ID from contextId
	if rpcResp.Result.ContextID != "" {
		state.span.SetAttributes(
			attribute.String("gen_ai.conversation.id", rpcResp.Result.ContextID),
		)
	}

	// Track the last event type to detect missing final LLM span
	lastEventType := ""

	// Iterate over history messages from the agent (skip user messages)
	for _, msg := range rpcResp.Result.History {
		if msg.Role != "agent" || len(msg.Parts) == 0 {
			continue
		}
		text := msg.Parts[0].Text

		// Classify using the same text patterns as classifySSEEvent
		var eventType string
		if strings.Contains(text, "tools:") {
			eventType = "tool"
		} else if strings.Contains(text, "assistant:") {
			eventType = "llm"
		}

		if eventType != "" {
			p.createChildSpan(state, eventType, text)
			lastEventType = eventType
		}
	}

	// If the last history event was a tool call and the task completed with
	// an artifact, infer the final LLM call that produced the answer.
	// LangGraph flow: LLM → tool → LLM(final) → artifact
	// The 3rd LLM step is often not in history (merged into completion).
	if lastEventType == "tool" &&
		rpcResp.Result.Status.State == "completed" &&
		len(rpcResp.Result.Artifacts) > 0 {

		// Create a "chat" span for the inferred final LLM call
		state.childSpanIndex++
		idx := state.childSpanIndex
		spanName := "chat"

		var attrs []attribute.KeyValue
		attrs = append(attrs,
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.system", agentProvider),
			attribute.Int("event.index", idx),
		)

		// Add the artifact output as completion text
		if len(rpcResp.Result.Artifacts[0].Parts) > 0 {
			output := rpcResp.Result.Artifacts[0].Parts[0].Text
			if len(output) > 1000 {
				output = output[:1000]
			}
			attrs = append(attrs, attribute.String("gen_ai.completion", output))
		}

		_, childSpan := otelTracer.Start(state.ctx, spanName,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attrs...),
		)
		childSpan.SetStatus(otelcodes.Ok, "")
		childSpan.End()
		log.Printf("[OTEL] Created inferred final LLM span: %s (step %d)", spanName, idx)
	}

	if state.childSpanIndex > 0 {
		log.Printf("[OTEL] Created %d child spans from JSON-RPC history", state.childSpanIndex)
	}
}

// cleanupSpan handles span completion when the stream disconnects.
func (p *processor) cleanupSpan(stream v3.ExternalProcessor_ProcessServer) {
	p.mu.Lock()
	state := p.streamSpans[stream]
	delete(p.streamSpans, stream)
	p.mu.Unlock()
	if state == nil || state.span == nil {
		return
	}

	// If the resubscribe goroutine is running, let it handle span completion.
	if state.taskID != "" && state.resubCancel != nil {
		log.Printf("[OTEL] Client disconnected — resubscribe goroutine will complete the trace (taskID=%s, %d child spans so far)", state.taskID, state.childSpanIndex)
		return
	}

	// No resubscribe running — end span with whatever we have
	if state.hasOutput {
		state.span.SetStatus(otelcodes.Ok, "")
	} else {
		state.span.SetStatus(otelcodes.Ok, "client disconnected")
	}
	state.span.End()
	log.Printf("[OTEL] Stream disconnected, no resubscribe available (%d child spans)", state.childSpanIndex)
}

// createChildSpan creates a nested child span under the root invoke_agent span.
func (p *processor) createChildSpan(state *streamSpanState, eventType string, text string) {
	if state == nil || state.span == nil || otelTracer == nil {
		return
	}

	state.childSpanIndex++
	idx := state.childSpanIndex

	spanName, genaiAttrs := extractGenAIAttrsFromJSON(text, eventType)

	if spanName == "" {
		if eventType == "llm" {
			spanName = "chat"
		} else {
			spanName = "execute_tool"
		}
	}

	var attrs []attribute.KeyValue
	if eventType == "llm" {
		attrs = append(attrs,
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.system", agentProvider),
		)
	} else {
		attrs = append(attrs, attribute.String("gen_ai.operation.name", "tool"))
	}
	attrs = append(attrs, genaiAttrs...)
	attrs = append(attrs, attribute.Int("event.index", idx))

	_, childSpan := otelTracer.Start(state.ctx, spanName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	childSpan.SetStatus(otelcodes.Ok, "")
	childSpan.End()
	log.Printf("[OTEL] Created child span: %s (step %d, %d attrs)", spanName, idx, len(attrs))
}

func (p *processor) Process(stream v3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			p.cleanupSpan(stream)
			return ctx.Err()
		default:
		}

		req, err := stream.Recv()
		if err != nil {
			p.cleanupSpan(stream)
			return status.Errorf(codes.Unknown, "cannot receive stream request: %v", err)
		}

		resp := &v3.ProcessingResponse{}

		switch r := req.Request.(type) {
		case *v3.ProcessingRequest_RequestHeaders:
			headers := r.RequestHeaders.Headers
			direction := getHeaderValue(headers.Headers, "x-authbridge-direction")
			path := getHeaderValue(headers.Headers, ":path")
			log.Printf("[ext_proc] RequestHeaders: direction=%q path=%q", direction, path)

			if direction == "outbound" {
				resp = p.handleOutbound(ctx, headers)
			} else {
				// Default: inbound (includes direction="" and direction="inbound")
				resp = p.handleInbound(stream, headers)
			}

		case *v3.ProcessingRequest_RequestBody:
			log.Printf("[ext_proc] RequestBody: %d bytes", len(r.RequestBody.Body))
			resp = p.handleRequestBody(stream, r.RequestBody.Body)

		case *v3.ProcessingRequest_ResponseHeaders:
			log.Println("[ext_proc] ResponseHeaders received")
			resp = &v3.ProcessingResponse{
				Response: &v3.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &v3.HeadersResponse{},
				},
			}

		case *v3.ProcessingRequest_ResponseBody:
			eos := r.ResponseBody.EndOfStream
			log.Printf("[ext_proc] ResponseBody: %d bytes (end_of_stream=%v)", len(r.ResponseBody.Body), eos)
			resp = p.handleResponseBody(stream, r.ResponseBody.Body, eos)

		default:
			log.Printf("[ext_proc] Unknown request type: %T", r)
		}

		if err := stream.Send(resp); err != nil {
			p.cleanupSpan(stream)
			return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
		}
	}
}

// ============================================================================
// OTEL tracing setup
// ============================================================================

func initOtelTracing() error {
	agentName = getEnvOrDefault("AGENT_NAME", "weather-assistant")
	agentVersion = getEnvOrDefault("AGENT_VERSION", "1.0.0")
	agentProvider = getEnvOrDefault("AGENT_PROVIDER", "langchain")
	serviceName = getEnvOrDefault("OTEL_SERVICE_NAME", "weather-service")
	otelEnabled = getEnvOrDefault("OTEL_TRACING_ENABLED", "true") == "true"

	if !otelEnabled {
		log.Println("[OTEL] Tracing disabled")
		return nil
	}

	otlpEndpoint := getEnvOrDefault(
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"http://otel-collector.kagenti-system.svc.cluster.local:8335",
	)

	log.Printf("[OTEL] Initializing: agent=%s service=%s endpoint=%s", agentName, serviceName, otlpEndpoint)

	ctx := context.Background()
	endpoint := strings.TrimPrefix(strings.TrimPrefix(otlpEndpoint, "http://"), "https://")
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(agentVersion),
		attribute.String("gen_ai.agent.name", agentName),
		attribute.String("gen_ai.system", agentProvider),
	))
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	textPropagator = propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	otel.SetTextMapPropagator(textPropagator)
	otelTracer = tp.Tracer("authbridge.otel.agent")

	log.Println("[OTEL] Tracing initialized")
	return nil
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ============================================================================
// OTEL SSE parsing and span helpers
// ============================================================================

// extractA2AOutput extracts agent output text from A2A response body.
// Handles both plain JSON-RPC responses and SSE-formatted streaming responses.
func extractA2AOutput(body []byte) string {
	// Try plain JSON-RPC response first
	var resp a2aResponse
	if err := json.Unmarshal(body, &resp); err == nil {
		if len(resp.Result.Artifacts) > 0 && len(resp.Result.Artifacts[0].Parts) > 0 {
			return resp.Result.Artifacts[0].Parts[0].Text
		}
	}

	// SSE format: split by lines and find JSON data events
	bodyStr := string(body)
	lines := strings.Split(bodyStr, "\n")

	var lastOutput string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonStr == "" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &event); err != nil {
			continue
		}

		if result, ok := event["result"].(map[string]interface{}); ok {
			if artifacts, ok := result["artifacts"].([]interface{}); ok && len(artifacts) > 0 {
				if artifact, ok := artifacts[0].(map[string]interface{}); ok {
					if parts, ok := artifact["parts"].([]interface{}); ok && len(parts) > 0 {
						if part, ok := parts[0].(map[string]interface{}); ok {
							if text, ok := part["text"].(string); ok && text != "" {
								lastOutput = text
							}
						}
					}
				}
			}
			if artifact, ok := result["artifact"].(map[string]interface{}); ok {
				if parts, ok := artifact["parts"].([]interface{}); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]interface{}); ok {
						if text, ok := part["text"].(string); ok && text != "" {
							lastOutput = text
						}
					}
				}
			}
		}
	}

	// If no SSE data found, try finding last JSON object in raw body
	if lastOutput == "" {
		lastBrace := strings.LastIndex(bodyStr, "}")
		if lastBrace >= 0 {
			depth := 0
			startIdx := -1
			for i := lastBrace; i >= 0; i-- {
				if bodyStr[i] == '}' {
					depth++
				} else if bodyStr[i] == '{' {
					depth--
					if depth == 0 {
						startIdx = i
						break
					}
				}
			}
			if startIdx >= 0 {
				var resp a2aResponse
				if err := json.Unmarshal([]byte(bodyStr[startIdx:lastBrace+1]), &resp); err == nil {
					if len(resp.Result.Artifacts) > 0 && len(resp.Result.Artifacts[0].Parts) > 0 {
						lastOutput = resp.Result.Artifacts[0].Parts[0].Text
					}
				}
			}
		}
	}

	return lastOutput
}

// parseSSEEvents extracts SSE data events from a chunk of response body.
func parseSSEEvents(chunk []byte) []string {
	var events []string
	lines := strings.Split(string(chunk), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if jsonStr != "" {
				events = append(events, jsonStr)
			}
		}
	}
	return events
}

// classifySSEEvent examines an A2A SSE event and returns its type and text content.
// Returns: eventType ("llm", "tool", "artifact", "status", ""), text content
func classifySSEEvent(jsonStr string) (string, string) {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &event); err != nil {
		return "", ""
	}

	result, ok := event["result"].(map[string]interface{})
	if !ok {
		return "", ""
	}

	kind, _ := result["kind"].(string)

	switch kind {
	case "artifact-update":
		if artifact, ok := result["artifact"].(map[string]interface{}); ok {
			if parts, ok := artifact["parts"].([]interface{}); ok && len(parts) > 0 {
				if part, ok := parts[0].(map[string]interface{}); ok {
					if text, ok := part["text"].(string); ok {
						return "artifact", text
					}
				}
			}
		}
		return "artifact", ""

	case "status-update":
		if status, ok := result["status"].(map[string]interface{}); ok {
			if msg, ok := status["message"].(map[string]interface{}); ok {
				if parts, ok := msg["parts"].([]interface{}); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]interface{}); ok {
						if text, ok := part["text"].(string); ok {
							if strings.Contains(text, "tools:") {
								return "tool", text
							}
							if strings.Contains(text, "assistant:") {
								return "llm", text
							}
						}
					}
				}
			}
		}
		return "status", ""

	default:
		return "", ""
	}
}

// extractTaskIDAndContext extracts the A2A task ID and context ID from an SSE event.
func extractTaskIDAndContext(jsonStr string) (string, string) {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &event); err != nil {
		return "", ""
	}
	result, ok := event["result"].(map[string]interface{})
	if !ok {
		return "", ""
	}

	var taskID, contextID string

	if cid, ok := result["contextId"].(string); ok {
		contextID = cid
	}

	kind, _ := result["kind"].(string)
	if kind == "task" {
		if id, ok := result["id"].(string); ok {
			taskID = id
		}
	}
	if taskID == "" {
		if tid, ok := result["taskId"].(string); ok {
			taskID = tid
		}
	}

	return taskID, contextID
}

// extractGenAIAttrsFromJSON parses event text as JSON to extract GenAI semantic
// convention attributes from LangChain message data.
func extractGenAIAttrsFromJSON(text string, eventType string) (spanName string, attrs []attribute.KeyValue) {
	jsonStart := strings.Index(text, "{")
	if jsonStart < 0 {
		return "", nil
	}
	jsonStr := text[jsonStart:]

	var eventData struct {
		Messages []struct {
			Type             string                 `json:"type"`
			Content          string                 `json:"content"`
			Name             string                 `json:"name"`
			ToolCallID       string                 `json:"tool_call_id"`
			ResponseMetadata map[string]interface{} `json:"response_metadata"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &eventData); err != nil {
		log.Printf("[OTEL] Could not parse event JSON: %v", err)
		return "", nil
	}

	if len(eventData.Messages) == 0 {
		return "", nil
	}

	switch eventType {
	case "llm":
		for i := len(eventData.Messages) - 1; i >= 0; i-- {
			msg := eventData.Messages[i]
			if msg.Type != "ai" {
				continue
			}

			if rm := msg.ResponseMetadata; rm != nil {
				if tu, ok := rm["token_usage"].(map[string]interface{}); ok {
					if v, ok := tu["input_tokens"].(float64); ok {
						attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", int(v)))
					} else if v, ok := tu["prompt_tokens"].(float64); ok {
						attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", int(v)))
					}
					if v, ok := tu["output_tokens"].(float64); ok {
						attrs = append(attrs, attribute.Int("gen_ai.usage.output_tokens", int(v)))
					} else if v, ok := tu["completion_tokens"].(float64); ok {
						attrs = append(attrs, attribute.Int("gen_ai.usage.output_tokens", int(v)))
					}
					if v, ok := tu["total_tokens"].(float64); ok {
						attrs = append(attrs, attribute.Int("gen_ai.usage.total_tokens", int(v)))
					}
				}

				var modelName string
				if model, ok := rm["model_name"].(string); ok && model != "" {
					modelName = model
					attrs = append(attrs, attribute.String("gen_ai.response.model", model))
					attrs = append(attrs, attribute.String("gen_ai.request.model", model))
				}

				if reason, ok := rm["finish_reason"].(string); ok && reason != "" {
					attrs = append(attrs, attribute.String("gen_ai.response.finish_reasons", reason))
				}

				if modelName != "" {
					spanName = fmt.Sprintf("chat %s", modelName)
				}
			}

			if len(msg.ToolCalls) > 0 {
				if spanName == "" {
					spanName = "chat"
				}
				var toolNames []string
				for _, tc := range msg.ToolCalls {
					name := tc.Name
					if name == "" {
						name = tc.Function.Name
					}
					if name != "" {
						toolNames = append(toolNames, name)
					}
				}
				if len(toolNames) > 0 {
					attrs = append(attrs, attribute.String("gen_ai.tool.calls", strings.Join(toolNames, ",")))
				}
			} else if msg.Content != "" && spanName == "" {
				spanName = "chat"
			}
			break
		}

	case "tool":
		for _, msg := range eventData.Messages {
			if msg.Type != "tool" {
				continue
			}
			if msg.Name != "" {
				spanName = fmt.Sprintf("execute_tool %s", msg.Name)
				attrs = append(attrs, attribute.String("gen_ai.tool.name", msg.Name))
			}
			if msg.ToolCallID != "" {
				attrs = append(attrs, attribute.String("gen_ai.tool.call.id", msg.ToolCallID))
			}
			break
		}
	}

	return spanName, attrs
}

// resubscribeAndCapture opens a new SSE streaming connection to the agent's
// tasks/resubscribe endpoint for disconnect recovery.
func resubscribeAndCapture(cancelCtx context.Context, taskID string, span trace.Span, spanCtx context.Context, startIndex int) (string, int) {
	reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":"ext-proc-resub","method":"tasks/resubscribe","params":{"id":"%s"}}`, taskID)

	req, err := http.NewRequestWithContext(cancelCtx, "POST", "http://127.0.0.1:8000/", strings.NewReader(reqBody))
	if err != nil {
		log.Printf("[OTEL] resubscribe request creation failed: %v", err)
		return "", startIndex
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if cancelCtx.Err() != nil {
			log.Printf("[OTEL] resubscribe cancelled (main stream completed normally)")
			return "", startIndex
		}
		log.Printf("[OTEL] resubscribe failed: %v", err)
		return "", startIndex
	}
	defer resp.Body.Close()

	var output string
	childIndex := startIndex
	buf := make([]byte, 0, 4096)
	readBuf := make([]byte, 1024)

	for {
		n, err := resp.Body.Read(readBuf)
		if n > 0 {
			buf = append(buf, readBuf[:n]...)

			for {
				idx := strings.Index(string(buf), "\n\n")
				if idx < 0 {
					break
				}
				eventData := string(buf[:idx])
				buf = buf[idx+2:]

				for _, line := range strings.Split(eventData, "\n") {
					line = strings.TrimSpace(line)
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if jsonStr == "" {
						continue
					}

					eventType, text := classifySSEEvent(jsonStr)
					switch eventType {
					case "llm", "tool":
						if otelTracer != nil {
							childIndex++
							var sName string
							var sAttrs []attribute.KeyValue
							if eventType == "llm" {
								sName = "chat"
								sAttrs = []attribute.KeyValue{
									attribute.String("gen_ai.operation.name", "chat"),
									attribute.String("gen_ai.system", agentProvider),
								}
							} else {
								sName = "execute_tool"
								sAttrs = []attribute.KeyValue{
									attribute.String("gen_ai.operation.name", "tool"),
								}
							}
							if text != "" {
								sAttrs = append(sAttrs, attribute.String("event.text", text))
							}
							sAttrs = append(sAttrs, attribute.Int("event.index", childIndex))
							_, childSpan := otelTracer.Start(spanCtx, sName,
								trace.WithSpanKind(trace.SpanKindInternal),
								trace.WithAttributes(sAttrs...),
							)
							childSpan.SetStatus(otelcodes.Ok, "")
							childSpan.End()
							log.Printf("[OTEL] resubscribe: created child span %s (step %d)", sName, childIndex)
						}
					case "artifact":
						if text != "" {
							output = text
							log.Printf("[OTEL] resubscribe: captured output (%d chars)", len(text))
						}
					case "status":
						var evt map[string]interface{}
						if err := json.Unmarshal([]byte(jsonStr), &evt); err == nil {
							if result, ok := evt["result"].(map[string]interface{}); ok {
								if final, ok := result["final"].(bool); ok && final {
									log.Printf("[OTEL] resubscribe: stream completed (final=true)")
									return output, childIndex
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[OTEL] resubscribe read error: %v", err)
			}
			break
		}
	}

	if output == "" && cancelCtx.Err() == nil {
		log.Printf("[OTEL] resubscribe got no output, falling back to tasks/get")
		time.Sleep(3 * time.Second)
		output = fetchTaskResult(taskID)
		if output != "" {
			log.Printf("[OTEL] tasks/get recovered output (%d chars)", len(output))
		}
	}

	return output, childIndex
}

// fetchTaskResult queries tasks/get for the completed task result.
func fetchTaskResult(taskID string) string {
	reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":"ext-proc-fetch","method":"tasks/get","params":{"id":"%s"}}`, taskID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post("http://127.0.0.1:8000/", "application/json", strings.NewReader(reqBody))
	if err != nil {
		log.Printf("[OTEL] tasks/get failed: %v", err)
		return ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	return extractA2AOutput(body)
}

// ============================================================================
// Main
// ============================================================================

func main() {
	log.Println("=== Go External Processor Starting ===")

	// Wait for credential files from client-registration (up to 60 seconds)
	// This handles the startup race condition with client-registration container
	waitForCredentials(60 * time.Second)

	// Load configuration from files (or environment variables as fallback)
	loadConfig()

	// Initialize inbound JWT validation
	_, _, tokenURL, _, _ := getConfig()
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

	// Initialize OTEL tracing
	if err := initOtelTracing(); err != nil {
		log.Printf("[OTEL] Failed to initialize tracing: %v", err)
		log.Println("[OTEL] Continuing without OTEL tracing")
	}

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
	v3.RegisterExternalProcessorServer(grpcServer, &processor{
		streamSpans: make(map[v3.ExternalProcessor_ProcessServer]*streamSpanState),
	})

	log.Printf("Starting Go external processor on %s", port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
