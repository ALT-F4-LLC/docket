package engine

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-546 — the run-scoped batch gate-override.
//
// The dominant operator toil the refit mining found was environmental gate
// parks resolved one at a time: the same "sandbox artifact, not a code defect"
// ruling re-made for every step of a run. `step resolve --as override-pass
// --batch` records that ruling ONCE, as one grant per failed gate (gate name +
// exit + reason — the failure signature), and the routing stage consults the
// run's grants before parking a later step whose failure carries the same
// signature.
//
// The gates STILL RUN. What the grant changes is the routing of a failure
// whose signature the operator already ruled on — a failure with a different
// signature parks exactly as before, which is what keeps a new real defect
// from sailing through under an old environmental ruling.
//
// THE COVERED STEP NEED NOT HAVE EXISTED WHEN THE GRANT WAS TAKEN (DKT-734).
// grantMatches compares the FAILURE SIGNATURE and nothing else — not the step,
// not the loop ordinal, not the round — because the scope rule lives one level
// up, in the grant's run_id. So a grant minted on a parked `fix@7` covers the
// `fix@8` a later fix round mints, which is what RUN-51 saw and misread as
// three authorizations. Deliberately so: an environmental failure does not
// become a code defect because the loop went around again, and expiring the
// grant per round would re-ask the identical settled question every round —
// the exact toil DKT-546 measured. The operator's protection is that the
// question stays settled only while the ANSWER does: a round whose gate fails
// with a different exit or reason parks for a fresh decision.

// failingCompletionRows reduces a step's recorded gate rows to the last-ordinal
// completion rows that did not pass — the rows a batch override grant records,
// and the rows it must cover to route a later step past its park. The same
// reduction verdictOverRows routes on: `pre` rows excluded, last ordinal per
// gate wins, so a flaky gate that failed twice and passed third is not failing.
func failingCompletionRows(rows []db.GateResultRow) []db.GateResultRow {
	last := make(map[string]db.GateResultRow)
	for _, r := range rows {
		if r.Pre {
			continue
		}
		prev, seen := last[r.Gate]
		if !seen || r.Ordinal >= prev.Ordinal {
			last[r.Gate] = r
		}
	}
	var failing []db.GateResultRow
	for _, r := range last {
		if r.Verdict != db.GateVerdictPass {
			failing = append(failing, r)
		}
	}
	// Sorted so the routing reason names the gates in the same order on every
	// run over the same rows — map range order is not.
	sort.Slice(failing, func(i, j int) bool {
		return failing[i].Gate < failing[j].Gate
	})
	return failing
}

// grantMatches reports whether one grant covers one failing row: same gate,
// same exit, same reason classification. NULL exit matches only NULL — an
// `unmatched` gate never ran, and "no process existed" is not exit 0.
func grantMatches(g db.GateOverrideGrant, r db.GateResultRow) bool {
	if g.Gate != r.Gate || g.Reason != r.Reason {
		return false
	}
	if (g.Exit == nil) != (r.Exit == nil) {
		return false
	}
	return g.Exit == nil || *g.Exit == *r.Exit
}

// batchCover is the routing stage's read of the run's grants against one
// failed step: which grants cover its failing gates, or why a full cover must
// not be auto-applied.
type batchCover struct {
	grantIDs []int
	gates    []string
	// blocked, when non-empty, is the park reason for a cover that matched but
	// must not auto-apply: the step's threshold interposes another step, and an
	// auto-applied override-pass would silently skip it — the DKT-470 defect,
	// at scale, with no operator present to read the warning.
	blocked string
}

// reason is the routing record an auto-pass carries, so the ledger shows every
// covered step and the grant(s) whose justification it rode on.
func (c *batchCover) reason() string {
	return fmt.Sprintf(
		"gate(s) %s failed with signature(s) covered by batch override "+
			"grant(s) %s; auto-passed per the operator's run-scoped ruling",
		strings.Join(c.gates, ", "), joinGrantIDs(c.grantIDs))
}

// eventData is the `step-batch-overridden` payload: the covering grant id(s).
func (c *batchCover) eventData() string {
	return joinGrantIDs(c.grantIDs)
}

func joinGrantIDs(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

// batchOverrideCover reads the run's grants against a step whose gate verdict
// is `fail` and reports the cover, nil when there is none.
//
// EVERY failing gate must match a grant, or nothing applies: a step that
// failed one ruled-on gate and one new one carries information the operator
// has not seen, and auto-passing it would spend the ruling past its terms. It
// reads on the pooled connection because the routing stage calls it before its
// transaction opens, exactly as gateVerdict reads.
func batchOverrideCover(
	conn *sql.DB, step *db.Step, spec *workflow.Step,
) (*batchCover, error) {
	grants, err := db.GateOverrideGrantsForRun(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		return nil, nil
	}
	rows, err := db.GateResultsForStep(conn, step.ID)
	if err != nil {
		return nil, err
	}
	failing := failingCompletionRows(rows)
	if len(failing) == 0 {
		return nil, nil
	}

	cover := &batchCover{}
	seen := make(map[int]bool)
	for _, r := range failing {
		matched := false
		for _, g := range grants {
			if grantMatches(g, r) {
				matched = true
				if !seen[g.ID] {
					seen[g.ID] = true
					cover.grantIDs = append(cover.grantIDs, g.ID)
				}
				break
			}
		}
		if !matched {
			return nil, nil
		}
		cover.gates = append(cover.gates, r.Gate)
	}

	// A threshold that interposes another step forbids the auto-apply
	// (DKT-470): override-pass records a generic `pass` without evaluating the
	// threshold, so the interposed step would be unconditionally skipped — and
	// unlike the operator's own override-pass, an auto-applied one has nobody
	// present to read OverridePassSkipsInterposedTargets' warning. The step
	// parks per its `on_fail` with the cover named, and the operator resolves
	// it individually, warning and all.
	if spec != nil {
		if targets := workflow.ThresholdTargets(spec.Threshold); len(targets) > 0 {
			cover.blocked = fmt.Sprintf(
				"batch override grant(s) %s cover the failed gate(s) %s, but "+
					"this step's threshold interposes %s, which an auto-applied "+
					"override-pass would silently skip (DKT-470); resolve "+
					"individually",
				joinGrantIDs(cover.grantIDs), strings.Join(cover.gates, ", "),
				strings.Join(targets, ", "))
		}
	}
	return cover, nil
}
