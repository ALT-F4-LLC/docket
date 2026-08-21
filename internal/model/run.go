package model

import (
	"encoding/json"
	"fmt"
)

// RunIDPrefix is the prefix run IDs render with, kept apart from IDPrefix so a
// `RUN-3` can never be mistaken for a `DKT-3` in an error string or an event.
const RunIDPrefix = "RUN"

// RunStatus is engine-core §1.1's run machine:
//
//	planning -> active <-> waiting-human -> done | abandoned
//
// All five exist at this stage. The step lifecycle routes into `waiting-human`
// and `abandoned` in phase 3; what stage 6 adds is the LEDGER ROLLUP, not the
// statuses.
type RunStatus string

const (
	RunPlanning     RunStatus = "planning"
	RunActive       RunStatus = "active"
	RunWaitingHuman RunStatus = "waiting-human"
	RunDone         RunStatus = "done"
	RunAbandoned    RunStatus = "abandoned"
)

// RunPauseOrigin records WHERE a `waiting-human` run's park was decided
// (DKT-305). It is a fact about the PARK, not about the run: a run that is not
// parked carries the empty origin, and so does one parked by its own steps.
//
// The distinction exists because the two kinds of park have opposite
// resolutions. A park the rollup created — some step is `waiting-human` — is
// one the rollup also RESOLVES: when the step moves, the run returns to
// `active` on its own, and it must, or every step-level park would need an
// operator to type `run resume` for a decision they never made. A park decided
// at the RUN level parks no step at all, so nothing in the step tables records
// it and the rollup's "any step still parked" count reads zero — which is how
// an operator's `run pause` came to be silently undone by the next step to
// finish (RUN-31, seq 3054 paused, seq 3077 resumed by nobody).
//
// So the row states which decision it was, and the rollup resumes only what it
// parked itself.
type RunPauseOrigin string

const (
	// RunPauseOriginNone is a run whose `waiting-human` (if any) came from its
	// STEPS, and the empty default every unparked run carries.
	RunPauseOriginNone RunPauseOrigin = ""
	// RunPauseOriginOperator is `docket run pause` — a person decided.
	RunPauseOriginOperator RunPauseOrigin = "operator"
	// RunPauseOriginBudget is a budget breach (DKT-68's case): the engine
	// flipped the run row and parked no step, and only `run resume` or a
	// breach-resolving cap raise moves it.
	RunPauseOriginBudget RunPauseOrigin = "budget"
)

// RunLevel reports whether this origin names a park the RUN machine decided,
// which is exactly the set the reconciliation rollup must not auto-resume.
func (o RunPauseOrigin) RunLevel() bool { return o != RunPauseOriginNone }

// runStatuses is the closed set, in machine order.
var runStatuses = []RunStatus{
	RunPlanning, RunActive, RunWaitingHuman, RunDone, RunAbandoned,
}

// ValidateRunStatus returns an error if s is not a recognized run status.
func ValidateRunStatus(s RunStatus) error {
	return validateOneOf(s, runStatuses, "run status")
}

// Terminal reports whether a run has reached an end state. A terminal run
// refuses re-activation (RA5) and drops out of `run status --active`.
func (s RunStatus) Terminal() bool {
	return s == RunDone || s == RunAbandoned
}

// Run is one `runs` row (TDD §5.1).
type Run struct {
	ID int
	// ProjectID is the v12 tenancy dimension; zero normalizes to the default
	// project on write. The engine derives every per-project decision from
	// THIS field — never from ambient state — so a verb operating on another
	// project's run stays correct.
	ProjectID int
	Request   string
	Status    RunStatus
	Reason    string
	// Budget is `run start --budget N`. It is STORED and enforces nothing
	// until S6 — accepting it now means the S6 upgrade adds enforcement
	// rather than a flag, and a flag appearing later would break the
	// `run start` invocation an S3-era harness scripted.
	Budget float64
	// UsageBudget is the cap over MEASURED usage — what the ledger recorded —
	// as opposed to Budget, which counts declared expected costs (DKT-238).
	//
	// Two dimensions because the two quantities are not commensurable: a raise
	// tribunal deliberated 140 -> 280 declared units while the same run's
	// measured spend ran to hundreds of millions of tokens. Collapsing them
	// into one number makes the token count swamp the declared discipline and
	// leaves neither question answerable. Zero means unlimited, as with Budget.
	UsageBudget   float64
	ActivatedAtMS *int64
	CreatedAtMS   int64
	UpdatedAtMS   int64
	RowVersion    int
	// Execution context (v12, G8): where and on what the run started. Several
	// worktrees of one project share a store, and a run that cannot say which
	// checkout it came from cannot be audited. All empty on runs created
	// before v12 or outside a checkout.
	ExecRoot  string
	Branch    string
	CommitSHA string
	Hostname  string
}

// FormatRunID renders a run's display identity.
func FormatRunID(id int) string {
	return fmt.Sprintf("%s-%d", RunIDPrefix, id)
}

// Ref renders this run's display identity.
func (r *Run) Ref() string { return FormatRunID(r.ID) }

// ParseRunID accepts `RUN-3` or a bare `3`, mirroring ParseID for issues so an
// operator's muscle memory carries over.
func ParseRunID(s string) (int, error) {
	return parseRefID(s, RunIDPrefix, "run")
}

// IssueRunDisposition is a RUN's terminal ruling on its work on ONE issue
// (DKT-404) — the fact the tracker row cannot state and readers kept guessing
// wrong about.
//
// It says "a run stopped working this issue", never "the issue is finished".
// The two abandon paths deliberately leave the issue's own status alone (see
// engine's abandonIssue), so an abandoned issue can sit at `todo` or `review`
// forever with nothing on the row explaining why nobody is moving it. RUN-14
// left four issues at `todo` after re-planning their work into replacements,
// and a later session read RUN-32's two operator-abandoned issues as "still
// parked on a gate that was never resolved" and re-asked both decisions.
//
// It lives in model rather than beside engine's report types because BOTH
// halves of the read — the engine query that recovers it and the renderer that
// prints it — need the same shape, and internal/render deliberately imports
// nothing heavier than this package.
type IssueRunDisposition struct {
	// RunID is the run that made the ruling. Not necessarily the LAST run to
	// touch the issue: an issue abandoned in RUN-14 and never resurfaced still
	// reports RUN-14, which is precisely the provenance a reader is missing.
	RunID int
	// Disposition is the ruling itself. Abandonment is the only one core
	// records as an event today (engine.DispositionAbandoned); a field rather
	// than a presence flag so a second ruling needs no second surface.
	Disposition string
	// By is the step instance whose routing abandoned the issue, empty when an
	// operator abandoned it from outside the graph with `run abandon --issue`
	// — where naming a step would be a fabrication.
	By string
	// Reason is the recorded ruling VERBATIM, from whichever of the two places
	// the deciding path left it. Empty when the path recorded no note.
	Reason string
	// AtMS is when the ruling was recorded, in epoch milliseconds — the event's
	// own timestamp, not the issue's `updated_at`.
	AtMS int64
}

// StatusCount is one status and how many rows carry it — the shape `run status`
// rolls steps up into.
type StatusCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// runJSON is the v1 wire shape of a run.
//
// `reason` and `activated_at_ms` are omitted when absent rather than emitted as
// null, matching the v6 `lease` object's rule: a field that is not a fact yet
// does not appear.
type runJSON struct {
	Run           string  `json:"run"`
	Request       string  `json:"request,omitempty"`
	Status        string  `json:"status"`
	Reason        string  `json:"reason,omitempty"`
	Budget        float64 `json:"budget,omitempty"`
	ActivatedAtMS *int64  `json:"activated_at_ms,omitempty"`
	CreatedAtMS   int64   `json:"created_at_ms"`
	UpdatedAtMS   int64   `json:"updated_at_ms"`
}

// MarshalJSON renders the v1 shape.
func (r *Run) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.wire())
}

func (r *Run) wire() runJSON {
	return runJSON{
		Run:           r.Ref(),
		Request:       r.Request,
		Status:        string(r.Status),
		Reason:        r.Reason,
		Budget:        r.Budget,
		ActivatedAtMS: r.ActivatedAtMS,
		CreatedAtMS:   r.CreatedAtMS,
		UpdatedAtMS:   r.UpdatedAtMS,
	}
}

// runVersionedJSON adds the CAS row version, which surfaces under --json=v2
// only (engine-spec §5) — v1 never consults the Versioned interface.
type runVersionedJSON struct {
	runJSON
	RowVersion int `json:"row_version"`
}

// VersionedPayload implements output.Versioned.
func (r *Run) VersionedPayload() any {
	return runVersionedJSON{runJSON: r.wire(), RowVersion: r.RowVersion}
}

// VersionedRun renders a run under --json=v2. It exists so a LIST's items each
// carry the row version: the v2 collection envelope consults output.Versioned
// on the items CONTAINER, not on every element, so a bare []*Run would render
// v1 items inside a v2 envelope.
type VersionedRun struct{ Run }

// MarshalJSON emits the v2 shape for one run.
func (v VersionedRun) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.Run.VersionedPayload())
}

// RunsWithVersion wraps runs for v2 marshaling.
func RunsWithVersion(runs []*Run) []VersionedRun {
	return withVersionSlice(runs, func(r *Run) VersionedRun {
		return VersionedRun{Run: *r}
	})
}
