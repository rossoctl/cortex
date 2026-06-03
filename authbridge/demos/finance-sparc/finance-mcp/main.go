// Finance MCP server for the SPARC demo.
//
// Speaks MCP (Model Context Protocol) over HTTP — JSON-RPC 2.0 POSTs to /mcp.
// Serves a small but non-trivial finance dataset through five tools and
// advertises them via tools/list (so the agent discovers them at runtime like
// any normal kagenti agent — nothing is hardcoded on the agent side).
//
// Tools:
//
//	get_transaction(transaction_id)        → transaction details
//	lookup_customer(customer_id)           → customer details
//	issue_refund(transaction_id[, reason]) → refund confirmation (id required)
//	get_invoice(invoice_id)                → invoice details
//	list_currencies()                      → static reference data (demo of skip_tools)
//
// There is deliberately NO search/list-transactions tool: when a user refers to
// a charge descriptively (no id), the agent cannot look the id up — a proactive
// model will fabricate a precise transaction_id, which SPARC catches as
// ungrounded before any refund touches the wrong record.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type mcpToolCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

type mcpToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// --- In-memory finance dataset (several of each entity) ---

var transactions = map[string]map[string]any{
	"TX4827": {"transaction_id": "TX4827", "amount": 450.00, "currency": "USD", "customer_id": "C921", "status": "settled", "description": "Annual subscription renewal"},
	"TX5310": {"transaction_id": "TX5310", "amount": 1200.00, "currency": "USD", "customer_id": "C742", "status": "settled", "description": "Hardware purchase"},
	"TX2981": {"transaction_id": "TX2981", "amount": 75.50, "currency": "EUR", "customer_id": "C921", "status": "pending", "description": "Bookstore order"},
	"TX6644": {"transaction_id": "TX6644", "amount": 99.00, "currency": "USD", "customer_id": "C305", "status": "settled", "description": "Monthly subscription"},
}

var customers = map[string]map[string]any{
	"C921": {"customer_id": "C921", "name": "Daniel Reed", "email": "daniel.reed@example.com", "tier": "gold"},
	"C742": {"customer_id": "C742", "name": "Maria Gomez", "email": "maria.gomez@example.com", "tier": "silver"},
	"C305": {"customer_id": "C305", "name": "Sven Olsen", "email": "sven.olsen@example.com", "tier": "bronze"},
}

var invoices = map[string]map[string]any{
	"INV-8834": {"invoice_id": "INV-8834", "vendor": "Acme Corp", "amount": 1280.00, "currency": "USD", "status": "due"},
	"INV-9102": {"invoice_id": "INV-9102", "vendor": "Globex", "amount": 540.00, "currency": "USD", "status": "paid"},
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func fn(name, desc string, props map[string]any, required []string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": desc,
		"parameters":  map[string]any{"type": "object", "properties": props, "required": required},
	}
}

// toolSpecs advertised via tools/list (OpenAI function-calling shape under
// inputSchema, the MCP convention the agent maps to its LLM tool list).
func toolSpecs() []map[string]any {
	return []map[string]any{
		fn("get_transaction", "Fetch transaction details by exact transaction id.",
			map[string]any{"transaction_id": strProp("Exact transaction identifier")}, []string{"transaction_id"}),
		fn("lookup_customer", "Fetch customer details by exact customer id.",
			map[string]any{"customer_id": strProp("Exact customer identifier")}, []string{"customer_id"}),
		fn("issue_refund", "Issue a refund for a transaction. Requires the exact transaction_id.",
			map[string]any{
				"transaction_id": strProp("Exact transaction identifier to refund"),
				"reason":         strProp("Optional reason for the refund"),
			}, []string{"transaction_id"}),
		fn("get_invoice", "Fetch an invoice by exact invoice id.",
			map[string]any{"invoice_id": strProp("Exact invoice identifier")}, []string{"invoice_id"}),
		fn("list_currencies", "List supported reference currencies (static data).",
			map[string]any{}, []string{}),
	}
}

func callTool(name string, args map[string]any) mcpToolCallResult {
	get := func(k string) string { s, _ := args[k].(string); return strings.TrimSpace(s) }
	notFound := func(kind, id string) mcpToolCallResult {
		return mcpToolCallResult{Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("%s %q not found.", kind, id)}}, IsError: true}
	}
	ok := func(v any) mcpToolCallResult {
		return mcpToolCallResult{Content: []mcpContent{{Type: "text", Text: toJSON(v)}}}
	}
	switch name {
	case "get_transaction":
		if t, found := transactions[get("transaction_id")]; found {
			return ok(t)
		}
		return notFound("transaction", get("transaction_id"))
	case "lookup_customer":
		if c, found := customers[get("customer_id")]; found {
			return ok(c)
		}
		return notFound("customer", get("customer_id"))
	case "issue_refund":
		tid := get("transaction_id")
		if _, found := transactions[tid]; !found {
			return notFound("transaction", tid)
		}
		return ok(map[string]any{"status": "refund_issued", "transaction_id": tid, "reason": get("reason")})
	case "get_invoice":
		if inv, found := invoices[get("invoice_id")]; found {
			return ok(inv)
		}
		return notFound("invoice", get("invoice_id"))
	case "list_currencies":
		return ok([]string{"USD", "EUR", "GBP", "JPY"})
	default:
		return mcpToolCallResult{Content: []mcpContent{{Type: "text", Text: "unknown tool: " + name}}, IsError: true}
	}
}

func writeError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: message}})
}

func writeResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, nil, -32700, "read body: "+err.Error())
		return
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	slog.Info("mcp request", "method", req.Method, "id", req.ID)
	switch req.Method {
	case "initialize":
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "finance-mcp", "version": "0.1.0"},
		})
	case "tools/list":
		tools := make([]map[string]any, 0)
		for _, s := range toolSpecs() {
			tools = append(tools, map[string]any{"name": s["name"], "description": s["description"], "inputSchema": s["parameters"]})
		}
		writeResult(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var params mcpToolCallParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeError(w, req.ID, -32602, "params: "+err.Error())
				return
			}
		}
		res := callTool(params.Name, params.Arguments)
		slog.Info("tools/call", "tool", params.Name, "args", toJSON(params.Arguments), "isError", res.IsError)
		writeResult(w, req.ID, res)
	default:
		writeError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func main() {
	http.HandleFunc("/mcp", handleMCP)
	addr := ":8888"
	slog.Info("finance MCP server starting", "addr", addr+"/mcp")
	if err := http.ListenAndServe(addr, nil); err != nil { //nolint:gosec // demo
		slog.Error("failed to start finance server", "error", err)
		os.Exit(1)
	}
}
