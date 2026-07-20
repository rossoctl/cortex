package edit

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// FenceMarker delimits the active pipeline subtree (above) from the
// commented templates reference (below) inside the abctl edit tempfile.
// The save path strips everything from this line onward before applying.
//
// The exact bytes matter: detection is a literal line match. Keep them
// in sync with templates_test.go and configmap.go's fence-stripping.
const FenceMarker = "# === ABCTL TEMPLATES BELOW (stripped on save) ==="

// templatesBanner is the prose shown immediately below FenceMarker,
// telling the operator how to use the reference. The leading and
// trailing rule lines bracket the fence visually so the boundary
// scans at-a-glance even after a long pipeline subtree.
//
// All lines are below the fence and get stripped on save. The visual
// padding ABOVE the fence is supplied by RenderTemplates as blank
// lines; StripTemplates trims trailing blank lines so the round-trip
// stays byte-identical.
//
// ASCII-only by design: a previous Unicode-bordered banner triggered
// editor-plugin failures on some operator setups. ASCII works
// everywhere without sacrificing visual hierarchy.
const templatesBanner = `# ====================================================================
#
# Reference: every plugin in the catalog.
#
# To use a template, copy a plugin block from below, paste it above the
# fence into your inbound: or outbound: chain, strip the leading "# "
# from each line, then adjust indentation (the templates use a 6-space
# "- name:" indent -- match whatever your existing plugins use).
#
# This entire section -- from the fence line down -- is stripped before
# the edited buffer is written back to the ConfigMap.
#
# ====================================================================`

// RenderTemplates returns a fence marker followed by a commented YAML
// template block per plugin in the catalog. Returns nil for an empty
// catalog so callers can append unconditionally.
//
// Every emitted line starts with "#" — the templates section is pure
// comments. If the operator deletes the fence marker by accident, the
// templates still parse as comment-only YAML (no semantic effect),
// which is the safe-fallback the plan committed to.
//
// Output starts with two blank lines for visual separation from the
// active pipeline subtree above, then the fence marker, then the
// banner block. The blank lines are safe because StripTemplates trims
// trailing whitespace-only lines after truncating at the fence — so a
// render-then-strip round-trip is byte-identical even with the
// padding. A render that emitted CONTENT before the fence would
// survive strip and surface as a spurious `+` line in the no-changes
// diff, which is why all decoration lives below the fence.
func RenderTemplates(plugins []apiclient.PluginCatalogEntry) []byte {
	if len(plugins) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("\n\n")
	b.WriteString(FenceMarker)
	b.WriteString("\n")
	b.WriteString(templatesBanner)
	b.WriteString("\n")
	for _, p := range plugins {
		renderPluginTemplate(&b, p)
	}
	return []byte(b.String())
}

func renderPluginTemplate(b *strings.Builder, p apiclient.PluginCatalogEntry) {
	b.WriteString("\n# --- ")
	b.WriteString(p.Name)
	b.WriteString(" ---\n")
	if p.Description != "" {
		// Description is single-line (Go struct tags can't carry newlines)
		// so a one-line "# <description>" comment is sufficient.
		b.WriteString("# ")
		b.WriteString(p.Description)
		b.WriteString("\n")
	}

	// Split top-level fields into required vs optional for ordering
	// (required render first inside the config: block). Object fields
	// with required descendants (e.g. identity wrapping identity.type)
	// are treated as effectively required so the parent renders at the
	// top of the block, matching the [REQUIRED] annotation it'll get
	// from renderField.
	var required, optional []apiclient.PluginFieldEntry
	for _, f := range p.Fields {
		if f.Required || hasRequiredDescendant(f) {
			required = append(required, f)
		} else {
			optional = append(optional, f)
		}
	}
	// Header lists every required field anywhere in the schema —
	// including nested sub-fields like identity.type — so operators
	// see the full set of must-fill slots without having to skim
	// the body looking for [REQUIRED] markers inside object blocks.
	if len(p.Fields) > 0 {
		b.WriteString("# Required: ")
		if reqPaths := collectRequiredPaths("", p.Fields); len(reqPaths) == 0 {
			b.WriteString("(none — every field is optional)")
		} else {
			b.WriteString(strings.Join(reqPaths, ", "))
		}
		b.WriteString("\n")
	}

	b.WriteString("#       - name: ")
	b.WriteString(p.Name)
	b.WriteString("\n")
	if len(p.Fields) == 0 {
		b.WriteString("#         # (no configurable fields)\n")
		return
	}
	b.WriteString("#         config:\n")
	for _, f := range required {
		renderField(b, f, "           ")
	}
	for _, f := range optional {
		renderField(b, f, "           ")
	}
}

// hasRequiredDescendant reports whether f or any field beneath it
// carries Required=true. An object field whose own Required tag is
// false but which contains a required leaf (e.g. tokenexchange's
// identity wrapping a required identity.type) is "effectively
// required" — the operator can't legally omit the block, so the
// renderer should show it as [REQUIRED] rather than [optional].
func hasRequiredDescendant(f apiclient.PluginFieldEntry) bool {
	for _, sf := range f.Fields {
		if sf.Required || hasRequiredDescendant(sf) {
			return true
		}
	}
	return false
}

// collectRequiredPaths walks the schema tree and returns dotted paths
// for every required field. Top-level required fields appear as
// `name`; nested ones as `parent.child` (e.g. `identity.type`). Used
// by the per-plugin "Required:" header so operators see the full
// must-fill set including ones buried inside object sub-trees.
func collectRequiredPaths(prefix string, fields []apiclient.PluginFieldEntry) []string {
	var out []string
	for _, f := range fields {
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		if f.Required {
			out = append(out, path)
		}
		if f.Type == "object" && len(f.Fields) > 0 {
			out = append(out, collectRequiredPaths(path, f.Fields)...)
		}
	}
	return out
}

// renderField emits two lines per field for readability:
//
//	#           # [REQUIRED] <description>
//	#           <name>: <placeholder>
//
// or, for optional fields:
//
//	#           # [optional, default=X, enum=a|b] <description>
//	#           <name>: <placeholder>
//
// For nested struct fields with their own sub-fields, the placeholder
// is replaced by a recursive block so operators see required nested
// fields (e.g. identity.type) instead of a misleading "identity: {}".
//
// The bracket prefix scans at the left margin so the operator can tell
// required from optional without parsing inline notes.
//
// indent is the column-prefix for the value line (counted after the
// leading "#"). The default value column is "           " (matching
// "          config:"); recursive calls deepen by two spaces per level.
func renderField(b *strings.Builder, f apiclient.PluginFieldEntry, indent string) {
	// Effective required-ness: a field is required either because its
	// own tag says so, or because it's an object containing a required
	// descendant (omitting the parent block makes the child unsettable).
	effectivelyRequired := f.Required || hasRequiredDescendant(f)

	// Annotation line.
	b.WriteString("#")
	b.WriteString(indent)
	b.WriteString("# ")
	if effectivelyRequired {
		b.WriteString("[REQUIRED]")
	} else {
		var attrs []string
		attrs = append(attrs, "optional")
		if f.Default != "" {
			attrs = append(attrs, fmt.Sprintf("default=%s", f.Default))
		}
		if len(f.Enum) > 0 {
			attrs = append(attrs, "enum="+strings.Join(f.Enum, "|"))
		}
		b.WriteString("[")
		b.WriteString(strings.Join(attrs, ", "))
		b.WriteString("]")
	}
	if f.Description != "" {
		b.WriteString(" ")
		b.WriteString(f.Description)
	}
	b.WriteString("\n")

	// Required field with an enum: still surface the choices, since
	// they're constraint information, not a default note.
	if f.Required && len(f.Enum) > 0 {
		b.WriteString("#")
		b.WriteString(indent)
		b.WriteString("#   choices: ")
		b.WriteString(strings.Join(f.Enum, " | "))
		b.WriteString("\n")
	}

	// Object field with sub-fields: render the sub-tree inline so
	// nested required fields (e.g. identity.type) are visible.
	if f.Type == "object" && len(f.Fields) > 0 {
		b.WriteString("#")
		b.WriteString(indent)
		b.WriteString(f.Name)
		b.WriteString(":\n")
		// Reorder sub-fields so required render first within the
		// nested block, matching top-level convention.
		var req, opt []apiclient.PluginFieldEntry
		for _, sf := range f.Fields {
			if sf.Required {
				req = append(req, sf)
			} else {
				opt = append(opt, sf)
			}
		}
		nestedIndent := indent + "  "
		for _, sf := range req {
			renderField(b, sf, nestedIndent)
		}
		for _, sf := range opt {
			renderField(b, sf, nestedIndent)
		}
		return
	}

	// Value line.
	b.WriteString("#")
	b.WriteString(indent)
	b.WriteString(f.Name)
	b.WriteString(": ")
	b.WriteString(placeholderFor(f))
	b.WriteString("\n")
}

// StripTemplates returns edited with the fence marker and everything
// after it removed. If the fence marker is absent, edited is returned
// unchanged — the safe-fallback contract from the plan: a missing
// fence marker means the operator either deleted it deliberately or
// the catalog wasn't available at fetch time, and either way the
// remaining buffer is what gets applied.
//
// Detection is a strict line match: a line whose first non-whitespace
// content is exactly FenceMarker. Whitespace before the marker is
// tolerated (some editors auto-indent comment lines), but the marker
// itself must be intact. Truncation occurs at the start of that line
// so the trailing newline of the previous line — the active pipeline
// subtree's terminator — survives.
//
// After truncating, any trailing whitespace-only lines are also
// removed. RenderTemplates inserts blank lines BEFORE the fence as
// visual padding; without this trim those blank lines would survive
// strip and surface as spurious `+` diff lines on save-without-changes.
// The previous real line's terminating \n is preserved.
func StripTemplates(edited []byte) []byte {
	target := FenceMarker
	// Walk lines manually rather than splitting; preserves the byte
	// position needed for the truncation cut.
	i := 0
	for i < len(edited) {
		// Find the next line's end.
		end := i
		for end < len(edited) && edited[end] != '\n' {
			end++
		}
		// Strip leading whitespace for the comparison.
		start := i
		for start < end && (edited[start] == ' ' || edited[start] == '\t') {
			start++
		}
		// Exact line match: tolerate a trailing CR (CRLF endings) but
		// reject any other extra content. A prefix match would let
		// "# === ABCTL TEMPLATES BELOW (stripped on save) ===extra"
		// trigger truncation, silently dropping operator edits made
		// on a line that happens to begin with the marker.
		lineEnd := end
		if lineEnd > start && edited[lineEnd-1] == '\r' {
			lineEnd--
		}
		if lineEnd-start == len(target) && string(edited[start:lineEnd]) == target {
			// Truncate at i — the start of this line — discarding the
			// fence and everything after, then trim trailing blank
			// lines so the renderer can prepend visual padding.
			return trimTrailingBlankLines(edited[:i])
		}
		// Advance past the newline.
		i = end + 1
	}
	return edited
}

// trimTrailingBlankLines drops trailing whitespace-only lines from
// out, preserving the terminating newline of the last non-blank line.
// "out" is expected to end with \n (the typical content shape) but
// the function is safe on any input.
func trimTrailingBlankLines(out []byte) []byte {
	for len(out) > 0 && out[len(out)-1] == '\n' {
		// Find the start of the line that ends at len(out)-1.
		prevNL := bytes.LastIndexByte(out[:len(out)-1], '\n')
		lineStart := prevNL + 1 // 0 when no earlier \n
		line := out[lineStart : len(out)-1]
		if !isBlankLine(line) {
			break
		}
		// Drop the blank line; the previous real line's terminating
		// \n (at position prevNL) is at out[lineStart-1] and stays.
		out = out[:lineStart]
	}
	return out
}

// isBlankLine reports whether b contains only horizontal whitespace.
// '\r' counts too: an editor that saves with CRLF line endings would
// otherwise leave a stray "\r\n" line surviving the trim and break
// the byte-identical round-trip on save-without-changes.
func isBlankLine(b []byte) bool {
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\r' {
			return false
		}
	}
	return true
}

// placeholderFor picks the YAML placeholder that goes after the field
// name. Documented defaults take priority for primitive types so the
// operator sees the actual fallback inline; otherwise an empty value
// matching the field's type.
func placeholderFor(f apiclient.PluginFieldEntry) string {
	if f.Default != "" && f.Type != "object" {
		// Quote strings so the line is valid YAML when uncommented.
		if f.Type == "string" {
			return fmt.Sprintf("%q", f.Default)
		}
		return f.Default
	}
	switch f.Type {
	case "string":
		return `""`
	case "int":
		return "0"
	case "bool":
		return "false"
	case "[]string":
		return "[]"
	case "object":
		return "{}"
	}
	return `""`
}
