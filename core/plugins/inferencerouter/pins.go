package inferencerouter

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/storage"
)

// pins is where the router keeps its pins: the process's durable store when one was
// injected, so a restart forgets none, else the process store, which a reload keeps
// and a restart empties.
type pins interface {
	// load is key's pin, and whether there is one.
	load(key string) (pin, bool)
	// save pins key to pn for pinTTL.
	save(key string, pn pin)
	// keep renews key's pin, pn, for another pinTTL: its session is still in use.
	// It reports whether it renewed it.
	keep(key string, pn pin) bool
}

// pinsFor is where pctx's pins are kept, nil when there is nowhere.
func (p *Router) pinsFor(pctx *pipeline.Context) pins {
	if p.store != nil {
		now := p.now
		if now == nil {
			now = time.Now
		}
		return storedPins{st: p.store, now: now}
	}
	if pctx.Shared != nil {
		return memPins{st: pctx.Shared}
	}
	return nil
}

// memPins keeps pins in the process store, as the pin struct itself.
type memPins struct{ st pipeline.SharedStore }

func (m memPins) load(key string) (pin, bool) {
	v, ok := m.st.Get(key)
	pn, isPin := v.(pin)
	return pn, ok && isPin
}

func (m memPins) save(key string, pn pin)      { m.st.Put(key, pn, pinTTL) }
func (m memPins) keep(key string, pn pin) bool { m.st.Put(key, pn, pinTTL); return true }

// pinRecord is a pin as the durable store holds it.
type pinRecord struct {
	Agent  string `json:"agent"`
	Server string `json:"server"` // "" for not routed
	// Renewed is the Unix time, in seconds, the pin's TTL was last set.
	Renewed int64 `json:"renewed"`
}

// renewAfter is how old a stored pin's TTL gets before a request renews it. Every
// request keeps its session's pin, and a store write each time would have the store
// save its file every interval while any session is busy; renewed once a day, a
// pin's TTL runs between 29 and 30 days from its session's last request.
const renewAfter = 24 * time.Hour

// storedPins keeps pins in the durable store, as JSON pin records.
type storedPins struct {
	st  storage.Store
	now func() time.Time
}

// record is key's stored record. A value that is not one is no pin: the session is
// decided as unpinned, and its pin is written over it.
func (d storedPins) record(key string) (pinRecord, bool) {
	raw, err := d.st.Get(context.Background(), key)
	if err != nil {
		slog.Warn("inference-router: reading a pin from the plugin store failed; the session is decided as unpinned",
			"error", err)
		return pinRecord{}, false
	}
	var rec pinRecord
	if raw == "" || json.Unmarshal([]byte(raw), &rec) != nil {
		return pinRecord{}, false
	}
	return rec, true
}

func (d storedPins) load(key string) (pin, bool) {
	rec, ok := d.record(key)
	return pin{agent: rec.Agent, server: rec.Server}, ok
}

func (d storedPins) save(key string, pn pin) {
	b, err := json.Marshal(pinRecord{Agent: pn.agent, Server: pn.server, Renewed: d.now().Unix()})
	if err == nil {
		err = d.st.Set(context.Background(), key, string(b), pinTTL)
	}
	if err != nil {
		slog.Warn("inference-router: saving a pin to the plugin store failed; the session's next request decides again",
			"error", err)
	}
}

func (d storedPins) keep(key string, pn pin) bool {
	if rec, ok := d.record(key); ok && d.now().Sub(time.Unix(rec.Renewed, 0)) < renewAfter {
		return false
	}
	d.save(key, pn)
	return true
}
