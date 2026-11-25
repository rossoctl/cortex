package toolscan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Result is what a scan found.
type Result struct {
	Since      time.Time
	Files      int
	Lines      int      // tool-call lines inside the window
	Called     []string // tool names actually invoked in the window, sorted
	CallCounts map[string]int
	Candidates []string // known, never called, not kept, not implied — sorted
	Kept       []string // names withheld by --keep or the implies table
}

// transcriptEntry is the minimum shape needed. Decoding only these fields keeps
// the parse cheap on 40MB+ transcripts.
type transcriptEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// DefaultProjectsDir is where Claude Code keeps per-project transcripts.
func DefaultProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// Scan walks dir for *.jsonl transcripts and derives a candidate list.
//
// Tool calls are deduplicated by the unique tool_use block id: the same
// assistant turn is rewritten into the transcript on every resume, so counting
// raw occurrences would inflate heavily-resumed sessions.
// AllTime, passed as days, disables the recency window: every tool call in every
// transcript counts as used. It is the safe direction to err in — a wider window
// can only ever find MORE tools in use, so it proposes fewer for removal.
const AllTime = 0

func Scan(dir string, days int, keep []string) (*Result, error) {
	// A zero Since means unbounded: no real timestamp is Before it.
	var since time.Time
	if days > 0 {
		since = time.Now().AddDate(0, 0, -days)
	}
	res := &Result{Since: since, CallCounts: map[string]int{}}

	seenIDs := make(map[string]struct{})
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree should not abort the whole scan.
			return nil //nolint:nilerr // best-effort walk
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		res.Files++
		// Propagate: a partial scan silently proposes more tools.
		return scanFile(path, since, seenIDs, res)
	})
	if err != nil {
		return nil, err
	}

	// Expand the keep set with anything implied by a tool that WAS called.
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		if k = strings.TrimSpace(k); k != "" {
			keepSet[k] = struct{}{}
		}
	}
	for name := range res.CallCounts {
		for _, dep := range implies[name] {
			keepSet[dep] = struct{}{}
		}
	}

	for name := range res.CallCounts {
		res.Called = append(res.Called, name)
	}
	sort.Strings(res.Called)

	for _, known := range knownTools {
		if _, called := res.CallCounts[known]; called {
			continue
		}
		if _, kept := keepSet[known]; kept {
			res.Kept = append(res.Kept, known)
			continue
		}
		res.Candidates = append(res.Candidates, known)
	}
	sort.Strings(res.Candidates)
	sort.Strings(res.Kept)
	return res, nil
}

func scanFile(path string, since time.Time, seenIDs map[string]struct{}, res *Result) error {
	f, err := os.Open(path) //nolint:gosec // operator-supplied transcript dir
	if err != nil {
		return nil //nolint:nilerr // skip unreadable file
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Transcript lines routinely exceed the default 64KB (a single tool result
	// can be hundreds of KB), so give the scanner room before it errors.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		// Hot path: the overwhelming majority of lines carry no tool call.
		// A literal substring check is far cheaper than parsing them.
		// bytes.Contains, not strings.Contains(string(line), …): converting would
		// copy every candidate line, and this is the hot path the prefilter exists
		// to keep cheap.
		if !bytes.Contains(line, []byte(`"tool_use"`)) {
			continue
		}

		var e transcriptEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if !e.Timestamp.IsZero() && e.Timestamp.Before(since) {
			continue
		}
		// Counted AFTER the window filter. Counting before it meant the summary
		// reported the same figure for --days 1 and --days 36500 while printing
		// "window N day(s)" beside it, which reads as a broken window.
		res.Lines++

		for _, c := range e.Message.Content {
			if c.Type != "tool_use" || c.Name == "" {
				continue
			}
			if c.ID != "" {
				if _, dup := seenIDs[c.ID]; dup {
					continue
				}
				seenIDs[c.ID] = struct{}{}
			}
			res.CallCounts[c.Name]++
		}
	}
	// A scanner error (a line past the 16MB cap, a read fault) silently stops
	// iteration. Swallowing it under-reports which tools were CALLED, which
	// makes the scan propose MORE for removal — failing toward removing a tool
	// the agent needs, the one direction this must not fail in.
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w (the tool list would be incomplete, so refusing to guess)", path, err)
	}
	return nil
}

// YAMLBlock renders the candidate list as the config fragment an operator
// pastes (or --write patches) into the context-guru entry.
func (r *Result) YAMLBlock() string {
	var b strings.Builder
	// on_error is omitted: it defaults to enforce, and the remove list below
	// is what gates the tool filter.
	b.WriteString("      - name: context-guru\n")
	b.WriteString("        config:\n")
	b.WriteString("          engine:\n")
	// An engine with components but no preset runs no preset, which turns
	// every compaction off. So the block names the built-in preset.
	b.WriteString("            preset: xhigh\n")
	b.WriteString("            components:\n")
	b.WriteString("              toolfilter:\n")
	if len(r.Candidates) == 0 {
		b.WriteString("                remove: []\n")
		return b.String()
	}
	fmt.Fprintf(&b, "                remove: [%s]\n", strings.Join(r.Candidates, ", "))
	return b.String()
}

// Summary is the human-readable preamble printed above the YAML block.
func (r *Result) Summary(days int) string {
	var b strings.Builder
	if days <= AllTime {
		fmt.Fprintf(&b, "Scanned %d transcript(s), %d tool-call line(s), all history (no window).\n",
			r.Files, r.Lines)
	} else {
		fmt.Fprintf(&b, "Scanned %d transcript(s), %d tool-call line(s), window %d day(s) since %s.\n",
			r.Files, r.Lines, days, r.Since.Format("2006-01-02"))
	}
	fmt.Fprintf(&b, "Called in window (%d): %s\n", len(r.Called), joinOrNone(r.Called))
	fmt.Fprintf(&b, "Removal candidates (%d): %s\n", len(r.Candidates), joinOrNone(r.Candidates))
	if len(r.Kept) > 0 {
		fmt.Fprintf(&b, "Withheld by --keep / implied-by-usage (%d): %s\n", len(r.Kept), joinOrNone(r.Kept))
	}
	b.WriteString("\nNames not in agentop's known-tool table are never proposed: removing a tool\n")
	b.WriteString("the model needs is the harmful failure, carrying extra definitions is not.\n")
	return b.String()
}

func joinOrNone(v []string) string {
	if len(v) == 0 {
		return "(none)"
	}
	return strings.Join(v, ", ")
}
