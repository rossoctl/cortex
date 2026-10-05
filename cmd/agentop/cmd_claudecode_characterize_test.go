package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/tlsbridge"
)

// These tests pin the output of each `agentop configure claude-code` enable and
// disable path below, byte for byte, as it was before enable and disable were split
// into plan and apply halves. They are deliberately exact: the split is meant to
// move code, not change what anyone sees.

type ccRun struct {
	code        int
	out, errOut string
}

func runEnable(t *testing.T, settings, cfg, state string, yes bool) ccRun {
	t.Helper()
	var out, errb bytes.Buffer
	code := claudeCodeEnable2(settings, cfg, state, yes, &out, &errb)
	return ccRun{code, out.String(), errb.String()}
}

func runDisable(t *testing.T, settings, state, cfg string, yes bool) ccRun {
	t.Helper()
	var out, errb bytes.Buffer
	code := claudeCodeDisable2(settings, state, cfg, yes, &out, &errb)
	return ccRun{code, out.String(), errb.String()}
}

func (r ccRun) check(t *testing.T, code int, out, errOut string) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit = %d, want %d", r.code, code)
	}
	if r.out != out {
		t.Errorf("stdout differs\n got: %q\nwant: %q", r.out, out)
	}
	if r.errOut != errOut {
		t.Errorf("stderr differs\n got: %q\nwant: %q", r.errOut, errOut)
	}
}

// caNotes is what enable prints while the CA files do not exist yet.
func caNotes(caDir string) string {
	return caCertNote(caDir) + bundleNote(caDir)
}

// caCertNote is the note enable prints while ca.crt does not exist yet.
func caCertNote(caDir string) string {
	return fmt.Sprintf("Note: %s does not exist yet.\n"+
		"  Cortex creates it on first start. Until then Claude Code cannot verify the\n"+
		"  bridge and every request tunnels through unparsed — which looks like nothing\n"+
		"  is wrong. Start Cortex, then check with: agentop configure claude-code status\n\n",
		filepath.Join(caDir, "ca.crt"))
}

// bundleNote is the note enable prints while the trust bundle does not exist yet.
func bundleNote(caDir string) string {
	return fmt.Sprintf("Note: %s does not exist yet.\n"+
		"  Cortex assembles it on start from the CA plus this machine's root store;\n"+
		"  git, curl and Python read it. If it is still missing after a start, Cortex\n"+
		"  could not find a system root bundle — check the proxy log for \"trust bundle\".\n\n",
		filepath.Join(caDir, tlsbridge.TrustBundleName))
}

// managedInOrder is the managed-key list spelled out by hand, in the order enable
// lists the keys and disable names them.
var managedInOrder = []string{
	"HTTPS_PROXY", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "GIT_SSL_CAINFO",
	"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
}

var allManaged = strings.Join(managedInOrder, ", ")

// enablePreview is everything enable prints from the macOS note through the backup
// line, for a settings file that has none of the managed keys.
func enablePreview(settings, caDir string) string {
	return enablePreviewOf(settings, caDir, managedInOrder)
}

// enablePreviewOf is enablePreview for a settings file that already holds Cortex's
// value for some keys: only keys, which must be in managedInOrder order, are listed.
func enablePreviewOf(settings, caDir string, keys []string) string {
	ca := filepath.Join(caDir, "ca.crt")
	bundle := filepath.Join(caDir, tlsbridge.TrustBundleName)
	value := map[string]string{
		"HTTPS_PROXY":         "http://127.0.0.1:47600",
		"NODE_EXTRA_CA_CERTS": ca,
		"SSL_CERT_FILE":       bundle,
		"GIT_SSL_CAINFO":      bundle,
		"REQUESTS_CA_BUNDLE":  bundle,
		"CURL_CA_BUNDLE":      bundle,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
	s := ""
	if runtime.GOOS == "darwin" {
		s += darwinGoNote(ca)
	}
	s += "Adds to the \"env\" block of " + settings + ":\n"
	for _, k := range keys {
		s += "  " + k + "=" + value[k] + "\n"
	}
	s += "Nothing else in the file changes; a copy is kept as " + settings + ".bak\n\n"
	return s
}

func TestCharacterize_ClaudeCodeEnable_Fresh(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	caDir := filepath.Join(filepath.Dir(cfg), "ca")
	state := filepath.Join(t.TempDir(), "state.json")
	runEnable(t, settings, cfg, state, true).check(t, 0,
		caNotes(caDir)+enablePreview(settings, caDir)+"Enabled — run `claude` as usual.\n", "")
	if _, err := os.Stat(state); err != nil {
		t.Errorf("no state record written: %v", err)
	}
}

// Some keys already hold Cortex's own value: the preview lists only the rest.
func TestCharacterize_ClaudeCodeEnable_Partial(t *testing.T) {
	settings, cfg := fixture(t,
		`{"env":{"HTTPS_PROXY":"http://127.0.0.1:47600","CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`)
	caDir := filepath.Join(filepath.Dir(cfg), "ca")
	caKeys := []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "GIT_SSL_CAINFO", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"}
	runEnable(t, settings, cfg, "", true).check(t, 0,
		caNotes(caDir)+enablePreviewOf(settings, caDir, caKeys)+"Enabled — run `claude` as usual.\n", "")
}

// With both trust files on disk, neither "does not exist yet" note prints.
func TestCharacterize_ClaudeCodeEnable_CAFilesPresent(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	caDir := filepath.Join(filepath.Dir(cfg), "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ca.crt", tlsbridge.TrustBundleName} {
		if err := os.WriteFile(filepath.Join(caDir, name), []byte("placeholder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runEnable(t, settings, cfg, "", true).check(t, 0,
		enablePreview(settings, caDir)+"Enabled — run `claude` as usual.\n", "")
}

// ca.crt present but no trust bundle — a host where Cortex found no root store.
// The two existence checks are independent, so only the bundle note prints.
func TestCharacterize_ClaudeCodeEnable_TrustBundleMissing(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	caDir := filepath.Join(filepath.Dir(cfg), "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caDir, "ca.crt"), []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	runEnable(t, settings, cfg, "", true).check(t, 0,
		bundleNote(caDir)+enablePreview(settings, caDir)+"Enabled — run `claude` as usual.\n", "")
}

func TestCharacterize_ClaudeCodeEnable_ConfigWillNotLoad(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	if err := os.WriteFile(cfg, []byte("listener: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, lerr := config.Load(cfg)
	if lerr == nil {
		t.Fatal("fixture config loaded; the case is not being exercised")
	}
	runEnable(t, settings, cfg, "", true).check(t, 1, "",
		"agentop: reading "+cfg+": "+lerr.Error()+"\n")
}

func TestCharacterize_ClaudeCodeEnable_Declined(t *testing.T) {
	answerPrompt(t, &claudeCodeConfirm, "n\n")
	settings, cfg := fixture(t, settingsWithSecret)
	caDir := filepath.Join(filepath.Dir(cfg), "ca")
	state := filepath.Join(t.TempDir(), "state.json")
	before, _ := os.ReadFile(settings)
	runEnable(t, settings, cfg, state, false).check(t, exitDeclined,
		caNotes(caDir)+enablePreview(settings, caDir)+"Apply? [y/N] Not changed.\n", "")
	if after, _ := os.ReadFile(settings); !bytes.Equal(before, after) {
		t.Error("a declined enable changed the settings file")
	}
	for _, p := range []string{settings + ".bak", state} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("a declined enable wrote %s", p)
		}
	}
}

// Answering yes at the prompt, rather than passing --yes: the branch where the
// confirmation and the write meet.
func TestCharacterize_ClaudeCodeInteractiveAccept(t *testing.T) {
	t.Run("enable", func(t *testing.T) {
		answerPrompt(t, &claudeCodeConfirm, "y\n")
		settings, cfg := fixture(t, settingsWithSecret)
		caDir := filepath.Join(filepath.Dir(cfg), "ca")
		state := filepath.Join(t.TempDir(), "state.json")
		runEnable(t, settings, cfg, state, false).check(t, 0,
			caNotes(caDir)+enablePreview(settings, caDir)+"Apply? [y/N] Enabled — run `claude` as usual.\n", "")
		if got := readEnv(t, settings)["HTTPS_PROXY"]; got != "http://127.0.0.1:47600" {
			t.Errorf("an accepted enable left HTTPS_PROXY = %q", got)
		}
	})
	t.Run("disable", func(t *testing.T) {
		answerPrompt(t, &claudeCodeConfirm, "y\n")
		settings, cfg := fixture(t, settingsWithSecret)
		state := filepath.Join(t.TempDir(), "state.json")
		runEnable(t, settings, cfg, state, true)
		runDisable(t, settings, state, cfg, false).check(t, 0,
			"This will remove from "+settings+": "+allManaged+"\n\n"+
				"Apply? [y/N] \nDisabled. Claude Code no longer routes through Cortex.\n", "")
		if got, ok := readEnv(t, settings)["HTTPS_PROXY"]; ok {
			t.Errorf("an accepted disable left HTTPS_PROXY = %q", got)
		}
	})
}

func TestCharacterize_ClaudeCodeEnable_AlreadyEnabled(t *testing.T) {
	settings, cfg := fixture(t, settingsWithSecret)
	caDir := filepath.Join(filepath.Dir(cfg), "ca")
	runEnable(t, settings, cfg, "", true)
	runEnable(t, settings, cfg, "", true).check(t, 0,
		caNotes(caDir)+"Already enabled: "+settings+" routes Claude Code through Cortex.\n", "")
}

func TestCharacterize_ClaudeCodeEnable_Refusals(t *testing.T) {
	t.Run("foreign proxy", func(t *testing.T) {
		settings, cfg := fixture(t, `{"env":{"HTTPS_PROXY":"http://corp:3128"}}`)
		runEnable(t, settings, cfg, "", true).check(t, 1, "",
			"agentop: HTTPS_PROXY is already set to \"http://corp:3128\" in "+settings+".\n"+
				"  Refusing to overwrite a value you set. Remove it first, or edit the file by hand.\n")
	})
	t.Run("bridge disabled", func(t *testing.T) {
		settings, cfg := fixture(t, settingsWithSecret)
		b, _ := os.ReadFile(cfg)
		if err := os.WriteFile(cfg, bytes.Replace(b, []byte("mode: enabled"), []byte("mode: disabled"), 1), 0o600); err != nil {
			t.Fatal(err)
		}
		runEnable(t, settings, cfg, "", true).check(t, 1, "",
			"agentop: "+errBridgeDisabled(cfg).Error()+"\n")
	})
	t.Run("broken settings JSON", func(t *testing.T) {
		settings, cfg := fixture(t, `{"env": {`)
		_, rerr := readSettings(settings)
		if rerr == nil {
			t.Fatal("fixture JSON parsed; the case is not being exercised")
		}
		runEnable(t, settings, cfg, "", true).check(t, 1, "", "agentop: "+rerr.Error()+"\n")
	})
}

func TestCharacterize_ClaudeCodeDisable(t *testing.T) {
	t.Run("after enable", func(t *testing.T) {
		settings, cfg := fixture(t, settingsWithSecret)
		state := filepath.Join(t.TempDir(), "state.json")
		runEnable(t, settings, cfg, state, true)
		runDisable(t, settings, state, cfg, true).check(t, 0,
			"This will remove from "+settings+": "+allManaged+"\n\n"+
				"\nDisabled. Claude Code no longer routes through Cortex.\n", "")
		if _, err := os.Stat(state); err == nil {
			t.Error("the state record survived disable")
		}
	})
	t.Run("restores a value the user had", func(t *testing.T) {
		settings, cfg := fixture(t, `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"1"}}`)
		state := filepath.Join(t.TempDir(), "state.json")
		runEnable(t, settings, cfg, state, true)
		runDisable(t, settings, state, cfg, true).check(t, 0,
			"This will remove from "+settings+": "+allManaged+"\n\n"+
				"\nRestored to the value(s) you had before: CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\n"+
				"\nDisabled. Claude Code no longer routes through Cortex.\n", "")
	})
	t.Run("nothing to do", func(t *testing.T) {
		settings, cfg := fixture(t, `{}`)
		runDisable(t, settings, "", cfg, true).check(t, 0,
			"Nothing to do: none of the Cortex variables are set in "+settings+".\n", "")
	})
	t.Run("declined", func(t *testing.T) {
		answerPrompt(t, &claudeCodeConfirm, "n\n")
		settings, cfg := fixture(t, settingsWithSecret)
		runEnable(t, settings, cfg, "", true)
		runDisable(t, settings, "", cfg, false).check(t, exitDeclined,
			"This will remove from "+settings+": "+allManaged+"\n\nApply? [y/N] Not changed.\n", "")
	})
}

// Guards the helpers above against drifting into always-equal: the managed-key list
// they spell out by hand must be the one the code writes.
func TestCharacterize_ManagedKeyListIsCurrent(t *testing.T) {
	if got := strings.Join(managedKeys, ", "); got != allManaged {
		t.Errorf("managedKeys = %q; update managedInOrder", got)
	}
}
