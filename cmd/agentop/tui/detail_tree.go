package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/cmd/agentop/tui/find"
)

// The event detail pane renders an event as a tree, so that the parts of it a reader does not
// need can fold to one line. A request event is mostly its conversation, and the conversation
// is re-sent whole on every turn: one measured event was 216KB, 180KB of it messages and one
// message 147KB. Folding is a view choice only — yank still writes the whole event.

const (
	clipLines  = 5    // a string longer than this many lines shows only these
	clipBytes  = 1024 // or this many bytes, whichever comes first
	labelRunes = 60   // a closed message's label shows this much of its first line
)

type nodeKind int

const (
	kindScalar nodeKind = iota
	kindString
	kindObject
	kindArray
	kindGroup // the messages before this turn's, under one head
)

// node is one JSON value. path names it for the fold state: "inference.messages[3].content".
type node struct {
	kind    nodeKind
	path    string
	key     string
	hasKey  bool
	plain   string // a scalar's or a string's text, unstyled: what a search matches
	text    string // a scalar's text, styled
	kids    []*node
	count   int // elements or keys in the JSON value (a group changes len(kids), not this)
	size    int // the value's JSON length in bytes, near enough to label it
	fold    bool
	defOpen bool
	label   string // a closed head's text, when it is not the count and the size
}

// foldState is what decides which folds render open. open holds the reader's own choices.
// Otherwise a fold opens when query matches inside it, and takes its default when it does
// not. all opens everything and draws no fold markers: the plugin detail pane, and
// ColorizeJSON's callers.
type foldState struct {
	open  map[string]foldChoice
	query string
	all   bool
}

// foldChoice is Enter on a fold. An open holds. A close holds while query is the search in force
// or there is none: a new search opens the closed folds that hold its matches, and no other.
type foldChoice struct {
	open  bool
	query string
}

// treeLine is one rendered line. path is the node the line belongs to. head says Enter on
// this line opens or closes the fold at path.
type treeLine struct {
	text string
	path string
	head bool
	open bool
}

// treeFromJSON builds the detail pane's tree for an event, with its default folds.
func treeFromJSON(data []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	root := buildTree(v, "", "", false)
	foldDefaults(root)
	return root, nil
}

func buildTree(v any, path, key string, hasKey bool) *node {
	n := &node{path: path, key: key, hasKey: hasKey}
	scalar := func(s string, style func(...string) string) {
		n.kind, n.plain, n.text, n.size = kindScalar, s, style(s), len(s)
	}
	switch x := v.(type) {
	case nil:
		scalar("null", styleJSONNull.Render)
	case bool:
		scalar(strconv.FormatBool(x), styleJSONBool.Render)
	case json.Number:
		scalar(string(x), styleJSONNumber.Render)
	case float64:
		scalar(strconv.FormatFloat(x, 'g', -1, 64), styleJSONNumber.Render)
	case int, int64, int32:
		scalar(strconv.FormatInt(toInt64(x), 10), styleJSONNumber.Render)
	case string:
		n.kind, n.plain, n.size = kindString, x, len(x)+2
		n.fold = strings.Count(x, "\n") >= clipLines || len(x) > clipBytes
	case []any:
		n.kind, n.count, n.size = kindArray, len(x), 2+max(len(x)-1, 0)
		for i, e := range x {
			k := buildTree(e, fmt.Sprintf("%s[%d]", path, i), "", false)
			n.kids = append(n.kids, k)
			n.size += k.size
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n.kind, n.count, n.size = kindObject, len(x), 2+max(len(x)-1, 0)
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			c := buildTree(x[k], p, k, true)
			n.kids = append(n.kids, c)
			n.size += len(k) + 3 + c.size
		}
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			raw = []byte("<unrenderable>")
		}
		scalar(string(raw), styleJSONPunct.Render)
	}
	if n.kind == kindArray || n.kind == kindObject {
		n.fold = len(n.kids) > 0 && path != ""
	}
	n.defOpen = n.kind != kindString
	return n
}

func (n *node) child(key string) *node {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.hasKey && k.key == key {
			return k
		}
	}
	return nil
}

func (n *node) str(key string) string {
	if c := n.child(key); c != nil && c.kind == kindString {
		return c.plain
	}
	return ""
}

// foldDefaults closes what is large and seldom the point: the tool manifest, tool results and
// calls, large MCP and A2A bodies, and the messages before this turn's.
func foldDefaults(root *node) {
	inf := root.child("inference")
	for key, label := range map[string]string{"tools": "name", "toolResults": "toolUseId", "toolCalls": "name"} {
		if a := inf.child(key); a != nil && a.fold {
			a.defOpen = false
			for i, e := range a.kids {
				e.defOpen = false
				name := escapeText(e.str(label))
				if name == "" {
					name = fmt.Sprintf("#%d", i)
				}
				e.label = name + " · " + formatBytes(int64(e.size))
			}
		}
	}
	// The exact capture: large, and opened when it is the point.
	if raw := root.child("raw"); raw != nil && raw.fold {
		raw.defOpen = false
	}
	for _, p := range [][2]string{{"mcp", "params"}, {"mcp", "result"}, {"a2a", "parts"}} {
		if c := root.child(p[0]).child(p[1]); c != nil && c.fold && c.size > clipBytes {
			c.defOpen = false
		}
	}
	if msgs := inf.child("messages"); msgs != nil && msgs.kind == kindArray {
		groupEarlier(msgs)
	}
}

// groupEarlier puts the messages before the last assistant message under one closed head. An
// agent loop sends the model's last reply and then the new input, so what follows that reply
// is this turn — read from this event alone, whatever conversation it is part of.
func groupEarlier(msgs *node) {
	k := -1
	for i, e := range msgs.kids {
		if e.str("role") == "assistant" {
			k = i
		}
	}
	if k <= 0 {
		return
	}
	g := &node{kind: kindGroup, path: fmt.Sprintf("%s[<%d]", msgs.path, k), fold: true}
	for i, e := range msgs.kids[:k] {
		repath(e, fmt.Sprintf("%s[%d]", g.path, i))
		e.defOpen = false
		e.label = escapeText(e.str("role")) + " · " + formatBytes(int64(e.size))
		if first := labelLine(e.str("content")); first != "" {
			e.label += " · " + first
		} else if names := labelLine(e.str("toolCallNames")); names != "" {
			e.label += " · " + names
		}
		g.kids = append(g.kids, e)
		g.size += e.size
	}
	g.label = fmt.Sprintf("%d earlier message%s · %s", k, plural(k), formatBytes(int64(g.size)))
	msgs.kids = append([]*node{g}, msgs.kids[k:]...)
}

func repath(n *node, p string) {
	old := n.path
	n.path = p
	for _, k := range n.kids {
		repath(k, p+k.path[len(old):])
	}
}

// labelLine is s's first non-blank line, escaped and cut to labelRunes.
func labelLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			l = escapeText(l)
			if utf8.RuneCountInString(l) > labelRunes {
				l = string([]rune(l)[:labelRunes]) + "…"
			}
			return l
		}
	}
	return ""
}

// escapeText makes event content safe to draw. A block has no quotes, so it must escape what
// strconv.Quote used to: a tool result can carry ANSI sequences, and a raw ESC would reach the
// terminal. A tab becomes four spaces. Every other rune unicode.IsPrint rejects is written as
// strconv.Quote writes it.
func escapeText(s string) string {
	bad := func(r rune) bool { return r == '\t' || !unicode.IsPrint(r) }
	if !strings.ContainsFunc(s, bad) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case bad(r):
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderTree draws root with fs. The root is never a fold.
func renderTree(root *node, fs foldState) []treeLine {
	r := renderer{fs: fs}
	if fs.query != "" {
		r.memo = map[*node]bool{}
	}
	r.value(root, 0, false)
	return r.lines
}

type renderer struct {
	fs    foldState
	memo  map[*node]bool
	lines []treeLine
}

func (r *renderer) emit(text string, n *node, head, open bool) {
	r.lines = append(r.lines, treeLine{text: text, path: n.path, head: head && !r.fs.all, open: open})
}

func (r *renderer) isOpen(n *node) bool {
	if r.fs.all || !n.fold {
		return true
	}
	if c, ok := r.fs.open[n.path]; ok && (c.open || r.fs.query == "" || c.query == r.fs.query || !r.hasMatch(n)) {
		return c.open
	}
	// Not the raw capture: it holds a second copy of the conversation and up to 10 MiB of
	// response, so nearly every query matches in it, and drawing it took over a second a
	// keystroke. It opens when the reader opens it.
	if r.fs.query != "" && n.path != "raw" && r.hasMatch(n) {
		return true
	}
	return n.defOpen
}

// hasMatch says whether the query occurs anywhere in n: its key, its text, its label, or
// any value inside it.
func (r *renderer) hasMatch(n *node) bool {
	if v, ok := r.memo[n]; ok {
		return v
	}
	q := r.fs.query
	m := (n.hasKey && find.Contains(n.key, q)) || find.Contains(n.plain, q) || find.Contains(n.label, q)
	for _, k := range n.kids {
		if m {
			break
		}
		m = r.hasMatch(k)
	}
	r.memo[n] = m
	return m
}

// head is the start of n's first line: its indent, its fold marker and its key. The marker
// hangs in the indent, so a fold's key lines up with its siblings'.
func (r *renderer) head(n *node, indent int, open bool) string {
	pad := strings.Repeat("  ", indent)
	if n.fold && !r.fs.all && indent > 0 {
		m := "▸ "
		if open {
			m = "▾ "
		}
		pad = pad[2:] + styleJSONPunct.Render(m)
	}
	if n.hasKey {
		pad += styleJSONKey.Render(strconv.Quote(n.key)) + styleJSONPunct.Render(": ")
	}
	return pad
}

func comma(c bool) string {
	if c {
		return styleJSONPunct.Render(",")
	}
	return ""
}

func (r *renderer) value(n *node, indent int, more bool) {
	switch n.kind {
	case kindScalar:
		r.emit(r.head(n, indent, true)+n.text+comma(more), n, false, false)
	case kindString:
		r.string(n, indent, more)
	case kindGroup:
		open := r.isOpen(n)
		r.emit(r.head(n, indent, open)+styleJSONPunct.Render(n.label), n, true, open)
		if open {
			for i, k := range n.kids {
				r.value(k, indent+1, i < len(n.kids)-1)
			}
		}
	default:
		r.container(n, indent, more)
	}
}

func (r *renderer) container(n *node, indent int, more bool) {
	ob, cb := "{", "}"
	if n.kind == kindArray {
		ob, cb = "[", "]"
	}
	if len(n.kids) == 0 {
		r.emit(r.head(n, indent, true)+styleJSONPunct.Render(ob+cb)+comma(more), n, false, false)
		return
	}
	if !r.isOpen(n) {
		label := n.label
		if label == "" {
			label = fmt.Sprintf("%s%d%s · %s", ob, n.count, cb, formatBytes(int64(n.size)))
		}
		r.emit(r.head(n, indent, false)+styleJSONPunct.Render(label), n, true, false)
		return
	}
	r.emit(r.head(n, indent, true)+styleJSONPunct.Render(ob), n, n.fold, true)
	for i, k := range n.kids {
		r.value(k, indent+1, i < len(n.kids)-1)
	}
	r.emit(strings.Repeat("  ", indent)+styleJSONPunct.Render(cb)+comma(more), n, n.fold, true)
}

// string draws a one-line string quoted, as it always was, and any other as a block: its key,
// then its lines, indented, escaped and unquoted. A long one is clipped until it is opened.
func (r *renderer) string(n *node, indent int, more bool) {
	s := n.plain
	if !strings.Contains(s, "\n") && len(s) <= clipBytes {
		r.emit(r.head(n, indent, true)+styleJSONString.Render(strconv.Quote(s))+comma(more), n, false, false)
		return
	}
	open := r.isOpen(n)
	shown, lines, rest := s, 0, 0
	if !open {
		shown, lines, rest = clip(s)
	}
	r.emit(r.head(n, indent, open)+styleJSONPunct.Render("│"), n, n.fold, open)
	pad := strings.Repeat("  ", indent+1)
	for _, l := range strings.Split(shown, "\n") {
		r.emit(pad+styleJSONString.Render(escapeText(l)), n, n.fold, open)
	}
	if rest > 0 {
		more := fmt.Sprintf("… %s more", formatBytes(int64(rest)))
		if lines > 0 {
			more = fmt.Sprintf("… %d more line%s · %s", lines, plural(lines), formatBytes(int64(rest)))
		}
		r.emit(pad+styleJSONPunct.Render(more), n, true, false)
	}
}

// clip is the start of s a closed string shows: clipLines lines, cut to clipBytes on a rune
// boundary. It returns how many whole lines and bytes it hides.
func clip(s string) (shown string, lines, rest int) {
	cut := len(s)
	for i, nl := 0, 0; i < len(s); i++ {
		if s[i] == '\n' {
			if nl++; nl == clipLines {
				cut = i
				break
			}
		}
	}
	if cut > clipBytes {
		cut = clipBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
	}
	shown = s[:cut]
	return shown, strings.Count(s, "\n") - strings.Count(shown, "\n"), len(s) - cut
}

// parentPath is the path of the node that holds the node at p: "a.b[2]" → "a.b" → "a" → "".
// A key that itself holds "." or "[" reads as two segments. Only the cursor uses this, to find
// the fold its line went into, and a wrong guess puts the cursor one fold higher.
func parentPath(p string) string {
	if i := strings.LastIndexByte(p, '['); i >= 0 && strings.HasSuffix(p, "]") {
		return p[:i]
	}
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return p[:i]
	}
	return ""
}

// styleDetailCursor marks the event detail pane's cursor line in the gutter.
var styleDetailCursor = lipgloss.NewStyle().Foreground(colorAccent)
