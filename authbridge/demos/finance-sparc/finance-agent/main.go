// Finance assistant agent for the SPARC demo.
//
// A regular rossoctl A2A agent (JSON-RPC over HTTP at /). It reasons with a
// local Ollama model and acts through a finance MCP server. It DISCOVERS its
// tools at runtime via MCP `tools/list` — nothing about the tools is hardcoded
// — so it behaves like any normal agent. There is no demo-specific steering or
// side-channel: SPARC sees only what the agent genuinely sends to its LLM (full
// conversation incl. system prompt, the discovered tool specs) and the tool
// call it makes.
//
// The agent is configured to be proactive (complete the user's request with the
// available tools). When a user refers to a charge descriptively — without a
// transaction id — the model fabricates a precise id to call a tool. The
// AuthBridge `sparc` plugin reflects on that call, finds the argument
// ungrounded, and returns a clarification as the MCP tool result; the agent
// relays it and asks the user for the exact id. Once supplied, SPARC approves
// and the refund proceeds.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// --- OpenAI-compatible chat types (Ollama /v1/chat/completions) ---

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Tools       []Tool        `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
}

type ChatChoice struct {
	Message ChatMessage `json:"message"`
}

// --- Outbound HTTP via the authbridge forward proxy ---

var proxiedClient *http.Client

func buildProxiedClient() *http.Client {
	proxyEnv := os.Getenv("HTTP_PROXY")
	if proxyEnv == "" {
		log.Printf("[Agent] HTTP_PROXY unset — outbound HTTP will be direct (authbridge will not see it)")
		return &http.Client{Timeout: 240 * time.Second}
	}
	u, err := url.Parse(proxyEnv)
	if err != nil {
		log.Printf("[Agent] HTTP_PROXY=%q invalid (%v) — direct", proxyEnv, err)
		return &http.Client{Timeout: 240 * time.Second}
	}
	log.Printf("[Agent] outbound HTTP via proxy: %s", u)
	return &http.Client{Timeout: 240 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
}

// --- MCP (tool discovery + execution) ---

func mcpURL() string { return envOr("FINANCE_MCP_URL", "http://localhost:8888") }

func mcpCall(method string, params any) (map[string]any, error) {
	rpc := map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("%d", time.Now().UnixNano()), "method": method}
	if params != nil {
		rpc["params"] = params
	}
	body, _ := json.Marshal(rpc)
	req, err := http.NewRequest(http.MethodPost, mcpURL()+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := proxiedClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return env, nil
}

var (
	toolsMu    sync.Mutex
	toolsCache []Tool
)

// discoverTools fetches the MCP tool inventory via tools/list and maps it to
// OpenAI tool specs for the LLM. The result is cached only on success, so a
// transient failure (MCP not ready yet, a flaky call) is retried on the next
// turn rather than poisoning the agent with an empty tool list for its lifetime.
func discoverTools() []Tool {
	toolsMu.Lock()
	defer toolsMu.Unlock()
	if len(toolsCache) > 0 {
		return toolsCache
	}
	env, err := mcpCall("tools/list", nil)
	if err != nil {
		log.Printf("[Agent] tools/list failed (will retry): %v", err)
		return nil
	}
	result, _ := env["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	var discovered []Tool
	for _, t := range list {
		tm, _ := t.(map[string]any)
		name, _ := tm["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := tm["description"].(string)
		params, _ := tm["inputSchema"].(map[string]any)
		discovered = append(discovered, Tool{Type: "function", Function: ToolFunction{Name: name, Description: desc, Parameters: params}})
	}
	if len(discovered) == 0 {
		log.Printf("[Agent] tools/list returned no tools (will retry)")
		return nil
	}
	toolsCache = discovered
	log.Printf("[Agent] discovered %d tools via MCP tools/list", len(toolsCache))
	return toolsCache
}

// execMCPTool runs a tool via tools/call and returns its text result (real data,
// or the SPARC clarification injected in place of an ungrounded call). The second
// return value is true when the result is a SPARC clarification (MCP `_meta.sparc.
// reflected`) — the agent surfaces that to the user rather than retrying the call.
func execMCPTool(name string, args map[string]any) (string, bool) {
	env, err := mcpCall("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return fmt.Sprintf("error calling tool: %v", err), false
	}
	if e, ok := env["error"].(map[string]any); ok {
		return fmt.Sprintf("tool error: %v", e["message"]), false
	}
	result, _ := env["result"].(map[string]any)
	reflected := false
	if meta, ok := result["_meta"].(map[string]any); ok {
		if sp, ok := meta["sparc"].(map[string]any); ok {
			reflected, _ = sp["reflected"].(bool)
		}
	}
	content, _ := result["content"].([]any)
	var parts []string
	for _, c := range content {
		cm, _ := c.(map[string]any)
		if txt, _ := cm["text"].(string); txt != "" {
			parts = append(parts, txt)
		}
	}
	if len(parts) == 0 {
		return toJSONStr(result), reflected
	}
	return strings.Join(parts, "\n"), reflected
}

func toJSONStr(v any) string { b, _ := json.Marshal(v); return string(b) }

// --- Ollama interaction ---

func callOllama(messages []ChatMessage, tools []Tool) (*ChatResponse, error) {
	ollamaURL := envOr("OLLAMA_URL", "http://localhost:11434")
	chatReq := ChatRequest{Model: envOr("OLLAMA_MODEL", "llama3.2:3b"), Messages: messages, Temperature: 0.1}
	if len(tools) > 0 {
		chatReq.Tools = tools
		chatReq.ToolChoice = "auto"
	}
	reqBody, _ := json.Marshal(chatReq)
	// Use the proxied client: it routes through the authbridge forward proxy
	// (so inference-parser captures the messages + tool specs SPARC needs) and
	// carries the explicit timeout — http.Post's default client has neither.
	req, err := http.NewRequest(http.MethodPost, ollamaURL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := proxiedClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(body))
	}
	var cr ChatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("unmarshal ollama response: %w", err)
	}
	return &cr, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- Agent loop ---
//
// A normal proactive finance-assistant prompt. No id-guessing instructions and
// no ground-truth leakage — the hallucination, when it happens, comes from the
// model trying to act on a request that's missing a required argument.
const systemPrompt = "You are a finance operations assistant. Use the available tools to fulfil the user's " +
	"request. To refund a charge you must call issue_refund with the transaction's id. Take action with the " +
	"tools rather than replying in prose. If a tool result tells you a value could not be verified or asks you " +
	"to clarify, relay that to the user and ask for the exact missing detail."

func runAgent(query string) (string, error) {
	tools := discoverTools()
	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: query},
	}
	lastToolResult := ""
	for i := 0; i < 8; i++ {
		resp, err := callOllama(messages, tools)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("no choices in ollama response")
		}
		msg := resp.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			return msg.Content, nil
		}
		messages = append(messages, msg)
		for _, tc := range msg.ToolCalls {
			log.Printf("[Agent] tool call: %s(%s)", tc.Function.Name, tc.Function.Arguments)
			var args map[string]any
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				args = map[string]any{}
			}
			result, reflected := execMCPTool(tc.Function.Name, args)
			lastToolResult = result
			log.Printf("[Agent] tool result (%s, reflected=%t): %.200s", tc.Function.Name, reflected, result)
			messages = append(messages, ChatMessage{Role: "tool", Content: result, ToolCallID: tc.ID})
			if reflected {
				// SPARC returned a clarification in place of the tool result
				// (an ungrounded/unverifiable argument). Relay it to the user
				// and stop — retrying would hit the same rejection and only
				// grow the prompt. This is the agent honoring its system
				// instruction to surface "could not verify / clarify" results.
				return result, nil
			}
		}
	}
	// The model kept calling tools without concluding (common with small models
	// when a tool keeps returning a clarification). Surface the last tool result
	// to the user rather than a bare error — it carries the actionable guidance.
	if lastToolResult != "" {
		return lastToolResult, nil
	}
	return "", fmt.Errorf("tool-calling loop exceeded max iterations")
}

// --- A2A (JSON-RPC 2.0) endpoint ---

type a2aRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		Message struct {
			Role      string `json:"role"`
			Parts     []part `json:"parts"`
			ContextID string `json:"contextId,omitempty"`
		} `json:"message"`
	} `json:"params"`
}

type part struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

type a2aResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  *a2aTask  `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

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
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type a2aArtifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []part `json:"parts"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func handleAgentCard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	publicURL := envOr("AGENT_PUBLIC_URL", "http://finance-agent.team1.svc.cluster.local:8080/")
	card := map[string]any{
		"name":               "Finance Assistant",
		"description":        "Finance operations agent: transaction lookups and refunds via an MCP finance backend. AuthBridge's SPARC plugin reflects on each tool call and blocks ungrounded/hallucinated arguments, transparently asking for clarification.",
		"protocolVersion":    "0.3.0",
		"version":            "0.0.1",
		"url":                publicURL,
		"preferredTransport": "JSONRPC",
		"defaultInputModes":  []string{"text"},
		"defaultOutputModes": []string{"text"},
		"capabilities":       map[string]any{"streaming": false},
		"skills": []map[string]any{{
			"id": "refund", "name": "Refund a transaction",
			"description": "Looks up and refunds a transaction; SPARC verifies the transaction id is grounded before any refund runs.",
			"tags":        []string{"demo", "finance", "sparc"},
			"examples":    []string{"Refund my duplicate $450 subscription charge from last week."},
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(card); err != nil {
		log.Printf("[Agent] encode agent card: %v", err)
	}
}

func handleA2A(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req a2aRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	if req.Method != "message/send" {
		writeRPCError(w, req.ID, -32601, "method not found: "+req.Method)
		return
	}
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
		sessionID = newUUID()
	}
	log.Printf("[Agent] A2A query (session=%s): %s", sessionID, query)
	result, err := runAgent(query)
	if err != nil {
		writeRPCError(w, req.ID, -32603, err.Error())
		return
	}
	writeRPCSuccess(w, req.ID, sessionID, result)
}

func writeRPCSuccess(w http.ResponseWriter, id any, sessionID, text string) {
	parts := []part{{Kind: "text", Text: text}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a2aResponse{
		JSONRPC: "2.0", ID: id,
		Result: &a2aTask{
			ID: newUUID(), ContextID: sessionID, Kind: "task",
			Status:    a2aStatus{State: "completed", Message: &a2aMessage{Role: "agent", Parts: parts}},
			Artifacts: []a2aArtifact{{ArtifactID: newUUID(), Name: "reply", Parts: parts}},
		},
	})
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(a2aResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func main() {
	proxiedClient = buildProxiedClient()
	http.HandleFunc("/", handleA2A)
	http.HandleFunc("/.well-known/agent-card.json", handleAgentCard)
	port := envOr("PORT", "8080")
	log.Printf("[Agent] Finance assistant starting on :%s (A2A at /)", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil { //nolint:gosec // demo
		log.Fatalf("server: %v", err)
	}
}
