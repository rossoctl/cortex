package cpex

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/pipeline"
)

// egressToolName is the tool a request no parser claimed is presented as
// when synthetic egress is on.
const egressToolName = "http_egress"

// egressExcerptBytes bounds the body excerpt in http_egress arguments.
const egressExcerptBytes = 8 << 10

// egressPart presents an unparsed request as a call to http_egress. Its
// arguments are what an egress policy decides on: method, host without
// port, the query-free path, the first egressExcerptBytes of the body,
// whether the body was cut, and the Content-Encoding (so a policy can
// refuse a body it cannot read). A CONNECT tunnel is opaque: it carries
// its method and host, an empty path and no body.
//
// The excerpt is policy input only; it is never recorded.
func egressPart(pctx *pipeline.Context) cmfPart {
	method := strings.ToUpper(pctx.Method)
	args := map[string]any{
		"method":           method,
		"host":             egressHost(pctx.Host),
		"path":             "",
		"body":             "",
		"truncated":        false,
		"content_encoding": "",
	}
	if method != http.MethodConnect {
		excerpt, truncated := bodyExcerpt(pctx.Body)
		args["path"] = pctx.Path
		args["body"] = excerpt
		args["truncated"] = truncated
		args["content_encoding"] = strings.ToLower(strings.TrimSpace(pctx.Headers.Get("Content-Encoding")))
	}
	return cmfPart{Kind: cmfPartToolCall, Name: egressToolName, Arguments: args}
}

// egressHost lowercases host and strips its port, keeping an IPv6
// literal's address.
func egressHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// bodyExcerpt returns at most egressExcerptBytes of body as valid UTF-8,
// cut on a rune boundary, and whether anything was left out.
func bodyExcerpt(body []byte) (string, bool) {
	truncated := len(body) > egressExcerptBytes
	if truncated {
		body = body[:egressExcerptBytes]
		for i := 0; i < utf8.UTFMax && len(body) > 0 && !utf8.Valid(body); i++ {
			body = body[:len(body)-1]
		}
	}
	return strings.ToValidUTF8(string(body), "�"), truncated
}

// egressDetails is what the Invocation records about a call to tool name
// with args: for http_egress, the excerpt's size and whether it was cut,
// never its content. Other tools record nothing.
func egressDetails(name string, args map[string]any) map[string]string {
	if name != egressToolName {
		return nil
	}
	body, _ := args["body"].(string)
	truncated, _ := args["truncated"].(bool)
	return map[string]string{
		"egress_excerpt_bytes": strconv.Itoa(len(body)),
		"egress_truncated":     strconv.FormatBool(truncated),
	}
}
