package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/cmd/abctl/apiclient"
)

// pipelineFetchTimeout bounds the one request this command makes. /v1/pipeline
// describes the composition the proxy already holds in memory — no storage read, no
// aggregation — so this is generous rather than tuned, and shorter than cost's budget
// for that reason. Held under apiclient.HeaderTimeout by
// TestCallerBudgets_FitUnderTheHeaderBackstop.
const pipelineFetchTimeout = 10 * time.Second

const pipelineUsage = `abctl pipeline — the plugin pipeline this proxy is running

Usage:
  abctl pipeline get             the active pipeline, as a table
  abctl pipeline get --json      the same, as JSON for a script
  abctl pipeline get --endpoint URL
                                 ask a specific proxy rather than the local one

Shows the same composition as the viewer's Pipeline pane: every plugin in order,
inbound chain then outbound, with the application between them.

DESCRIPTION is last and unpadded, because descriptions are long enough that a
padded column pushes the table past any normal terminal width. A plugin that
declares none simply ends its row early.

Two columns the pane has are deliberately absent here. EVENTS counts invocations
seen in cached session events, which a one-shot command has none of, and DEPS is a
✓/✗ derived from the chain rather than something the proxy states. --json carries
requires/requiresAny for anything that wants to compute the latter.

Flags:
`

// runPipeline implements `abctl pipeline <action>`.
func runPipeline(args []string, stdout, stderr io.Writer) int {
	// An explicit request for help is a successful answer, so it goes to stdout and
	// exits 0; a missing or wrong action is an error on stderr. Same split as
	// `abctl tools` and `abctl service`.
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprint(stdout, pipelineUsage)
			return 0
		}
	}
	if len(args) == 0 || args[0] != "get" {
		fmt.Fprint(stderr, pipelineUsage)
		return 2
	}

	fs := flag.NewFlagSet("pipeline get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false,
		"emit the pipeline as JSON, in /v1/pipeline's own shape and field names")
	endpoint := fs.String("endpoint", "",
		"session API URL of the proxy (default: the Cortex installed on this machine)")
	fs.Usage = func() {
		fmt.Fprint(stderr, pipelineUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Parse has already written the usage block. Help is not a usage error.
			return 0
		}
		return 2
	}

	target := *endpoint
	if target == "" {
		target = localSessionEndpoint()
	}
	if target == "" {
		fmt.Fprintln(stderr, "abctl pipeline get: no --endpoint given and no local Cortex is configured")
		fmt.Fprintln(stderr, "  is Cortex installed and running? `abctl service status`")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), pipelineFetchTimeout)
	defer cancel()
	view, err := apiclient.New(target).GetPipeline(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "abctl pipeline get: %v\n", err)
		switch {
		case errors.Is(err, apiclient.ErrNotFound):
			// A reachable proxy whose session API was built without the pipeline
			// endpoint. Different problem from a proxy that is down, so a different
			// hint — telling this user to check whether Cortex is running sends them
			// to the one thing that is working.
			fmt.Fprintln(stderr, "  this proxy does not serve /v1/pipeline — it may predate the endpoint")
		default:
			fmt.Fprintln(stderr, "  is Cortex running? `abctl service status`")
		}
		return 1
	}

	if *asJSON {
		return writePipelineJSON(view, stdout, stderr)
	}
	writePipelineTable(view, stdout)
	return 0
}

// writePipelineJSON emits the view in /v1/pipeline's own shape.
//
// The server's struct re-encoded, NOT a spelling of its own: a script reading this and
// a script reading the endpoint should not have to learn two vocabularies for one
// answer. PipelinePlugin.Config is json.RawMessage, so a plugin's config passes through
// byte-for-byte as the proxy redacted it.
func writePipelineJSON(view *apiclient.PipelineView, stdout, stderr io.Writer) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(view); err != nil {
		fmt.Fprintf(stderr, "abctl pipeline get: writing JSON: %v\n", err)
		return 1
	}
	return 0
}

// writePipelineTable renders the human view: one row per plugin, inbound chain then the
// application divider then outbound, with each plugin's config indented beneath its row.
//
// PLAIN TEXT, with no lipgloss styling, unlike the pane this mirrors. The pane knows it
// is drawing to a terminal; this writes to whatever stdout is, which is a pipe often
// enough that colour escapes would land in a file or a diff as noise. The columns are
// padded to a measured width instead, which is the part that survives redirection.
func writePipelineTable(view *apiclient.PipelineView, stdout io.Writer) {
	if len(view.Inbound) == 0 && len(view.Outbound) == 0 {
		// Stated rather than printing a bare header. An empty pipeline is a real and
		// legal configuration — a proxy passing traffic through with no plugins — and a
		// blank table cannot be told from a failed decode.
		fmt.Fprintln(stdout, "  (no plugins configured in either direction)")
		return
	}

	// One width for both chains, so the inbound and outbound halves line up under one
	// header rather than reading as two unrelated tables.
	nameW := len("PLUGIN")
	for _, p := range append(append([]apiclient.PipelinePlugin{}, view.Inbound...), view.Outbound...) {
		if n := len(p.Name); n > nameW {
			nameW = n
		}
	}

	fmt.Fprintf(stdout, "  %-2s  %-9s  %-*s  %-4s  %s\n",
		"#", "DIRECTION", nameW, "PLUGIN", "BODY", "DESCRIPTION")
	for _, p := range view.Inbound {
		writePipelineRow(p, nameW, stdout)
	}
	// The application sits between the two chains, which is what makes the ordering
	// readable: inbound plugins run before it, outbound ones after. The pane draws the
	// same divider.
	fmt.Fprintln(stdout, "                 ── (app) ──")
	for _, p := range view.Outbound {
		writePipelineRow(p, nameW, stdout)
	}
}

// writePipelineRow prints one plugin's row, and its config beneath when it has one.
func writePipelineRow(p apiclient.PipelinePlugin, nameW int, stdout io.Writer) {
	body := "no"
	if p.ReadsBody {
		body = "yes"
	}
	// DESCRIPTION runs LAST and is NOT padded, which is the whole reason the table stays
	// readable. Measured across the shipped plugins, descriptions run 14 to 110 characters
	// with a median of 67: pad a column to the longest and every row is 150 columns wide,
	// so the columns that fit in 80 stop fitting in order to accommodate the one that
	// never will. Truncating is the other option and it is worse — a description cut at 30
	// characters is a sentence with its verb removed.
	//
	// What this does NOT do is keep every line inside 80 columns. A long description still
	// runs past it and wraps. The difference is that it wraps ALONE, on the row it belongs
	// to, while #/DIRECTION/PLUGIN/BODY stay aligned down the page and a row without a
	// description stays short.
	//
	// BODY is padded only when a description follows it. Padding unconditionally left a
	// trailing run of spaces on every plugin that declares none — invisible on screen,
	// but it lands in a redirected file and in a diff, and nothing in the repo's hooks
	// inspects a program's output for it.
	fmt.Fprintf(stdout, "  %-2d  %-9s  %-*s  ", p.Position, p.Direction, nameW, p.Name)
	if p.Description == "" {
		fmt.Fprintln(stdout, body)
	} else {
		fmt.Fprintf(stdout, "%-4s  %s\n", body, p.Description)
	}
	if len(p.Config) == 0 {
		return
	}
	// Re-indented rather than printed raw: the proxy sends compact JSON, and one long
	// line per plugin is the shape this command exists to improve on. Failure to indent
	// falls back to the bytes as they arrived — a config is worth showing even when it
	// is not worth reformatting.
	var buf bytes.Buffer
	if err := json.Indent(&buf, p.Config, "        ", "  "); err != nil {
		fmt.Fprintf(stdout, "        %s\n", strings.TrimSpace(string(p.Config)))
		return
	}
	fmt.Fprintf(stdout, "        %s\n", buf.String())
}
