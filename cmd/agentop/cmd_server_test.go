package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rossoctl/cortex/core/plugins/inferencerouter/routerconfig"
)

// routerBlock is the inference-router entry agentop writes, with two servers and
// claude-code routed to glm.
const routerBlock = `      - name: inference-router
        config:
          servers:
            ete:
              url: https://ete.example.com
              key: sk-ete
              main: opus
              helper: haiku
            glm:
              url: https://glm.example.com:8443
              key: sk-glm
              main: glm
              helper: nemotron
          agents:
            claude-code: glm
`

// closedAddr is a loopback address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// serverEnv gives a test a scratch HOME, so nothing reads or writes the real
// ~/.claude or ~/.cortex, and a config in it whose stats address is statsAddr and
// whose outbound chain ends with router (none when ""). It returns the config's
// path, which sits directly in that HOME: filepath.Dir of it is the scratch home.
//
// Its session API is an address nothing listens on, so no test reaches whatever
// answers on the default :9094 — a port-forward to a cluster, as often as not.
func serverEnv(t *testing.T, statsAddr, router string) string {
	t.Helper()
	return serverEnvWithSessions(t, statsAddr, closedAddr(t), router)
}

// testModels is each test server's model list, by host, as serverModels answers in
// every test: nothing here reaches the network.
var testModels = map[string][]string{
	"ete.example.com":      {"aws/claude-opus-4-8", "aws/claude-opus-5-5", "aws/claude-haiku-4-5"},
	"glm.example.com:8443": {"rits/zai-org/glm-5-3", "rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4"},
}

// anyModels is the list a test server not in testModels answers with.
var anyModels = []string{"claude-opus-5-5", "claude-haiku-4-5", "glm-5-3", "nemotron-3"}

// stubServerModels has serverModels answer from lists for the rest of the test, and
// for a host lists does not name with fallback, or a 401 when fallback is nil.
func stubServerModels(t *testing.T, lists map[string][]string, fallback []string) {
	t.Helper()
	prev := serverModels
	serverModels = func(s routerconfig.Server) ([]string, error) {
		ep, err := routerconfig.ParseURL(s.URL)
		if err != nil {
			return nil, err
		}
		if ids, ok := lists[ep.Host]; ok {
			return ids, nil
		}
		if fallback != nil {
			return fallback, nil
		}
		return nil, errors.New("the server answered 401 to its model list")
	}
	t.Cleanup(func() { serverModels = prev })
}

// serverEnvWithSessions is serverEnv with the config's session API at sessionsAddr.
func serverEnvWithSessions(t *testing.T, statsAddr, sessionsAddr, router string) string {
	t.Helper()
	stubServerModels(t, testModels, anyModels)
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := fmt.Sprintf(`mode: proxy-sidecar
stats:
  address: %s
listener:
  session_api_addr: %s
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: tool-prune
        config:
          remove: []
%s`, statsAddr, sessionsAddr, router)
	path := filepath.Join(home, "cortex.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runServerCmd(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = runServer(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// flat joins s's words with single spaces, so a phrase can be found across the
// line wrapping printCheck applies.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestServer_SaysWhenThereAreNoServers(t *testing.T) {
	path := serverEnv(t, closedAddr(t), "")
	code, out, _ := runServerCmd(t, "", "--config", path)
	if code != 0 || !strings.Contains(out, "No inference servers yet") || !strings.Contains(out, "agentop server add <name> <url>") {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}

func TestServer_ListsEachServerWithItsHostMappingAndAgents(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(out, "\n")
	if got := flat(lines[0]); got != "ete ete.example.com main opus → aws/claude-opus-5-5 · helper haiku → aws/claude-haiku-4-5" {
		t.Errorf("line 1 = %q", got)
	}
	if got := flat(lines[1]); got != "glm glm.example.com:8443 main glm → rits/zai-org/glm-5-3 · "+
		"helper nemotron → rits/nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B-NVFP4 claude-code" {
		t.Errorf("line 2 = %q", got)
	}
	if strings.Index(lines[0], "main") != strings.Index(lines[1], "main") {
		t.Errorf("columns do not line up:\n%s\n%s", lines[0], lines[1])
	}
	if strings.Contains(out, "Claude Code") {
		t.Errorf("the listing checks Claude Code's settings, which capture made unnecessary:\n%s", out)
	}
}

func TestServer_ListsPlainHTTPServersByTheirWholeURL(t *testing.T) {
	path := serverEnv(t, closedAddr(t), strings.Replace(routerBlock, "https://ete.example.com", "http://localhost:4000", 1))
	_, out, _ := runServerCmd(t, "", "--config", path)
	if !strings.Contains(out, "http://localhost:4000") {
		t.Errorf("want the plain-http server listed with its scheme:\n%s", out)
	}
}

func TestServer_HelpGoesToStdoutAndAWrongActionToStderr(t *testing.T) {
	if code, out, _ := runServerCmd(t, "", "--help"); code != 0 || !strings.Contains(out, "agentop server add <name> <url>") {
		t.Errorf("--help: exit %d, stdout:\n%s", code, out)
	}
	if code, _, errOut := runServerCmd(t, "", "rename"); code != 2 || !strings.Contains(errOut, `unknown action "rename"`) {
		t.Errorf("rename: exit %d, stderr:\n%s", code, errOut)
	}
}

func TestServer_AnActionAfterAFlagIsRefusedWithTheFix(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	code, _, errOut := runServerCmd(t, "", "--config", path, "remove", "ete")
	if code != 2 || !strings.Contains(errOut, "the action comes first") {
		t.Errorf("exit %d, stderr:\n%s", code, errOut)
	}
}

// A key pasted into a server URL as user info must not reach the terminal, and
// url.URL.Redacted would not stop it: it masks a password but not a username.
func TestServer_ListsNoURLCredentials(t *testing.T) {
	router := strings.NewReplacer(
		"https://ete.example.com", "https://sk-SECRET-ETE@ete.example.com/sk-SECRET-PATH",
		"https://glm.example.com:8443", "https://user:sk-SECRET-GLM@glm.example.com:8443",
	).Replace(routerBlock)
	path := serverEnv(t, closedAddr(t), router)
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if all := out + errOut; strings.Contains(all, "sk-SECRET") || strings.Contains(all, "user:") {
		t.Errorf("the listing prints URL user info:\n%s%s", out, errOut)
	}
	lines := strings.Split(out, "\n")
	if got := flat(lines[0]); !strings.HasPrefix(got, "ete not a valid URL main") {
		t.Errorf("line 1 = %q, want not a valid URL and nothing of the URL", got)
	}
	if got := flat(lines[1]); !strings.HasPrefix(got, "glm not a valid URL main") {
		t.Errorf("line 2 = %q, want not a valid URL and nothing of the URL", got)
	}
}

// A server no agent is routed to must not end the column block: tabwriter aligns a
// column only across consecutive rows that all have it.
func TestServer_AgentsLineUpAroundAServerWithNone(t *testing.T) {
	path := serverEnv(t, closedAddr(t), `      - name: inference-router
        config:
          servers:
            aaa:
              url: https://a.example.com
              key: sk-a
              main: big
              helper: small
            bbb:
              url: https://b.example.com
              key: sk-b
              main: big
              helper: small
            ccc:
              url: https://c.example.com
              key: sk-c
              main: big
              helper: small
          agents:
            claude-code: aaa
            opencode: ccc
`)
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(out, "\n")
	// Columns, not bytes: the mapping's arrows are multi-byte.
	col := func(line, word string) int {
		i := strings.Index(line, word)
		if i < 0 {
			t.Fatalf("no %q in %q", word, line)
		}
		return utf8.RuneCountInString(line[:i])
	}
	if a, c := col(lines[0], "claude-code"), col(lines[2], "opencode"); a != c {
		t.Errorf("agents start in columns %d and %d:\n%s", a, c, out)
	}
	for i, line := range lines[:3] {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line %d ends in spaces: %q", i+1, line)
		}
	}
}

// A key given as a username that contains '/', '?' or '#' ends the authority
// early, so url.Parse takes the KEY for the host: https://sk-SECRET/x@h.example.com
// parses with Host "sk-SECRET". Nothing of a refused server URL may be shown, and
// no host of an ANTHROPIC_BASE_URL containing '@'.
func TestServer_AKeyTakenForTheHostIsNeverShown(t *testing.T) {
	for _, raw := range []string{
		"https://sk-SECRET/x@h.example.com",
		"https://sk-SECRET?x@h.example.com",
		"https://sk-SECRET#x@h.example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			path := serverEnv(t, closedAddr(t), strings.Replace(routerBlock, "https://ete.example.com", raw, 1))
			code, out, errOut := runServerCmd(t, "", "--config", path)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if all := strings.ToLower(out + errOut); strings.Contains(all, "secret") {
				t.Errorf("prints the key:\n%s%s", out, errOut)
			}
			if got := flat(strings.Split(out, "\n")[0]); !strings.HasPrefix(got, "ete not a valid URL main") {
				t.Errorf("line 1 = %q, want the server listed as not a valid URL, with nothing of the URL", got)
			}
		})
	}
}

// A server whose model list cannot be read shows its words and why, and nothing of
// its URL or key.
func TestServer_SaysWhenAServersModelListIsUnavailable(t *testing.T) {
	path := serverEnv(t, closedAddr(t), routerBlock)
	stubServerModels(t, map[string][]string{"glm.example.com:8443": testModels["glm.example.com:8443"]}, nil)
	code, out, errOut := runServerCmd(t, "", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := "ete ete.example.com main opus · helper haiku (model list unavailable: the server answered 401 to its model list)"
	if got := flat(strings.Split(out, "\n")[0]); got != want {
		t.Errorf("line 1 = %q, want %q", got, want)
	}
	if strings.Contains(out, "sk-ete") {
		t.Errorf("the listing prints a key:\n%s", out)
	}
}
