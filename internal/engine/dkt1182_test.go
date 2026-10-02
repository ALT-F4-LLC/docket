package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1182: `run activate --dry-run` could not flag an exactly-one-WRONG
// workflow binding.
//
// HRN-1118 was scoped entirely to `internal/tui/screens/conversation_test.go` —
// a TUI test file — and carried only the `qa` label. Routing is keyed on
// hand-applied labels while scope is path-derived, so the issue matched the
// label-less baseline pipeline exactly once and bound it, losing the UI
// pipeline's judge-design fanout and its render/copy verification gates. Nothing
// refused and nothing warned: activation refuses ZERO matches and refuses
// SEVERAL, and one wrong match is neither. The only check that existed was a
// conductor diffing every bound issue's labels against its scope paths by hand,
// before every activation.
//
// The lint closes that gap WITHOUT changing routing: a workflow may declare the
// paths its domain occupies (`[match] domain_paths`), and an issue whose whole
// declared scope sits inside another workflow's domain while lacking only that
// workflow's labels is named in the activation report. The binding still follows
// the labels — this reports the disagreement and leaves the call to the operator.

// baselineSource is the standard-change shape: no positive selector at all, an
// exclusion list, and therefore the pipeline every unlabelled issue falls into.
// It declares no domain — its business is whatever nothing else claims.
const baselineSource = `
[pipeline]
name = "baseline-change"
version = 1
[match]
unless_labels = ["ui"]
[[step]]
name = "implement"
executor = "worker"
emits = "change-summary"
after = []
`

// uiSource is the ui-change shape: selected by a label, and — new here —
// declaring the paths its domain occupies.
const uiSource = `
[pipeline]
name = "ui-change"
version = 1
[match]
labels_any = ["ui"]
domain_paths = ["internal/tui/**", "web/"]
[[step]]
name = "implement"
executor = "worker"
emits = "change-summary"
after = []
`

// TestActivateFlagsExactlyOneWrongMatch is the defect itself, reproduced: an
// issue scoped entirely inside the UI pipeline's domain, labelled for neither,
// binds the baseline — and the activation now says so.
func TestActivateFlagsExactlyOneWrongMatch(t *testing.T) {
	conn := mustDB(t)
	baseline := registerSource(t, conn, []byte(baselineSource), "baseline-change.toml")
	ui := registerSource(t, conn, []byte(uiSource), "ui-change.toml")

	issue := createIssue(t, conn, "flaky conversation screen test", "a body", "task",
		[]string{"qa"})
	err := db.SetIssueScopeGlobs(conn, issue,
		`["internal/tui/screens/conversation_test.go"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate refused a mis-labelled issue: %v", err)

	// It WARNS. Binding is untouched — the issue is bound where its labels put
	// it, the run is active, and its steps exist.
	if result.Run.Status != model.RunActive {
		t.Errorf("run status = %q, want %q — the lint warns, it does not refuse",
			result.Run.Status, model.RunActive)
	}
	if countRows(t, conn, "steps") == 0 {
		t.Error("no steps written despite the activation succeeding")
	}
	if len(result.BoundIssues) != 1 || result.BoundIssues[0].Workflow != baseline.Ref() {
		t.Fatalf("bound issues = %+v, want the issue bound to %s — the lint must "+
			"not re-route anything", result.BoundIssues, baseline.Ref())
	}

	if len(result.BindingWarnings) != 1 {
		t.Fatalf("binding warnings = %+v, want exactly 1", result.BindingWarnings)
	}
	w := result.BindingWarnings[0]
	if w.IssueID != model.FormatID(issue) {
		t.Errorf("warning names issue %q, want %q", w.IssueID, model.FormatID(issue))
	}
	// Both workflows, named: what will actually run, and what the paths say
	// should have.
	if w.BoundWorkflow != baseline.Ref() {
		t.Errorf("bound workflow = %q, want %q", w.BoundWorkflow, baseline.Ref())
	}
	if w.DomainWorkflow != ui.Ref() {
		t.Errorf("domain workflow = %q, want %q", w.DomainWorkflow, ui.Ref())
	}
	// And the remedy: the label that would move it.
	if len(w.MissingLabels) != 1 || w.MissingLabels[0] != "ui" {
		t.Errorf("missing labels = %v, want [ui]", w.MissingLabels)
	}
	if len(w.Scope) != 1 || w.Scope[0] != "internal/tui/screens/conversation_test.go" {
		t.Errorf("warning carries scope %v, want the issue's declared scope", w.Scope)
	}
	if !strings.Contains(w.Reason, ui.Ref()) || !strings.Contains(w.Reason, "ui") {
		t.Errorf("reason %q names neither the domain workflow nor the label", w.Reason)
	}
}

// TestActivateDoesNotFlagCorrectlyLabelledIssues is the control the whole lint
// rests on. Same scope, same domain, same two workflows — the only difference is
// the label, and a correctly labelled issue must be silent. A lint that fired on
// the correct case would be noise, and noise is what makes an operator skim past
// the mis-bindings that are real.
func TestActivateDoesNotFlagCorrectlyLabelledIssues(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(baselineSource), "baseline-change.toml")
	ui := registerSource(t, conn, []byte(uiSource), "ui-change.toml")

	issue := createIssue(t, conn, "fix the conversation screen", "a body", "task",
		[]string{"ui"})
	err := db.SetIssueScopeGlobs(conn, issue,
		`["internal/tui/screens/conversation_test.go"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The silence must come from the LABEL, not from an issue the lint never
	// saw: without this the test would pass just as well if binding had
	// produced nothing to lint.
	if len(result.BoundIssues) != 1 || result.BoundIssues[0].Workflow != ui.Ref() {
		t.Fatalf("bound issues = %+v, want the issue bound to %s",
			result.BoundIssues, ui.Ref())
	}
	if len(result.BindingWarnings) != 0 {
		t.Errorf("binding warnings = %+v, want none — the issue is labelled for "+
			"the workflow whose domain it sits in", result.BindingWarnings)
	}
}

// TestBindingLintDiscriminates covers the conditions that keep the warning worth
// reading. Each case binds the baseline exactly as the defect case does; what
// changes is the one fact the lint consults.
func TestBindingLintDiscriminates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		scope  string
		want   bool
	}{
		// The defect shape, restated here so the table's negatives are read
		// against a positive that fires under the same harness.
		{"wholly inside the domain, wrong label",
			[]string{"qa"}, `["internal/tui/screens/conversation_test.go"]`, true},

		// Outside the domain: the paths and the labels agree, which is every
		// ordinary issue in every run.
		{"outside the domain",
			[]string{"qa"}, `["internal/engine/activate.go"]`, false},

		// A sibling directory is NOT inside `internal/tui`. Prefix comparison
		// without a path boundary would have called this a mis-binding.
		{"a sibling directory the prefix would swallow",
			[]string{"qa"}, `["internal/tuixyz/widget.go"]`, false},

		// Cross-cutting work: one glob inside the domain, one outside. Which
		// pipeline should own it is a judgment this lint has no basis to make,
		// so it stays quiet rather than guessing.
		{"partially inside the domain",
			[]string{"qa"}, `["internal/tui/app.go", "internal/db/issues.go"]`, false},

		// `[]` is "declared to touch nothing", a decision somebody made on
		// purpose — and NULL scope is lintUnscopedHolders' subject, not this
		// one.
		{"declared to touch nothing", []string{"qa"}, `[]`, false},
		{"no scope declared", []string{"qa"}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			registerSource(t, conn, []byte(baselineSource), "baseline-change.toml")
			registerSource(t, conn, []byte(uiSource), "ui-change.toml")

			issue := createIssue(t, conn, tc.name, "a body", "task", tc.labels)
			if tc.scope != "" {
				err := db.SetIssueScopeGlobs(conn, issue, tc.scope)
				testsupport.Must(t, err, "setting scope: %v", err)
			}
			run := startRun(t, conn, issue)

			result, err := activate(conn, run.ID)
			testsupport.Must(t, err, "activate: %v", err)

			if got := len(result.BindingWarnings) > 0; got != tc.want {
				t.Errorf("binding warnings = %+v, want warned=%v",
					result.BindingWarnings, tc.want)
			}
		})
	}
}

// TestBindingLintIsSilentOnDeliberateExclusions is the other half of "reachable"
// (workflow.LabelGapFor): a workflow that EXCLUDES this issue by kind or by
// `unless_labels` is not a workflow the issue was mis-labelled out of. Advising
// an operator to defeat either would be advice to break a decision the workflow
// author made on purpose.
func TestBindingLintIsSilentOnDeliberateExclusions(t *testing.T) {
	// A UI pipeline that declines security work outright and takes only `bug`s.
	// Each issue below is scoped squarely inside its domain and one label short
	// of its `labels_any` — so each would warn if the guard under test were
	// removed, and the assertion is not vacuous.
	const pickySource = `
[pipeline]
name = "ui-change"
version = 1
[match]
kind = ["bug"]
labels_any = ["ui"]
unless_labels = ["security"]
domain_paths = ["internal/tui/**"]
[[step]]
name = "implement"
executor = "worker"
emits = "change-summary"
after = []
`
	for _, tc := range []struct {
		name   string
		kind   string
		labels []string
	}{
		{"kind the domain workflow does not take", "task", []string{"qa"}},
		{"a label the domain workflow declines", "bug", []string{"security"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			registerSource(t, conn, []byte(baselineSource), "baseline-change.toml")
			registerSource(t, conn, []byte(pickySource), "ui-change.toml")

			issue := createIssue(t, conn, tc.name, "a body", tc.kind, tc.labels)
			err := db.SetIssueScopeGlobs(conn, issue, `["internal/tui/app.go"]`)
			testsupport.Must(t, err, "setting scope: %v", err)
			run := startRun(t, conn, issue)

			result, err := activate(conn, run.ID)
			testsupport.Must(t, err, "activate: %v", err)

			if len(result.BindingWarnings) != 0 {
				t.Errorf("binding warnings = %+v, want none — no labelling could "+
					"have bound this issue to the domain workflow",
					result.BindingWarnings)
			}
		})
	}
}

// TestBindingLintIsDormantWithoutDomainPaths is the compatibility assertion: a
// corpus whose workflows declare no domain — every corpus, until an author
// states one — activates exactly as it did before, warning about nothing.
func TestBindingLintIsDormantWithoutDomainPaths(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "ordinary work", "a body", "task", []string{"qa"})
	err := db.SetIssueScopeGlobs(conn, issue, `["internal/tui/screens/x_test.go"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if result.IssuesBound != 1 {
		t.Fatalf("issues bound = %d, want 1", result.IssuesBound)
	}
	if len(result.BindingWarnings) != 0 {
		t.Errorf("binding warnings = %+v, want none — no registered workflow "+
			"declares a domain", result.BindingWarnings)
	}
}

// TestBindingWarningRidesOnDryRun is the placement that matters most, and the
// one the issue actually asked for. `--dry-run` is where a conductor decides
// whether to commit a run, so a mis-binding visible only after activation is one
// the dry run failed to report — and after activation the binding is already
// made.
func TestBindingWarningRidesOnDryRun(t *testing.T) {
	conn := mustDB(t)
	baseline := registerSource(t, conn, []byte(baselineSource), "baseline-change.toml")
	ui := registerSource(t, conn, []byte(uiSource), "ui-change.toml")

	issue := createIssue(t, conn, "flaky conversation screen test", "a body", "task",
		[]string{"qa"})
	err := db.SetIssueScopeGlobs(conn, issue,
		`["internal/tui/screens/conversation_test.go"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)

	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	testsupport.Must(t, err, "dry run: %v", err)

	if !result.DryRun {
		t.Fatal("result is not marked as a dry run")
	}
	if len(result.BindingWarnings) != 1 {
		t.Fatalf("binding warnings = %+v, want 1 on a dry run", result.BindingWarnings)
	}
	w := result.BindingWarnings[0]
	if w.BoundWorkflow != baseline.Ref() || w.DomainWorkflow != ui.Ref() {
		t.Errorf("warning = %+v, want %s bound / %s by domain",
			w, baseline.Ref(), ui.Ref())
	}

	// And the dry run still wrote nothing, so the warning cost no state.
	assertNothingWritten(t, conn)
}
