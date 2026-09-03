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
