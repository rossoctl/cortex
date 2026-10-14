package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
)

// configureState is the one verdict every agent's status reduces to, for the table
// `agentop configure status` prints.
//
// FOUR values, not a bool, and configureUnknown is not a flavour of "no". Every one of the
// five agents already distinguishes a case where agentop cannot answer from one where
// it can answer "not routed", and collapsing those loses the only part that tells a
// user what to do next:
//
//   - claude-code and codex compute "N of M keys set" — a partially-written settings
//     file is neither enabled (a session would miss the CA) nor untouched.
//   - bob states this outright with three package-level strings, bobStatusUnknown
//     among them: with no settings document there is nothing to make a claim about.
//   - opencode cannot read Cortex's config to compare against, or cannot run the
//     opencode CLI at all; the keys it can see say nothing on their own.
//   - bobshell can see the exported variable but never the shell's function table,
//     so "the variable is set" is not "bob runs through Cortex".
//
// The issue's mockup asked for exactly this with its `uncertain` row (#1164).
type configureState int

const (
	// configureUnknown is "agentop cannot tell", and is the ZERO VALUE on purpose: a
	// status a future agent forgets to set reads as an absent answer rather than as a
	// confident "not enabled", which is the wrong way to be wrong on this surface.
	configureUnknown configureState = iota
	// configureOff is a positive finding: agentop looked and the agent is not routed
	// through Cortex.
	configureOff
	// configurePartial is "some of it is in place" — the N-of-M case, and a drifted
	// value that points at a proxy that is not this machine's Cortex. Deliberately
	// distinct from configureOff: enable has something left to do, and from
	// configureUnknown: agentop did read the configuration.
	configurePartial
	// configureOn is fully routed through Cortex, by that agent's own definition of
	// fully — which differs per agent and is each agent's own to decide.
	configureOn
)

// String is the word the table prints. The vocabulary is the issue's:
// "enabled", "uncertain". The other two are its obvious complements.
//
// A method on the type rather than a map, so a new state cannot be added without the
// compiler's switch-exhaustiveness-adjacent default landing on something safe.
func (s configureState) String() string {
	switch s {
	case configureOn:
		return "enabled"
	case configureOff:
		return "not enabled"
	case configurePartial:
		return "partly enabled"
	default:
		return "uncertain"
	}
}

// configureStatus is one row of the table: which agent, the verdict, and one short
// line of why.
//
// Detail is ONE LINE and no more. The per-agent `status` commands print paragraphs —
// bob's runs to a verdict, a liveness warning, a detail block and a CA note — and the
// whole value of the table is that five agents fit on five lines. Anyone who wants the
// paragraph has `agentop configure <agent> status`, which the table's footer says.
type configureStatus struct {
	Agent  string
	State  configureState
	Detail string
}

// configureStatusFuncs is the table's source: one closure per agent, in the order
// configureUsage lists them.
//
// A slice of closures rather than a switch inside the renderer, because the thing
// that must be true of this list is that it matches configureUsage's five agents —
// and a list is what a test can compare against that usage text
// (TestConfigureStatusTable_CoversEveryAgentInUsage).
//
// home is passed in rather than read per agent: runConfigureStatus has already failed
// if it is unavailable, and five separate os.UserHomeDir() calls would be five places
// for that failure to be handled differently.
//
// Each closure takes the HOME DIRECTORY and resolves its own agent's paths from it,
// which is what keeps the table reading the same defaults as each `configure <harness>
// status` command — both sides go through settingsRel, bobSettingsPath,
// defaultCodexEnvPath, findOpenCode and cortexCfgRel rather than through two copies of
// the same join. None of them may return an error: see runConfigureStatus on why one
// agent's missing file must not end the table.
func configureStatusFuncs() []func(home string) configureStatus {
	return []func(home string) configureStatus{
		func(home string) configureStatus {
			return claudeCodeState(filepath.Join(home, settingsRel))
		},
		func(home string) configureStatus {
			cfg := filepath.Join(home, cortexCfgRel)
			settings, err := bobSettingsPath(home)
			if err != nil {
				// Not an error row for the whole table and not "not enabled" either:
				// on a platform where IBM Bob's settings path is not known, agentop
				// has not looked at a file and must not report as though it had.
				return configureStatus{Agent: "bob", State: configureUnknown, Detail: err.Error()}
			}
			wantProxy, _ := bobWanted(cfg, home)
			return bobState(settings, cfg, wantProxy)
		},
		func(string) configureStatus { return bobShellState() },
		func(home string) configureStatus {
			bin, err := findOpenCode()
			if err != nil {
				// The CLI is how OpenCode's service environment is read at all, so
				// without it there is nothing to read — "uncertain", not "not
				// enabled". A machine with no OpenCode installed lands here, and
				// "not enabled" would read as a finding about a configuration that
				// does not exist.
				return configureStatus{Agent: "opencode", State: configureUnknown, Detail: err.Error()}
			}
			return openCodeState(bin, filepath.Join(home, cortexCfgRel))
		},
		func(home string) configureStatus {
			return codexState(defaultCodexEnvPath(home), filepath.Join(home, cortexCfgRel))
		},
	}
}

// runConfigureStatus prints the table: every agent `agentop configure <x> status`
// covers, and whether it is wired into Cortex (#1164).
//
// ONE AGENT'S FAILURE IS ONE ROW, never the whole table. Every state function returns
// a configureStatus and no error, so a missing settings file, an unreadable Cortex
// config or an absent opencode binary becomes an "uncertain" row with the reason in
// its detail column — which is the answer someone ran this to get, since the common
// case is that most of the five agents are not installed at all. Pinned by
// TestConfigureStatusTable_OneAgentsMissingConfigDoesNotBreakTheTable.
//
// Exit 0 whatever the rows say, on the same reasoning every per-agent status uses: a
// report of "not enabled" is a successful report, and a non-zero status here would
// make the command unusable in a pipeline.
func runConfigureStatus(args []string, stdout, stderr io.Writer) int {
	// No flags of its own, deliberately. Five agents read five different files and the
	// per-agent overrides (--settings, --env, --config, --opencode) do not compose into
	// one flag set — `--config` alone is Cortex's config for three agents and the
	// agent's own for a fourth. Anyone who needs to point at a non-default path wants
	// one agent's status, which already takes the flag.
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprint(stdout, configureUsage)
			return 0
		}
		fmt.Fprintf(stderr, "agentop: configure status takes no arguments (got %q)\n", args[0])
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintf(stderr, "agentop: cannot determine your home directory: %v\n", err)
		return 1
	}

	rows := make([]configureStatus, 0, 5)
	for _, fn := range configureStatusFuncs() {
		rows = append(rows, fn(home))
	}
	writeConfigureStatusTable(rows, stdout)
	return 0
}

// writeConfigureStatusTable renders the rows.
//
// text/tabwriter, the same helper cmd_server.go's listing uses, rather than measured
// padding: these three columns are ASCII agent names, a fixed four-word vocabulary,
// and a detail string, so there is no wide-character width problem of the kind
// writePipelineTable has to measure around — and reusing the stdlib writer is the
// repo's existing answer for a plain-text column listing.
//
// Every row has all three cells, the detail one empty when there is nothing to add:
// tabwriter aligns a column only across consecutive rows that have it, so a short row
// would restart the alignment below it. The padding that leaves on such a row is
// trimmed off again, which is why the output goes through a buffer first — the same
// shape, and for the same reason, as runServerList's.
func writeConfigureStatusTable(rows []configureStatus, stdout io.Writer) {
	var table strings.Builder
	tw := tabwriter.NewWriter(&table, 0, 0, 3, ' ', 0)
	fmt.Fprint(tw, "HARNESS\tSTATUS\tDETAIL\n")
	for _, r := range rows {
		// WHITESPACE COLLAPSED BEFORE SANITISING, and the order is the point. Several
		// details that reach this column are multi-line — errBridgeDisabled and
		// codexWanted's missing-ca_dir error are wrapped paragraphs written for a
		// paragraph-shaped surface. sanitizeCell maps every rune under 0x20 to U+FFFD,
		// so a newline arriving here rendered as a literal "�" mid-sentence rather
		// than as a space, twice in the codex row under tls_bridge.mode: disabled.
		//
		// strings.Fields splits on every run of whitespace and drops the empties, so
		// one Join collapses newlines AND the two-space continuation indents that
		// follow them — which a plain ReplaceAll("\n", " ") would have left behind as
		// three spaces. Sanitising still runs, on the rest: this makes the column keep
		// its one-line promise, it does not make the string trusted.
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Agent, r.State, sanitizeCell(strings.Join(strings.Fields(r.Detail), " ")))
	}
	tw.Flush()
	for line := range strings.Lines(table.String()) {
		fmt.Fprintln(stdout, strings.TrimRight(line, " \n"))
	}
	// The table is a verdict per agent and nothing more; this names where the
	// paragraph went. Without it the one-line detail column reads as all agentop has
	// to say, and bob's status alone has four distinct things to report.
	fmt.Fprint(stdout, "\nFor the detail on one of them: agentop configure <harness> status\n")
}

// claudeCodeState is claude-code's verdict as data.
//
// The tri-state is claudeCodeStatus's own, extracted rather than invented: it already
// counted how many of managedKeys the env block sets and printed one of two closing
// lines from the count. "Some but not all" is configurePartial, and an unreadable or
// non-JSON settings file is configureUnknown — the file exists and agentop cannot read
// what it says, which is not the same as reading it and finding nothing.
//
// Note this reads only the settings file and never Cortex's config, so "enabled" here
// means the seven keys are set, not that they point at THIS machine's proxy. That is
// the pre-existing contract of `configure claude-code status` (codexStatus's doc
// comment names it as the difference between the two), and widening it would change
// what that command reports.
func claudeCodeState(settingsPath string) configureStatus {
	doc, err := readSettings(settingsPath)
	return claudeCodeStateFrom(settingsPath, doc, err)
}

// claudeCodeStateFrom is claudeCodeState over a settings document already read.
//
// Split out so claudeCodeStatus reads the file ONCE and prints both its per-key lines
// and its verdict from that one read. Reading it twice — once for the state, once for
// the lines — would let a file edited between the two reads produce a verdict that
// contradicts the keys printed above it.
func claudeCodeStateFrom(settingsPath string, doc map[string]any, err error) configureStatus {
	if err != nil {
		return configureStatus{Agent: "claude-code", State: configureUnknown, Detail: err.Error()}
	}
	env := envStrings(doc)
	set := 0
	for _, k := range managedKeys {
		if _, ok := env[k]; ok {
			set++
		}
	}
	switch {
	case set == len(managedKeys):
		return configureStatus{Agent: "claude-code", State: configureOn, Detail: settingsPath}
	case set == 0:
		return configureStatus{Agent: "claude-code", State: configureOff, Detail: "no Cortex keys in " + settingsPath}
	default:
		return configureStatus{
			Agent:  "claude-code",
			State:  configurePartial,
			Detail: fmt.Sprintf("%d of %d keys set in %s", set, len(managedKeys), settingsPath),
		}
	}
}

// bobState is IBM Bob's verdict as data, mapped off bobOwns — the same ownership test
// bobStatus prints from.
//
// The mapping is the one bobStatus's three package-level strings already encode:
// bobOurs is configureOn, bobUnknown is configureUnknown (a loopback proxy with no
// readable Cortex config to compare it against), no document at all is
// configureUnknown, and everything else is a finding. A DRIFTED loopback proxy — one
// Cortex address where the config now names another — is configurePartial rather than
// configureOff, because it is the one "no" that is about Cortex: something configured
// this and the port moved. bobStatus prints two paragraphs about that case; this is
// the one-word version of them.
func bobState(settingsPath, cortexCfgPath, wantProxy string) configureStatus {
	row := func(s configureState, detail string) configureStatus {
		return configureStatus{Agent: "bob", State: s, Detail: detail}
	}
	if nerr := bobNoDocument(settingsPath); nerr != nil {
		return row(configureUnknown, nerr.Error())
	}
	if derr := bobDuplicateKey(settingsPath); derr != nil {
		// Two values for one key and no way to say which IBM Bob uses without
		// reimplementing its precedence — so agentop does not KNOW whether Bob is
		// routed, which is configureUnknown and not a finding. codexState answers the
		// same situation the same way, for the reason stated in its own comment: the
		// value agentop read may not be the value the agent uses, so the read is not a
		// verdict. These two were inconsistent until #1164's review caught it.
		//
		// bobStatus's own wording is untouched: it changes its verdict only on
		// configureOn, so it still prints bobStatusNo here and names the duplicate as
		// the actionable part.
		return row(configureUnknown, derr.Error())
	}
	doc, err := readSettings(settingsPath)
	if err != nil {
		// The file exists and agentop cannot read what it says, which is not the same
		// as reading it and finding nothing — claudeCodeStateFrom's words for the
		// identical case, and the distinction this table exists to draw.
		return row(configureUnknown, err.Error())
	}
	return bobStateFrom(settingsPath, doc, cortexCfgPath, wantProxy)
}

// bobStateFrom is bobState over a settings document already read.
//
// Split out for the reason claudeCodeStateFrom, codexStateFrom and openCodeStateFrom
// are, and it was the last of the four to get it (#1164's review caught the gap):
// bobStatus reads the settings file once and prints its per-key lines from that read,
// but it took its VERDICT from bobState, which read the file again. A file edited
// between the two reads produced a verdict contradicting the keys printed above it.
// With this, bobStatus hands over the document it already has.
//
// The early returns in bobState stay there rather than moving here, because each is a
// question about the FILE rather than about a document — whether it exists at all, and
// whether it defines the key twice, which a decoded map can no longer answer. bobStatus
// asks those two itself, before this, and answers them in its own words.
func bobStateFrom(settingsPath string, doc map[string]any, cortexCfgPath, wantProxy string) configureStatus {
	row := func(s configureState, detail string) configureStatus {
		return configureStatus{Agent: "bob", State: s, Detail: detail}
	}
	existing, ok := doc[bobProxyKey].(string)
	if !ok {
		if doc[bobProxyKey] == nil {
			return row(configureOff, fmt.Sprintf("%q is unset in %s", bobProxyKey, settingsPath))
		}
		return row(configureOff, fmt.Sprintf("%q is a %T, not a string", bobProxyKey, doc[bobProxyKey]))
	}
	switch bobOwns(existing, wantProxy) {
	case bobOurs:
		return row(configureOn, fmt.Sprintf("%q=%s", bobProxyKey, existing))
	case bobUnknown:
		return row(configureUnknown, fmt.Sprintf("%q=%s is a loopback proxy, but %s is not readable",
			bobProxyKey, existing, cortexCfgPath))
	default:
		if wantProxy != "" && bobIsLoopbackProxy(existing) {
			return row(configurePartial, fmt.Sprintf("%q=%s, but Cortex is at %s", bobProxyKey, existing, wantProxy))
		}
		return row(configureOff, fmt.Sprintf("%q=%s is not this machine's Cortex proxy", bobProxyKey, existing))
	}
}

// bobShellState is the bob shell function's verdict as data.
//
// NEVER configureOn, and that is the whole point of modelling this one. bobShellStatus
// spends a paragraph on why: the exported variable is inherited by every child
// process, including a non-interactive subshell that never reads the rc file and so
// has no bob function at all. agentop is itself such a child, and a shell's function
// table is never exported — so "the variable is set" is evidence and not a verdict,
// and configurePartial is what that is. Reporting "enabled" from an inherited variable
// is exactly the confident wrong answer bobShellStatus's comment refuses to give.
func bobShellState() configureStatus {
	if v, ok := os.LookupEnv(bobShellEnvVar); ok {
		return configureStatus{
			Agent:  "bobshell",
			State:  configurePartial,
			Detail: fmt.Sprintf("%s=%s; run \"type bob\" to confirm the function in your shell", bobShellEnvVar, v),
		}
	}
	return configureStatus{
		Agent:  "bobshell",
		State:  configureOff,
		Detail: bobShellEnvVar + " is unset in this shell",
	}
}

// openCodeState is OpenCode's verdict as data.
//
// Two distinct uncertainties here, and both have to stay uncertain rather than
// collapsing to "not enabled": the opencode CLI may not run (so the service
// environment cannot be read at all), and Cortex's config may not be readable (so the
// nine values that ARE readable have nothing to be judged against). openCodeStatus
// prints the keys in both cases and declines to close with a verdict; this says the
// same thing in one word.
//
// The service's own running state is deliberately NOT part of this verdict, matching
// openCodeStatus: a stopped service is the normal state of a laptop and says nothing
// about whether the environment is configured.
func openCodeState(bin, cortexCfgPath string) configureStatus {
	row := func(s configureState, detail string) configureStatus {
		return configureStatus{Agent: "opencode", State: s, Detail: detail}
	}
	env, err := openCodeServiceEnv(bin)
	if err != nil {
		return row(configureUnknown, err.Error())
	}
	want, _, err := wantedFromConfig(cortexCfgPath)
	if err != nil {
		return row(configureUnknown, fmt.Sprintf("cannot compare with Cortex's config: %v", err))
	}
	return openCodeStateFrom(env, openCodeValues(want))
}

// openCodeStateFrom is OpenCode's verdict over a service environment already read and
// the wanted values already derived, so openCodeStatus does not run the opencode CLI a
// second time to ask for the verdict it prints — that is a subprocess per call, and a
// second run could see a different environment from the keys printed above it.
func openCodeStateFrom(env, vals map[string]string) configureStatus {
	row := func(s configureState, detail string) configureStatus {
		return configureStatus{Agent: "opencode", State: s, Detail: detail}
	}
	// Set means set to Cortex's value, the same count openCodeStatus closes with: a
	// variable left at an older Cortex address is not counted, because a service
	// started with it would not reach this one.
	set := 0
	for _, k := range openCodeKeys {
		if w, ok := vals[k]; ok && env[k] == w {
			set++
		}
	}
	switch {
	case set == len(openCodeKeys):
		return row(configureOn, "OpenCode's service environment")
	case set == 0:
		return row(configureOff, "no Cortex values in OpenCode's service environment")
	default:
		return row(configurePartial, fmt.Sprintf("%d of %d keys set", set, len(openCodeKeys)))
	}
}

// codexState is Codex's verdict as data.
//
// codexStatus's own tri-state, extracted: it counts how many of codexKeys hold the
// value Cortex's config says they should and prints one of two closing lines — unless
// the config cannot be read, where it says so and declines to close at all. That
// declining is configureUnknown.
//
// A duplicate definition of a managed key is configureUnknown too, and it has to be:
// Codex's own dotenv loading decides which line wins, so the value agentop read may
// not be the value Codex uses, and the count is therefore not a verdict. codexStatus
// prints that as a warning alongside its count; the table has no room for both, and
// the honest one of the two is the warning.
func codexState(envPath, cortexCfgPath string) configureStatus {
	row := func(s configureState, detail string) configureStatus {
		return configureStatus{Agent: "codex", State: s, Detail: detail}
	}
	f, err := parseCodexEnv(envPath)
	if err != nil {
		return row(configureUnknown, err.Error())
	}
	want, err := codexWanted(cortexCfgPath)
	if err != nil {
		return row(configureUnknown, fmt.Sprintf("cannot compare with Cortex's config: %v", err))
	}
	// The duplicate check belongs HERE and not in codexStateFrom, which codexStatus
	// also calls. codexStatus prints the warning as a line of its own and then its
	// count, and has done since before this extraction; folding the check into the
	// shared function would have turned its "enabled in ..." closing line into "not
	// fully enabled" for a file whose five keys all match. The table has no second
	// line to put the warning on, so it spends its one word on it instead.
	if dup := f.ambiguous(codexKeys); len(dup) > 0 {
		return row(configureUnknown, fmt.Sprintf("%s defines %s more than once, so Codex decides which line wins",
			envPath, strings.Join(dup, ", ")))
	}
	return codexStateFrom(envPath, f, want)
}

// codexStateFrom is codex's verdict over a dotenv file already parsed and a wanted set
// already derived, so codexStatus prints its per-key lines and its verdict from one
// parse rather than parsing the file a second time to ask for the verdict.
func codexStateFrom(envPath string, f *codexEnvFile, want map[string]string) configureStatus {
	row := func(s configureState, detail string) configureStatus {
		return configureStatus{Agent: "codex", State: s, Detail: detail}
	}
	set := 0
	for _, k := range codexKeys {
		if v, ok := f.get(k); ok && v == want[k] {
			set++
		}
	}
	switch {
	case set == len(codexKeys):
		return row(configureOn, envPath)
	case set == 0:
		return row(configureOff, "no Cortex values in "+envPath)
	default:
		return row(configurePartial, fmt.Sprintf("%d of %d keys set in %s", set, len(codexKeys), envPath))
	}
}

// sortedKeys is the managed-key order the per-agent status commands print in. Kept
// here beside the state functions so the two readings of the same key list sit
// together; `sort.Strings` on a copy, never on the package-level slice, which the
// enable path iterates in its own meaningful order.
func sortedKeys(keys []string) []string {
	out := slices.Clone(keys)
	sort.Strings(out)
	return out
}
