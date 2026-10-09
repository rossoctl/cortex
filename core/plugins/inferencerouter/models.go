package inferencerouter

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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

// chooseModel decides the model a request routed to server is sent with, by the
// order the package doc gives: the session's substitute for the name, else a
// substitute when the server refused the name within refusedFor, else the name the
// agent asked for. The decision is applied with SetRequestModel.
func (p *Router) chooseModel(pctx *pipeline.Context, server string) {
	requested, ok := pctx.RequestModel()
	if !ok {
		return
	}
	sub := p.sessionSubstitute(pctx, server, requested)
	if sub == "" && p.isRefused(pctx, server, requested) {
		if sub = p.substitute(server, pctx.Extensions.Inference); sub != "" {
			p.keepSubstitute(pctx, server, requested, sub)
		}
	}
	if sub == "" || sub == requested {
		return
	}
	if err := pctx.SetRequestModel(sub); err != nil {
		slog.Warn("inference-router: could not give a request its substitute model; it goes with the model it asked for",
			"server", server, "error", err)
	}
}

// Resend implements pipeline.Resender. When the server a request was routed to
// refused the model the agent asked for, the refusal is recorded for refusedFor, and
// the request is sent again with the server's substitute, before the agent sees
// anything. A request already sent with a substitute is never sent again, so a
// refused substitute reaches the agent as the server answered it.
func (p *Router) Resend(_ context.Context, pctx *pipeline.Context, status int, body []byte) bool {
	if !refusesModel(status, body) || !pctx.Redirected() {
		return false
	}
	server, ok := p.byHost[routerconfig.Hostname(pctx.Host)]
	ext := pctx.Extensions.Inference
	if !ok || ext == nil || ext.RequestedModel != "" {
		return false
	}
	requested, ok := pctx.RequestModel()
	if !ok {
		return false
	}
	p.markRefused(pctx, server, requested)
	details := map[string]string{"server": server, "model": requested}
	sub := p.substitute(server, ext)
	if sub == "" || sub == requested {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_substitute", Details: details})
		return false
	}
	if err := pctx.SetRequestModel(sub); err != nil {
		pctx.Record(pipeline.Invocation{Action: pipeline.ActionSkip, Reason: "no_substitute", Details: details})
		return false
	}
	p.keepSubstitute(pctx, server, requested, sub)
	details["substitute"] = sub
	pctx.Record(pipeline.Invocation{Action: pipeline.ActionModify, Reason: "resent", Details: details})
	return true
}

// refusesModel reports whether a server's answer refuses the model a request named:
// a 403 with refusedType, or a 400 whose error, a string or an object's message,
// contains invalidModel.
func refusesModel(status int, body []byte) bool {
	switch status {
	case http.StatusForbidden:
		return gjson.GetBytes(body, "error.type").String() == refusedType
	case http.StatusBadRequest:
		e := gjson.GetBytes(body, "error")
		if e.IsObject() {
			e = e.Get("message")
		}
		return e.Type == gjson.String && strings.Contains(e.String(), invalidModel)
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
	if pctx.Session == nil || pctx.Session.ID == "" || synthetic(pctx.Session.ID) {
		return ""
	}
	return substitutePrefix + pctx.Session.ID + "/" + agentOf(pctx) + "/" + server
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
