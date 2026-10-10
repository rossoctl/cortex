package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// fetchUnreadReport fetches the running proxy's /tls-bridge/unread report. A var so the
// doctor scene can stop its tests reaching whatever proxy runs on this machine.
var fetchUnreadReport = fetchUnread

func fetchUnread(base string) (tlsbridge.UnreadReport, bool) {
	c := &http.Client{Timeout: localProbeTimeout}
	resp, err := c.Get(base + "/tls-bridge/unread") //nolint:noctx // bounded by Timeout
	if err != nil {
		return tlsbridge.UnreadReport{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tlsbridge.UnreadReport{}, false
	}
	var rep tlsbridge.UnreadReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return tlsbridge.UnreadReport{}, false
	}
	return rep, true
}

// checkUnread lists the programs the running proxy is passing through unread, from its
// /tls-bridge/unread report, and on macOS says how to have the Go ones read. It replaces
// a check that searched the login keychain for the CA: the proxy's own answer counts
// trust granted in any domain, and names the programs it affects.
//
// Advice only, never a failure: a program passed through is working. Silent when the
// bridge is off, or when nothing at the stats address gives a report — a stopped proxy is
// another check's to report, and an older one has no list.
func checkUnread(env *setupEnv, ui *checklist.UI) {
	cfg, err := config.Load(env.configPath())
	if err != nil || !bridgeEnabled(cfg) {
		return
	}
	base := dialURL(cfg.Stats.StatsAddress)
	if base == "" {
		return
	}
	rep, ok := fetchUnreadReport(base)
	if !ok {
		return
	}
	// The host memory is unread traffic too, from clients whose program Cortex could not
	// name, so an empty program list alone is not everything read.
	if len(rep.Programs) == 0 && len(rep.Hosts) == 0 {
		ui.Done("unread", "no program is passed through for distrusting Cortex's CA", 0)
		return
	}
	goPrograms := false
	for _, p := range rep.Programs {
		ui.Note("unread", unreadLine(env, p))
		goPrograms = goPrograms || p.Reason == tlsbridge.UnreadOSTrustOnly
	}
	if len(rep.Hosts) > 0 {
		ui.Note("unread", unreadHostsLine(rep.Hosts))
	}
	if goPrograms && env.goos == "darwin" {
		// What trusting the CA buys is said under the fix rather than promised by it: Go
		// programs are then read on the hosts Cortex intercepts, and go's and gh's own
		// hosts are not among them, so neither tool is read for its usual traffic. And
		// not every Go program trusts what macOS trusts: gvproxy relays the podman VM's
		// clients, which trust their own CAs, and a program given its own CA list trusts
		// that. Once the prediction stops passing them through they refuse, and the
		// program memory passes them through again only after its refusals.
		keychain := filepath.Join(env.home, "Library", "Keychains", "login.keychain-db")
		ui.Advise("Go tools", "work through Cortex but are not read (a Go agent's model calls are not priced either): macOS does not trust its CA",
			"security add-trusted-cert -k "+env.tilde(keychain)+" -p ssl "+env.tilde(filepath.Join(cfg.TLSBridge.CADir, "ca.crt")))
		ui.Faint("    once macOS trusts the CA, Cortex reads Go programs' traffic to the hosts it intercepts, within a minute") // at the fix's indent
		ui.Faint("    programs that relay another system's traffic (gvproxy, for the podman VM) or bring their own CA list are then shown " +
			"Cortex's certificate, and fail up to three times per Cortex run before Cortex passes them through again")
	}
}

// unreadLine is one program's line: its name, the agent it ran under, why it is not
// read, the process when the entry is one process's, and the last host. The agent is
// shown as a path because an agent's executable is often named for its version (Claude
// Code's is), which says nothing on its own.
func unreadLine(env *setupEnv, p tlsbridge.UnreadProgram) string {
	s := filepath.Base(p.Program)
	if p.Agent != "" {
		s += " under " + env.tilde(p.Agent)
	}
	s += ": " + p.Reason
	var aside []string
	if p.PID != 0 {
		// Only a process older than the CA is remembered by its PID: it cannot have
		// loaded the CA, so the program itself is not at fault. Restarting that process
		// reads it again even once its entry has stopped, so a stop is not mentioned.
		aside = append(aside, fmt.Sprintf("pid %d, started before Cortex's CA — restarting it is enough", p.PID))
	} else if p.Stopped {
		s += ", not retried until Cortex restarts"
	}
	if p.LastHost != "" {
		aside = append(aside, "last "+p.LastHost)
	}
	if len(aside) > 0 {
		s += " (" + strings.Join(aside, "; ") + ")"
	}
	return s
}

// unreadHostsShown is how many hosts unreadHostsLine names before it only counts.
const unreadHostsShown = 3

// unreadHostsLine is the host memory as one line: how many hosts are passed through for
// clients Cortex could not name, and the first few of them.
func unreadHostsLine(hosts []tlsbridge.UnreadHost) string {
	s := fmt.Sprintf("%d hosts", len(hosts))
	if len(hosts) == 1 {
		s = "1 host"
	}
	names := make([]string, 0, unreadHostsShown)
	for _, h := range hosts[:min(len(hosts), unreadHostsShown)] {
		names = append(names, h.Host)
	}
	s += " passed through for clients Cortex could not name: " + strings.Join(names, ", ")
	if more := len(hosts) - len(names); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}
