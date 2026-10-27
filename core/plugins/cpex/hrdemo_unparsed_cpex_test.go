// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build cpex

package cpex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/mcpparser"
)

// rawCall runs body through the real mcp-parser and then the plugin, as
// the sidecar's outbound pipeline does.
func rawCall(t *testing.T, p *CPEX, idp *hrIdP, user, body string) pipeline.Action {
	t.Helper()
	return rawCallTo(t, p, idp, "hr-mcp.cpex-demo:9100", user, body)
}

// rawCallTo is rawCall to host.
func rawCallTo(t *testing.T, p *CPEX, idp *hrIdP, host, user, body string) pipeline.Action {
	t.Helper()
	return rawCallEncoded(t, p, idp, host, user, "", []byte(body))
}

// rawCallEncoded is rawCallTo with body sent under Content-Encoding
// encoding ("" for none).
func rawCallEncoded(t *testing.T, p *CPEX, idp *hrIdP, host, user, encoding string, body []byte) pipeline.Action {
	t.Helper()
	pctx := &pipeline.Context{
		Direction: pipeline.Outbound,
		Host:      host,
		Path:      "/mcp",
		Method:    http.MethodPost,
		Headers: http.Header{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer " + idp.clientToken(t)},
			"X-User-Token":  {idp.userToken(t, user)},
		},
		Body: body,
	}
	if encoding != "" {
		pctx.Headers.Set("Content-Encoding", encoding)
	}
	parser := mcpparser.NewMCPParser()
	if err := parser.Configure(json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	parser.OnRequest(context.Background(), pctx)
	return p.OnRequest(context.Background(), pctx)
}

// A tool call to hr-mcp (a strict_hosts destination) that the mcp-parser
// cannot decode would otherwise reach CPEX as global traffic, which no tool
// route judges, while Python's json reads NaN, Infinity, 1e400, a byte
// order mark and UTF-16 and runs the tool. Each is refused before CPEX sees
// it, with or without a jsonrpc key, and so are colliding keys.
func TestHRDemoUnparseableToolCallDenied(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	const adjust = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"adjust_compensation","arguments":{"employee_id":"EMP-001234","amount":%s}}}`
	const bare = `{"id":1,"method":"tools/call","params":{"name":"adjust_compensation","arguments":{"employee_id":"EMP-001234","amount":%s}}}`
	for label, body := range map[string]string{
		"NaN beside the arguments":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_compensation","arguments":{"employee_id":"EMP-001234","include_ssn":true,"x":NaN}}}`,
		"NaN amount":                fmt.Sprintf(adjust, "NaN"),
		"NaN amount, no jsonrpc":    fmt.Sprintf(bare, "NaN"),
		"NaN amount, JSONRPC":       `{"JSONRPC":"2.0",` + fmt.Sprintf(bare, "NaN")[1:],
		"NaN amount, renamed":       `{"json-rpc":"2.0",` + fmt.Sprintf(bare, "NaN")[1:],
		"Infinity amount":           fmt.Sprintf(adjust, "Infinity"),
		"-Infinity amount":          fmt.Sprintf(bare, "-Infinity"),
		"1e400 amount":              fmt.Sprintf(bare, "1e400"),
		"byte order mark":           "\xef\xbb\xbf" + fmt.Sprintf(bare, "50000"),
		"Method of another type":    `{"id":1,"Method":5,"method":"tools/call","params":{"name":"get_compensation","arguments":{"employee_id":"EMP-001234"}}}`,
		"UTF-16BE body":             string(utf16Body(true, fmt.Sprintf(adjust, "50000"))),
		"UTF-16BE body, no jsonrpc": string(utf16Body(true, fmt.Sprintf(bare, "50000"))),
		"UTF-16LE body with a BOM":  "\xff\xfe" + string(utf16Body(false, fmt.Sprintf(adjust, "50000"))),
		"UTF-32BE body":             string(utf32BEBody(fmt.Sprintf(bare, "50000"))),
	} {
		wantReject(t, label, rawCall(t, p, idp, "alice", body), "cpex.unparseable_request")
	}
	// A repeated method the mcp-parser does claim is refused as ambiguous.
	wantReject(t, "repeated method", rawCall(t, p, idp, "alice",
		`{"method":"tools/list","method":"tools/call","params":{"name":"get_compensation","arguments":{"employee_id":"EMP-001234"}}}`),
		"cpex.ambiguous_request")
	if n := idp.exchangeCount(); n != 0 {
		t.Fatalf("token exchanges = %d, want 0", n)
	}
	if n := idp.cibaCount(); n != 0 {
		t.Fatalf("CIBA requests = %d, want 0", n)
	}
	// What follows the first value is not read, so trailing data is not
	// unparseable; hr-mcp refuses it itself.
	if a := rawCall(t, p, idp, "alice", fmt.Sprintf(adjust, "5000")+`{"x":1}`); a.Violation != nil && a.Violation.Code == "cpex.unparseable_request" {
		t.Errorf("trailing data: refused as unparseable")
	}
}

// hr-mcp reached by address rather than name is still a strict_hosts
// destination: the manifest lists kind's Service and pod ranges, so a
// UTF-16 tool call to its cluster IP or pod IP is refused, as it is when
// the address is also on bypass_hosts.
func TestHRDemoUnparseableByAddress(t *testing.T) {
	idp := newHRIdP(t)
	body := string(utf16Body(true, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"adjust_compensation","arguments":{"employee_id":"EMP-001234","amount":50000}}}`))
	for _, extra := range []map[string]any{nil, {"bypass_hosts": []string{"10.96.*", "10.244.*"}}} {
		p := hrPluginWith(t, idp, nil, extra)
		for _, host := range []string{"10.96.140.7:9100", "10.244.0.12:9100", "0x0a.0x60.0x8c.7:9100"} {
			wantReject(t, host, rawCallTo(t, p, idp, host, "alice", body), "cpex.unparseable_request")
		}
		if a := rawCallTo(t, p, idp, "192.0.2.10:9100", "alice", body); a.Violation != nil && a.Violation.Code == "cpex.unparseable_request" {
			t.Errorf("address outside strict_hosts: refused as unparseable")
		}
	}
	if n := idp.exchangeCount(); n != 0 {
		t.Fatalf("token exchanges = %d, want 0", n)
	}
}

// The check is scoped to strict_hosts: the same bodies to any other host
// are opaque egress input and reach CPEX as global traffic.
func TestHRDemoUnparseableOnlyOnStrictHosts(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	for label, body := range map[string]string{
		"NDJSON":            "{\"a\":1}\n{\"b\":2}\n",
		"NaN metrics":       `{"loss":NaN,"step":3}`,
		"non-string method": `{"method":{"type":"card"},"amount":5}`,
		"UTF-16 object":     string(utf16Body(true, `{"a":1}`)),
	} {
		pctx := &pipeline.Context{
			Direction: pipeline.Outbound,
			Host:      "metrics.example.com",
			Path:      "/v1/push",
			Method:    http.MethodPost,
			Headers:   http.Header{"Content-Type": {"application/json"}},
			Body:      []byte(body),
		}
		if a := p.OnRequest(context.Background(), pctx); a.Violation != nil && a.Violation.Code == "cpex.unparseable_request" {
			t.Errorf("%s to another host: refused as unparseable", label)
		}
	}
}

// Every JSON number form reaches the decision the tool would act on.
// The policy reads the amount as a float64 and hr-mcp as the integer it
// applies, so the two sides differ only past 2^53, where both are far
// over the threshold: every such amount needs an approval whose scope
// (|amount| <= 25000) then refuses it (TestHRDemoAdjustNeedsApproval).
func TestHRDemoAdjustAmountForms(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	const adjust = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"adjust_compensation","arguments":{"employee_id":"EMP-001234","amount":%s}}}`
	for _, amount := range []string{"5000", "5000.0", "5e3", "-5000", "-0", "0", "10000", "-10000", "10000.0", "1.0e4", "-1e4"} {
		wantContinue(t, "amount "+amount, rawCall(t, p, idp, "bob", fmt.Sprintf(adjust, amount)))
	}
	for _, amount := range []string{"10001", "-10001", "1.5e4", "15000.0", "25000", "-25000", "25001", "-25001",
		"9007199254740993", "-9007199254740993", "1e300"} {
		wantPending(t, "amount "+amount, rawCall(t, p, idp, "bob", fmt.Sprintf(adjust, amount)))
	}
}

// A zstd-encoded tool call to hr-mcp is parsed like a plain one, so its
// tool route judges it: a change over $10,000 waits for approval and a
// smaller one goes through, rather than passing as global traffic.
func TestHRDemoZstdToolCallRouted(t *testing.T) {
	idp := newHRIdP(t)
	p := hrPlugin(t, idp)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	const adjust = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"adjust_compensation","arguments":{"employee_id":"EMP-001234","amount":%d}}}`
	call := func(amount int) pipeline.Action {
		return rawCallEncoded(t, p, idp, "hr-mcp.cpex-demo:9100", "bob", "zstd", enc.EncodeAll([]byte(fmt.Sprintf(adjust, amount)), nil))
	}
	wantPending(t, "zstd bob +15000", call(15000))
	wantContinue(t, "zstd bob +5000", call(5000))
}
