package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// keyBinding is one row in the help overlay: the key(s) and what they do.
type keyBinding struct {
	keys string
	desc string
}

// keyGroup is a titled block of bindings in the help overlay.
//
// purpose says what the group IS — for a pane, what it shows. It is the field
// the pane-focused rewrite added, and it exists because the overlay used to name
// nine panes and describe none of them: "what can I look at, and why" was
// unanswerable from the one surface that exists to answer it.
//
// notes carry prose that belongs to the group rather than to any single key.
// They exist so a caveat stops being smuggled into the key column as a binding
// with an empty `keys` field, which is how the spend-scope note used to ship — a
// sentence wearing a keybinding's clothes.
type keyGroup struct {
	title    string
	purpose  string
	bindings []keyBinding
	notes    []string
}

// Section titles. Named constants because the jump section is conditional and
// two others are asserted on, so a literal would drift.
const (
	jumpSectionTitle  = "GO TO ANOTHER PANE"
	drillSectionTitle = "THE DRILL PATH"
	anywhereTitle     = "ANYWHERE"
	helpNavTitle      = "MOVING AROUND THIS HELP"
	spendDrawerTitle  = "INSIDE THE SPEND DRAWER"
	everyPaneTitle    = "EVERY PANE"

	// thisPaneSuffix marks the active pane's group. Carried in the stored title
	// rather than added at render time so paneName can strip it and the two forms
	// can never disagree.
	thisPaneSuffix = " (this pane)"
)

// anywhereKeys are the bindings that work on every pane regardless of what is on
// screen.
//
// WHAT IS NO LONGER HERE IS THE POINT. This was `globalKeys`, and it had become a
// bin: the overlay's own scroll keys, the keys that OPEN other panes (`P`, `C`),
// the spend drawer's `a`/`w` — inert unless the drawer is already up — and a
// caveat with an empty key column, all in one flat list under one heading that
// claimed they worked everywhere. Three of those groups did not.
//
// The pane-opening keys moved to jumpTargets, which renders them per pane and
// only where they work. The drawer's keys moved to spendDrawerKeys, with the
// caveat as a note.
var anywhereKeys = keyGroup{
	title: anywhereTitle,
	bindings: []keyBinding{
		{"?", "this help"},
		{"/", "search what this pane shows (enter keeps, esc cancels)"},
		{"n / N", "next / previous match"},
		{"p", "pause / resume the stream"},
		{"q · ctrl+c", "quit"},
	},
}

// helpNavKeys move this overlay. Their own group because that is the one framing in
// which every one of them is unconditionally true.
//
// THE FIRST TWO ATTEMPTS AT THIS WERE BOTH WRONG, in opposite directions, and the
// measured behaviour is why. While the overlay is up, handleKey sends everything
// except ?/esc/q/g/G to helpVp.Update, and bubbles' viewport binds `b`/`f` to
// PageUp/PageDown — so all three rows move the overlay on EVERY pane, usage
// included. With it closed they move the pane instead, and there the coverage is
// ragged: pageActivePane has no paneUsage case, goTop/goBottom cover neither usage
// nor the two pickers, and on usage `b` is the breakdown cycle.
//
// So listing them under ANYWHERE claimed a pane behaviour that does not hold
// (the original bug), and gating a row off usage denied an overlay behaviour that
// does (the first fix). Describing them as what they are — the overlay's own
// navigation — is true everywhere and needs no gating at all. The one pane where
// the closed-overlay meaning differs is named in the note rather than by removing a
// row the reader needs to page these 86 lines.
//
// `?`/`esc`/`q` close the overlay and are deliberately NOT here: that is the one
// hint pinned in the frame's footer (see renderHelpOverlay), where it cannot be
// scrolled away, and a second copy could drift from it.
var helpNavKeys = keyGroup{
	title: helpNavTitle,
	bindings: []keyBinding{
		{"↑↓ / jk", "scroll a line"},
		{"b / f", "page"},
		{"g / G", "jump to the top / bottom"},
	},
	notes: []string{
		"All three work on every pane, because they move this overlay rather than " +
			"what is behind it. With it closed they move the pane's own list instead — " +
			"except on the usage pane, which has no list: there b cycles the breakdown " +
			"and g / G do nothing.",
	},
}

// spendDrawerKeys are live only while the spend drawer is open. Their own
// section, because listing `a` and `w` beside the keys that work everywhere
// taught two bindings that do nothing most of the time.
//
// No `$` row: `$` both opens and closes the drawer, and it is already listed as a jump
// target. A second row would advertise one key for two things, which
// TestGlobalKeys_AdvertiseNoKeyTwice refuses, so the purpose line says it closes.
var spendDrawerKeys = keyGroup{
	title:   spendDrawerTitle,
	purpose: "opened and closed with $ over the spend band; a and w are live only while it is up",
	bindings: []keyBinding{
		{"a", "cycle the axis"},
		{"w", "cycle the span (the band's four)"},
	},
	// The scope, stated where there is always room for it — and THIS IS STILL THE ONLY PLACE
	// IT IS SPELLED OUT. The sessions footer used to carry it as a notice, and the pane title
	// after that as " · lifetime totals"; both are gone, because a note that has to fit in a
	// title could only name a span, and the table has no single span to name (see paneView's
	// sessions case).
	//
	// "continues across proxy restarts with a session archive, resets without one" rather than
	// "lifetime", which is the correction that motivated dropping the title note. Without an
	// archive the store is in memory, so a session's figures cover only as far back as the
	// current proxy process. With one, every row reaches back through restarts — a resident row
	// adds the archive's fold of what came before its entry, and a row only the archive holds is
	// that fold — but a clear or retention can still drop a session whose cost the ledger keeps.
	// A reader comparing the table against the band's day figure and finding it smaller is
	// seeing that, not a bug.
	//
	// A NOTE NOW, not a binding with keys:"". The same sentence, no longer pretending to be a key.
	notes: []string{
		"Every band cell names its own span. The sessions table is per session: with a " +
			"session archive it continues across proxy restarts, without one it resets, so its " +
			"figures can read smaller than the band's.",
	},
}

// helpGlobalGroups returns the groups that are not panes and that apply on pane,
// in render order.
//
// A FUNCTION, AND THE ONE helpBodyLines RENDERS FROM. As a plain slice it was a
// registry only the tests read, so a group added to the body and not to the slice
// would have escaped every invariant check — the exact drift those checks exist to
// catch. The drawer's group is the only conditional one left, for the reason the
// jump section drops `$`: on the pickers and on usage the key is refused, and a
// titled block explaining what `a` and `w` do inside a surface that cannot be
// opened is three keys of pure noise.
func helpGlobalGroups(pane paneID) []keyGroup {
	groups := []keyGroup{anywhereKeys, helpNavKeys}
	if ok, _ := spendDrawerHostPane(pane); ok {
		groups = append(groups, spendDrawerKeys)
	}
	return groups
}

// drillPath is the spine: the panes you reach by drilling in, in order, each a
// step deeper than the last. Rendered as a single line because the SHAPE is the
// information — the old overlay listed all nine panes flat, so nothing said that
// events sits under sessions, or that esc walks back up rather than out.
var drillPath = []paneID{paneNamespaces, panePods, paneSessions, paneEvents, paneDetail}

// jumpTarget is a key that opens a surface from somewhere else, as opposed to one
// that acts within a pane.
//
// pane is paneNone for `$`: it opens a drawer over the current pane rather than a
// pane, so its availability comes from spendDrawerHostPane instead of from a
// handler changing m.pane, and label names it. For real panes the label is
// derived from paneName, so it cannot drift from the group title.
type jumpTarget struct {
	key   string
	pane  paneID
	label string
	desc  string
}

// name is how the jump row labels the target.
func (jt jumpTarget) name() string {
	if jt.pane == paneNone {
		return jt.label
	}
	return strings.ToLower(paneName(jt.pane))
}

// jumpTargets are the four keys that leave the pane you are on.
//
// `C` WAS `P` UNTIL THE PIPELINE TOOK THAT LETTER, and this section is the reason
// that swap was cheap: `P`-for-catalog was never shown in any footer, so there
// was close to no muscle memory behind it — and the discoverability problem the
// overlay existed to solve for it is now a titled section rather than one line in
// a group of ten.
var jumpTargets = []jumpTarget{
	{key: "u", pane: paneUsage,
		desc: "charts over the events the proxy has seen — every session from the " +
			"sessions table, the selected one from events"},
	{key: "P", pane: panePipeline,
		desc: "the plugin chain this proxy runs, editable in $EDITOR"},
	{key: "C", pane: paneCatalog,
		desc: "every plugin the proxy offers, from /v1/plugins"},
	{key: "A", pane: paneAgents,
		desc: "what each coding agent has spent today; pick one to scope the views"},
	{key: "$", pane: paneNone, label: "spend",
		desc: "a drawer over the spend band: tiers and a breakdown"},
}

// jumpsFrom returns the jump targets that actually work from p.
//
// THE KEYS DO NOT SHARE ONE ALLOWLIST, which is why this is a switch and not a
// single membership test: `u` and `P` open only from the session views, `C` opens
// from anything past the pickers, and `$` follows the drawer's own host rule. The
// old overlay papered over that by writing "(session views)" beside two keys and
// "(not on usage)" beside a third, in a group that rendered identically on every
// pane — so on the namespaces picker it advertised three keys that do nothing and
// on usage it advertised a fourth.
//
// Kept honest by TestHelpBody_JumpSectionMatchesTheKeysThatActuallyWork, which
// drives the real handlers for every pane rather than re-stating these sets.
func jumpsFrom(p paneID) []jumpTarget {
	var out []jumpTarget
	for _, jt := range jumpTargets {
		// Never offer a jump to the pane the reader is already standing on.
		if jt.pane == p {
			continue
		}
		switch jt.key {
		case "u", "P":
			switch p {
			case paneSessions, paneEvents, paneDetail:
			default:
				continue
			}
		case "C", "A":
			// Both need a connection and nothing else, so both are offered everywhere past
			// the pickers. Sharing the arm rather than duplicating it: the rule is the same
			// rule, and two copies would be two places to forget.
			switch p {
			case paneNamespaces, panePods:
				continue
			}
		case "$":
			if ok, _ := spendDrawerHostPane(p); !ok {
				continue
			}
		}
		out = append(out, jt)
	}
	return out
}

// paneKeys maps each pane to its purpose and its own bindings, rendered first
// (and emphasized) when the overlay opens over that pane, and again in full under
// everyPaneTitle for the panes the reader is not on.
//
// EVERY GROUP CARRIES A PURPOSE, enforced by TestHelpPurpose_EveryPaneHasOne. A
// pane the overlay names but does not describe is a pane the reader has to visit
// to find out whether it was the one they wanted.
//
// NO GROUP REPEATS A JUMP KEY. `P` and `u` used to appear here and in globalKeys
// both; they now live only in jumpTargets, which renders directly beneath this
// block and knows which of them work from where.
var paneKeys = map[paneID]keyGroup{
	paneNamespaces: {
		title: "NAMESPACES (this pane)",
		// "Kubernetes" rather than the bare "agents" this used to say, and pinned by
		// TestPaneKeys_TheTwoAgentPanesAreDistinguishable. These are workloads; AGENTS is the
		// pane about coding agents, and one word is what keeps the two apart in an overlay
		// that shows both.
		purpose: "Kubernetes agent workloads grouped by namespace; where agentop starts",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵", "open namespace"},
			{"l", "connect to the local session API"},
			{"r", "reload agent list"},
			// `esc` ALONE, not "q · esc": quit is already in ANYWHERE, and `q` here made
			// this the one pane group that restated a key from it. What is pane-specific
			// is that esc quits — everywhere else it goes back, and there is nowhere
			// above this.
			{"esc", "quit — nothing is above this pane"},
		},
	},
	panePods: {
		title:   "PODS (this pane)",
		purpose: "the agent pods in that namespace, each with a proxy to connect to",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵", "port-forward + connect"},
			{"esc", "back to namespaces"},
			{"r", "reload agent list"},
		},
	},
	paneSessions: {
		title: "SESSIONS (this pane)",
		purpose: "one row per agent session, with its tokens, cost and context. " +
			"Where the proxy keeps a session archive, every session it holds is listed.",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "drill into session"},
			{"X", "clear all history, in memory and on disk (asks first; the cost ledger is kept)"},
			{"esc", "back to the agents picker, else pods picker"},
		},
	},
	paneEvents: {
		title:   "EVENTS (this pane)",
		purpose: "the live request stream for one session, newest last",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "event detail"},
			{"s", "toggle passthru/skip rows"},
			{"c", "column picker (checkboxes + descriptions)"},
			{"c then s", "sort by a column: desc → asc → chronological"},
			{"o", "load the page before the oldest event shown"},
			{"t", "back to the live tail (resumes updates)"},
			{"esc / ← / h", "back to sessions"},
		},
	},
	paneDetail: {
		title:   "EVENT DETAIL (this pane)",
		purpose: "one event in full, as the proxy recorded it",
		bindings: []keyBinding{
			{"↑↓", "scroll"},
			{"y", "yank event JSON to ~/.cortex/agentop-events"},
			{"esc / ← / h", "back to events"},
		},
	},
	panePipeline: {
		title:   "PIPELINE (this pane)",
		purpose: "the plugin chain this proxy runs, in the order it runs them",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "plugin detail"},
			{"e", "edit pipeline in $EDITOR"},
			{"esc / ← / h", "back to where P was pressed"},
		},
	},
	panePluginDetail: {
		title:   "PLUGIN DETAIL (this pane)",
		purpose: "one plugin's resolved config, as the proxy loaded it",
		bindings: []keyBinding{
			{"↑↓", "scroll"},
			{"esc / ← / h", "back"},
		},
	},
	paneUsage: {
		title:   "USAGE (this pane)",
		purpose: "stacked-bar charts over the events the proxy has seen",
		bindings: []keyBinding{
			{"m", "cycle metric (tokens/requests/errors/latency/cost)"},
			{"w", "cycle window (10m/1h/6h)"},
			{"b", "cycle breakdown (none/status/method/plugin/host; not for latency or a scope)"},
			{"s", "toggle session / all sessions"},
			{"esc", "back"},
		},
	},
	paneCatalog: {
		title:   "PLUGIN CATALOG (this pane)",
		purpose: "every plugin the proxy offers, whether or not the pipeline runs it",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			{"↵ / → / l", "plugin detail"},
			{"r", "refresh from /v1/plugins"},
			{"esc / ← / h", "back"},
		},
	},
	paneAgents: {
		title: "AGENTS (this pane)",
		// "coding" is load-bearing and pinned by
		// TestPaneKeys_TheTwoAgentPanesAreDistinguishable: NAMESPACES lists Kubernetes
		// workloads and its own purpose called them "agents" too, and this overlay renders
		// both purposes at once — so without the word a reader cannot tell which pane they
		// want.
		purpose: "coding agents seen on the wire, and what each has spent",
		bindings: []keyBinding{
			{"↑↓ / jk", "navigate"},
			// Said here too because the footer only describes the row under the cursor.
			{"↵", "scope to this agent and list its sessions; All agents clears"},
			{"S", "choose the inference server this agent's new sessions use"},
			{"esc", "back, keeping the scope"},
		},
		// ONE SHORT NOTE, NOT TWO LONG ONES, and the budget is real rather than stylistic: an
		// earlier draft pushed the overlay body from 91 to 103 lines and
		// TestHelpOverlayScrollHint_AbsentWhenEverythingFits demanded helpNoScrollHeight be
		// raised to 106, which would make every reader of every other pane scroll for this
		// pane's explanation. The two ↵/esc rows above cost lines too, so this note is shorter
		// than the one it replaces. The argument belongs in agents_pane.go; what a reader needs
		// on screen is what changes when they press something.
		//
		// IT NO LONGER SAYS "READ-ONLY". It did, on the true-at-the-time grounds that /v1/usage
		// filters by session and nothing else — so the scope is computed client-side instead,
		// the same narrowing `agentop cost --agent` uses, and the endpoint's limit now bounds WHICH
		// views can honour it rather than whether any can.
		notes: []string{
			"Opens by itself when two or more agents have been seen. It scopes the sessions " +
				"list, the usage pane and the spend band. Other pools every agent the proxy " +
				"does not recognise.",
		},
	},
}

// otherPaneOrder fixes the render order of the "OTHER PANES" section so
// the overlay is stable across openings (Go map iteration is random).
var otherPaneOrder = []paneID{
	paneNamespaces, panePods, paneSessions, paneEvents,
	paneDetail, paneAgents, paneUsage, panePipeline, panePluginDetail, paneCatalog,
}

// helpKeyColWidth is the fixed width of the key column so descriptions
// align into a readable second column across every group.
const helpKeyColWidth = 12

// helpMinProseWidth is the narrowest budget helpBodyLines will wrap to. Below it
// wrapping produces a column of single words, which is less readable than letting
// the viewport clip — and a terminal that narrow has already lost the key columns.
const helpMinProseWidth = 40

// paneName is a pane's display name without the active-pane marker: "EVENT
// DETAIL", never "EVENT DETAIL (this pane)". One derivation, so the drill path,
// the jump rows and the everyPaneTitle section can never disagree with the title
// the pane's own group renders.
func paneName(p paneID) string {
	return strings.TrimSuffix(paneKeys[p].title, thisPaneSuffix)
}

// wrapWords breaks s into lines of at most width display columns, on spaces
// first and then — for a single word still too wide — on `/`.
//
// THE SLASH PASS IS NOT FOR PATHS. An earlier version split on spaces only and
// justified it by claiming the only over-long words here were paths and URLs,
// which was wrong: the words that actually overran are the enum lists in the usage
// pane's own bindings — "(tokens/requests/errors/latency/cost)" at 37 columns and
// "(none/status/method/plugin/host;" at 31. Under everyPaneTitle the description
// column starts at 18, so both overran on any terminal below 61 columns and the
// viewport clipped them: 53 lines across the nine panes at widths 40/41/48, and a
// 50-column reader saw "(tokens/requests/erro". That is the exact defect this
// rewrite exists to remove, reintroduced one layer down.
//
// An enum list reads perfectly well broken after a slash, which is why the original
// objection does not apply to the real offenders. A path broken the same way reads
// worse, but only ever when it genuinely does not fit — and a wrapped path still
// shows every character, where a clipped one does not.
func wrapWords(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var (
		lines []string
		cur   string
	)
	flush := func() {
		if cur != "" {
			lines = append(lines, cur)
			cur = ""
		}
	}
	for _, word := range strings.Fields(s) {
		if lipgloss.Width(word) > width {
			flush()
			lines = append(lines, splitOnSlashes(word, width)...)
			continue
		}
		switch {
		case cur == "":
			cur = word
		case lipgloss.Width(cur)+1+lipgloss.Width(word) <= width:
			cur += " " + word
		default:
			flush()
			cur = word
		}
	}
	flush()
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// splitOnSlashes packs word into lines of at most width columns, breaking only
// after `/` and joining the pieces with NO separator — the fragments are parts of
// one token, so the space wrapWords puts between words would invent one the reader
// would take for real ("(tokens/ requests/").
//
// A piece with no slash left to break on comes back over-long rather than cut
// mid-character. Nothing in the overlay hits that today; if something does the line
// is too wide instead of silently truncated, which the wrap test catches.
func splitOnSlashes(word string, width int) []string {
	var (
		out []string
		cur string
	)
	for _, piece := range strings.SplitAfter(word, "/") {
		switch {
		case cur == "":
			cur = piece
		case lipgloss.Width(cur)+lipgloss.Width(piece) <= width:
			cur += piece
		default:
			out = append(out, cur)
			cur = piece
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// proseBlock renders prefix followed by text, wrapped to width with a hanging
// indent under the prefix, so a pane's purpose reads as one paragraph attached to
// its name rather than as a loose sentence between the keys.
//
// A LONG PREFIX BREAKS INSTEAD OF HANGING. Past a third of the width the hanging
// indent costs more columns than the paragraph has left — "INSIDE THE SPEND
// DRAWER · " is 26 of 76 — so the text drops to its own indented lines. Pane
// titles are all under the threshold and stay inline, which is the common case.
func proseBlock(prefix, text string, width int, prefixStyle lipgloss.Style) string {
	indent := lipgloss.Width(prefix)
	if indent > width/3 {
		lines := wrapWords(text, width-4)
		var b strings.Builder
		b.WriteString(prefixStyle.Render(strings.TrimSuffix(prefix, " · ")))
		for _, ln := range lines {
			b.WriteString("\n    " + styleHint.Render(ln))
		}
		return b.String()
	}

	body := width - indent
	if body < 1 {
		body = 1
	}
	lines := wrapWords(text, body)

	var b strings.Builder
	b.WriteString(prefixStyle.Render(prefix) + styleHint.Render(lines[0]))
	for _, ln := range lines[1:] {
		b.WriteString("\n" + strings.Repeat(" ", indent) + styleHint.Render(ln))
	}
	return b.String()
}

// renderKeyGroup renders one titled group: its title and purpose as a paragraph,
// then its bindings, then any notes. emphasize bolds the title and the key column
// — used for the pane the overlay was opened over. indent shifts the whole block
// right, which is how the everyPaneTitle section nests nine of them.
//
// Descriptions wrap to the remaining width rather than running off the edge. They
// did run off it: the body was built width-blind and the viewport clipped
// whatever did not fit, so the one long line the old overlay had (the spend-scope
// caveat, ~100 columns) was simply cut in half on an 80-column terminal.
func renderKeyGroup(g keyGroup, emphasize bool, width, indent int) string {
	titleStyle := styleHint
	keyStyle := styleMuted
	if emphasize {
		titleStyle = styleTitle
		keyStyle = styleOK
	}
	pad := strings.Repeat(" ", indent)

	var b strings.Builder
	if g.purpose == "" {
		b.WriteString(pad + titleStyle.Render(g.title))
	} else {
		b.WriteString(proseBlock(pad+g.title+" · ", g.purpose, width, titleStyle))
	}

	// The key column widens to fit the group rather than clipping at
	// helpKeyColWidth, because the jump section puts "C  plugin catalog" in it —
	// the key AND the pane it opens, so the target names line up in a column of
	// their own instead of running into the descriptions behind an em dash.
	keyCol := helpKeyColWidth
	for _, kb := range g.bindings {
		if w := lipgloss.Width(kb.keys); w > keyCol {
			keyCol = w
		}
	}

	// A TWO-COLUMN GUTTER, not one. With a single space the group's widest key —
	// the one that sets keyCol and so gets no padding — ran straight into its
	// description: "C  plugin catalog every plugin the proxy offers".
	keyIndent := indent + 2
	descCol := keyIndent + keyCol + 2
	for _, kb := range g.bindings {
		keys := padRight(kb.keys, keyCol)
		b.WriteString("\n" + strings.Repeat(" ", keyIndent) + keyStyle.Render(keys) + "  ")
		for i, ln := range wrapWords(kb.desc, width-descCol) {
			if i > 0 {
				b.WriteString("\n" + strings.Repeat(" ", descCol))
			}
			b.WriteString(styleHint.Render(ln))
		}
	}
	for _, note := range g.notes {
		b.WriteString("\n")
		for i, ln := range wrapWords(note, width-keyIndent) {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(strings.Repeat(" ", keyIndent) + styleHint.Render(ln))
		}
	}
	return b.String()
}

// renderJumpSection renders the keys that leave the pane the reader is on, or —
// when none of them work there — the section's title over one line saying why.
//
// THE "WHY" LINE IS NOT DECORATION. On the two picker panes every jump key is
// inert, and an overlay that simply omitted the section would read as "there is
// nothing else to look at" on the very pane a new reader opens first.
//
// IT KEEPS THE TITLE, though, which it did not at first: the bare sentence sat at
// the same 2-column indent and muted style as everything else and read as a
// trailing row of the pane block above it rather than as the answer to "where can I
// go". Carried as the group's purpose, so the section holds its place in the
// overlay's shape on every pane while listing no key that does nothing.
func renderJumpSection(pane paneID, width int) string {
	jumps := jumpsFrom(pane)
	if len(jumps) == 0 {
		return renderKeyGroup(keyGroup{
			title: jumpSectionTitle,
			purpose: "usage, pipeline and the plugin catalog open once you are connected " +
				"to an agent.",
		}, false, width, 0)
	}

	// The pane each key opens goes in the KEY column, beside the key, so the four
	// target names align. Putting it at the head of the description instead left
	// "usage", "pipeline", "plugin catalog" and "spend" starting at four different
	// columns, which is the opposite of what a section listing places should do.
	g := keyGroup{title: jumpSectionTitle, bindings: make([]keyBinding, 0, len(jumps))}
	for _, jt := range jumps {
		g.bindings = append(g.bindings, keyBinding{
			keys: jt.key + "  " + jt.name(),
			desc: jt.desc,
		})
	}
	return renderKeyGroup(g, false, width, 0)
}

// renderDrillPath renders the spine as one line, with the pane the reader is on
// marked. The marker is what makes it a map rather than a list: "you are here"
// plus "esc goes left" answers the two questions a lost reader has.
//
// FOUR PANES ARE NOT ON THE SPINE — usage, pipeline, plugin detail and the catalog
// — and they used to get the bare line with no marker at all, which is the one case
// where a reader most needs telling where they are. They are key-opened surfaces
// rather than steps, so there is no position to bracket; they get a sentence naming
// that and naming where esc returns them, which is the fact the missing marker was
// standing in for.
func renderDrillPath(pane paneID, width int) string {
	names := make([]string, 0, len(drillPath))
	onSpine := false
	for _, p := range drillPath {
		name := strings.ToLower(paneName(p))
		if p == pane {
			name = "[" + name + "]"
			onSpine = true
		}
		names = append(names, name)
	}

	out := styleHint.Render(drillSectionTitle) + "\n" +
		proseBlock("  ", strings.Join(names, " → "), width, styleHint)
	if !onSpine {
		out += "\n" + proseBlock("  ", "you are on "+strings.ToLower(paneName(pane))+
			", which sits off this path — esc returns you to the pane that opened it.",
			width, styleHint)
	}
	return out
}

// helpBodyLines builds the scrollable body of the key-help overlay, wrapped to
// width: the active pane in full (emphasized), then where it can go, then the
// spine, then the keys that work anywhere, then the spend drawer's own, then every
// other pane in full. Returned as a single string so a viewport can page it.
//
// ORDERED BY WHAT A LOST READER ASKS FIRST: where am I, where can I go, how do I
// get back, and only then the complete reference. The old body ran active pane →
// GLOBAL → a glyph wall, which answered the last question badly and the first
// three not at all.
//
// The other panes are rendered IN FULL, descriptions and all, rather than
// compacted to their bare keys. `USAGE  m  w  b  s  esc` told a reader that the
// pane has five keys and nothing about what any of them do, which is the defect
// this rewrite exists to fix. It costs roughly forty lines of scroll; the close
// hint and g/G are the reason that is affordable.
//
// The close hint is deliberately NOT included — it lives in the overlay's fixed
// footer so it can't be scrolled out of reach.
func helpBodyLines(pane paneID, width int) string {
	if width < helpMinProseWidth {
		width = helpMinProseWidth
	}
	var sections []string

	if g, ok := paneKeys[pane]; ok {
		sections = append(sections, renderKeyGroup(g, true, width, 0))
	}
	sections = append(sections,
		renderJumpSection(pane, width),
		renderDrillPath(pane, width),
	)
	for _, g := range helpGlobalGroups(pane) {
		sections = append(sections, renderKeyGroup(g, false, width, 0))
	}

	// Every other pane, in full. The active pane is already rendered above.
	others := []string{styleHint.Render(everyPaneTitle)}
	for _, p := range otherPaneOrder {
		if p == pane {
			continue
		}
		g, ok := paneKeys[p]
		if !ok {
			continue
		}
		g.title = paneName(p)
		others = append(others, renderKeyGroup(g, false, width, 2))
	}
	if len(others) > 1 {
		sections = append(sections, strings.Join(others, "\n\n"))
	}

	return strings.Join(sections, "\n\n")
}

// helpBodyWidth is the natural (unwrapped) width of the help body, used
// to size the overlay before the terminal cap is applied.
func helpBodyWidth(body string) int {
	w := 0
	for _, ln := range strings.Split(body, "\n") {
		if lw := lipgloss.Width(ln); lw > w {
			w = lw
		}
	}
	return w
}

// helpOverlayFrameH is the number of rows the overlay frame costs beyond
// the scrollable body: top border, footer hint, bottom border.
const helpOverlayFrameH = 3

// helpViewportSize returns the width and height the help body's viewport
// should occupy for the given terminal size. Height leaves room for the
// frame; both are floored at 1 so a pathologically small terminal still
// renders something rather than panicking inside lipgloss.
func helpViewportSize(width, height, bodyW int) (int, int) {
	frameW := styleBorder.GetHorizontalBorderSize() + helpPadX*2
	w := bodyW
	if max := width - frameW; max > 0 && w > max {
		w = max
	}
	if w < 1 {
		w = 1
	}
	h := height - helpOverlayFrameH
	if h < 1 {
		h = 1
	}
	return w, h
}

// helpPadX is the overlay's horizontal padding inside its border.
const helpPadX = 2

// renderHelpOverlay draws the key-help panel around an already-scrolled
// viewport. The active pane's bindings come first and are emphasized;
// the global group follows; every other pane is summarized underneath so
// the overlay is a complete reference rather than a per-pane cheat sheet.
//
// vp must already hold the body content (see model.syncHelpViewport) —
// this function only frames it, so scroll position is owned by the model
// and survives re-renders.
//
// Returns the panel only — placement over the underlying view is the
// caller's job (see overlayCenter).
func renderHelpOverlay(vp viewport.Model, width, height int, prompt, status string) string {
	// Scroll affordance: only shown when the body doesn't fit, so a
	// terminal tall enough for the whole reference stays uncluttered.
	// Degrades to the bare close hint when the viewport is too narrow for
	// the annotated form — a clipped "clos" is worse than no percentage.
	const closeHint = "[?] or [esc] close"
	hint := closeHint
	if vp.TotalLineCount() > vp.VisibleLineCount() {
		annotated := fmt.Sprintf("[↑↓] scroll  %d%%  ·  %s",
			int(vp.ScrollPercent()*100), closeHint)
		if lipgloss.Width(annotated) <= vp.Width {
			hint = annotated
		}
	}
	// The overlay is drawn over the footer, so its search is drawn here: the prompt in place
	// of the hint while it is open, else the search's status ahead of the hint.
	switch {
	case prompt != "":
		hint = prompt
	case status != "":
		hint = status + "  ·  " + hint
	}
	if lipgloss.Width(hint) > vp.Width {
		if prompt != "" {
			hint = ansi.Truncate(hint, vp.Width, "")
		} else {
			hint = truncToWidth(hint, vp.Width)
		}
	}

	inner := vp.View() + "\n" + styleHint.Render(hint)

	// No Width()/MaxHeight() here: the viewport is already sized to the
	// terminal by helpViewportSize (which reserves helpOverlayFrameH rows
	// for this frame), so the block fits by construction. Setting Width
	// would make lipgloss re-wrap the pre-aligned key columns, and
	// MaxHeight would clip the footer hint that reservation exists to
	// protect. MaxWidth stays as a backstop against a terminal narrower
	// than one padded column.
	box := styleBorder.Padding(0, helpPadX)
	if width > styleBorder.GetHorizontalBorderSize() {
		box = box.MaxWidth(width)
	}
	return box.Render(inner)
}

// padRight pads s with spaces to n display columns. Narrower than n is
// left untouched.
func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// overlayCenter draws overlay centered on top of base, line by line,
// preserving the surrounding view. Both are treated as plain rendered
// blocks; the overlay's lines replace the base's at the computed offset.
//
// ANSI-aware only insofar as lipgloss.Width is used for column math —
// good enough because the overlay fully covers the columns it occupies,
// so no partial-escape splicing happens on the overlay's own rows.
func overlayCenter(base, overlay string, width, height int) string {
	baseLines := strings.Split(base, "\n")
	overLines := strings.Split(overlay, "\n")

	// Pad the base up to the terminal height so a short base view still
	// gets a centered overlay rather than one pinned to the top.
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}

	overH := len(overLines)
	overW := 0
	for _, l := range overLines {
		if w := lipgloss.Width(l); w > overW {
			overW = w
		}
	}

	top := (len(baseLines) - overH) / 2
	if top < 0 {
		top = 0
	}
	left := (width - overW) / 2
	if left < 0 {
		left = 0
	}
	// Never let centering push the panel past the right edge: if the panel
	// is wider than the terminal, render it flush-left and let MaxWidth in
	// renderHelpOverlay have already bounded it.
	if left+overW > width {
		left = width - overW
		if left < 0 {
			left = 0
		}
	}

	out := make([]string, len(baseLines))
	copy(out, baseLines)
	for i, ol := range overLines {
		row := top + i
		if row >= len(out) {
			break
		}
		// Rebuild the row as: base prefix (left columns) + overlay line.
		// Anything the overlay covers to the right is dropped — the panel
		// is opaque, and reconstructing a styled tail past an ANSI-laden
		// prefix is not worth the complexity for a modal.
		prefix := truncToWidth(out[row], left)
		if w := lipgloss.Width(prefix); w < left {
			prefix += strings.Repeat(" ", left-w)
		}
		out[row] = prefix + ol
	}
	return strings.Join(out, "\n")
}

// truncToWidth clips a possibly-ANSI-styled line to n display columns,
// closing any open style with a reset so the overlay that follows starts
// clean. Escape sequences are copied through without counting toward the
// width budget.
func truncToWidth(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	var b strings.Builder
	w := 0
	sawEscape := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// Copy the whole escape sequence verbatim (through the final
			// byte of a CSI sequence, or a single byte otherwise).
			j := i + 1
			for j < len(s) && !isCSITerminator(s[j]) {
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			sawEscape = true
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := lipgloss.Width(string(r))
		if w+rw > n {
			break
		}
		b.WriteString(s[i : i+size])
		w += rw
		i += size
	}
	if sawEscape {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// isCSITerminator reports whether b ends an ANSI CSI escape sequence.
func isCSITerminator(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}
