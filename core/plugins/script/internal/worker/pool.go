package worker

import (
	"strings"
	"sync"
	"time"
)

// Pool keeps workers alive across pipeline builds. A config reload rebuilds every
// plugin, and a script that restarted on every build would lose its state whenever
// anything at all in the config changed. The pool hands the next build the worker
// the previous build was using, for as long as the script file and the child's
// command and environment are unchanged.
//
// A build is a holder. It acquires workers while it is configured, activates once
// its pipeline starts, and releases them when its pipeline is shut down. A worker
// stops when no holder has it, so:
//
//   - an edited script gets a new worker beside the old one, and the old one keeps
//     serving the running pipeline until that pipeline is drained and shut down;
//   - a removed script's worker stops with the last pipeline that ran it;
//   - a build that never starts — a reload refused after its plugins were
//     configured — is never shut down either, so its holds would leak. Activate
//     sweeps them: builds are made one at a time, so any holder older than the one
//     starting that never started itself was abandoned.
type Pool struct {
	mu      sync.Mutex
	next    int
	entries map[key]*poolEntry
	active  map[int]bool
}

// key is what a running child is made from. A build that changes any of it gets a
// new child; the call timeout is not in it, and each build's Worker carries its own.
type key struct {
	path, hash, args, env string
	start                 time.Duration
}

func keyOf(cfg Config, hash string) key {
	return key{
		path: cfg.Path, hash: hash,
		args: strings.Join(cfg.Args, "\x00"), env: strings.Join(cfg.Env, "\x00"),
		start: cfg.StartTimeout,
	}
}

type poolEntry struct {
	c       *child
	holders map[int]bool
}

// NewPool returns an empty pool.
func NewPool() *Pool { return &Pool{entries: map[key]*poolEntry{}, active: map[int]bool{}} }

// NewHolder returns a holder id for one build. Ids increase, which is what lets
// Activate tell an abandoned build from a live one.
func (p *Pool) NewHolder() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	return p.next
}

// Acquire returns the worker for this version of cfg.Path and this cfg, starting a
// new one when none runs it.
func (p *Pool) Acquire(holder int, cfg Config, hash string) *Worker {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := keyOf(cfg, hash)
	e, ok := p.entries[k]
	if !ok {
		e = &poolEntry{c: New(cfg).child, holders: map[int]bool{}}
		p.entries[k] = e
	}
	e.holders[holder] = true
	return e.c.with(cfg.CallTimeout)
}

// Activate marks holder's pipeline as running, and stops every worker held only by
// builds that were abandoned before it.
func (p *Pool) Activate(holder int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[holder] = true
	for k, e := range p.entries {
		for h := range e.holders {
			if h < holder && !p.active[h] {
				delete(e.holders, h)
			}
		}
		p.reap(k, e)
	}
}

// Release gives up every worker holder holds, stopping each one no other holder has.
func (p *Pool) Release(holder int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, holder)
	for k, e := range p.entries {
		delete(e.holders, holder)
		p.reap(k, e)
	}
}

// reap stops e's worker when nothing holds it. Caller holds mu. The stop runs
// without mu, since it can wait a second for the script to exit.
func (p *Pool) reap(k key, e *poolEntry) {
	if len(e.holders) > 0 {
		return
	}
	delete(p.entries, k)
	go e.c.Stop()
}
