package tui

import (
	"fmt"
	"strings"

	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/apiclient"
)

// showPluginDetail loads the focused plugin into the detail viewport.
// Uses a simple labelled block rather than JSON — the values are short
// and human-readable.
//
// When m.pipeline is non-nil and the plugin's direction is "inbound" or
// "outbound", the Requires/RequiresAny sections render with ✓/✗
// indicators against the active chain. For catalog-pane invocations
// (no live pipeline / direction), those sections render as informational
// lists without satisfaction status.
func (m *model) showPluginDetail(p *apiclient.PipelinePlugin) {
	m.detailPlugin = p
	counts := m.countEventsPerPlugin()
	chain := sameDirectionChain(p, m.pipeline)

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", styleTitle.Render("Plugin:"), p.Name)
	if p.Description != "" {
		fmt.Fprintf(&b, "%s\n", styleHint.Render(p.Description))
	}
	fmt.Fprintln(&b)
	if p.Direction != "" {
		fmt.Fprintf(&b, "%s %s\n", styleMuted.Render("Direction:"), p.Direction)
	}
	if p.Position > 0 {
		fmt.Fprintf(&b, "%s %d\n", styleMuted.Render("Position: "), p.Position)
	}
	body := "no"
	if p.ReadsBody {
		body = "yes"
	}
	fmt.Fprintf(&b, "%s %s\n", styleMuted.Render("Body:     "), body)
	if p.Position > 0 {
		fmt.Fprintf(&b, "%s %d events in cached sessions\n", styleMuted.Render("Activity: "), counts[p.Name])
	}

	// Dependency sections. Render only when the plugin declares them.
	if len(p.Requires) > 0 {
		fmt.Fprintln(&b)
		b.WriteString(styleMuted.Render("Requires:"))
		b.WriteString("\n")
		for _, c := range requiresStatus(p, chain) {
			b.WriteString("  ")
			b.WriteString(formatDepCheck(c, chain != nil))
			b.WriteString("\n")
		}
	}
	if len(p.RequiresAny) > 0 {
		fmt.Fprintln(&b)
		b.WriteString(styleMuted.Render("Requires any of:"))
		b.WriteString("\n")
		for _, c := range requiresAnyStatus(p, chain) {
			b.WriteString("  ")
			b.WriteString(formatDepCheck(c, chain != nil))
			b.WriteString("\n")
		}
	}
	fmt.Fprintln(&b)
	// Always-newline format keeps the visual layout consistent whether
	// the plugin is Configurable (JSON body, multi-line) or not ("(none)",
	// single line). Earlier inline-(none) variant caused jitter when
	// navigating between plugins with and without config.
	b.WriteString(styleMuted.Render("Config:"))
	b.WriteString("\n")
	if len(p.Config) == 0 {
		b.WriteString("  (none)\n")
	} else {
		b.WriteString(ColorizeJSONBytes(p.Config))
		b.WriteString("\n")
	}

	m.detailVp.SetContent(b.String())
	m.detailVp.GotoTop()
}
