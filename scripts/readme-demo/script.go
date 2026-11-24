package main

// script is the storyboard as data. Editing the demo means editing demo.yaml, not
// recompiling the generator.

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Step is one typed command and the output it produces.
type Step struct {
	Cmd string   `yaml:"cmd"`
	Out []string `yaml:"out"`
	// Slow overrides the delay before individual output lines, by index. This is
	// what buys a demo its realism: a 2.5s pause before "Downloading" reads as
	// work being done, where an evenly-paced dump reads as a text file.
	//
	// Durations are strings in YAML ("2500ms") and parsed in normalize: yaml.v3
	// decodes a bare time.Duration as an integer count of nanoseconds, so "2500ms"
	// would be a type error and 2500 would be two and a half microseconds.
	SlowStr []string        `yaml:"slow"`
	Slow    []time.Duration `yaml:"-"`
	// ANSI says Out is what the program writes to a terminal, SGR escapes and all
	// (YAML spells ESC "\e"). Each line is decoded as the agentop screens are, and
	// outputColor's guesses for plain output do not apply.
	ANSI bool `yaml:"ansi"`
	// Bar is the index in Out of a progress bar redrawn in place: its run of ━ fills
	// left to right over that line's delay, and the rest of the line appears once it
	// is full. nil for no bar.
	Bar *int `yaml:"bar"`
}

// WindowSpec is one mini-terminal in the stacked act.
type WindowSpec struct {
	Title string   `yaml:"title"`
	Lines []string `yaml:"lines"`
}

// Beat is one captured agentop screen: the keys that get there, and how long it
// stays on screen.
type Beat struct {
	Label   string        `yaml:"label"`
	Keys    []string      `yaml:"keys"`
	HoldStr string        `yaml:"hold"`
	Hold    time.Duration `yaml:"-"`
	// At is how far into the fixture's replay this beat is captured. Every replayed
	// event up to it reaches the TUI over the live stream before the keys are sent,
	// so the panes show a session that grows from beat to beat. Beats without one
	// keep the previous beat's moment.
	AtStr string        `yaml:"at"`
	At    time.Duration `yaml:"-"`
	// Frames splits the beat into that many captures, evenly spaced from the previous
	// beat's At to this one's and sharing its hold, so a replay plays as motion rather
	// than as a slideshow. Keys go with the first frame only.
	Frames int `yaml:"frames"`
	// Alt describes the screen for a reader who cannot see it. Only stills.yaml uses
	// it: each of its beats is one still image.
	Alt string `yaml:"alt"`
}

// Act is one narrative segment.
type Act struct {
	Name       string        `yaml:"name"`
	Kind       string        `yaml:"kind"` // shell | windows | tui
	RuntimeStr string        `yaml:"runtime"`
	Runtime    time.Duration `yaml:"-"`

	Steps   []Step       `yaml:"steps"`
	Windows []WindowSpec `yaml:"windows"`
	Beats   []Beat       `yaml:"beats"`
	Fixture Fixture      `yaml:"fixture"`
}

// Script is the whole storyboard.
type Script struct {
	TotalStr string        `yaml:"total"`
	Total    time.Duration `yaml:"-"`
	Grid     Grid          `yaml:"grid"`
	Acts     []Act         `yaml:"acts"`
}

// Load reads and validates a storyboard.
func Load(path string) (*Script, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Script
	if err := yaml.Unmarshal(blob, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// normalize parses every duration string into its time.Duration field.
func (s *Script) normalize() error {
	var err error
	if s.Total, err = time.ParseDuration(s.TotalStr); err != nil {
		return fmt.Errorf("total: %w", err)
	}
	for i := range s.Acts {
		a := &s.Acts[i]
		if a.Runtime, err = time.ParseDuration(a.RuntimeStr); err != nil {
			return fmt.Errorf("act %d (%q) runtime: %w", i, a.Name, err)
		}
		for j := range a.Steps {
			st := &a.Steps[j]
			st.Slow = make([]time.Duration, 0, len(st.SlowStr))
			for k, raw := range st.SlowStr {
				if raw == "" {
					st.Slow = append(st.Slow, 0)
					continue
				}
				d, perr := time.ParseDuration(raw)
				if perr != nil {
					return fmt.Errorf("act %d step %d slow[%d]: %w", i, j, k, perr)
				}
				st.Slow = append(st.Slow, d)
			}
		}
		for j := range a.Beats {
			b := &a.Beats[j]
			if b.HoldStr == "" {
				return fmt.Errorf("act %d (%q) beat %q: hold is required", i, a.Name, b.Label)
			}
			if b.Hold, err = time.ParseDuration(b.HoldStr); err != nil {
				return fmt.Errorf("act %d (%q) beat %q hold: %w", i, a.Name, b.Label, err)
			}
			if b.AtStr != "" {
				if b.At, err = time.ParseDuration(b.AtStr); err != nil {
					return fmt.Errorf("act %d (%q) beat %q at: %w", i, a.Name, b.Label, err)
				}
			} else if j > 0 {
				b.At = a.Beats[j-1].At
			}
			if j > 0 && b.At < a.Beats[j-1].At {
				return fmt.Errorf("act %d (%q) beat %q: at goes back in time", i, a.Name, b.Label)
			}
		}
		a.Beats = expandFrames(a.Beats)
	}
	return nil
}

// expandFrames replaces each beat with Frames > 1 by that many beats. The holds add up
// to the beat's own, so the act's runtime check still holds.
func expandFrames(in []Beat) []Beat {
	var out []Beat
	var prev time.Duration
	for _, b := range in {
		n := b.Frames
		if n <= 1 {
			out = append(out, b)
			prev = b.At
			continue
		}
		for k := 1; k <= n; k++ {
			f := b
			f.Label = fmt.Sprintf("%s-%d", b.Label, k)
			f.At = prev + (b.At-prev)*time.Duration(k)/time.Duration(n)
			f.Hold = b.Hold*time.Duration(k)/time.Duration(n) - b.Hold*time.Duration(k-1)/time.Duration(n)
			if k > 1 {
				f.Keys = nil
			}
			out = append(out, f)
		}
		prev = b.At
	}
	return out
}

func (s *Script) validate() error {
	if s.Grid.Cols <= 0 || s.Grid.Rows <= 0 {
		return fmt.Errorf("grid must have positive cols and rows, got %dx%d", s.Grid.Cols, s.Grid.Rows)
	}
	if len(s.Acts) == 0 {
		return fmt.Errorf("no acts")
	}
	var sum time.Duration
	for i, a := range s.Acts {
		switch a.Kind {
		case "shell":
			if len(a.Steps) == 0 {
				return fmt.Errorf("act %d (%q): kind shell needs steps", i, a.Name)
			}
		case "windows":
			if len(a.Windows) == 0 {
				return fmt.Errorf("act %d (%q): kind windows needs windows", i, a.Name)
			}
		case "tui":
			if len(a.Beats) == 0 {
				return fmt.Errorf("act %d (%q): kind tui needs beats", i, a.Name)
			}
			if len(a.Fixture.Sessions) == 0 && a.Fixture.Replay == nil {
				return fmt.Errorf("act %d (%q): kind tui needs fixture sessions", i, a.Name)
			}
		default:
			return fmt.Errorf("act %d (%q): unknown kind %q (want shell, windows or tui)", i, a.Name, a.Kind)
		}
		if a.Runtime <= 0 {
			return fmt.Errorf("act %d (%q): runtime must be positive", i, a.Name)
		}
		sum += a.Runtime
	}
	// The emitter lays every state on one absolute timeline of exactly Total, so
	// a mismatch here would silently shift every keyframe.
	if sum != s.Total {
		return fmt.Errorf("act runtimes sum to %s but total is %s", sum, s.Total)
	}
	return nil
}
