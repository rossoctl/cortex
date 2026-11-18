package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// probeServer is probeAPI. A var so tests can stand in for a server.
var probeServer = probeAPI

// probePaths are the paths the probe tries for each protocol, in order.
var probePaths = map[string][]string{
	"anthropic-messages": {"/v1/messages", "/messages", "/anthropic/v1/messages"},
	"chat-completions":   {"/v1/chat/completions", "/chat/completions"},
	"responses":          {"/v1/responses", "/responses"},
}

// usualHeader is the header each protocol's own clients send the key in.
func usualHeader(protocol string) string {
	if protocol == "anthropic-messages" {
		return "x-api-key"
	}
	return "authorization"
}

// maxProbeReply bounds the part of a reply the probe reads.
const maxProbeReply = 64 << 10

// probeReply is the last reply to one candidate path.
type probeReply struct {
	status int
	body   []byte
	served bool
	header string // the auth header it was sent with
}

// probeAPI finds which protocols the server s serves, and on which paths. It sends
// one small request with model to each candidate path, and a path is served only
// when the reply is 2xx and its body has the protocol's success shape. The key goes
// in header when fixed, else first in the protocol's usual header and, on 401 or
// 403, once more in the other one (authorization or x-api-key).
//
// It returns the served protocols with their paths, and the auth_header to write:
// header when fixed, else "" when each protocol worked with its usual header, else
// the one header that they all worked with.
func probeAPI(ctx context.Context, client *http.Client, s routerconfig.Server, model, header string, fixed bool) (map[string]string, string, error) {
	ep, err := routerconfig.ParseURL(s.URL)
	if err != nil {
		return nil, "", err
	}
	api, worked := map[string]string{}, map[string]string{}
	var failed []probeReply
	var reachErr error
	for _, p := range routerconfig.APIProtocols {
		first := header
		if !fixed {
			first = usualHeader(p)
		}
		other := "authorization"
		if strings.EqualFold(first, "authorization") {
			other = "x-api-key"
		}
		for _, path := range probePaths[p] {
			r, err := probeOnce(ctx, client, ep.URL()+path, s, p, model, first)
			if err == nil && !fixed && (r.status == http.StatusUnauthorized || r.status == http.StatusForbidden) {
				r, err = probeOnce(ctx, client, ep.URL()+path, s, p, model, other)
			}
			if err != nil {
				reachErr = err
				continue
			}
			if r.served {
				api[p], worked[p] = path, r.header
				break
			}
			failed = append(failed, r)
		}
	}
	if len(api) == 0 {
		if reachErr != nil {
			return nil, "", reachErr
		}
		return nil, "", noneServed(failed, s.Key)
	}
	if fixed {
		return api, header, nil
	}
	allUsual := true
	for p, h := range worked {
		allUsual = allUsual && h == usualHeader(p)
	}
	if allUsual {
		return api, "", nil
	}
	one := ""
	for _, h := range worked {
		if one != "" && one != h {
			return nil, "", errors.New("the server takes a different auth header for each protocol; give --auth-header and --api")
		}
		one = h
	}
	return api, one, nil
}

// noneServed is the error when no candidate path served: the reply itself when every
// path gave the same one, as then the model or the key is wrong, not the path.
func noneServed(failed []probeReply, key string) error {
	same := len(failed) > 0
	for _, r := range failed {
		same = same && r.status == failed[0].status && bytes.Equal(head(r.body), head(failed[0].body))
	}
	if !same {
		return errors.New("no path tried served a protocol; give the protocols with --api <protocol>=<path>")
	}
	return fmt.Errorf("every path answered %d: %s. The model or the key is wrong, not the path.",
		failed[0].status, replyText(head(failed[0].body), key))
}

// head is the first 512 bytes of b.
func head(b []byte) []byte { return b[:min(len(b), 512)] }

// replyText is b as one line safe to print: valid UTF-8, no control characters,
// and the key, should the server repeat it, left out.
func replyText(b []byte, key string) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(string(b), "?"))
	if key != "" {
		s = strings.ReplaceAll(s, key, "<key>")
	}
	return strings.Join(strings.Fields(s), " ")
}

type probeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesProbe struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Messages  []probeMessage `json:"messages"`
}

type responsesProbe struct {
	Model           string `json:"model"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Input           string `json:"input"`
}

// probeOnce sends protocol's probe request to u with the key in header h. An error
// is a request that got no answer. Its text quotes no part of the URL.
func probeOnce(ctx context.Context, client *http.Client, u string, s routerconfig.Server, protocol, model, h string) (probeReply, error) {
	var body any = messagesProbe{Model: model, MaxTokens: 1, Messages: []probeMessage{{Role: "user", Content: "ping"}}}
	if protocol == "responses" {
		// Responses can refuse a max_output_tokens of 1.
		body = responsesProbe{Model: model, MaxOutputTokens: 16, Input: "ping"}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return probeReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return probeReply{}, errors.New("could not build the probe request")
	}
	req.Header.Set("Content-Type", "application/json")
	if protocol == "anthropic-messages" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	for k, v := range routerconfig.HeaderOf(s.Headers) {
		req.Header[k] = v
	}
	routerconfig.SetAuth(req.Header, h, s.Key)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // its text quotes the URL
		}
		return probeReply{}, fmt.Errorf("could not reach the server: %v", err)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeReply))
	r := probeReply{status: resp.StatusCode, body: reply, header: h}
	r.served = resp.StatusCode/100 == 2 && servedShape(protocol, reply)
	return r, nil
}

// servedShape reports whether body is protocol's success reply: type "message" for
// Anthropic Messages, a choices array for chat completions, an output array for
// Responses. A status alone does not say: some servers answer 200 with an error.
func servedShape(protocol string, body []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return false
	}
	isArray := func(raw json.RawMessage) bool {
		var a []json.RawMessage
		return json.Unmarshal(raw, &a) == nil && a != nil
	}
	switch protocol {
	case "anthropic-messages":
		var t string
		return json.Unmarshal(obj["type"], &t) == nil && t == "message"
	case "chat-completions":
		return isArray(obj["choices"])
	case "responses":
		return isArray(obj["output"])
	}
	return false
}

// probeTotal is the probe's whole time: perRequest for each candidate path.
func probeTotal(perRequest time.Duration) time.Duration {
	n := 0
	for _, paths := range probePaths {
		n += len(paths)
	}
	return time.Duration(n) * perRequest
}
