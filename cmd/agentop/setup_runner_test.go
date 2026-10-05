package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/cmd/agentop/checklist"
)

// newTestSetupEnv is a setupEnv over a temp HOME with zsh and a PATH that does
// not hold its bin dir.
func newTestSetupEnv(t *testing.T) *setupEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return &setupEnv{
		home: home, goos: runtime.GOOS, shell: "/bin/zsh", pathEnv: "/usr/bin:/bin",
		binDir: filepath.Join(home, ".local", "bin"), cortexDir: filepath.Join(home, ".cortex"),
	}
}

type fakeStep struct {
	label    string
	planned  stepPlan
	prob     *problem
	applyErr error
	undoErr  error
	log      *[]string
	setPrior bool
	noUndo   bool   // apply changed nothing, so it returns an empty undo
	manual   string // the undo's do-it-yourself line
	priorErr error  // what restorePrior returns
	bareUndo bool   // the undo carries no label of its own
}

func (f fakeStep) name() string { return f.label }

func (f fakeStep) plan(*setupEnv) (stepPlan, *problem) {
	p := f.planned
	p.label = f.label
	if !p.done {
		p.verb, p.what, p.where = "do", f.label, "here"
	}
	return p, f.prob
}

func (f fakeStep) apply(env *setupEnv, _ *checklist.Running) (string, undo, error) {
	*f.log = append(*f.log, "apply "+f.label)
	if f.setPrior {
		env.restorePrior = func() error { *f.log = append(*f.log, "restore prior"); return f.priorErr }
		env.priorDesc = "the old one is running again"
	}
	if f.noUndo {
		return "did " + f.label, undo{label: f.label}, f.applyErr
	}
	label := f.label
	if f.bareUndo {
		label = ""
	}
	return "did " + f.label, undo{label: label, fn: func() error {
		*f.log = append(*f.log, "undo "+f.label)
		return f.undoErr
	}, manual: f.manual}, f.applyErr
}

func run(t *testing.T, env *setupEnv, steps []step, sig <-chan os.Signal) (bool, string) {
	t.Helper()
	plainOutput(t)
	ps, probs := planSteps(env, steps)
	if len(probs) > 0 {
		t.Fatalf("unexpected problems: %v", probs)
	}
	var b bytes.Buffer
	ok := applySteps(env, checklist.New(&b, false), ps, sig)
	return ok, b.String()
}

func TestApplyStepsInOrderAndSkipsDone(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	ok, out := run(t, env, []step{
		fakeStep{label: "a", log: &log},
		fakeStep{label: "b", log: &log, planned: stepPlan{done: true, doneMsg: "already b"}},
		fakeStep{label: "c", log: &log},
	}, nil)
	if !ok || strings.Join(log, ",") != "apply a,apply c" {
		t.Errorf("ok=%v log=%v", ok, log)
	}
	for _, want := range []string{"  ✓ a            did a\n", "  · b            already b\n", "  ✓ c            did c\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAFailureUndoesInReverseIncludingItself(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	ok, out := run(t, env, []step{
		fakeStep{label: "a", log: &log},
		fakeStep{label: "b", log: &log, applyErr: stepError{reason: "broke", detail: []string{"why"}}},
		fakeStep{label: "c", log: &log},
	}, nil)
	if ok {
		t.Fatal("reported success")
	}
	if got := strings.Join(log, ","); got != "apply a,apply b,undo b,undo a" {
		t.Errorf("log = %s", got)
	}
	for _, want := range []string{"  ✗ b            broke\n", "      why\n",
		"    undone       b\n    undone       a\n    rolled back  the changes above are undone\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestASignalBetweenStepsRollsBack(t *testing.T) {
	var log []string
	sig := make(chan os.Signal, 1)
	sig <- os.Interrupt
	env := newTestSetupEnv(t)
	ok, out := run(t, env, []step{fakeStep{label: "a", log: &log}, fakeStep{label: "b", log: &log}}, sig)
	if ok || strings.Join(log, ",") != "apply a,undo a" {
		t.Errorf("ok=%v log=%v", ok, log)
	}
	if !strings.Contains(out, "Interrupted.") {
		t.Errorf("no interruption line:\n%s", out)
	}
	if !strings.Contains(out, "    undone       a\n    rolled back  the changes above are undone\n") {
		t.Errorf("a rollback of one undo does not say the change is undone:\n%s", out)
	}
}

func TestThePriorServiceIsRestoredAfterTheUndos(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	ok, out := run(t, env, []step{
		fakeStep{label: "service", log: &log, setPrior: true},
		fakeStep{label: "z", log: &log, applyErr: errors.New("boom")},
	}, nil)
	if ok || strings.Join(log, ",") != "apply service,apply z,undo z,undo service,restore prior" {
		t.Errorf("ok=%v log=%v", ok, log)
	}
	if !strings.Contains(out, "reverted     the old one is running again") {
		t.Errorf("no reverted line:\n%s", out)
	}
	if !strings.Contains(out, "    undone       z\n    undone       service\n    reverted     the old one is running again\n") {
		t.Errorf("the undone lines do not come newest first, before the reverted line:\n%s", out)
	}
}

func TestAFailedUndoIsReportedNotHidden(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	_, out := run(t, env, []step{
		fakeStep{label: "a", log: &log, undoErr: errors.New("disk full")},
		fakeStep{label: "b", log: &log, applyErr: errors.New("boom")},
	}, nil)
	if !strings.Contains(out, "could not roll back a: disk full") {
		t.Errorf("the failed undo is not reported:\n%s", out)
	}
	if !strings.Contains(out, "    undone       b\n") || strings.Contains(out, "undone       a") {
		t.Errorf("want b listed as undone and a not:\n%s", out)
	}
	if strings.Contains(out, "the changes above are undone") {
		t.Errorf("claimed a full rollback after a failed undo:\n%s", out)
	}
}

func TestPlanStepsCollectsEveryProblem(t *testing.T) {
	env := newTestSetupEnv(t)
	var log []string
	_, probs := planSteps(env, []step{
		fakeStep{label: "a", log: &log, prob: &problem{reason: "no"}},
		fakeStep{label: "b", log: &log},
		fakeStep{label: "c", log: &log, prob: &problem{reason: "nor this"}},
	})
	if len(probs) != 2 || probs[0].label != "a" || probs[1].label != "c" {
		t.Errorf("problems = %+v", probs)
	}
}

func TestTilde(t *testing.T) {
	env := newTestSetupEnv(t)
	if got := env.tilde(filepath.Join(env.home, ".local", "bin")); got != "~/.local/bin" {
		t.Errorf("tilde = %q", got)
	}
	if got := env.tilde("/opt/x"); got != "/opt/x" {
		t.Errorf("tilde outside HOME = %q", got)
	}
}

// rerun names the command to run again: the one setup was run as, with paths under
// HOME as ~/…, since `make dev-install` runs setup for the user (#1282); or the
// installer, whose stage setup deletes on its way out.
func TestRerunNamesTheCommandToRunAgain(t *testing.T) {
	env := newTestSetupEnv(t)
	bin := filepath.Join(env.home, "src", "cortex", "bin")
	env.argv = []string{filepath.Join(bin, "agentop"), "setup", "--from", bin, "--yes", "--restart"}
	if got, want := env.rerun(), "re-run: ~/src/cortex/bin/agentop setup --from ~/src/cortex/bin --yes --restart"; got != want {
		t.Errorf("rerun = %q, want %q", got, want)
	}
	env.argv = []string{"agentop", "setup", "--from", "/opt/my bin"}
	if got, want := env.rerun(), "re-run: agentop setup --from '/opt/my bin'"; got != want {
		t.Errorf("rerun = %q, want %q", got, want)
	}
	env.opts.handoffBytes = 1
	if got, want := env.rerun(), "re-run the installer"; got != want {
		t.Errorf("under the installer, rerun = %q, want %q", got, want)
	}
}

func TestARollbackWithNothingToUndoSaysNothingChanged(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	ok, out := run(t, env, []step{
		fakeStep{label: "a", log: &log, noUndo: true, applyErr: errors.New("boom")},
		fakeStep{label: "b", log: &log},
	}, nil)
	if ok || strings.Join(log, ",") != "apply a" {
		t.Errorf("ok=%v log=%v", ok, log)
	}
	if !strings.Contains(out, "rolled back  nothing had been changed\n") {
		t.Errorf("a rollback with nothing to undo does not say so:\n%s", out)
	}
	if strings.Contains(out, "the changes above are undone") {
		t.Errorf("claimed changes were undone when none had been made:\n%s", out)
	}
}

// A step can leave no undo of its own yet have stopped the Cortex that was
// serving; bringing that back is a change undone, not "nothing".
func TestARollbackThatOnlyRestoresThePriorServiceIsNotNothing(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	_, out := run(t, env, []step{
		fakeStep{label: "service", log: &log, setPrior: true, noUndo: true, applyErr: errors.New("boom")},
	}, nil)
	if got := strings.Join(log, ","); got != "apply service,restore prior" {
		t.Errorf("log = %s", got)
	}
	if !strings.Contains(out, "rolled back  the changes above are undone\n") || strings.Contains(out, "nothing had been changed") {
		t.Errorf("restoring the prior service read as nothing changed:\n%s", out)
	}
}

func TestAHiddenStepPrintsNothingAndIsNotApplied(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	ok, out := run(t, env, []step{
		fakeStep{label: "a", log: &log},
		fakeStep{label: "cleanup", log: &log, planned: stepPlan{done: true, hidden: true, doneMsg: "nothing to clean"}},
	}, nil)
	if !ok || strings.Join(log, ",") != "apply a" {
		t.Errorf("ok=%v log=%v", ok, log)
	}
	if strings.Count(out, "\n") != 1 || strings.Contains(out, "cleanup") || strings.Contains(out, "nothing to clean") {
		t.Errorf("a hidden step printed a line:\n%s", out)
	}
}

func TestADoneStepWithAdviceWarnsAndIsNotApplied(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	ok, out := run(t, env, []step{
		fakeStep{label: "path", log: &log, planned: stepPlan{done: true, doneMsg: "already on PATH",
			advice: &problem{reason: "not on PATH in this shell", fix: []string{"open a new terminal", "or run: exec zsh"}}}},
	}, nil)
	if !ok || len(log) != 0 {
		t.Errorf("ok=%v log=%v", ok, log)
	}
	for _, want := range []string{"  ! path         not on PATH in this shell\n", "      fix: open a new terminal\n", "      fix: or run: exec zsh\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("advice output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "already on PATH") {
		t.Errorf("printed the done line as well as the advice:\n%s", out)
	}
}

func TestAllDone(t *testing.T) {
	done := plannedStep{p: stepPlan{done: true}}
	hidden := plannedStep{p: stepPlan{done: true, hidden: true}} // as a hidden plan is built
	todo := plannedStep{p: stepPlan{verb: "install"}}
	for _, c := range []struct {
		name string
		ps   []plannedStep
		want bool
	}{
		{"done and hidden", []plannedStep{done, hidden}, true},
		{"one to do between done ones", []plannedStep{done, todo, hidden}, false},
		{"one to do at the end", []plannedStep{hidden, done, todo}, false},
	} {
		if got := allDone(c.ps); got != c.want {
			t.Errorf("%s: allDone = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConsentItemsListsOnlyTheStepsToDoInOrder(t *testing.T) {
	ps := []plannedStep{
		{p: stepPlan{verb: "install", what: "agentop", where: "~/.local/bin"}},
		{p: stepPlan{done: true, verb: "replace", what: "abctl", where: "~/bin"}},
		{p: stepPlan{done: true, hidden: true, verb: "remove", what: "authbridge-proxy", where: "~/old"}},
		{p: stepPlan{verb: "write", what: "config.yaml", where: "~/.cortex"}},
	}
	want := []checklist.Item{
		{Verb: "install", What: "agentop", Where: "~/.local/bin"},
		{Verb: "write", What: "config.yaml", Where: "~/.cortex"},
	}
	if got := consentItems(ps); !slices.Equal(got, want) {
		t.Errorf("consentItems = %+v, want %+v", got, want)
	}
}

func TestAFailedUndoNamesItsManualFix(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	_, out := run(t, env, []step{
		fakeStep{label: "bare", log: &log, undoErr: errors.New("busy")},
		fakeStep{label: "file", log: &log, undoErr: errors.New("read-only"), manual: "rm x"},
		fakeStep{label: "z", log: &log, applyErr: errors.New("boom")},
	}, nil)
	if !strings.Contains(out, "    could not roll back file: read-only\n      do it yourself: rm x\n") {
		t.Errorf("the failed undo does not name its manual fix:\n%s", out)
	}
	if !strings.HasSuffix(out, "    could not roll back bare: busy\n") || strings.Count(out, "do it yourself") != 1 {
		t.Errorf("an undo with no manual fix printed one:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("plain output holds a control byte: %q", out)
	}
}

func TestAFailedRestoreOfThePriorServiceNamesTheRestartCommand(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	_, out := run(t, env, []step{
		fakeStep{label: "service", log: &log, setPrior: true, noUndo: true,
			priorErr: errors.New("bootstrap failed"), applyErr: errors.New("boom")},
	}, nil)
	if !strings.Contains(out, "    could not roll back the previous Cortex: bootstrap failed\n      do it yourself: agentop service restart\n") {
		t.Errorf("a failed restore does not name the restart command:\n%s", out)
	}
}

func TestMultiLineErrorsKeepTheirIndent(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	_, out := run(t, env, []step{
		fakeStep{label: "a", log: &log, undoErr: errors.Join(errors.New("stop failed"), errors.New("unlink failed")), manual: "rm y"},
		fakeStep{label: "b", log: &log, applyErr: errors.Join(errors.New("write failed"), errors.New("rename failed"))},
	}, nil)
	for _, want := range []string{
		"  ✗ b            write failed\n      rename failed\n",
		"    could not roll back a: stop failed\n      unlink failed\n      do it yourself: rm y\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("multi-line error output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nrename failed") || strings.Contains(out, "\nunlink failed") {
		t.Errorf("an error's second line is printed flush left:\n%s", out)
	}
}

func TestTildeTextShortensOnlyWholeHomePaths(t *testing.T) {
	env := &setupEnv{home: "/Users/al"}
	for in, want := range map[string]string{
		"open /Users/al/.cortex/ca.pem: denied": "open ~/.cortex/ca.pem: denied",
		"/Users/al":                             "~",
		"see '/Users/al/x' and /Users/al/y":     "see '~/x' and ~/y",
		"/Users/alice/.cortex":                  "/Users/alice/.cortex",
		"/srv/Users/al/x":                       "/srv/Users/al/x",
		"nothing here":                          "nothing here",
	} {
		if got := env.tildeText(in); got != want {
			t.Errorf("tildeText(%q) = %q, want %q", in, got, want)
		}
	}
}

// An undo with no label of its own is named after its step, in the rollback's
// lines as on the checklist.
func TestAnUnlabelledUndoTakesItsStepsLabel(t *testing.T) {
	var log []string
	env := newTestSetupEnv(t)
	_, out := run(t, env, []step{
		fakeStep{label: "a", log: &log, bareUndo: true},
		fakeStep{label: "b", log: &log, bareUndo: true, undoErr: errors.New("busy")},
		fakeStep{label: "z", log: &log, applyErr: errors.New("boom")},
	}, nil)
	for _, want := range []string{"    could not roll back b: busy\n", "    undone       a\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("an unlabelled undo is not named after its step, %q:\n%s", want, out)
		}
	}
}

func TestErrorLinesSkipsEmptyLines(t *testing.T) {
	for _, c := range []struct {
		in, reason string
		rest       []string
	}{
		{"boom", "boom", nil},
		{"write failed\nrename failed\n", "write failed", []string{"rename failed"}},
		{"write failed\n\nrename failed", "write failed", []string{"rename failed"}},
		{"\nwrite failed\nrename failed", "write failed", []string{"rename failed"}},
		{"", "", nil},
	} {
		reason, rest := errorLines(errors.New(c.in))
		if reason != c.reason || !slices.Equal(rest, c.rest) {
			t.Errorf("errorLines(%q) = %q %q, want %q %q", c.in, reason, rest, c.reason, c.rest)
		}
	}
}

// notedStep applies with a detail of more than one line, as a step that passes
// on a warning does.
type notedStep struct{ detail string }

func (notedStep) name() string { return "noted" }

func (notedStep) plan(*setupEnv) (stepPlan, *problem) {
	return stepPlan{verb: "do", what: "noted", where: "here"}, nil
}

func (n notedStep) apply(*setupEnv, *checklist.Running) (string, undo, error) {
	return n.detail, undo{}, nil
}

// A detail's lines after its first are rows of their own under the ✓ line, at
// Fail's detail indent: one marked line, not a line break inside it.
func TestADetailsLaterLinesAreRowsUnderIt(t *testing.T) {
	ok, out := run(t, newTestSetupEnv(t), []step{notedStep{"done\nwatch out\n\nreally"}}, nil)
	want := "  ✓ noted        done\n      watch out\n      really\n"
	if !ok || out != want {
		t.Errorf("ok=%v, output %q, want %q", ok, out, want)
	}
}

// Whole-path means whole in s itself: HOME straight after a shortened HOME is
// inside that path, not a second one. A byte of 0x80 or more is part of a name,
// and a HOME with a trailing slash still shortens.
func TestTildeTextReadsBytesAsTheyStandInTheLine(t *testing.T) {
	for _, tc := range []struct{ home, in, want string }{
		{"/Users/al", "/Users/al/Users/al/x", "~/Users/al/x"},
		{"/Users/al", "/Users/alé/x and /Users/al/y", "/Users/alé/x and ~/y"},
		{"/Users/al/", "open /Users/al/x", "open ~/x"},
	} {
		if got := (&setupEnv{home: tc.home}).tildeText(tc.in); got != tc.want {
			t.Errorf("HOME %s: tildeText(%q) = %q, want %q", tc.home, tc.in, got, tc.want)
		}
	}
}
