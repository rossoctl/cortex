package catalog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Every entry Cortex ships loads, and adding a script is adding a folder: this
// test covers each one without being told about it.
func TestCatalog_EveryEntryIsInstallable(t *testing.T) {
	entries, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the catalog is empty")
	}
	for _, e := range entries {
		t.Run(e.Name, func(t *testing.T) {
			if _, ok := e.Files[entryFile]; ok {
				t.Errorf("%s is in Files; it is metadata, not part of the slot", entryFile)
			}
			for _, name := range e.Scripts() {
				if !bytes.HasPrefix(e.Files[name], []byte("#!")) {
					t.Errorf("%s does not start with #!, so it cannot run as a script", name)
				}
			}
		})
	}
}

// What Python writes beside a script is not part of the entry, so a test run in
// a checkout cannot break the catalog the binary embeds.
func TestCatalog_SkipsWhatPythonGenerates(t *testing.T) {
	for _, name := range []string{"__pycache__", "x.cpython-312.pyc"} {
		if !generated(name) {
			t.Errorf("%s is not skipped", name)
		}
	}
	for _, name := range []string{"moca-sandbox.py", "_helper.py", "entry.yaml"} {
		if generated(name) {
			t.Errorf("%s is skipped", name)
		}
	}
}

// moca-sandbox is the entry moca installs, so its name and slot settings are a
// contract.
func TestCatalog_MocaSandbox(t *testing.T) {
	entries, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name != "moca-sandbox" {
			continue
		}
		if !e.FailClosed || e.Timeout != "" {
			t.Errorf("moca-sandbox slot = fail_closed %v, timeout %q; want fail_closed and the default timeout", e.FailClosed, e.Timeout)
		}
		if got := e.Scripts(); len(got) != 1 || got[0] != "moca-sandbox.py" {
			t.Errorf("scripts = %v", got)
		}
		return
	}
	t.Fatal("no moca-sandbox entry")
}

// Each script sends a valid handshake when run with the SDK on its path. Skipped
// where there is no python3, as TestPythonScriptThroughTheSDK is.
func TestCatalog_EveryScriptHandshakes(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 on PATH")
	}
	// The SDK, from its source in the repo: this package must not import the
	// plugin that embeds it.
	sdk, err := filepath.Abs(filepath.Join("..", "sdk"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		dir := t.TempDir()
		for name, b := range e.Files {
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range e.Scripts() {
			t.Run(e.Name+"/"+name, func(t *testing.T) {
				cmd := exec.Command(py, filepath.Join(dir, name))
				cmd.Env = append(os.Environ(), "PYTHONPATH="+sdk)
				cmd.Stdin = bytes.NewReader(nil)
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer cmd.Process.Kill()
				line := make(chan []byte, 1)
				go func() {
					s := bufio.NewScanner(out)
					if s.Scan() {
						line <- append([]byte(nil), s.Bytes()...)
					}
					close(line)
				}()
				select {
				case b := <-line:
					var hs struct {
						Cortex int      `json:"cortex"`
						Hooks  []string `json:"hooks"`
					}
					if err := json.Unmarshal(b, &hs); err != nil || hs.Cortex != 1 || len(hs.Hooks) == 0 {
						t.Fatalf("handshake %q: %v", b, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("no handshake within 10s")
				}
			})
		}
	}
}
