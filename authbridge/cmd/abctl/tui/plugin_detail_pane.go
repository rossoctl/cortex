package tui

import (
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// showPluginDetail loads the focused plugin into the detail viewport.
// Uses a simple labelled block rather than JSON — the values are short
// and human-readable.
//
// resetScroll says whether this is an OPENING of the pane (true — start at the top)
// or a re-render of what it already shows (false — leave the reader where they are).
// The pane is re-rendered from the pipeline poll for as long as it is open, so that
// Metrics counters tick rather than freezing at whatever they were when it opened,
// and this used to GotoTop every time. Entering the pane fires an immediate fetch as
// well, so a scrolled reader was thrown back to the top about a second after opening
// the pane and then every two seconds after that — anything past the first screenful
// was unreadable. Same shape, and same spelling, as syncHelpViewport's flag.
//
// When m.pipeline is non-nil and the plugin's direction is "inbound" or
// "outbound", the Requires/RequiresAny sections render with ✓/✗
// indicators against the active chain. For catalog-pane invocations
// (no live pipeline / direction), those sections render as informational
// lists without satisfaction status.
func (m *model) showPluginDetail(p *apiclient.PipelinePlugin, resetScroll bool) {
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
	// Metrics section, for plugins implementing pipeline.MetricsProvider.
	// Same always-newline treatment as Config below: the header is drawn
	// whether or not there are rows, so navigating between a plugin that
	// reports counters and one that does not doesn't shift the layout.
	fmt.Fprintln(&b)
	b.WriteString(styleMuted.Render("Metrics:"))
	b.WriteString("\n")
	if len(p.Metrics) == 0 {
		b.WriteString("  (none)\n")
	} else {
		b.WriteString(formatPluginMetrics(p.Metrics))
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
	if resetScroll {
		m.detailVp.GotoTop()
		return
	}
	// Held position, clamped to what the new content and height can show: assigning
	// viewport.Height moves maxYOffset without touching YOffset, and SetContent only
	// clamps against the LINE COUNT — so a re-wrap at a wider terminal, or a refresh
	// whose content shrank, can leave the offset past the bottom and render the body
	// with dead space beneath it. SetYOffset is the clamping setter; a no-op when the
	// offset is already in range. Same three lines as syncHelpViewport.
	m.detailVp.SetYOffset(m.detailVp.YOffset)
}

// livePipelinePlugin re-resolves a plugin against the current pipeline view by
// name and direction, so a refreshed view can be rendered into an already-open
// detail pane. Returns nil for a catalog entry (blank direction), which has no
// counterpart in the active chain.
func (m *model) livePipelinePlugin(want *apiclient.PipelinePlugin) *apiclient.PipelinePlugin {
	if want == nil || m.pipeline == nil || want.Direction == "" {
		return nil
	}
	for _, set := range [][]apiclient.PipelinePlugin{m.pipeline.Inbound, m.pipeline.Outbound} {
		for i := range set {
			if set[i].Name == want.Name && set[i].Direction == want.Direction {
				return &set[i]
			}
		}
	}
	return nil
}
