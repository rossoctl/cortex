package tui

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Minimal JSON pretty-printer with lipgloss styling. Takes any value and
// emits indented, colorized text. Used by the detail pane to render a
// SessionEvent. Stdlib-only — no deps on go-prettyjson / colorjson etc.
//
// Style choices mirror common editor palettes: keys in one color, strings
// another, numbers/booleans/null each distinct so the eye can scan a dense
// payload quickly.
var (
	styleJSONKey    = lipgloss.NewStyle().Foreground(colorAccent)
	styleJSONString = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#047857", Dark: "#6EE7B7"})
	styleJSONNumber = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"})
	styleJSONBool   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"})
	styleJSONNull   = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
	styleJSONPunct  = lipgloss.NewStyle().Foreground(colorMuted)
)

// ColorizeJSON takes a Go value (typically something decoded from json) and
// returns an indented, colorized string, every fold open. A multi-line string
// renders as an indented block rather than one quoted line — see detail_tree.go.
func ColorizeJSON(v any) string {
	lines := renderTree(buildTree(v, "", "", false), foldState{all: true})
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return strings.Join(out, "\n")
}

// ColorizeJSONBytes parses data then colorizes. Falls back to a muted raw
// string on parse failure so the caller always gets something renderable.
func ColorizeJSONBytes(data []byte) string {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return styleJSONPunct.Render(string(data))
	}
	return ColorizeJSON(v)
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	}
	return 0
}
