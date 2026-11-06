package inferencerouter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/tidwall/gjson"
)

const (
	// refusedPrefix namespaces the records of models a server refused, by server and
	// model name.
	refusedPrefix = Name + "/refused/"

	// refusedFor is how long a refusal is believed. The model list cannot say when a
	// refused model becomes available, because a server serves aliases it does not
	// list, so the record lapses on a timer and the next request tries the agent's own
	// model again. Being wrong costs one refused round trip, which the resend hides.
	refusedFor = time.Hour

	// substitutePrefix namespaces a session's substitutes, by session, agent and
	// server.
	substitutePrefix = Name + "/substitute/"

	// catalogPrefix namespaces each server's last good model list.
	catalogPrefix = Name + "/models/"

	// catalogEvery is how often each server's model list is fetched again, so a word
	// resolves to a model the server added since.
	catalogEvery = time.Hour

	// refusedType is LiteLLM's error type for a model the key's team may not use.
	refusedType = "team_model_access_denied"

	// invalidModel is in LiteLLM's 400 for a model it has no route for, which a key
	// that may use every model gets in place of refusedType.
	invalidModel = "Invalid model name passed in model="
)

// modelNotFound are the phrases a model server's error message carries for a model it
// does not have, %s standing for the name the refused attempt was sent with, and the
// statuses each was recorded at. Each was recorded from the server named, on
// /v1/chat/completions, /v1/messages and /v1/responses alike for Ollama 0.40.4 and
// llama.cpp, and on /v1/chat/completions alone for Ollama 0.7.0, the only one of the
// three it serves; a phrase is added only once a server has been seen to answer with
// it. A phrase carries no quoting around
// the name — each server quotes it in its own style, Ollama 0.40.4 and llama.cpp
// 'name', Ollama 0.7.0 "name" with ", try pulling it first" appended, vLLM `name` —
// and refusesModel tries each style a server has been seen to use.
var modelNotFound = []struct {
	phrase   string
	statuses []int
}{
	// Ollama 0.40.4 (404), Ollama 0.7.0 (404) and llama.cpp b11429 serving several
	// models (400).
	{"model %s not found", []int{http.StatusBadRequest, http.StatusNotFound}},
	// vLLM 0.31.0.
	{"The model %s does not exist.", []int{http.StatusNotFound}},
}

// capturedPaths are the paths a captured request is sent to, by how the path the
// agent named ends: the API format, whoever the agent and whatever its provider.
var capturedPaths = []struct{ suffix, path string }{
	{"/chat/completions", "/v1/chat/completions"},
	{"/v1/messages", "/v1/messages"},
	{"/responses", "/v1/responses"},
}

// capturePath is the server path an inference request addressed to a host that is no
// server is sent to, and whether the router captures it at all: a POST, with a path
// ending in one of capturedPaths. The parse is checked by the caller; see capture.
func capturePath(method, path string) (string, bool) {
	if method != http.MethodPost {
		return "", false
	}
	for _, c := range capturedPaths {
		if strings.HasSuffix(path, c.suffix) {
			return c.path, true
		}
	}
	return "", false
}

// capture is the server path for pctx when it is a request the router captures: an
// inference request — the parser's word, not the path's, since an agent's own tools
// can POST to any API whose path happens to end the same way — that capturePath
// takes. ok is false for anything else.
func capture(pctx *pipeline.Context) (path string, ok bool) {
	if pctx.Extensions.Inference == nil {
		return "", false
	}
	return capturePath(pctx.Method, pctx.Path)
}

// catalogs is each server's model list, by server name, as last fetched.
type catalogs struct {
	mu  sync.RWMutex
	ids map[string][]string
}

func (c *catalogs) get(server string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ids[server]
}

func (c *catalogs) set(server string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ids == nil {
		c.ids = map[string][]string{}
	}
	c.ids[server] = ids
}

// substitute is the model server sends for a request it refused: its main model when
// the request carries tools, its helper otherwise, as the server's latest list
// resolves the word; "" when the list names none or there is no list yet.
func (p *Router) substitute(server string, ext *pipeline.InferenceExtension) string {
	srv := p.servers[server]
	word := srv.helper
	if kindOf(ext) == kindMain {
		word = srv.main
	}
	return routerconfig.Resolve(word, p.catalogs.get(server))
}

// The kinds of substitute: a request with tools gets the server's main model, one
// without its helper.
const (
	kindMain   = "main"
	kindHelper = "helper"
)

// kindOf is the kind of substitute a request with parse ext gets.
func kindOf(ext *pipeline.InferenceExtension) string {
	if ext != nil && len(ext.Tools) > 0 {
		return kindMain
	}
	return kindHelper
}

// substituteEntry is where a session's substitutes keep the one for requested, sent by
// a request with parse ext: one for each kind, so each kind of request keeps its own.
func substituteEntry(ext *pipeline.InferenceExtension, requested string) string {
	return kindOf(ext) + " " + requested
}

// chooseModel sends a request routed to server with the model chooseModelFor
// decides, by the order the package doc gives, and keeps a new substitute for the
// session. The decision is applied with SetRequestModel.
func (p *Router) chooseModel(pctx *pipeline.Context, server string) {
	requested, ok := pctx.RequestModel()
	if !ok {
		return
	}
	model, keep := chooseModelFor(requested, p.sessionSubstitute(pctx, server, requested),
		func() bool { return p.isRefused(pctx, server, requested) },
		func() string { return p.substitute(server, pctx.Extensions.Inference) })
	if keep {
		p.keepSubstitute(pctx, server, requested, model)
	}
	if model == requested {
		return
	}
	if err := pctx.SetRequestModel(model); err != nil {
		slog.Warn("inference-router: could not give a request its substitute model; it goes with the model it asked for",
			"server", server, "error", err)
	}
}

// Resend implements pipeline.Resender. A refusal of the model the agent asked for is
// answered with the server's substitute, as decideResend says; a refusal for a
// limit teaches the router that limit for the model, and the request is fitted and
// sent again. decideResend decides which, and each kind is answered at most once per
// request.
func (p *Router) Resend(_ context.Context, pctx *pipeline.Context, status int, body []byte) bool {
	ext := pctx.Extensions.Inference
	server, routed := p.byHost[routerconfig.Key(pctx.Host)]
	model, hasModel := pctx.RequestModel()
	step := decideResend(resendFacts{
		status: status, refusal: body, body: pctx.Body, model: model, hasModel: hasModel,
		routed: routed, parsed: ext != nil, substituted: ext != nil && ext.RequestedModel != "",
		redirected: pctx.Redirected(), switched: switched(pctx),
		limitResent: slices.Contains(pctx.ResendReasons(), "limit"),
	}, func() string { return p.substitute(server, ext) })
	switch step.reason {
	case "model":
		return p.resendForModel(pctx, server, model, step.sub)
	case "limit":
		return p.resendForLimit(pctx, server, model, step)
	}
	return false
}

// resendForLimit learns step's limit for model, the model a routed request was sent
// to server with, and fits the request to it.
func (p *Router) resendForLimit(pctx *pipeline.Context, server, model string, step resendStep) bool {
	p.saveLimits(pctx, server, model, p.loadLimits(pctx, server, model).learn(step.kind, step.n))
	which := "window"
	if step.kind == limitOutput {
		which = "output"
	}
	details := map[string]string{"server": server, "model": model, "limit": which,
		"value": strconv.Itoa(step.n), "stated": strconv.FormatBool(step.stated)}
	if !p.applyLimits(pctx, server, false) {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "limit_not_fitted", Details: details})
		return false
	}
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "resent", Details: details})
	pctx.MarkResend("limit")
	return true
}

// resendForModel answers server's refusal of requested, the model the agent asked
// for. The refusal is recorded for refusedFor, and the request is sent again with sub,
// the server's substitute, before the agent sees anything. With no sub, or one the
// request cannot take, the refusal reaches the agent as the server answered it.
func (p *Router) resendForModel(pctx *pipeline.Context, server, requested, sub string) bool {
	p.markRefused(pctx, server, requested)
	details := map[string]string{"server": server, "model": requested}
	if sub == "" || pctx.SetRequestModel(sub) != nil {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_substitute", Details: details})
		return false
	}
	p.keepSubstitute(pctx, server, requested, sub)
	details["substitute"] = sub
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "resent", Details: details})
	pctx.MarkResend("model")
	return true
}

// refusesModel reports whether a server's answer refuses model, the name the refused
// attempt was sent with: a 403 with refusedType, a 400 whose error, a string or an
// object's message, contains invalidModel, or an answer whose error message carries
// one of modelNotFound's phrases for model at a status that phrase was recorded at.
// A phrase for another name, or for none, is not a refusal of this one.
func refusesModel(status int, body []byte, model string) bool {
	switch status {
	case http.StatusForbidden:
		if gjson.GetBytes(body, "error.type").String() == refusedType {
			return true
		}
	case http.StatusBadRequest:
		e := gjson.GetBytes(body, "error")
		if e.IsObject() {
			e = e.Get("message")
		}
		if e.Type == gjson.String && strings.Contains(e.String(), invalidModel) {
			return true
		}
	}
	if model == "" {
		return false
	}
	msg := errorMessage(body)
	for _, nf := range modelNotFound {
		if !slices.Contains(nf.statuses, status) {
			continue
		}
		for _, quotes := range []string{"'", `"`, "`"} {
			if strings.Contains(msg, fmt.Sprintf(nf.phrase, quotes+model+quotes)) {
				return true
			}
		}
	}
	return false
}

// isRefused reports whether server refused model within refusedFor.
func (p *Router) isRefused(pctx *pipeline.Context, server, model string) bool {
	key := refusedPrefix + server + "/" + model
	if p.store != nil {
		v, err := p.store.Get(context.Background(), key)
		return err == nil && v != ""
	}
	if pctx.Shared != nil {
		_, ok := pctx.Shared.Get(key)
		return ok
	}
	return false
}

// markRefused records that server refused model, for refusedFor.
func (p *Router) markRefused(pctx *pipeline.Context, server, model string) {
	key := refusedPrefix + server + "/" + model
	if p.store != nil {
		if err := p.store.Set(context.Background(), key, "1", refusedFor); err != nil {
			slog.Warn("inference-router: could not record a refused model; the next request tries it again", "error", err)
		}
		return
	}
	if pctx.Shared != nil {
		pctx.Shared.Put(key, true, refusedFor)
	}
}

// substituteKey is where pctx's session keeps its substitutes on server, "" when
// there is no one session to keep them for: none, or one of the listener's synthetic
// ones, which hold many conversations. They are the agent's, as a pin is.
func substituteKey(pctx *pipeline.Context, server string) string {
	if pctx.SessionID == "" || synthetic(pctx.SessionID) {
		return ""
	}
	return substitutePrefix + pctx.SessionID + "/" + agentOf(pctx) + "/" + server
}

// substitutes is the session's substitutes kept at key.
func (p *Router) substitutes(pctx *pipeline.Context, key string) map[string]string {
	if p.store != nil {
		raw, err := p.store.Get(context.Background(), key)
		var subs map[string]string
		if err != nil || raw == "" || json.Unmarshal([]byte(raw), &subs) != nil {
			return nil
		}
		return subs
	}
	if pctx.Shared != nil {
		if v, ok := pctx.Shared.Get(key); ok {
			subs, _ := v.(map[string]string)
			return subs
		}
	}
	return nil
}

// sessionSubstitute is the model the session already sends requested as on server,
// "" when it sends none.
func (p *Router) sessionSubstitute(pctx *pipeline.Context, server, requested string) string {
	key := substituteKey(pctx, server)
	if key == "" {
		return ""
	}
	return p.substitutes(pctx, key)[substituteEntry(pctx.Extensions.Inference, requested)]
}

// keepSubstitute keeps sub as the model the session sends requested as on server,
// for as long as a pin is kept: a conversation must not change model part-way, when
// a refusal lapses or a list changes what a word resolves to.
func (p *Router) keepSubstitute(pctx *pipeline.Context, server, requested, sub string) {
	key := substituteKey(pctx, server)
	if key == "" {
		return
	}
	subs := map[string]string{}
	for k, v := range p.substitutes(pctx, key) {
		subs[k] = v
	}
	subs[substituteEntry(pctx.Extensions.Inference, requested)] = sub
	if p.store != nil {
		b, _ := json.Marshal(subs)
		if err := p.store.Set(context.Background(), key, string(b), pinTTL); err != nil {
			slog.Warn("inference-router: could not keep a session's substitute model", "error", err)
		}
		return
	}
	if pctx.Shared != nil {
		pctx.Shared.Put(key, subs, pinTTL)
	}
}

// Init implements pipeline.Initializer: each server's model list is read back from
// the durable store, so a substitute resolves before the first fetch answers, and
// then fetched now and every catalogEvery until Shutdown.
func (p *Router) Init(context.Context) error {
	for name := range p.servers {
		if ids := p.loadCatalog(name); ids != nil {
			p.catalogs.set(name, ids)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.stopCatalogs = cancel
	go func() {
		t := time.NewTicker(catalogEvery)
		defer t.Stop()
		for {
			p.fetchCatalogs(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Shutdown implements pipeline.Shutdowner: it stops the fetches Init started.
func (p *Router) Shutdown(context.Context) error {
	if p.stopCatalogs != nil {
		p.stopCatalogs()
	}
	return nil
}

// fetchCatalogs fetches every server's model list once. A failed fetch keeps the
// list the server had.
func (p *Router) fetchCatalogs(ctx context.Context) {
	client := p.client
	if client == nil {
		client = routerconfig.NewModelsClient()
	}
	for name, srv := range p.servers {
		ids, err := routerconfig.FetchModels(ctx, client, srv.endpoint, srv.key)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("inference-router: could not fetch a server's model list; its main and helper resolve against the last one",
					"server", name, "error", err)
			}
			continue
		}
		p.catalogs.set(name, ids)
		p.saveCatalog(name, ids)
	}
}

func (p *Router) loadCatalog(server string) []string {
	if p.store == nil {
		return nil
	}
	raw, err := p.store.Get(context.Background(), catalogPrefix+server)
	var ids []string
	if err != nil || raw == "" || json.Unmarshal([]byte(raw), &ids) != nil {
		return nil
	}
	return ids
}

func (p *Router) saveCatalog(server string, ids []string) {
	if p.store == nil {
		return
	}
	b, _ := json.Marshal(ids)
	if err := p.store.Set(context.Background(), catalogPrefix+server, string(b), pinTTL); err != nil {
		slog.Warn("inference-router: could not keep a server's model list", "server", server, "error", err)
	}
}
