package cli

import (
	"encoding/json"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// issuePayload wraps a single issue so it marshals with its CAS `version`
// field under --json=v2 and without it under v1 (engine-spec.md §5,
// "versions in .data").
//
// The version rides on the wrapper rather than on model.Issue's MarshalJSON
// because that marshaler is the frozen v1 wire format — adding a field there
// would change every existing verb's output.
type issuePayload struct{ issue *model.Issue }

// MarshalJSON emits the v1 issue shape (the wrapper is transparent under v1).
func (p issuePayload) MarshalJSON() ([]byte, error) { return p.issue.MarshalJSON() }

// VersionedPayload implements output.Versioned: under v2 the issue marshals
// with its version.
func (p issuePayload) VersionedPayload() any {
	return model.VersionedIssue{Issue: *p.issue}
}

// withIssueVersion wraps a single issue for emission. Safe on nil.
func withIssueVersion(issue *model.Issue) any {
	if issue == nil {
		return nil
	}
	return issuePayload{issue: issue}
}

// runPayload wraps a single run for the same reason issuePayload wraps an
// issue: model.Run's MarshalJSON is the v1 wire format, and the CAS row version
// rides on the wrapper so v1 output is untouched.
type runPayload struct{ run *model.Run }

// MarshalJSON emits the v1 run shape (the wrapper is transparent under v1).
func (p runPayload) MarshalJSON() ([]byte, error) { return p.run.MarshalJSON() }

// VersionedPayload implements output.Versioned.
func (p runPayload) VersionedPayload() any {
	return model.VersionedRun{Run: *p.run}
}

// withRunVersion wraps a single run for emission. Safe on nil.
func withRunVersion(run *model.Run) any {
	if run == nil {
		return nil
	}
	return runPayload{run: run}
}

// runAbandonPayload is `run abandon`'s response: the run under v1, and under
// v2 the run plus its `worktrees` — the checkouts the run's steps recorded
// that no close will ever sweep now (DKT-116). v1 stays byte-identical to the
// pre-DKT-116 shape; the fact rides the v2 payload, the event, and the
// message.
type runAbandonPayload struct {
	run       *model.Run
	worktrees []string
}

// MarshalJSON emits the v1 run shape (the wrapper is transparent under v1).
func (p runAbandonPayload) MarshalJSON() ([]byte, error) { return p.run.MarshalJSON() }

// VersionedPayload implements output.Versioned: the v2 run shape, with
// `worktrees` beside it when there are any.
func (p runAbandonPayload) VersionedPayload() any {
	versioned := model.VersionedRun{Run: *p.run}
	if len(p.worktrees) == 0 {
		return versioned
	}
	raw, err := json.Marshal(versioned)
	if err != nil {
		return versioned
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return versioned
	}
	m["worktrees"] = p.worktrees
	return m
}

// runResumePayload is `run resume`'s response when the run's pins no longer
// match disk (DKT-408): the frozen v1 run shape untouched, and under v2 the
// run plus `pin_drift` — the same v1/v2 split runAbandonPayload uses for its
// worktrees, for the same reason. The drift also rides the human message, so
// neither channel resumes a wedged run silently.
type runResumePayload struct {
	run      *model.Run
	pinDrift []engine.PinVerdict
}

// MarshalJSON emits the v1 run shape (the wrapper is transparent under v1).
func (p runResumePayload) MarshalJSON() ([]byte, error) { return p.run.MarshalJSON() }

// VersionedPayload implements output.Versioned: the v2 run shape, with
// `pin_drift` beside it when there is any.
func (p runResumePayload) VersionedPayload() any {
	versioned := model.VersionedRun{Run: *p.run}
	if len(p.pinDrift) == 0 {
		return versioned
	}
	raw, err := json.Marshal(versioned)
	if err != nil {
		return versioned
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return versioned
	}
	m["pin_drift"] = p.pinDrift
	return m
}

// abandonIssuePayload is `run abandon --issue`'s response wrapper, for the
// same v1/v2 split: the outcome's frozen shape under v1, plus `worktrees`
// under v2 (DKT-116).
type abandonIssuePayload struct{ outcome *engine.AbandonIssueOutcome }

// MarshalJSON emits the v1 outcome shape (Worktrees is json:"-" there).
func (p abandonIssuePayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(*p.outcome)
}

// VersionedPayload implements output.Versioned.
func (p abandonIssuePayload) VersionedPayload() any {
	return struct {
		engine.AbandonIssueOutcome
		Worktrees []string `json:"worktrees,omitempty"`
	}{*p.outcome, p.outcome.Worktrees}
}

// runListPayload wraps a slice of runs so the v2 collection envelope reaches
// each item's row version. The envelope consults output.Versioned on the items
// CONTAINER rather than on every element, so a bare []*Run would render v1
// items inside a v2 envelope.
type runListPayload struct{ runs []*model.Run }

// MarshalJSON emits the v1 array shape.
func (p runListPayload) MarshalJSON() ([]byte, error) {
	if p.runs == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(p.runs)
}

// VersionedPayload implements output.Versioned for a list of runs.
func (p runListPayload) VersionedPayload() any {
	return model.RunsWithVersion(p.runs)
}

// issueListPayload wraps a slice of issues for the same purpose.
type issueListPayload struct{ issues []*model.Issue }

// MarshalJSON emits the v1 array shape.
func (p issueListPayload) MarshalJSON() ([]byte, error) {
	if p.issues == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(p.issues)
}

// VersionedPayload implements output.Versioned for a list of issues.
func (p issueListPayload) VersionedPayload() any {
	return model.WithVersion(p.issues)
}
