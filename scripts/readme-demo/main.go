package main

// readme-demo generates the animated SVG on the cortex README.
//
//	go run .                 # regenerate docs/assets/cortex-demo.svg
//	go run . -out /tmp/x.svg # write somewhere else
//	go run . -stills         # regenerate the still screens in docs/assets/agentop/
//
// The agentop screens are captured from the real TUI (tuicapture.go); the typed
// terminal acts are authored in demo.yaml. Nothing reads the operator's own data:
// HOME is redirected to a scratch directory for the whole run.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	script := flag.String("script", "demo.yaml", "storyboard to render")
	out := flag.String("out", filepath.Join("..", "..", "docs", "assets", "cortex-demo.svg"),
		"SVG to write")
	stills := flag.Bool("stills", false, "render stills.yaml: one still SVG for each beat, into -outdir")
	outDir := flag.String("outdir", stillsDir, "directory for -stills")
	flag.Parse()

	var err error
	if *stills {
		err = runStills("stills.yaml", *outDir)
	} else {
		err = run(*script, *out)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "readme-demo: %v\n", err)
		os.Exit(1)
	}
}

func run(scriptPath, outPath string) error {
	s, err := Load(scriptPath)
	if err != nil {
		return err
	}
	return inScratch(func(home, ledger string) error {
		states, err := buildStates(s, home, ledger)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		f, err := os.Create(outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := EmitSVG(f, states, s.Total, s.Grid); err != nil {
			return err
		}
		if info, err := f.Stat(); err == nil {
			fmt.Printf("wrote %s (%d states, %s, %.0f KB)\n",
				outPath, len(states), s.Total, float64(info.Size())/1024)
		}
		return nil
	})
}

// inScratch runs fn with HOME pointed at a scratch directory, and with a scratch
// directory for the cost ledger.
func inScratch(fn func(home, ledger string) error) error {
	// A scratch HOME for the whole run. The generator must never read the
	// operator's Claude Code history or cortex state, and redirecting HOME is
	// what lets it exercise the real title-loading path without doing so.
	home, err := os.MkdirTemp("", "readme-demo-home")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	prevHome, hadHome := os.LookupEnv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		return err
	}
	defer func() {
		if hadHome {
			_ = os.Setenv("HOME", prevHome)
			return
		}
		_ = os.Unsetenv("HOME")
	}()

	ledger, err := os.MkdirTemp("", "readme-demo-ledger")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ledger)
	return fn(home, ledger)
}

// buildStates dispatches each act to its producer and lays them end to end on one
// timeline.
func buildStates(s *Script, home, ledgerDir string) ([]State, error) {
	var (
		states []State
		at     time.Duration
	)
	for i, a := range s.Acts {
		var (
			produced []State
			err      error
		)
		switch a.Kind {
		case "shell":
			produced, err = ShellStates(a, at, s.Grid)
		case "windows":
			produced, err = WindowStates(a, at, s.Grid)
		case "tui":
			produced, err = TUIStates(a, at, s.Grid, home, filepath.Join(ledgerDir, fmt.Sprint(i)))
		default:
			err = fmt.Errorf("unknown kind %q", a.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("act %d (%q): %w", i, a.Name, err)
		}
		states = append(states, produced...)
		at += a.Runtime
	}
	if len(states) > 0 {
		states[len(states)-1].Final = true
	}
	return states, nil
}
