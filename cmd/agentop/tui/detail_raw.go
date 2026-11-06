package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/core/pipeline"
)

// withRaw adds the event's exact capture to the detail JSON under "raw", so it renders, folds
// and searches as one more node of the tree. The bodies are decoded into JSON where they are
// JSON, which puts thinking blocks in place among the messages, and every image or document
// becomes "[type size]" rather than its base64.
func withRaw(data []byte, raw *pipeline.RawCapture) []byte {
	if raw == nil {
		return data
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return data
	}
	r := map[string]any{}
	if raw.Response != nil {
		p := raw.Response
		resp := map[string]any{"status": p.Status}
		if len(p.Headers) > 0 {
			resp["headers"] = flatHeaders(p.Headers)
		}
		if p.Truncated {
			resp["truncated"] = true
		}
		if p.Incomplete {
			resp["incomplete"] = true
		}
		if p.Decoded {
			resp["decoded"] = true
		}
		switch {
		case p.BodyBase64 != nil:
			enc := p.Headers.Get("Content-Encoding")
			if enc == "" {
				enc = "binary"
			}
			resp["body"] = fmt.Sprintf("[%s body %s]", enc, formatBytes(int64(len(p.BodyBase64))))
		case p.Body != "":
			resp["body"] = bodyView(p.Body, nil)
		}
		r["response"] = resp
	} else {
		r["request"] = bodyView(raw.Request, raw.RequestBase64)
		switch {
		case raw.Forwarded != nil:
			r["forwarded"] = fmt.Sprintf("rewritten by a plugin: %s → %s",
				formatBytes(int64(len(raw.Request)+len(raw.RequestBase64))), formatBytes(int64(len(*raw.Forwarded))))
			r["forwardedBody"] = bodyView(*raw.Forwarded, nil)
		case raw.ForwardedBase64 != nil:
			r["forwarded"] = fmt.Sprintf("rewritten by a plugin: %s", formatBytes(int64(len(raw.ForwardedBase64))))
		default:
			r["forwarded"] = "forwarded as sent"
		}
		if len(raw.RequestHeaders) > 0 {
			r["headers"] = flatHeaders(raw.RequestHeaders)
		}
		if raw.Query != "" {
			r["query"] = raw.Query
		}
	}
	m["raw"] = r
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// bodyView is a captured body for the tree: its JSON with every image and document replaced by
// a label, or the text itself when it is not JSON.
func bodyView(text string, b64 []byte) any {
	if b64 != nil {
		return fmt.Sprintf("[binary body %s]", formatBytes(int64(len(b64))))
	}
	var v any
	if json.Unmarshal([]byte(text), &v) != nil {
		return text
	}
	return labelMedia(v)
}

// labelMedia replaces each content block of type image or document with "[type media size]",
// the size being of the decoded data.
func labelMedia(v any) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = labelMedia(x[i])
		}
	case map[string]any:
		if t, _ := x["type"].(string); t == "image" || t == "document" {
			if src, ok := x["source"].(map[string]any); ok {
				data, _ := src["data"].(string)
				media, _ := src["media_type"].(string)
				return strings.TrimSpace(fmt.Sprintf("[%s %s %s]", t, media, formatBytes(int64(len(data)*3/4))))
			}
		}
		for k, e := range x {
			x[k] = labelMedia(e)
		}
	}
	return v
}

// flatHeaders is headers as one string per name, for the tree.
func flatHeaders(h map[string][]string) map[string]any {
	out := make(map[string]any, len(h))
	for k, vs := range h {
		out[k] = strings.Join(vs, ", ")
	}
	return out
}
