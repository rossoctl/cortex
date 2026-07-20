package edit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/rossocortex/authbridge/cmd/abctl/apiclient"
)

func TestRenderTemplates_EmptyCatalog(t *testing.T) {
	if got := RenderTemplates(nil); got != nil {
		t.Fatalf("nil catalog should produce nil output, got %q", string(got))
	}
	if got := RenderTemplates([]apiclient.PluginCatalogEntry{}); got != nil {
		t.Fatalf("empty catalog should produce nil output, got %q", string(got))
	}
}

func TestRenderTemplates_FenceMarkerPresent(t *testing.T) {
	out := RenderTemplates([]apiclient.PluginCatalogEntry{{Name: "noop"}})
	if !strings.Contains(string(out), FenceMarker) {
		t.Fatalf("output missing fence marker:\n%s", string(out))
	}
}

func TestRenderTemplates_PluginWithFields(t *testing.T) {
	cat := []apiclient.PluginCatalogEntry{
		{
			Name:        "ibac",
			Description: "Intent-based access control via LLM judge.",
			Fields: []apiclient.PluginFieldEntry{
				{Name: "judge_endpoint", Type: "string", Required: true,
					Description: "Base URL of the LLM judge."},
				{Name: "judge_model", Type: "string", Required: true,
					Description: "Model name."},
				{Name: "timeout_ms", Type: "int", Default: "5000",
					Description: "Per-call timeout."},
				{Name: "unclassified_policy", Type: "string", Default: "passthrough",
					Enum: []string{"passthrough", "judge"},
					Description: "Behavior when no parser claimed the request."},
			},
		},
	}
	out := string(RenderTemplates(cat))

	for _, want := range []string{
		"# --- ibac ---",
		"Intent-based access control via LLM judge.",
		"# Required: judge_endpoint, judge_model",
		"#       - name: ibac",
		"#         config:",
		"# [REQUIRED] Base URL of the LLM judge.",
		`#           judge_endpoint: ""`,
		"# [optional, default=5000] Per-call timeout.",
		"#           timeout_ms: 5000",
		"[optional, default=passthrough, enum=passthrough|judge]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n----\n%s", want, out)
		}
	}

	// Required fields should appear before optional ones.
	reqIdx := strings.Index(out, "judge_endpoint:")
	optIdx := strings.Index(out, "timeout_ms:")
	if reqIdx < 0 || optIdx < 0 {
		t.Fatalf("could not locate fields in output:\n%s", out)
	}
	if reqIdx > optIdx {
		t.Errorf("required fields should render before optional ones; got required at %d, optional at %d", reqIdx, optIdx)
	}
}

func TestRenderTemplates_PluginNoFields(t *testing.T) {
	cat := []apiclient.PluginCatalogEntry{
		{Name: "a2a-parser", Description: "A2A protocol parser."},
	}
	out := string(RenderTemplates(cat))
	if !strings.Contains(out, "# (no configurable fields)") {
		t.Fatalf("expected no-fields hint:\n%s", out)
	}
	if strings.Contains(out, "config:") {
		t.Errorf("plugin without fields shouldn't emit config: line:\n%s", out)
	}
}

func TestRenderTemplates_AllOptionalShowsRequiredNoneHeader(t *testing.T) {
	cat := []apiclient.PluginCatalogEntry{
		{
			Name: "all-opt",
			Fields: []apiclient.PluginFieldEntry{
				{Name: "x", Type: "string"},
			},
		},
	}
	out := string(RenderTemplates(cat))
	if !strings.Contains(out, "# Required: (none — every field is optional)") {
		t.Fatalf("plugin with no required fields should explicitly say so:\n%s", out)
	}
}

func TestRenderTemplates_HeaderListsNestedRequiredPaths(t *testing.T) {
	cat := []apiclient.PluginCatalogEntry{
		{
			Name: "te",
			Fields: []apiclient.PluginFieldEntry{
				{Name: "token_url", Type: "string"},
				{
					Name: "identity", Type: "object",
					Fields: []apiclient.PluginFieldEntry{
						{Name: "type", Type: "string", Required: true},
					},
				},
			},
		},
	}
	out := string(RenderTemplates(cat))
	if !strings.Contains(out, "# Required: identity.type") {
		t.Fatalf("nested required field should appear in header as identity.type:\n%s", out)
	}
}

func TestRenderTemplates_NestedObjectRecurses(t *testing.T) {
	cat := []apiclient.PluginCatalogEntry{
		{
			Name: "te",
			Fields: []apiclient.PluginFieldEntry{
				{
					Name:        "identity",
					Type:        "object",
					Description: "Client credentials.",
					Fields: []apiclient.PluginFieldEntry{
						{Name: "type", Type: "string", Required: true,
							Enum:        []string{"spiffe", "client-secret"},
							Description: "Identity scheme."},
						{Name: "client_id", Type: "string",
							Description: "Inline client id."},
					},
				},
			},
		},
	}
	out := string(RenderTemplates(cat))
	if strings.Contains(out, "identity: {}") {
		t.Errorf("nested object should NOT collapse to {}; got:\n%s", out)
	}
	if !strings.Contains(out, "identity:\n") {
		t.Errorf("nested object should render `identity:` then sub-fields, got:\n%s", out)
	}
	if !strings.Contains(out, "[REQUIRED] Identity scheme.") {
		t.Errorf("nested required field annotation missing:\n%s", out)
	}
	// Object parent of a required leaf should itself read [REQUIRED]
	// even though its own tag isn't required:"true" — operators can't
	// legally omit the block, so [optional] would mislead.
	if !strings.Contains(out, "[REQUIRED] Client credentials.") {
		t.Errorf("parent of required leaf should annotate as [REQUIRED]:\n%s", out)
	}
	// Nested fields should be indented further than the top-level
	// config fields. Top-level uses "#           " (11 spaces);
	// nested adds 2 more.
	if !strings.Contains(out, "#             type:") {
		t.Errorf("nested field should indent by 2 spaces beyond parent:\n%s", out)
	}
}

func TestRenderTemplates_RequiredEnumShowsChoices(t *testing.T) {
	cat := []apiclient.PluginCatalogEntry{
		{
			Name: "p",
			Fields: []apiclient.PluginFieldEntry{
				{Name: "mode", Type: "string", Required: true,
					Enum: []string{"a", "b", "c"}, Description: "Pick one."},
			},
		},
	}
	out := string(RenderTemplates(cat))
	if !strings.Contains(out, "# [REQUIRED] Pick one.") {
		t.Errorf("required field annotation missing:\n%s", out)
	}
	if !strings.Contains(out, "#   choices: a | b | c") {
		t.Errorf("required+enum field should surface choices line:\n%s", out)
	}
}

func TestRenderTemplates_PlaceholderTypes(t *testing.T) {
	cat := []apiclient.PluginCatalogEntry{
		{
			Name: "broker",
			Fields: []apiclient.PluginFieldEntry{
				{Name: "name", Type: "string"},
				{Name: "count", Type: "int"},
				{Name: "flag", Type: "bool"},
				{Name: "items", Type: "[]string"},
				{Name: "nested", Type: "object"},
				{Name: "with_default", Type: "string", Default: "abc"},
			},
		},
	}
	out := string(RenderTemplates(cat))
	for _, want := range []string{
		`name: ""`,
		"count: 0",
		"flag: false",
		"items: []",
		"nested: {}",
		`with_default: "abc"`, // string default should be quoted
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n----\n%s", want, out)
		}
	}
}

func TestFetchCmd_AppendsTemplatesWhenCatalogProvided(t *testing.T) {
	r := func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(fixtureCMYAML), nil
	}
	cat := []apiclient.PluginCatalogEntry{
		{Name: "ibac", Description: "test plugin"},
	}
	cmd := FetchCmd(context.Background(), r, nil, "team1", "email-agent", cat)
	msg := cmd().(FetchedMsg)
	if msg.Err != nil {
		t.Fatalf("FetchCmd err: %v", msg.Err)
	}
	body, err := os.ReadFile(msg.TempPath)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	if !strings.Contains(string(body), FenceMarker) {
		t.Fatalf("tempfile missing fence marker; templates not appended:\n%s", string(body))
	}
	if !strings.Contains(string(body), "# --- ibac ---") {
		t.Fatalf("tempfile missing plugin block:\n%s", string(body))
	}
}

func TestStripTemplates_RemovesFenceAndBelow(t *testing.T) {
	original := "pipeline:\n  inbound:\n    plugins: []\n"
	edited := []byte(original + "\n" + FenceMarker + "\n# --- ibac ---\n# stuff\n")
	got := string(StripTemplates(edited))
	// Trailing blank line(s) before the fence are also stripped — the
	// renderer prepends them as visual padding, so preserving them
	// would create drift on save-without-changes.
	if got != original {
		t.Fatalf("StripTemplates mismatch\nwant: %q\ngot:  %q", original, got)
	}
}

func TestStripTemplates_TrimsMultipleBlankLinesBeforeFence(t *testing.T) {
	original := "pipeline: {}\n"
	edited := []byte(original + "\n\n\n" + FenceMarker + "\nbanner\n")
	got := string(StripTemplates(edited))
	if got != original {
		t.Fatalf("multiple blank lines before fence should all be stripped\nwant: %q\ngot:  %q", original, got)
	}
}

func TestStripTemplates_TrimsWhitespaceOnlyLineBeforeFence(t *testing.T) {
	// A line with just spaces / tabs is also "blank" for trim purposes.
	original := "pipeline: {}\n"
	edited := []byte(original + "  \t  \n" + FenceMarker + "\n")
	got := string(StripTemplates(edited))
	if got != original {
		t.Fatalf("whitespace-only line before fence should be stripped\nwant: %q\ngot:  %q", original, got)
	}
}

func TestStripTemplates_TrimsCRLFBlankLineBeforeFence(t *testing.T) {
	// An editor that normalizes line endings to CRLF on save would
	// leave "\r\n" blank lines above the fence. Without the \r-aware
	// blank check, those would survive the trim and break the
	// byte-identical round-trip the PR is built around.
	original := "pipeline: {}\n"
	edited := []byte(original + "\r\n\r\n" + FenceMarker + "\n")
	got := string(StripTemplates(edited))
	if got != original {
		t.Fatalf("CRLF blank line before fence should be stripped\nwant: %q\ngot:  %q", original, got)
	}
}

func TestStripTemplates_NoFenceReturnsUnchanged(t *testing.T) {
	in := []byte("pipeline:\n  inbound:\n    plugins: []\n")
	got := StripTemplates(in)
	if string(got) != string(in) {
		t.Fatalf("input without fence should be unchanged\nwant: %q\ngot:  %q", string(in), string(got))
	}
}

func TestStripTemplates_ToleratesLeadingWhitespace(t *testing.T) {
	original := "pipeline: {}\n"
	edited := []byte(original + "  " + FenceMarker + "\n# stuff\n")
	got := string(StripTemplates(edited))
	if got != original {
		t.Fatalf("leading-whitespace fence not stripped\nwant: %q\ngot:  %q", original, got)
	}
}

func TestStripTemplates_FenceAtEOF(t *testing.T) {
	original := "pipeline: {}\n"
	edited := []byte(original + FenceMarker)
	got := string(StripTemplates(edited))
	if got != original {
		t.Fatalf("fence-at-eof case\nwant: %q\ngot:  %q", original, got)
	}
}

func TestStripTemplates_NotFooledBySimilarLine(t *testing.T) {
	// A YAML comment that mentions the marker as part of prose, not on its own line.
	in := []byte("pipeline:\n  # see " + FenceMarker + " for details\n  inbound: {}\n")
	got := StripTemplates(in)
	if string(got) != string(in) {
		t.Fatalf("inline mention should not match; input should be unchanged")
	}
}

func TestStripTemplates_RequiresExactFenceMatch(t *testing.T) {
	// A line that starts with the marker but has trailing extra content
	// must NOT match — otherwise an operator's edits on that line could
	// be silently truncated.
	original := "pipeline: {}\n"
	in := []byte(original + FenceMarker + " trailing comment\nstill active\n")
	got := StripTemplates(in)
	if string(got) != string(in) {
		t.Fatalf("fence with trailing content should not trigger truncation\nwant: %q\ngot:  %q", string(in), string(got))
	}
}

func TestStripTemplates_AcceptsCRLFFenceLine(t *testing.T) {
	// CRLF line endings on the fence line itself must still match —
	// a trailing CR is the one tolerated suffix.
	original := "pipeline: {}\n"
	in := []byte(original + FenceMarker + "\r\nbanner\r\n")
	got := StripTemplates(in)
	if string(got) != original {
		t.Fatalf("CRLF on fence line should still match\nwant: %q\ngot:  %q", original, string(got))
	}
}

func TestStripTemplates_IntegratesWithRender(t *testing.T) {
	// Round-trip: render templates after a real subtree, strip them,
	// and get the subtree back byte-for-byte. This is the no-changes-
	// diff case: opening + saving without edits must produce the same
	// bytes the operator started with — otherwise the diff prompt
	// misleadingly shows a `+` line and asks to apply.
	subtree := []byte("pipeline:\n  inbound:\n    plugins:\n      - name: ibac\n")
	templates := RenderTemplates([]apiclient.PluginCatalogEntry{
		{Name: "ibac", Description: "test"},
	})
	combined := append([]byte{}, subtree...)
	combined = append(combined, templates...)
	stripped := StripTemplates(combined)
	if string(stripped) != string(subtree) {
		t.Fatalf("round-trip mismatch (would surface as spurious diff)\nwant: %q\ngot:  %q",
			string(subtree), string(stripped))
	}
}

func TestFetchCmd_FetchesCatalogInlineWhenCacheNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/plugins" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plugins":[{"name":"fetched-from-stub"}]}`))
	}))
	defer srv.Close()
	client := apiclient.New(srv.URL)
	r := func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(fixtureCMYAML), nil
	}
	cmd := FetchCmd(context.Background(), r, client, "team1", "email-agent", nil)
	msg := cmd().(FetchedMsg)
	if msg.Err != nil {
		t.Fatalf("FetchCmd err: %v", msg.Err)
	}
	body, err := os.ReadFile(msg.TempPath)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	if !strings.Contains(string(body), "fetched-from-stub") {
		t.Fatalf("inline-fetched catalog not rendered:\n%s", string(body))
	}
	if msg.Catalog == nil {
		t.Fatal("FetchedMsg.Catalog should be set when fetched inline (so the TUI can cache it)")
	}
}

func TestFetchCmd_FetcherErrorIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := apiclient.New(srv.URL)
	r := func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(fixtureCMYAML), nil
	}
	cmd := FetchCmd(context.Background(), r, client, "team1", "email-agent", nil)
	msg := cmd().(FetchedMsg)
	if msg.Err != nil {
		t.Fatalf("catalog-fetch failure should not break edit: %v", msg.Err)
	}
	body, err := os.ReadFile(msg.TempPath)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	if strings.Contains(string(body), FenceMarker) {
		t.Fatalf("templates should be absent when fetcher errored:\n%s", string(body))
	}
}

func TestFetchCmd_NoTemplatesWhenCatalogNil(t *testing.T) {
	r := func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte(fixtureCMYAML), nil
	}
	cmd := FetchCmd(context.Background(), r, nil, "team1", "email-agent", nil)
	msg := cmd().(FetchedMsg)
	if msg.Err != nil {
		t.Fatalf("FetchCmd err: %v", msg.Err)
	}
	body, err := os.ReadFile(msg.TempPath)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	if strings.Contains(string(body), FenceMarker) {
		t.Fatalf("tempfile should not contain fence marker when catalog is nil:\n%s", string(body))
	}
}
