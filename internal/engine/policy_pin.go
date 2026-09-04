package engine

import (
	"database/sql"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// policyForRun resolves a run's pinned policy.toml, or nil when the run has
// none pinned.
//
// A NIL POLICY IS DORMANCY, NOT AN ERROR (DKT-1282 AC3): a repo or corpus
// that ships no policy.toml — or a run activated before one existed — leaves
// every row's model/effort/variant absent, byte-identical to `next --run`
// before this feature existed. A wave launched against such a run has no
// policyText to fall back to either, so it routes every row through
// whatever engine-agnostic default it holds — the point of AC3 is that a
// run WITH a pinned policy.toml needs no separate text channel for it at
// all, not that every run must carry one.
//
// A run that DOES pin one and fails to resolve it — a hash mismatch against
// what activation recorded, the file missing from every instance-config
// root, or a malformed/underspecified policy.toml — returns the error
// verbatim rather than degrading to dormancy: a policy that cannot be read
// is not the same fact as a repo that never had one, and conflating them
// would route real rows against a policy nobody verified.
func policyForRun(conn *sql.DB, runID int) (*policyDoc, error) {
	pins, err := db.ListPins(conn, runID)
	if err != nil {
		return nil, err
	}
	pinned := packetPinsForRun(pins)
	if _, ok := pinned[policyPinRef]; !ok {
		return nil, nil
	}

	body, _, err := readPinnedPacketFile(
		model.FormatRunID(runID), pinned, instanceConfigRoots(), policyPinRef)
	if err != nil {
		return nil, err
	}
	return parsePolicy([]byte(body))
}

// ResolveSeats resolves each named seat against the run's pinned policy.toml,
// returning the same {model, effort, variant} a vote step row's
// voter_assignments carry.
//
// A CONVERSATIONAL GATE — ack-reap, activation, budget, fix-batch — has no
// step row for routing to ride on, so its panel is seated by a conductor whose
// contract forbids choosing a model, tier or effort. This is that lookup, from
// the run's own pinned policy: it calls the SAME (*policyDoc).ResolveSeat
// resolveRowPolicy calls, so the never-list merge, the [security] ceiling
// clamp and the [escalation.fallback] redirect apply identically.
//
// labels stands in for the issue labels a vote step row snapshots. A
// conversational gate carries none of its own, so the caller supplies whatever
// governs the [security] sensitivity check; empty means "not sensitive by
// label", exactly as an unlabelled row resolves.
//
// EVERY SEAT RESOLVES OR THE CALL FAILS. A caller building a judge panel needs
// the whole roster, and returning the seats that happened to resolve would
// seat a panel nobody chose while hiding which seat the policy has no row for.
func ResolveSeats(conn *sql.DB, runID int, seats, labels []string) ([]model.VoterAssignment, error) {
	if _, err := db.GetRun(conn, runID); err != nil {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	policy, err := policyForRun(conn, runID)
	if err != nil {
		return nil, err
	}
	// Dormancy is an ERROR HERE, unlike on the row path: a row with no policy
	// keeps its fields absent and the wave routes it by its own default, but a
	// caller asking this question has nothing to fall back to — an empty
	// answer would read as "these seats route to nothing".
	if policy == nil {
		return nil, notFoundErr(db.ErrNotFound,
			"run %s has no pinned policy.toml to resolve seats against",
			model.FormatRunID(runID))
	}

	out := make([]model.VoterAssignment, 0, len(seats))
	for _, seat := range seats {
		assignment, err := policy.ResolveSeat(seat, labels)
		if err != nil {
			return nil, notFoundErr(db.ErrNotFound,
				"resolving seat %q from %s's pinned policy.toml: %v",
				seat, model.FormatRunID(runID), err)
		}
		out = append(out, model.VoterAssignment{
			Voter: seat, Model: assignment.Model,
			Effort: assignment.Effort, Variant: assignment.Variant,
		})
	}
	return out, nil
}

// resolveRowPolicy fills Model/Effort/Variant (executor rows) and
// VoterAssignments (vote rows) on every row policy can resolve, in place.
//
// A row that is neither — a human step, an action step, a `type` step — is
// untouched: wave.js's resolve() only ever ran on executor rows and
// resolveSeat() only on vote rows, and this mirrors that exactly (DKT-1282
// AC1/AC3).
func resolveRowPolicy(policy *policyDoc, rows []model.StepRow) error {
	if policy == nil {
		return nil
	}
	for i := range rows {
		row := &rows[i]
		switch {
		case row.Executor != "":
			assignment, err := policy.ResolveExecutor(row.Executor, row.Attempt, row.Instance, row.Labels)
			if err != nil {
				return validationErr(
					"resolving %s@%s from the run's pinned policy.toml: %v", row.Step, row.Instance, err)
			}
			row.Model, row.Effort, row.Variant = assignment.Model, assignment.Effort, assignment.Variant
		case len(row.Voters) > 0:
			assignments := make([]model.VoterAssignment, 0, len(row.Voters))
			for _, voter := range row.Voters {
				assignment, err := policy.ResolveSeat(voter, row.Labels)
				if err != nil {
					return validationErr(
						"resolving %s@%s voter %q from the run's pinned policy.toml: %v",
						row.Step, row.Instance, voter, err)
				}
				assignments = append(assignments, model.VoterAssignment{
					Voter: voter, Model: assignment.Model, Effort: assignment.Effort, Variant: assignment.Variant,
				})
			}
			row.VoterAssignments = assignments
		}
	}
	return nil
}
