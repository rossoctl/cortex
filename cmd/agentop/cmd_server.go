package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// routerName is the inference-router plugin's registered name: the outbound entry
// every `agentop server` form reads and writes. servers.PluginName, which the TUI's S
// picker writes too.
const routerName = servers.PluginName

const serverUsage = `agentop server — choose the inference server each coding agent's new sessions use

Usage:
  agentop server [--config PATH]
  agentop server add <name> <url> --main WORD --helper WORD [--key-stdin] [--yes] [--config PATH]
  agentop server remove <name> [--config PATH]
  agentop server use <name> --agent <agent> [--config PATH]
  agentop server reset --agent <agent> [--config PATH]

With no action, lists the servers, the model each one's main and helper word
names now, and the agents routed to each.

A server is a gateway, such as LiteLLM, with its own URL and key. Routing is per
agent and opt-in: until "use" gives an agent a server, its traffic goes wherever
the agent sends it. Once it does, the agent's inference goes to the server
wherever the agent addressed it — its provider, Anthropic, OpenCode Zen, Bob's
gateway — so the agent itself needs no setting beyond going through Cortex. A
session stays on the server it started on, so a change applies to new sessions
only, and a proxy restart moves none, since each session's server is kept in
~/.cortex. A conversation that began before routing was set up stays where the
agent sends it.

"add" reads the server's API key at a prompt that does not echo, or from stdin
with --key-stdin; it is never an argument. Adding a name that exists asks before
replacing its URL, key and models (--yes skips the question), which is also how a
key is rotated.

A request goes with the model the agent asked for. Only when the server refuses
that model is it sent again, once and before the agent sees the refusal, with
one of the server's own: the newest model whose name contains --main for a
request with tools, the agent's main work, and the one --helper names for a
request without, such as a title. "add" reads the server's model list and
refuses a word that names none of its models.

Every change is written to ~/.cortex/config.yaml, or to --config PATH, and returns
once the proxy has reloaded it. Nothing restarts. Flags go after the action.
`

// runServer handles the `server` subcommand. Returns the process exit code.
func runServer(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	switch action {
	case "":
		return serverList(args, stdout, stderr)
	case "help":
		fmt.Fprint(stdout, serverUsage)
		return 0
	case "add":
		return serverAdd(args, stdin, stdout, stderr)
	case "remove":
		return serverRemove(args, stdout, stderr)
	case "use":
		return serverUse(args, stdout, stderr)
	case "reset":
		return serverReset(args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agentop server: unknown action %q\n\n", action)
	fmt.Fprint(stderr, serverUsage)
	return 2
}

// serverFlags parses flags and positional arguments in any order, so
// `agentop server use glm --agent claude-code` reads as written; the flag package
// alone stops at the first positional argument and would leave --agent unparsed.
// ok is false when the command should stop, with code its exit status: 0 for a help
// request, which goes to stdout, and 2 for a bad flag.
func serverFlags(flags *flag.FlagSet, args []string, stdout, stderr io.Writer) (pos []string, code int, ok bool) {
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(stdout, serverUsage)
				return nil, 0, false
			}
			fmt.Fprint(stderr, serverUsage)
			return nil, 2, false
		}
		rest := flags.Args()
		if len(rest) == 0 {
			return pos, 0, true
		}
		// Parse consumed a "--": everything after it is positional.
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			return append(pos, rest...), 0, true
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// serverTarget is the config `agentop server` reads and writes, and the stats URL
// whose /reload/status confirms a write — "" when no proxy answers there.
//
// Not localEditTargets, which it otherwise mirrors: that returns no path at all when
// the proxy is down, and a write with no proxy running is still worth making, since
// it applies at the next start. And --config names a file localEditTargets cannot.
func serverTarget(configPath string) (cfg *config.Config, path, statsURL string, err error) {
	if configPath == "" {
		cfg, path, err = localCortexConfig()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", "", errors.New("no Cortex config at ~/.cortex/config.yaml; install Cortex with `agentop setup`, or name a config with --config")
		}
	} else {
		path = configPath
		cfg, err = config.Load(path)
	}
	if err != nil {
		return nil, "", "", err
	}
	if statsURL = dialURL(cfg.Stats.StatsAddress); statsURL != "" && !localStatsUp(statsURL) {
		statsURL = ""
	}
	return cfg, path, statsURL, nil
}

// readRouter is the router entry's config, decoded without the plugin's validation
// so a listing still works on a config the proxy would refuse. present is false
// when the outbound chain has no router entry.
func readRouter(cfg *config.Config) (c routerconfig.Config, present bool, err error) {
	for _, e := range cfg.Pipeline.Outbound.Plugins {
		if e.Name != routerName {
			continue
		}
		if len(e.Config) > 0 {
			if err := json.Unmarshal(e.Config, &c); err != nil {
				return routerconfig.Config{}, true, fmt.Errorf("the %s entry's config: %w", routerName, err)
			}
		}
		return c, true, nil
	}
	return routerconfig.Config{}, false, nil
}

// routerInactive is why the router entry in cfg routes nothing although it is in
// the chain, or "" when it routes: its on_error, worded by servers.Inactive, which
// the TUI's S shares. path is the config file, for the fix.
func routerInactive(cfg *config.Config, path string) string {
	for _, e := range cfg.Pipeline.Outbound.Plugins {
		if e.Name == routerName {
			return servers.Inactive(e.OnError, homeTilde(path))
		}
	}
	return ""
}

// agentsOn lists the agents routed to server, sorted.
func agentsOn(c routerconfig.Config, server string) []string {
	var out []string
	for agent, s := range c.Agents {
		if s == server {
			out = append(out, agent)
		}
	}
	slices.Sort(out)
	return out
}

func serverList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "agentop server: unexpected argument %q; the action comes first: agentop server <action> [flags]\n", pos[0])
		return 2
	}
	cfg, path, _, err := serverTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server: %v\n", err)
		return 1
	}
	if len(c.Servers) == 0 {
		fmt.Fprintln(stdout, "No inference servers yet. Add one:")
		fmt.Fprintln(stdout, "  agentop server add <name> <url>")
		return 0
	}

	// Every row has all four cells, the agents one empty when no agent is routed
	// there: tabwriter aligns a column only across consecutive rows that have it, so
	// a short row would restart the alignment below it. The padding that leaves after
	// the mapping of such a row is trimmed.
	var table strings.Builder
	tw := tabwriter.NewWriter(&table, 0, 0, 3, ' ', 0)
	for _, name := range slices.Sorted(maps.Keys(c.Servers)) {
		s := c.Servers[name]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, servers.Host(s), resolvedWords(s), strings.Join(agentsOn(c, name), ", "))
	}
	tw.Flush()
	for line := range strings.Lines(table.String()) {
		fmt.Fprintln(stdout, strings.TrimRight(line, " \n"))
	}
	fmt.Fprintln(stdout)

	if why := routerInactive(cfg, path); why != "" {
		printCheck(stdout, false, why)
	}
	return 0
}

// resolvedWords is a server's main and helper words with the models they name on its
// list now, read with the server's own key, as the router resolves them. A server
// whose list cannot be read says why rather than showing a model it may not name.
func resolvedWords(s routerconfig.Server) string {
	ids, err := serverModels(s)
	if err != nil {
		return fmt.Sprintf("%s (model list unavailable: %v)", servers.Mapping(s), err)
	}
	named := func(word string) string {
		if m := routerconfig.Resolve(word, ids); m != "" {
			return word + " → " + m
		}
		return word + " → none of its models"
	}
	return "main " + named(s.Main) + " · helper " + named(s.Helper)
}

// serverModels is s's model list. A var so tests can stand in for a server.
var serverModels = func(s routerconfig.Server) ([]string, error) {
	ep, err := routerconfig.ParseURL(s.URL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return routerconfig.FetchModels(ctx, routerconfig.NewModelsClient(), ep, s.Key)
}

// printCheck writes one check as "  ✓ text", wrapped at 80 columns with the
// continuation lines under the text.
func printCheck(w io.Writer, ok bool, text string) {
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	line, words := "  "+mark, 0
	for _, word := range strings.Fields(text) {
		if words > 0 && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > 80 {
			fmt.Fprintln(w, line)
			line = "   "
		}
		line += " " + word
		words++
	}
	fmt.Fprintln(w, line)
}

// homeTilde shows path with the home directory as ~, the way the docs name it.
func homeTilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return path
}
