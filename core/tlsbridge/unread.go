package tlsbridge

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// The unread report's reasons. They are the strings the forward proxy's rows carry for
// the same cases (pipeline.TunnelProgramRefused and TunnelOSTrustOnly). This package
// does not import pipeline, so a forwardproxy test holds the two in step.
const (
	UnreadProgramRefused = "program-refused"
	UnreadOSTrustOnly    = "os-trust-only"
)

// unreadMax bounds the UnreadLog; past it, the least recently seen program is forgotten.
const unreadMax = 256

// UnreadLog remembers, per program, the connections the bridge did not read because of
// something about the program — a refusal, or a predicted one — for the unread report.
// It decides nothing; the skip sets and Trust do.
type UnreadLog struct {
	mu sync.Mutex
	m  map[string]*unreadEntry
}

type unreadEntry struct {
	reason      string
	connections int
	lastHost    string
	lastSeen    time.Time
}

func NewUnreadLog() *UnreadLog { return &UnreadLog{m: make(map[string]*unreadEntry)} }

// Note records one connection to host that the bridge did not read for reason. key is
// what decided it: the program memory's key that passed it through, Program.Key or
// Program.ProcessKey, so the report can join the note to that entry, or Program.Key for
// a prediction. Nil-safe, so an Engine without a log records nothing.
func (u *UnreadLog) Note(key, reason, host string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	e, ok := u.m[key]
	if !ok {
		if len(u.m) >= unreadMax {
			u.evictOldestLocked()
		}
		e = &unreadEntry{}
		u.m[key] = e
	}
	e.reason = reason
	e.connections++
	e.lastHost = host
	e.lastSeen = time.Now()
}

func (u *UnreadLog) evictOldestLocked() {
	var oldK string
	var oldT time.Time
	for k, e := range u.m {
		if oldK == "" || e.lastSeen.Before(oldT) {
			oldK, oldT = k, e.lastSeen
		}
	}
	delete(u.m, oldK)
}

func (u *UnreadLog) snapshot() map[string]unreadEntry {
	out := map[string]unreadEntry{}
	if u == nil {
		return out
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, e := range u.m {
		out[k] = *e
	}
	return out
}

// UnreadReport is what GET /tls-bridge/unread serves: the programs and hosts the bridge
// is passing through unread, and whether the OS trusts the bridge CA.
type UnreadReport struct {
	// OSTrustsCA is whether the OS trusts the bridge CA where the bridge asks (darwin),
	// and absent elsewhere, so a reader can tell "not trusted" from "not checked".
	OSTrustsCA       *bool           `json:"osTrustsCA,omitempty"`
	OSTrustCheckedAt *time.Time      `json:"osTrustCheckedAt,omitempty"`
	Programs         []UnreadProgram `json:"programs"`
	Hosts            []UnreadHost    `json:"hosts"`
}

// UnreadProgram is one program the bridge is not reading.
type UnreadProgram struct {
	Program string `json:"program"`
	Agent   string `json:"agent,omitempty"`
	// PID is set when the entry is one process's rather than the program's: a process
	// that started before the bridge CA, whose refusal is remembered against it alone
	// (Program.ProcessKey), so restarting it is what reads it again.
	PID    int32  `json:"pid,omitempty"`
	Reason string `json:"reason"`
	// Connections counts this program's connections the bridge did not read.
	Connections int `json:"connections"`
	// Failures and Stopped come from the program memory, on program-refused only.
	Failures int  `json:"failures,omitempty"`
	Stopped  bool `json:"stopped,omitempty"`
	// Until is when the bridge tries the program again: absent when it has stopped, and
	// on os-trust-only, which is decided afresh on every connection.
	Until    *time.Time `json:"until,omitempty"`
	LastHost string     `json:"lastHost,omitempty"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// UnreadHost is one host in the host memory: what a client whose program cannot be
// named is passed through for.
type UnreadHost struct {
	Host     string    `json:"host"`
	Failures int       `json:"failures"`
	Until    time.Time `json:"until"`
}

// UnreadReport assembles the report from the program memory, the unread log, the host
// memory and Trust, each optional.
func (e *Engine) UnreadReport() UnreadReport {
	r := UnreadReport{Programs: []UnreadProgram{}, Hosts: []UnreadHost{}}
	if tr, ok := e.Trust.(TrustReporter); ok {
		if trusted, at, checked := tr.OSTrustState(); checked {
			r.OSTrustsCA, r.OSTrustCheckedAt = &trusted, &at
		}
	}
	notes := e.Unread.snapshot()
	if e.Programs != nil {
		for _, en := range e.Programs.Entries() {
			p := ProgramFromKey(en.Key)
			// An exited process's entry can match no connection again, so it is not
			// listed. The report leaves the program memory alone: the entry stays until
			// evicted, which costs nothing, since a new process never shares its key.
			if p.PID != 0 && e.ProcessAlive != nil && !e.ProcessAlive(p.PID, p.Start) {
				continue
			}
			up := UnreadProgram{Program: p.Exe, Agent: p.Agent, PID: p.PID, Reason: UnreadProgramRefused,
				Failures: en.Failures, Stopped: en.Stopped}
			if !en.Stopped {
				until := en.Until
				up.Until = &until
			}
			if n, ok := notes[en.Key]; ok {
				fillUnread(&up, n)
				delete(notes, en.Key)
			}
			r.Programs = append(r.Programs, up)
		}
	}
	// What is left are notes with no live program entry. A refusal whose window ended
	// is no longer being passed through. A prediction is, unless the OS is known to
	// trust the CA now, in which case the program is bridged again.
	osTrusts := r.OSTrustsCA != nil && *r.OSTrustsCA
	for k, n := range notes {
		if n.reason != UnreadOSTrustOnly || osTrusts {
			continue
		}
		p := ProgramFromKey(k)
		up := UnreadProgram{Program: p.Exe, Agent: p.Agent, Reason: UnreadOSTrustOnly}
		fillUnread(&up, n)
		r.Programs = append(r.Programs, up)
	}
	if e.Skip != nil {
		for _, en := range e.Skip.Entries() {
			r.Hosts = append(r.Hosts, UnreadHost{Host: en.Key, Failures: en.Failures, Until: en.Until})
		}
	}
	sort.Slice(r.Programs, func(i, j int) bool {
		a, b := r.Programs[i], r.Programs[j]
		if a.Program != b.Program {
			return a.Program < b.Program
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.PID < b.PID
	})
	sort.Slice(r.Hosts, func(i, j int) bool { return r.Hosts[i].Host < r.Hosts[j].Host })
	return r
}

func fillUnread(up *UnreadProgram, n unreadEntry) {
	up.Connections = n.connections
	up.LastHost = n.lastHost
	seen := n.lastSeen
	up.LastSeen = &seen
}

// UnreadHandler serves UnreadReport as JSON. GET only: serving it decides and records
// nothing. The one thing it can change is the OS-trust answer, which it refreshes when
// that has outlived its window, as the next Go program's connection would have.
func (e *Engine) UnreadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e.UnreadReport())
	})
}
