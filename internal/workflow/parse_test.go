package workflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// fixturePath is the canonical register-test fixture named in the TDD.
const fixturePath = "../../docs/design/example-workflow.toml"

// TestFixtureRegistersClean is the regression guard for the whole validation
// table: the committed fixture must survive every future edit to it.
//
// It exercises fanout, loops, `after_loop`, thresholds, pre-gates, `action`,
// and `type="human"` in one file — and it is the file that found the §4.3.1
// defect, where a V11 reading only `emits` rejects an `action` step that names
// its produced kind in `params.output`.
func TestFixtureRegistersClean(t *testing.T) {
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading the fixture: %v", err)

	def, err := Load(src)
	testsupport.Must(t, err, "the committed fixture must register clean, but: %v", err)

	// The features the fixture is committed to exercise, asserted so a future
	// edit that quietly drops one is caught: a fixture that no longer covers
	// fanout would still pass Load and would stop being the regression guard
	// this test claims it is.
	var sawFanout, sawLoop, sawAfterLoop, sawThreshold, sawPreGate, sawAction, sawHuman bool
	for _, step := range def.Steps {
		if len(step.Fanout) > 0 {
			sawFanout = true
		}
		if step.Loop {
			sawLoop = true
		}
		if step.AfterLoop != "" {
			sawAfterLoop = true
		}
		if len(step.Threshold) > 0 {
			sawThreshold = true
		}
		for _, g := range step.Gates {
			if g.Pre {
				sawPreGate = true
			}
		}
		if step.Action != "" {
			sawAction = true
		}
		if step.Type == TypeHuman {
			sawHuman = true
		}
	}

	for _, c := range []struct {
		feature string
		seen    bool
	}{
		{"fanout", sawFanout},
		{"loop", sawLoop},
		{"after_loop", sawAfterLoop},
		{"threshold", sawThreshold},
		{"pre-gate", sawPreGate},
		{"action", sawAction},
		{`type="human"`, sawHuman},
	} {
		if !c.seen {
			t.Errorf("the fixture no longer exercises %s", c.feature)
		}
	}
}

// TestFixtureHumanStepDeclaresOnFail pins the A4 amendment's edit to the
// fixture: `commit-gate` carries `on_fail = "fix-loop"`, which is the routing
// the flow actually wants — a rejected commit gate sends the issue back through
// the fix loop rather than parking it.
func TestFixtureHumanStepDeclaresOnFail(t *testing.T) {
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading the fixture: %v", err)
	def, err := Load(src)
	testsupport.Must(t, err, "Load: %v", err)

	for _, step := range def.Steps {
		if step.Type != TypeHuman {
			continue
		}
		if step.OnFail == "" {
			t.Errorf("human step %q declares no on_fail", step.Name)
		}
		if step.EffectiveOnFail() == OnFailWaitingHuman {
			t.Errorf("human step %q routes rejects to %q", step.Name, OnFailWaitingHuman)
		}
	}
}

// TestEmbeddedTemplatesRegister: every shipped template parses, validates, and
// lints. A shipped template that fails its own validator is the worst possible
// first experience, and it is the kind of thing that rots silently as the
// validator grows.
func TestEmbeddedTemplatesRegister(t *testing.T) {
	names := TemplateNames()
	if len(names) == 0 {
		t.Fatal("no templates are embedded")
	}

	// The shipped set at this stage (§4.4).
	want := map[string]bool{"standard-dev": true, "parallel-check": true}
	for _, name := range names {
		if !want[name] {
			t.Errorf("unexpected shipped template %q — template names are core surface", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("template %q is not shipped", name)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			src, err := Template(name)
			testsupport.Must(t, err, "Template: %v", err)
			def, err := Load(src)
			testsupport.Must(t, err, "shipped template %q does not register clean: %v", name, err)
			if def.Pipeline.Name != name {
				t.Errorf("template %q declares [pipeline].name = %q; the file name and the "+
					"pipeline name must agree or `init` then `register` names a different workflow",
					name, def.Pipeline.Name)
			}
		})
	}
}

// TestStandardDevIsTheStrangerTestArtifact pins §4.4's description of the
// template the §9 item 1 stranger test rests on: two worked steps, one
// fenced-command gate, no fanout — plus the `loop = true` fix body DKT-196
// added, without which the template's own `fix-loop` routings superseded work
// and instantiated nothing. A human-only team gets exactly this by typing one
// command.
func TestStandardDevIsTheStrangerTestArtifact(t *testing.T) {
	src, err := Template("standard-dev")
	testsupport.Must(t, err, "Template: %v", err)
	def, err := Load(src)
	testsupport.Must(t, err, "Load: %v", err)

	if len(def.Steps) != 3 {
		t.Errorf("standard-dev has %d steps, want 3 (check, approve, fix)", len(def.Steps))
	}
	if !hasLoopStep(def) {
		t.Error("standard-dev declares no `loop = true` step; its fix-loop " +
			"routings would supersede work and instantiate nothing (DKT-196)")
	}

	var fenced int
	for _, step := range def.Steps {
		if len(step.Fanout) > 0 {
			t.Errorf("standard-dev step %q fans out; the template is two steps and no fanout",
				step.Name)
		}
		for _, g := range step.Gates {
			if len(g.Source) > len("fence:") && g.Source[:len("fence:")] == "fence:" {
				fenced++
			}
		}
	}
	if fenced != 1 {
		t.Errorf("standard-dev declares %d fenced-command gates, want 1", fenced)
	}
}

// TestUnknownTemplateNamesTheAvailableSet: `workflow init --template typo` must
// tell the operator what they could have typed instead.
func TestUnknownTemplateNames(t *testing.T) {
	_, err := Template("no-such-template")
	if err == nil {
		t.Fatal("expected an error for an unknown template")
	}
	for _, name := range TemplateNames() {
		if !bytes.Contains([]byte(err.Error()), []byte(name)) {
			t.Errorf("error %q does not list the available template %q", err, name)
		}
	}

	// A template name is a name, not a path: accepting one would make
	// `workflow init` a file reader.
	for _, bad := range []string{"../parse", "a/b", `a\b`, "..", "."} {
		if _, err := Template(bad); err == nil {
			t.Errorf("template name %q was accepted as a path", bad)
		}
	}
}

// TestTemplatesAreWritableFiles: what `workflow init` writes is what
// `workflow register` reads, so the file name and the template name agree.
func TestTemplateFileName(t *testing.T) {
	for _, name := range TemplateNames() {
		if got, want := TemplateFileName(name), name+".toml"; got != want {
			t.Errorf("TemplateFileName(%q) = %q, want %q", name, got, want)
		}
		if filepath.Base(TemplateFileName(name)) != TemplateFileName(name) {
			t.Errorf("TemplateFileName(%q) is not a bare file name", name)
		}
	}
}
