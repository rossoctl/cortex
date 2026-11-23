package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/cmd/agentop/servers"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
	"github.com/rossoctl/cortex/core/session"
)

// serverConfirm asks before `server add` replaces a server. Its own var, as
// claudeCodeConfirm is, so a test stubbing it cannot disarm another command's prompt.
var serverConfirm = confirm

// writePluginConfig is edit.WritePluginConfig. A var so a test can stand in for a
// proxy that stops answering mid-poll, which the real poller takes about 16s of
// backoff to give up on.
var writePluginConfig = edit.WritePluginConfig

// readServerKey reads a server's API key at a prompt that does not echo, on the
// controlling terminal rather than stdin, so it works when stdin is a pipe and the
// prompt is not written into redirected output. A var so tests can stand in for the
// terminal a test process does not have.
var readServerKey = func(name string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to read the key from; pipe it in with --key-stdin")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "API key for %s: ", name)
	b, err := term.ReadPassword(tty.Fd())
	fmt.Fprintln(tty)
	if err != nil {
		return "", fmt.Errorf("reading the key: %w", err)
	}
	return string(b), nil
}

// stdinIsTerminal reports whether stdin is a terminal. A var so a test can say it
// is: a test's stdin never is.
var stdinIsTerminal = func(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// readKey is the key for server name: from stdin with --key-stdin, else at the
// hidden prompt. Never from an argument, which would put it in shell history.
func readKey(name string, fromStdin bool, stdin io.Reader) (string, error) {
	if !fromStdin {
		return readServerKey(name)
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return "", fmt.Errorf("reading the key from stdin: %w", err)
	}
	key := strings.TrimRight(string(b), "\r\n")
	if strings.ContainsAny(key, "\r\n") {
		return "", errors.New("stdin holds more than one line; --key-stdin reads the key alone")
	}
	return key, nil
}

func serverAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server add", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	keyStdin := flags.Bool("key-stdin", false, "read the API key from stdin instead of a prompt")
	yes := flags.Bool("yes", false, "replace an existing server without asking")
	var models routerconfig.Server
	flags.StringVar(&models.Main, "main", "", "a word naming the server's model for an agent's main work, such as opus or glm")
	flags.StringVar(&models.Helper, "helper", "", "a word naming the server's model for an agent's helper calls, such as haiku")
	api, headers := pairFlag{flag: "--api", want: "<protocol>=<path>"}, pairFlag{flag: "--header", want: "<name>=<value>"}
	flags.Var(&api, "api", "a protocol the server serves and its path, such as chat-completions=/v1/chat/completions (repeatable)")
	flags.StringVar(&models.AuthHeader, "auth-header", "", "the header that carries the key, such as x-api-key (default: the agent's)")
	flags.Var(&headers, "header", "a fixed header set on every request to the server, as <name>=<value> (repeatable)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "agentop server add: want a name and a URL: agentop server add <name> <url> --main WORD --helper WORD")
		return 2
	}
	name, rawURL := pos[0], pos[1]
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	if name == ownProvider {
		fmt.Fprintf(stderr, "agentop server add: %q is reserved: agentop server move <session-id> %s sends a session to its own provider\n", name, ownProvider)
		return 2
	}
	// ParseURL's error, never rawURL: a refused URL may carry a key anywhere in it,
	// and ParseURL's errors quote no part of the URL.
	ep, err := routerconfig.ParseURL(rawURL)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	// Before anything is read or asked, like the name and URL: a missing word
	// should not cost the user a pasted key.
	if err := checkWordFlags(models); err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	models.API, models.Headers = api.m, headers.m
	if err := checkProtocolFlags(models); err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 2
	}
	// Before anything is asked or read: --key-stdin reads stdin as typed, so on a
	// terminal the key would echo, character by character, onto the screen and into
	// any recording of it. The prompt without the flag reads it without echo.
	if *keyStdin && stdinIsTerminal(stdin) {
		fmt.Fprintln(stderr, "agentop server add: --key-stdin reads the key from stdin, and stdin is a terminal, "+
			"so the key would echo as you typed it. Pipe the key in, or drop --key-stdin to be asked for it without echo.")
		return 1
	}

	cfg, path, statsURL, err := serverWriteTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	old, replacing := c.Servers[name]
	if replacing {
		// The whole server is replaced, models included, so the question says what it
		// will map to: re-adding it without the flags drops its mapping.
		fmt.Fprintf(stdout, "%s is already configured (%s). Replacing it gives it this URL, key and models (%s), "+
			"and the sessions on it use them from their next request.\n", name, servers.Host(old), servers.Mapping(models))
		if !*yes && !serverConfirm(stdout) {
			return exitDeclined
		}
	}

	key, err := readKey(name, *keyStdin, stdin)
	if err == nil {
		err = routerconfig.CheckKey(key)
	}
	if err == nil && strings.Contains(key, "$") {
		// config.Load expands $NAME and ${NAME} across the whole file, so a literal $
		// in a key would reach the server as something else.
		err = errors.New("the key contains $, which Cortex reads as an environment variable when it loads the config; " +
			"put the key in an environment variable the proxy has, and write key: ${NAME} by hand")
	}
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: %v\n", err)
		return 1
	}
	if ep.PlaintextRemote() {
		fmt.Fprintf(stderr, "agentop server add: warning: %s is plain http on another machine, so requests routed there "+
			"cross the network decrypted, key and prompts included.\n", ep.URL())
	}

	// The words are checked against the server's own list, read with the key just
	// given, so a word naming none of its models is refused before it is written,
	// and a wrong key or URL is caught here rather than on the first routed request.
	models.URL, models.Key = ep.URL(), key
	ids, _, err := serverModels(models)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server add: could not read %s's model list: %v. Check its URL and key.\n", name, err)
		return 1
	}
	for _, w := range []struct{ flag, word string }{{"--main", models.Main}, {"--helper", models.Helper}} {
		m := routerconfig.Resolve(w.word, ids)
		if m == "" {
			fmt.Fprintf(stderr, "agentop server add: %s %s names none of %s's models: %s\n", w.flag, w.word, name, strings.Join(ids, ", "))
			return 1
		}
		fmt.Fprintf(stdout, "%s %s → %s\n", strings.TrimPrefix(w.flag, "--"), w.word, m)
	}
	// With no --api, the probe finds the protocols, and the auth header when it is
	// not the usual one. A given --auth-header is fixed: the probe does not change it.
	if len(models.API) == 0 {
		client := routerconfig.NewModelsClient()
		client.Timeout = 60 * time.Second // a local server can load the model at the first request
		ctx, cancel := context.WithTimeout(context.Background(), probeTotal(client.Timeout))
		api, auth, err := probeServer(ctx, client, models, routerconfig.Resolve(models.Main, ids),
			models.AuthHeader, models.AuthHeader != "")
		cancel()
		if err != nil {
			fmt.Fprintf(stderr, "agentop server add: could not find which protocols %s serves: %v\n", name, err)
			return 1
		}
		for _, p := range slices.Sorted(maps.Keys(api)) {
			fmt.Fprintf(stdout, "api %s → %s\n", p, api[p])
		}
		if auth != "" && auth != models.AuthHeader {
			fmt.Fprintf(stdout, "auth_header → %s\n", auth)
			models.AuthHeader = auth
		}
		models.API = api
	}

	value := edit.MapValue("url", ep.URL(), "key", key, "main", models.Main, "helper", models.Helper)
	if len(models.API) > 0 {
		value.Content = append(value.Content, edit.ScalarValue("api"), sortedMap(models.API))
	}
	if models.AuthHeader != "" {
		value.Content = append(value.Content, edit.ScalarValue("auth_header"), edit.ScalarValue(models.AuthHeader))
	}
	if len(models.Headers) > 0 {
		value.Content = append(value.Content, edit.ScalarValue("headers"), sortedMap(models.Headers))
	}
	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"servers", name},
		Value: value, CreatePlugin: true}
	done := "Added " + name + "."
	if replacing {
		done = "Replaced " + name + "."
	}
	if len(agentsOn(c, name)) == 0 {
		done += " No agent uses it yet: press S on agentop's agents pane, or run\n  agentop server use " + name + " --agent claude-code"
	}
	return runServerWrite(stdout, stderr, path, statsURL, []edit.ConfigChange{ch}, writeReport{
		done:      done,
		unchanged: name + " already has that URL, key and models; nothing to change.",
		note:      routerInactive(cfg, path),
	})
}

// checkWordFlags checks add's --main and --helper: both given, each a word the router
// can match, and no $, which config.Load would expand.
func checkWordFlags(models routerconfig.Server) error {
	// Both at once, so a user who gave neither learns it in one run, not two.
	if models.Main == "" && models.Helper == "" {
		return errors.New("--main and --helper are empty: give a word of each model's name, such as opus and haiku")
	}
	for _, w := range []struct{ flag, word string }{{"--main", models.Main}, {"--helper", models.Helper}} {
		if err := routerconfig.CheckWord(w.word); err != nil {
			return fmt.Errorf("%s %w", w.flag, err)
		}
		if strings.Contains(w.word, "$") {
			return fmt.Errorf("%s contains $, which Cortex reads as an environment variable when it loads the config", w.flag)
		}
	}
	return nil
}

// pairFlag is a repeatable <key>=<value> flag. A second value for one key replaces the first.
type pairFlag struct {
	flag, want string
	m          map[string]string
}

func (f *pairFlag) String() string { return "" }

func (f *pairFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want %s", f.want)
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[k] = v
	return nil
}

// checkProtocolFlags checks add's --api, --auth-header and --header with the rules the
// router loads them by, and refuses a header value with $, which config.Load would expand.
// The errors never repeat a header value.
func checkProtocolFlags(s routerconfig.Server) error {
	if err := routerconfig.CheckAPI(s.API); err != nil {
		return fmt.Errorf("--%w", err)
	}
	if err := routerconfig.CheckAuthHeader(s.AuthHeader); err != nil {
		return fmt.Errorf("--auth-header: %w", err)
	}
	if err := routerconfig.CheckHeaders(s.Headers, s.AuthHeader); err != nil {
		return fmt.Errorf("--header: %w", err)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Headers)) {
		if strings.Contains(s.Headers[name], "$") {
			return fmt.Errorf("--header %s: the value contains $, which Cortex reads as an environment variable when it "+
				"loads the config; write headers: {%s: ${NAME}} by hand", name, name)
		}
	}
	return nil
}

// sortedMap is m as a YAML mapping, its keys sorted, so the same flags write the same file.
func sortedMap(m map[string]string) *yaml.Node {
	var pairs []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		pairs = append(pairs, k, m[k])
	}
	return edit.MapValue(pairs...)
}

func serverRemove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server remove", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	yes := flags.Bool("yes", false, "take the router out with the only server without asking, though running sessions last used it")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "agentop server remove: want a server name: agentop server remove <name>")
		return 2
	}
	name := pos[0]
	// First, as add does: every message below echoes name, and a name CheckName
	// accepts carries nothing a terminal would act on. Its own refusal quotes it.
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverWriteTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server remove: %v\n", err)
		return 1
	}
	if _, ok := c.Servers[name]; !ok {
		fmt.Fprintf(stderr, "agentop server remove: no server named %s%s\n", name, configuredServers(c))
		return 1
	}
	agents := agentsOn(c, name)
	moved := sessionsOn(c, name)
	// The only server takes the router entry out with it — the router needs a
	// server — which undoes the add that created the entry. Not while anything is
	// routed to it, as for any server; the refusal names every step that ends that
	// at once, rather than naming one and refusing again once it is taken.
	if len(c.Servers) == 1 && (len(agents) > 0 || len(moved) > 0) {
		fmt.Fprintf(stderr, "agentop server remove: %s is the only server, so removing it takes the %s entry out of %s. "+
			"First stop routing to it:\n", name, routerName, homeTilde(path))
		for _, a := range agents {
			fmt.Fprintf(stderr, "  agentop server reset --agent %s\n", a)
		}
		for _, id := range moved {
			fmt.Fprintf(stderr, "  agentop server move %s own\n", id)
		}
		fmt.Fprintf(stderr, "Then agentop server remove %s takes it out.\n", name)
		return 1
	}
	// Before the session and agent checks: the config validation refuses the
	// removal while an entry names name, so this is the first thing to fix.
	var routed []string
	for _, k := range slices.Sorted(maps.Keys(c.KeyRoutes)) {
		if k == name || c.KeyRoutes[k] == name {
			routed = append(routed, k)
		}
	}
	if len(routed) > 0 {
		clauses := make([]string, len(routed))
		for i, k := range routed {
			clauses[i] = fmt.Sprintf("key_routes.%s: %s names %s", k, c.KeyRoutes[k], name)
		}
		fmt.Fprintf(stderr, "agentop server remove: %s; remove the entry first:\n", strings.Join(clauses, ", "))
		for _, k := range routed {
			fmt.Fprintf(stderr, "  agentop server reset --key-of %s\n", k)
		}
		return 1
	}
	if len(moved) > 0 {
		// Not rewritten to "": a remove the user asked for should not edit the
		// moves they made, and a config that names no listed server cannot reload —
		// or, with the proxy stopped, start.
		fmt.Fprintf(stderr, "agentop server remove: %d %s moved to %s; stop routing %s with:\n",
			len(moved), sessionWord(len(moved)), name, moved[0])
		for _, id := range moved {
			fmt.Fprintf(stderr, "  agentop server move %s own\n", id)
		}
		return 1
	}
	if len(agents) > 0 {
		var other string
		for _, s := range slices.Sorted(maps.Keys(c.Servers)) {
			if s != name {
				other = s
				break
			}
		}
		fmt.Fprintf(stderr, "%s is %s's server for new sessions. Pick another first:\n", name, strings.Join(agents, " and "))
		for _, a := range agents {
			fmt.Fprintf(stderr, "  agentop server use %s --agent %s\n", other, a)
		}
		return 1
	}
	// Only against a proxy that answered: its session API is the one with the pins.
	sessionsURL := ""
	if statsURL != "" {
		sessionsURL = dialURL(cfg.Listener.SessionAPIAddr)
	}
	n, unread := runningOn(sessionsURL, c, name)
	if len(c.Servers) == 1 {
		return removeLastServer(stdout, stderr, path, statsURL, c, name, n, unread, *yes)
	}
	if n > 0 {
		fmt.Fprintf(stderr, "agentop server remove: warning: %s last sent inference to %s. A session the proxy routed "+
			"there gets an error asking for a new session, until %s is added back.\n", runningSessions(n), name, name)
	}
	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"servers", name}}
	return runServerWrite(stdout, stderr, path, statsURL, []edit.ConfigChange{ch}, writeReport{
		done: "Removed " + name + ".",
		live: fmt.Sprintf("A session the proxy routed to it now gets an error asking for a new session. "+
			"Adding %s back routes them to it again.", name),
	})
}

// removeLastServer takes the inference-router entry out with name, its only server,
// once nothing is routed to it: the inverse of the add that created the entry, so
// add then remove leaves the config as it was.
//
// With the router gone, nothing reads the pins of running sessions, so the running
// sessions that last sent inference there (running, from runningOn) go where their
// agent sends them from their next request — partway through their conversation,
// which is no 503 but is a move. So that case asks first, and so does a proxy whose
// sessions could not be read (unread), which may have some. With none, removing the
// router changes no traffic and asks nothing. The pins stay in the plugin store, so
// adding name back takes those sessions back to it.
//
// The write takes the whole entry out, so it checks the entry is still c, the one
// this decided on: a server added or anything routed since would go with it, and the
// question can wait any length of time.
func removeLastServer(stdout, stderr io.Writer, path, statsURL string, c routerconfig.Config, name string, running int, unread error, yes bool) int {
	why := ""
	switch {
	case unread != nil:
		why = "agentop could not read the proxy's running sessions, so some may have last sent inference to it"
	case running > 0:
		why = runningSessions(running) + " last sent inference to it"
	}
	if why != "" {
		fmt.Fprintf(stdout, "%s is the only server, so removing it takes the %s entry out, and %s. "+
			"With no router, each goes where its agent sends it from its next request, partway through its conversation; "+
			"adding %s back takes it back there.\n", name, routerName, why, name)
		if !yes && !serverConfirm(stdout) {
			return exitDeclined
		}
	}
	unchanged := func(now *config.Config) error {
		if got, _, err := readRouter(now); err != nil || !reflect.DeepEqual(got, c) {
			return fmt.Errorf("the %s entry changed since agentop read it, so nothing was removed; "+
				"run agentop server remove %s again", routerName, name)
		}
		return nil
	}
	ch := edit.ConfigChange{Chain: "outbound", Plugin: routerName, RemoveEntry: true}
	return runServerWriteIf(stdout, stderr, path, statsURL, []edit.ConfigChange{ch}, unchanged, writeReport{
		done: fmt.Sprintf("Removed %s, the only server, and the %s entry with it.", name, routerName),
		live: "Requests now go where each agent sends them.",
	})
}

// serverRename gives server old a new name in one write: the key in place, so the
// server's URL, key — a ${NAME} reference included — models, comments and place in the
// file stay, and every agents:, sessions: and key_routes: entry naming it, without which
// the config would not load.
//
// The router pins a running session to its server by name, and cannot rename its pins.
// So each running session on old is moved with the rename, in the same write, by the
// sessions: entry agentop server move would write: the entry beats the pin, and the pin
// follows it on the session's next request. One that already has an entry follows by the
// rewrite. A session the proxy no longer holds, idle or not resumed since a restart, is
// not seen, and gets the removed server's error if it is resumed.
func serverRename(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server rename", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "agentop server rename: want the server's name and its new one: agentop server rename <name> <new-name>")
		return 2
	}
	old, name := pos[0], pos[1]
	// Both first, as add and remove check theirs: every message below echoes them.
	// The old name is not refused as "own": a server given that name by hand can only
	// be renamed away from it.
	for _, n := range []string{old, name} {
		if err := routerconfig.CheckName(n); err != nil {
			fmt.Fprintf(stderr, "agentop server rename: %v\n", err)
			return 2
		}
	}
	if name == ownProvider {
		fmt.Fprintf(stderr, "agentop server rename: %q is reserved: agentop server move <session-id> %s sends a session to its own provider\n", name, ownProvider)
		return 2
	}
	cfg, path, statsURL, err := serverWriteTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server rename: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server rename: %v\n", err)
		return 1
	}
	if _, ok := c.Servers[old]; !ok {
		fmt.Fprintf(stderr, "agentop server rename: no server named %s%s\n", old, configuredServers(c))
		return 1
	}
	if old == name {
		fmt.Fprintf(stdout, "%s is already called %s; nothing to change.\n", old, name)
		return 0
	}
	if _, ok := c.Servers[name]; ok {
		fmt.Fprintf(stderr, "agentop server rename: there is already a server named %s; remove it first, or pick another name\n", name)
		return 1
	}

	changes := []edit.ConfigChange{{Chain: "outbound", Plugin: routerName, Path: []string{"servers", old}, RenameTo: name}}
	agents := agentsOn(c, old)
	for _, a := range agents {
		changes = append(changes, servers.AgentChanges(a, name)...)
	}
	for _, id := range sessionsOn(c, old) {
		changes = append(changes, servers.SessionChanges(id, name)...)
	}
	changes = append(changes, keyRouteRenames(c, old, name)...)
	done := fmt.Sprintf("Renamed %s to %s.", old, name)
	if len(agents) > 0 {
		done = fmt.Sprintf("Renamed %s to %s. New %s sessions go to %s.", old, name, strings.Join(agents, " and "), name)
	}
	// Only against a proxy that answered: its session API is the one with the pins.
	moving := 0
	var unread error
	if statsURL != "" {
		var ids []string
		ids, unread = runningOnIDs(dialURL(cfg.Listener.SessionAPIAddr), c, old)
		slices.Sort(ids) // the entries in id order, as sessionsOn gives them
		for _, id := range ids {
			switch to, has := c.Sessions[id]; {
			case !has && routerconfig.CheckSessionID(id) == nil:
				changes = append(changes, servers.SessionChanges(id, name)...)
				moving++
			case to == old:
				moving++ // its entry, rewritten above, moves it
			}
		}
	}
	if unread != nil {
		fmt.Fprintf(stderr, "agentop server rename: warning: agentop could not read the proxy's running sessions, so none "+
			"was moved to %s. One the proxy routed to %s gets an error asking for a new session: "+
			"agentop server move <session-id> %s moves it.\n", name, old, name)
	}
	live := ""
	if moving == 1 {
		live = fmt.Sprintf("1 running session on %s moves to %s with it.", old, name)
	} else if moving > 1 {
		live = fmt.Sprintf("%d running sessions on %s move to %s with it.", moving, old, name)
	}
	return runServerWrite(stdout, stderr, path, statsURL, changes, writeReport{
		done: done,
		live: live,
		note: routerInactive(cfg, path),
	})
}

// keyRouteRenames is the changes that make every key_routes: entry naming old, on either
// side, name name instead: a key renamed in place, as the server's is, and a value set.
func keyRouteRenames(c routerconfig.Config, old, name string) []edit.ConfigChange {
	var changes []edit.ConfigChange
	for _, k := range slices.Sorted(maps.Keys(c.KeyRoutes)) {
		from := k
		if k == old {
			changes = append(changes, edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"key_routes", old}, RenameTo: name})
			from = name
		}
		if c.KeyRoutes[k] == old {
			changes = append(changes, edit.ConfigChange{Chain: "outbound", Plugin: routerName, Path: []string{"key_routes", from},
				Value: edit.ScalarValue(name)})
		}
	}
	return changes
}

// runningOn counts the sessions the proxy at sessionsURL holds in memory whose inference
// traffic last went to server name: the ones a remove can leave asking for a new session,
// or, when name is the only server, sends where their agent sends them.
// servers.ForSession names each session's server: by the name the proxy recorded, else by host.
//
// NOT THE LISTENER'S SYNTHETIC SESSIONS — the default bucket and the pending:<agent> ones.
// Each holds many conversations, so the router never pins one; their requests follow the
// agent's current server, which cannot be name, since remove refuses a server an agent is
// routed to. None of them can get the error, whatever host it last sent to.
//
// IN MEMORY, NOT IN THE ARCHIVE: running means a session this proxy has seen a request from.
// One resumed after a restart counts, its host its history's: the router keeps its pins in the
// plugin store, so its pin survived. One only the archive holds has sent nothing since and is
// not running. SessionSummary.Active would not do — it marks the one most recently updated
// session.
//
// 0 and the error when the proxy's session API does not answer. A warning takes that as
// none, the quiet side: the remove itself is what matters, and it goes ahead either way.
// The last server's question takes it as unknown, and asks. With no proxy to ask
// (sessionsURL ""), nothing runs, so that is 0 and no error.
func runningOn(sessionsURL string, c routerconfig.Config, name string) (int, error) {
	ids, err := runningOnIDs(sessionsURL, c, name)
	return len(ids), err
}

// runningOnIDs is the ids of the sessions runningOn counts.
func runningOnIDs(sessionsURL string, c routerconfig.Config, name string) ([]string, error) {
	if sessionsURL == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	list, err := apiclient.New(sessionsURL).ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range list {
		if !session.IsConversation(s.ID) {
			continue
		}
		if on, ok := servers.ForSession(c, s); ok && on == name {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

// sessionsOn is the session ids whose sessions: entry names server, sorted.
func sessionsOn(c routerconfig.Config, server string) []string {
	var out []string
	for id, s := range c.Sessions {
		if s == server {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// sessionWord is "session" or "sessions", for a count.
func sessionWord(n int) string {
	if n == 1 {
		return "session"
	}
	return "sessions"
}

// runningSessions is "1 running session" or "n running sessions".
func runningSessions(n int) string {
	if n == 1 {
		return "1 running session"
	}
	return fmt.Sprintf("%d running sessions", n)
}

func serverUse(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server use", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	agent := flags.String("agent", "", "the agent whose new sessions go to the server, such as claude-code")
	keyOf := flags.String("key-of", "", "the server whose key the new sessions send, such as team-b")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if *agent != "" && *keyOf != "" {
		fmt.Fprintln(stderr, "agentop server use: --agent and --key-of are exclusive; give one")
		return 2
	}
	if len(pos) != 1 || (*agent == "" && *keyOf == "") {
		fmt.Fprintln(stderr, "agentop server use: want a server and an agent or a key: agentop server use <name> --agent <agent> | --key-of <server>")
		return 2
	}
	name := pos[0]
	// First, as add does: see serverRemove.
	if err := routerconfig.CheckName(name); err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 2
	}
	if *keyOf != "" {
		return serverKeyRoute("use", name, *keyOf, *cfgPath, stdout, stderr)
	}
	if err := routerconfig.CheckAgent(*agent); err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 2
	}
	// The canonical spelling, so the key this command writes and the messages it
	// prints name the agent today's requests report, however the user spelled it.
	*agent = pipeline.CanonicalAgent(*agent)
	cfg, path, statsURL, err := serverWriteTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server use: %v\n", err)
		return 1
	}
	if _, ok := c.Servers[name]; !ok {
		fmt.Fprintf(stderr, "agentop server use: no server named %s%s. Add it first:\n  agentop server add %s <url>\n",
			name, configuredServers(c), name)
		return 1
	}
	return runServerWrite(stdout, stderr, path, statsURL, servers.AgentChanges(*agent, name), writeReport{
		done:      fmt.Sprintf("New %s sessions → %s.", *agent, name),
		live:      runningSessionsStay,
		unchanged: fmt.Sprintf("New %s sessions already go to %s.", *agent, name),
		note:      routerInactive(cfg, path),
	})
}

// serverKeyRoute is `agentop server use <to> --key-of <from>`: the new sessions
// that send from's key go to to. With to == from, which is also
// `agentop server reset --key-of <from>`, it removes the entry.
func serverKeyRoute(action, to, from, cfgPath string, stdout, stderr io.Writer) int {
	if err := routerconfig.CheckName(from); err != nil {
		fmt.Fprintf(stderr, "agentop server %s: %v\n", action, err)
		return 2
	}
	cfg, path, statsURL, err := serverWriteTarget(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server %s: %v\n", action, err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server %s: %v\n", action, err)
		return 1
	}
	for _, n := range []string{from, to} {
		if _, ok := c.Servers[n]; !ok {
			fmt.Fprintf(stderr, "agentop server %s: no server named %s%s. Add it first:\n  agentop server add %s <url>\n",
				action, n, configuredServers(c), n)
			return 1
		}
	}
	r := writeReport{
		done:      fmt.Sprintf("New sessions that send %s's key → %s.", from, to),
		live:      runningSessionsStay,
		unchanged: fmt.Sprintf("New sessions that send %s's key already go to %s.", from, to),
		note:      routerInactive(cfg, path),
	}
	if to == from {
		r.done = fmt.Sprintf("New sessions that send %s's key go to %s.", from, from)
		r.unchanged = fmt.Sprintf("%s's key is not routed to another server; nothing to change.", from)
	} else if shared := sameKeyAs(c, from); len(shared) > 0 {
		// The router matches no server by a key that two servers share (buildKeys),
		// so the entry would change nothing.
		fmt.Fprintf(stderr, "Warning: %s has the same key as %s, so a request that sends it selects no server and this entry has no effect.\n",
			strings.Join(shared, " and "), from)
	}
	return runServerWrite(stdout, stderr, path, statsURL, servers.KeyRouteChanges(from, to), r)
}

// sameKeyAs lists, sorted, the servers other than name whose key is name's key.
func sameKeyAs(c routerconfig.Config, name string) []string {
	var out []string
	key := c.Servers[name].Key
	for _, n := range slices.Sorted(maps.Keys(c.Servers)) {
		if n != name && key != "" && c.Servers[n].Key == key {
			out = append(out, n)
		}
	}
	return out
}

func serverReset(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server reset", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	agent := flags.String("agent", "", "the agent to stop routing, such as claude-code")
	keyOf := flags.String("key-of", "", "the server whose key the new sessions send, such as team-b")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if *agent != "" && *keyOf != "" {
		fmt.Fprintln(stderr, "agentop server reset: --agent and --key-of are exclusive; give one")
		return 2
	}
	if len(pos) != 0 || (*agent == "" && *keyOf == "") {
		fmt.Fprintln(stderr, "agentop server reset: want an agent or a key: agentop server reset --agent <agent> | --key-of <server>")
		return 2
	}
	if *keyOf != "" {
		return serverKeyRoute("reset", *keyOf, *keyOf, *cfgPath, stdout, stderr)
	}
	if err := routerconfig.CheckAgent(*agent); err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 2
	}
	// The canonical spelling, for the same reason as use: the check below and the
	// key the reset removes must name the agent the router routes.
	*agent = pipeline.CanonicalAgent(*agent)
	cfg, path, statsURL, err := serverWriteTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server reset: %v\n", err)
		return 1
	}
	if c.Agents[*agent] == "" {
		fmt.Fprintf(stdout, "%s is not routed; nothing to change.\n", *agent)
		return 0
	}
	return runServerWrite(stdout, stderr, path, statsURL, servers.AgentChanges(*agent, ""), writeReport{
		done:      fmt.Sprintf("New %s sessions are no longer routed.", *agent),
		live:      runningSessionsStay,
		unchanged: fmt.Sprintf("%s is not routed; nothing to change.", *agent),
	})
}

// runningSessionsStay is what use and reset mean for sessions already running,
// said once the running proxy has the change. It holds a session by its pin or by
// the request to a server its history records.
const runningSessionsStay = "Sessions already running stay where they are."

// writeReport is what runServerWrite says about a change, by how it landed.
type writeReport struct {
	// done is the change, said once the proxy has it or will load it at start.
	done string
	// live is what the change means for sessions already running. Said only when
	// the running proxy reloaded it.
	live string
	// unchanged is said when there was nothing to write.
	unchanged string
	// note, when set, follows any of those: why the router will route nothing
	// whatever was changed (see routerInactive).
	note string
}

// runServerWrite applies changes to the router entry — one agent's changes can be
// several, a removal per retired spelling plus the canonical set — and reports how
// the write landed.
func runServerWrite(stdout, stderr io.Writer, path, statsURL string, changes []edit.ConfigChange, r writeReport) int {
	return runServerWriteIf(stdout, stderr, path, statsURL, changes, nil, r)
}

// runServerWriteIf is runServerWrite with a precondition on the file as the write reads it
// (edit.ConfigWrite.Precondition).
func runServerWriteIf(stdout, stderr io.Writer, path, statsURL string, changes []edit.ConfigChange,
	precondition func(*config.Config) error, r writeReport) int {
	res, err := writePluginConfig(context.Background(), edit.ConfigWrite{
		Path: path, StatsURL: statsURL, Changes: changes, Verify: servers.Verify, Precondition: precondition,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agentop server: %v\n", err)
		return 1
	}
	say := func(lines ...string) {
		for _, l := range lines {
			if l != "" {
				fmt.Fprintln(stdout, l)
			}
		}
	}
	shown := homeTilde(path)
	switch res.Outcome {
	case edit.WriteUnchanged:
		say(r.unchanged, r.note)
	case edit.WriteReloaded:
		say(r.done, r.live, r.note)
	case edit.WriteNotRunning:
		next := fmt.Sprintf("Written to %s. No Cortex answered at its stats address, so this applies when the proxy next starts.", shown)
		say(r.done, next, r.note)
	case edit.WriteReloadFailed:
		// WritePluginConfig has put the file back, so it does not hold the change;
		// saying it was written would send the user looking for an edit that is not
		// there. Where it could not, it returned an error saying so, handled above.
		fmt.Fprintf(stderr, "agentop server: the change was not applied: the proxy refused it and keeps its previous configuration, "+
			"so %s was put back as it was:\n  %s\n", shown, res.ReloadError)
		return 1
	case edit.WriteStatusUnreachable:
		// Not a refusal: nothing said no. The proxy went away, and whether it took
		// the change before it did is unknown, so the file was put back to what it
		// last ran, which is what it starts from if it was stopped.
		fmt.Fprintf(stderr, "agentop server: the change was not confirmed: the proxy stopped answering at %s before it reported the reload, "+
			"so %s was put back as it was; check the proxy is running and try again:\n  %s\n", statsURL, shown, res.ReloadError)
		return 1
	case edit.WriteReloadTimedOut:
		fmt.Fprintf(stderr, "agentop server: wrote %s, but the proxy reported no reload within %s; check %s/reload/status\n",
			shown, edit.LocalPollDeadline, statsURL)
		return 1
	}
	return 0
}

// configuredServers is "; configured: a, b" for an error naming a missing server,
// or "" when there are none.
func configuredServers(c routerconfig.Config) string {
	if len(c.Servers) == 0 {
		return ""
	}
	return "; configured: " + strings.Join(slices.Sorted(maps.Keys(c.Servers)), ", ")
}

// ownProvider is the word serverMove takes for a session's own provider, so no
// server may be added under it.
const ownProvider = "own"

// serverMove moves one running session to a server, or stops routing it with
// "own". Unlike use and reset, this moves the session the user named, not the
// agent's next one.
func serverMove(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("server move", flag.ContinueOnError)
	cfgPath := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(stderr, "agentop server move: want a session and a server: agentop server move <session-id> <name|own>")
		return 2
	}
	sid, server := pos[0], pos[1]
	if err := routerconfig.CheckSessionID(sid); err != nil {
		fmt.Fprintf(stderr, "agentop server move: %v\n", err)
		return 2
	}
	if server == ownProvider {
		server = ""
	} else if err := routerconfig.CheckName(server); err != nil {
		fmt.Fprintf(stderr, "agentop server move: %v\n", err)
		return 2
	}
	cfg, path, statsURL, err := serverWriteTarget(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server move: %v\n", err)
		return 1
	}
	c, _, err := readRouter(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentop server move: %v\n", err)
		return 1
	}
	if server != "" {
		if _, ok := c.Servers[server]; !ok {
			fmt.Fprintf(stderr, "agentop server move: no server named %s%s. Add it first:\n  agentop server add %s <url>\n",
				server, configuredServers(c), server)
			return 1
		}
	}
	sessionsURL := ""
	if cfg.Listener.SessionAPIAddr != "" {
		sessionsURL = dialURL(cfg.Listener.SessionAPIAddr)
	}
	changes := servers.SessionChanges(sid, server)
	changes = append(changes, pruneFrom(sessionsURL, c, sid)...)
	name := server
	if name == "" {
		name = "its own provider"
	}
	return runServerWrite(stdout, stderr, path, statsURL, changes, writeReport{
		done:      fmt.Sprintf("Session %s → %s.", sid, name),
		live:      moveNote(sessionsURL, sid, server),
		unchanged: fmt.Sprintf("Session %s already goes to %s.", sid, name),
	})
}

// moveNote is what a move costs the session it moves, from the session API's own
// figure: its conversation is re-sent uncached on the first turn at the new
// server. "" when neither the proxy nor the session can be read — the move does
// not depend on it.
func moveNote(sessionsURL, sid, server string) string {
	if sessionsURL == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	list, err := apiclient.New(sessionsURL).ListSessions(ctx)
	if err != nil {
		return ""
	}
	for _, s := range list {
		if s.ID != sid || s.PromptContext == nil {
			continue
		}
		name := server
		if name == "" {
			name = "its own provider"
		}
		return fmt.Sprintf("%d tokens of %s's context go to %s uncached on its first turn there.",
			s.PromptContext.Tokens, sid, name)
	}
	return ""
}

// pruneFrom is the removals for every sessions: override whose session the
// session API no longer lists — inert config a move left behind. Nothing when the
// API cannot be read: a live session must never lose its override to a blind
// prune.
func pruneFrom(sessionsURL string, c routerconfig.Config, moved string) []edit.ConfigChange {
	if sessionsURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// THE ARCHIVED LIST, not the live one: a session the archive holds — evicted
	// from memory, or resumed after a restart — still has its pin, kept in the
	// plugin store, so its override must survive the prune too.
	list, err := apiclient.New(sessionsURL).ListSessionsArchived(ctx)
	if err != nil {
		return nil
	}
	if len(list.Sessions) == 0 {
		return nil
	}
	live := map[string]bool{moved: true}
	for _, s := range list.Sessions {
		live[s.ID] = true
	}
	return servers.PruneSessions(c, live)
}
