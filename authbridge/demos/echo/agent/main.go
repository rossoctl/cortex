// Demo echo agent for the credential placeholder-swap walkthrough.
//
// Modeled on the IBAC demo agent (authbridge/demos/ibac/agent/main.go),
// keeping all the A2A / agent-card / Task scaffolding verbatim so the
// agent stays discoverable and chat-able in the rossoctl UI. The IBAC
// demo's Ollama/LLM/tool/email machinery is removed — this agent does
// exactly one thing on each message/send:
//
//  1. Read the inbound Authorization header off the incoming HTTP
//     request. With authbridge's credential placeholder mode on, this
//     is a "Bearer abph_<random>" placeholder, NOT the real token.
//  2. Make ONE outbound HTTP GET through the proxied client (HTTP_PROXY,
//     i.e. the authbridge sidecar's forward proxy) to UPSTREAM_URL/echo,
//     forwarding that same Authorization header value unchanged. On the
//     way out, the sidecar's token-exchange plugin resolves the
//     placeholder back to the real credential and exchanges it for an
//     echo-upstream-audience token.
//  3. Read echo-upstream's plaintext reply (the Authorization it saw).
//  4. Report both values back in the A2A response so the placeholder
//     swap is visible end-to-end.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

// proxiedClient is the HTTP client used for the outbound echo call that
// MUST flow through the authbridge proxy-sidecar (so the token-exchange
// plugin can resolve the credential placeholder). Built once in main()
// with an explicit http.ProxyURL transport — http.DefaultClient honors
// HTTP_PROXY in theory, but Go's ProxyFromEnvironment caches its
// decision per process and has subtle no-proxy rules around in-cluster
// hostnames that have caused silent bypasses in this exact deployment
// shape. Hard-coding http.ProxyURL is unambiguous: every call goes
// through the proxy unconditionally.
var proxiedClient *http.Client

func buildProxiedClient() *http.Client {
	proxyEnv := os.Getenv("HTTP_PROXY")
	if proxyEnv == "" {
		log.Printf("[Agent] HTTP_PROXY unset — outbound HTTP is direct. proxy-sidecar mode would set HTTP_PROXY; envoy-sidecar mode leaves it unset and proxy-init iptables transparently routes egress through Envoy/ext_proc, so the placeholder is still resolved.")
		return &http.Client{}
	}
	u, err := url.Parse(proxyEnv)
	if err != nil {
		log.Printf("[Agent] HTTP_PROXY=%q is not a valid URL (%v) — falling back to direct", proxyEnv, err)
		return &http.Client{}
	}
	log.Printf("[Agent] All outbound HTTP via explicit proxy: %s", u)
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(u)},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// callUpstreamEcho makes the single outbound GET to UPSTREAM_URL/echo
// through the proxied client, forwarding the inbound Authorization
// header value unchanged. Returns the upstream's plaintext body (the
// Authorization echo-upstream actually saw, after the sidecar's
// outbound resolve + token-exchange).
func callUpstreamEcho(inboundAuth string) (string, error) {
	upstreamURL := envOr("UPSTREAM_URL", "http://echo-upstream.team1.svc.cluster.local:8080")
	req, err := http.NewRequest(http.MethodGet, upstreamURL+"/echo", nil)
	if err != nil {
		return "", fmt.Errorf("creating upstream request: %w", err)
	}
	// Forward the inbound Authorization value verbatim. With placeholder
	// mode on this is "Bearer abph_<random>"; the sidecar swaps it on
	// the way out so echo-upstream never sees the placeholder.
	if inboundAuth != "" {
		req.Header.Set("Authorization", inboundAuth)
	}
	resp, err := proxiedClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling upstream: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading upstream response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream returned %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

// --- A2A (JSON-RPC 2.0) endpoint ---
//
// Wire shape:
//
//	POST / HTTP/1.1
//	Content-Type: application/json
//
//	{
//	  "jsonrpc": "2.0",
//	  "id": "1",
//	  "method": "message/send",
//	  "params": {
//	    "message": {
//	      "role": "user",
//	      "parts": [{"kind": "text", "text": "echo my auth"}],
//	      "contextId": "demo-session-1"
//	    }
//	  }
//	}

type jsonRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Method  string         `json:"method"`
	Params  jsonRPCMessage `json:"params"`
}

type jsonRPCMessage struct {
	Message struct {
		Role      string    `json:"role"`
		Parts     []a2aPart `json:"parts"`
		ContextID string    `json:"contextId,omitempty"`
	} `json:"message"`
}

type a2aPart struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  *a2aTask      `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

// a2aTask is the A2A v0.3.0 Task response shape. We use this rather
// than the simpler Message shape (role+parts directly under result)
// because the authbridge a2a-parser's response-side artifact
// extraction (extractSendResponse in plugin.go) keys off
// `result.status.state` and `result.artifacts[].parts[].text`. Without
// the Task shape, abctl and the session-event JSON show only the
// REQUEST text on response events, which makes the agent's reply
// invisible in the platform observability layer.
//
// rossoctl's backend chat handler accepts both shapes (chat.py:211
// handles Task, chat.py:219 handles Message), so emitting a Task
// here doesn't break the UI.
type a2aTask struct {
	ID        string        `json:"id"`
	ContextID string        `json:"contextId,omitempty"`
	Kind      string        `json:"kind"`
	Status    a2aStatus     `json:"status"`
	Artifacts []a2aArtifact `json:"artifacts,omitempty"`
}

type a2aStatus struct {
	State   string      `json:"state"`
	Message *a2aMessage `json:"message,omitempty"`
}

type a2aMessage struct {
	Role  string    `json:"role"`
	Parts []a2aPart `json:"parts"`
}

type a2aArtifact struct {
	ArtifactID string    `json:"artifactId"`
	Name       string    `json:"name,omitempty"`
	Parts      []a2aPart `json:"parts"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// handleAgentCard serves the A2A agent card at
// /.well-known/agent-card.json. The rossoctl operator's
// AgentCardReconciler fetches this URL through the agent's Service
// and stuffs the result into an AgentCard CR; the rossoctl UI's agent
// detail page renders that. Without the endpoint the UI shows
// "Agent card not available."
//
// jwt-validation's bypass list includes /.well-known/* by default
// (bypass.DefaultPatterns at authlib/bypass), so the operator's
// reconciler can hit this without a Bearer token.
func handleAgentCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// AGENT_PUBLIC_URL is what the UI displays as the agent's
	// callable address. Defaults to the in-cluster Service URL,
	// which is what rossoctl UI actually uses for the chat call.
	publicURL := envOr("AGENT_PUBLIC_URL", "http://echo-agent.team1.svc.cluster.local:8080/")
	card := map[string]any{
		"name":               "Echo",
		"description":        "Echoes the Authorization header it received, then calls echo-upstream and reports the Authorization the upstream saw",
		"protocolVersion":    "0.3.0",
		"version":            "0.0.1",
		"url":                publicURL,
		"preferredTransport": "JSONRPC",
		"defaultInputModes":  []string{"text"},
		"defaultOutputModes": []string{"text"},
		"capabilities": map[string]any{
			"streaming": false,
		},
		"skills": []map[string]any{
			{
				"id":          "echo_authorization",
				"name":        "Echo Authorization",
				"description": "Reports the inbound Authorization header it received, then makes one outbound call to echo-upstream and reports the Authorization the upstream saw — making the credential placeholder swap visible end-to-end.",
				"tags":        []string{"demo", "echo", "placeholder-swap"},
				"examples": []string{
					"Echo my authorization.",
				},
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(card); err != nil {
		log.Printf("[Agent] failed to encode agent card: %v", err)
	}
}

func handleA2A(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Capture the inbound Authorization header BEFORE reading the body.
	// With authbridge placeholder mode on, jwt-validation rewrites this
	// to "Bearer abph_<random>" before the request reaches the agent.
	inboundAuth := r.Header.Get("Authorization")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	if req.Method != "message/send" {
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
		return
	}

	// First text-kind part wins. Real A2A clients pack one text part
	// per user turn; multiple parts are reserved for multi-modal.
	var query string
	for _, p := range req.Params.Message.Parts {
		if p.Kind == "text" && p.Text != "" {
			query = p.Text
			break
		}
	}
	if query == "" {
		writeRPCError(w, req.ID, -32602, "no text part in message")
		return
	}

	sessionID := req.Params.Message.ContextID
	if sessionID == "" {
		// Best-effort fallback: many A2A clients also pass an explicit
		// session header. Keep this for parity with the legacy endpoint.
		sessionID = r.Header.Get("X-Session-Id")
	}
	if sessionID == "" {
		// A2A spec §6.6: when the client omits a contextId, the server
		// SHOULD assign one and return it. The rossoctl UI currently
		// sends bare message/send calls without any contextId field;
		// returning a fresh UUID restores per-conversation bucketing
		// while staying backward-compatible.
		sessionID = newUUID()
	}
	log.Printf("[Agent] A2A query (session=%s): %s", sessionID, query)

	result := runEcho(inboundAuth)

	writeRPCSuccess(w, req.ID, sessionID, result)
}

// runEcho implements the demo's single behavior: report the inbound
// Authorization header, then forward it on one outbound GET to
// echo-upstream and report what the upstream saw. Errors are handled
// gracefully — if the outbound call fails we still report the inbound
// header plus the error text.
func runEcho(inboundAuth string) string {
	inboundDisplay := inboundAuth
	if inboundDisplay == "" {
		inboundDisplay = "(no Authorization header)"
	}

	upstreamBody, err := callUpstreamEcho(inboundAuth)
	if err != nil {
		log.Printf("[Agent] upstream echo call failed: %v", err)
		upstreamBody = fmt.Sprintf("(outbound call failed: %v)", err)
	}

	return fmt.Sprintf(
		"Inbound Authorization I received: %s\n\nAuthorization echo-upstream received (after outbound resolve+exchange): %s",
		inboundDisplay, upstreamBody,
	)
}

// writeRPCSuccess emits an A2A v0.3.0 Task response. Both
// status.message AND artifacts carry the agent's reply: the rossoctl
// backend's chat handler reads from status.message (chat.py:211),
// while the authbridge a2a-parser's response-side artifact extractor
// reads from artifacts[].parts[].text (plugin.go:188-195). Carrying
// the text in both keeps the rossoctl UI working AND gets the reply
// into the session-event JSON for abctl / show-result.
func writeRPCSuccess(w http.ResponseWriter, id any, sessionID, text string) {
	taskID := newUUID()
	parts := []a2aPart{{Kind: "text", Text: text}}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: &a2aTask{
			ID:        taskID,
			ContextID: sessionID,
			Kind:      "task",
			Status: a2aStatus{
				State: "completed",
				Message: &a2aMessage{
					Role:  "agent",
					Parts: parts,
				},
			},
			Artifacts: []a2aArtifact{
				{
					ArtifactID: newUUID(),
					Name:       "reply",
					Parts:      parts,
				},
			},
		},
	})
}

// newUUID returns a hex-encoded random ID. Good enough for A2A
// task / artifact IDs in this demo — we don't need RFC 4122 UUIDs.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors are HTTP 200 with body.error
	json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	})
}

func main() {
	proxiedClient = buildProxiedClient()

	http.HandleFunc("/", handleA2A)
	http.HandleFunc("/.well-known/agent-card.json", handleAgentCard)

	// Honor PORT env so the demo's Pod manifest can land the agent
	// on a port that doesn't collide with the authbridge sidecar's
	// reverse proxy (the operator port-steals — see agent.yaml).
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("[Agent] Starting on %s (A2A at /)", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}
}
