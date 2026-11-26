package cli

import (
	"encoding/json"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// withIssueVersion renders a single issue with its CAS `version` (and its lease
// when one is held) in .data. Safe on nil.
func withIssueVersion(issue *model.Issue) any {
	if issue == nil {
		return nil
	}
	return model.VersionedIssue{Issue: *issue}
}

// withRunVersion renders a single run with its CAS row version. Safe on nil.
func withRunVersion(run *model.Run) any {
	if run == nil {
		return nil
	}
	return model.VersionedRun{Run: *run}
}

// versionedRunWith renders a run with its version plus one sibling key beside
// the run's own fields. model.VersionedRun has its own MarshalJSON, which an
// embedding struct would inherit and so drop the sibling; the key is spliced
// into the encoded object instead.
func versionedRunWith(run *model.Run, key string, value any) ([]byte, error) {
	raw, err := json.Marshal(model.VersionedRun{Run: *run})
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	fields[key] = encoded
	return json.Marshal(fields)
}

// runAbandonPayload is `run abandon`'s response: the versioned run plus its
// `worktrees`, the checkouts the run's steps recorded that no close will ever
// sweep now (DKT-116). The key is present only when there are any.
type runAbandonPayload struct {
	run       *model.Run
	worktrees []string
}

func (p runAbandonPayload) MarshalJSON() ([]byte, error) {
	if len(p.worktrees) == 0 {
		return json.Marshal(model.VersionedRun{Run: *p.run})
	}
	return versionedRunWith(p.run, "worktrees", p.worktrees)
}

// runResumePayload is `run resume`'s response when the run's pins no longer
// match disk (DKT-408): the versioned run plus `pin_drift`, present only when
// something drifted. The drift also rides the human message, so neither
// channel resumes a wedged run silently.
type runResumePayload struct {
	run      *model.Run
	pinDrift []engine.PinVerdict
}

func (p runResumePayload) MarshalJSON() ([]byte, error) {
	if len(p.pinDrift) == 0 {
		return json.Marshal(model.VersionedRun{Run: *p.run})
	}
	return versionedRunWith(p.run, "pin_drift", p.pinDrift)
}

// abandonIssuePayload is `run abandon --issue`'s response: the outcome plus
// `worktrees` when there are any (DKT-116). The outcome itself tags Worktrees
// json:"-", so the key is added here.
type abandonIssuePayload struct{ outcome *engine.AbandonIssueOutcome }

func (p abandonIssuePayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		engine.AbandonIssueOutcome
		Worktrees []string `json:"worktrees,omitempty"`
	}{*p.outcome, p.outcome.Worktrees})
}

// issueListPayload wraps a slice of issues so a collection envelope reaches
// each item's version: the envelope consults output.Versioned on the items
// CONTAINER rather than on every element, so a bare []*Issue would render its
// items without one.
//
// Its own MarshalJSON is the unversioned array, which is what a payload that
// nests it as an ordinary field (a `board` column) emits.
type issueListPayload struct{ issues []*model.Issue }

// MarshalJSON emits the unversioned issue array. Never null.
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
