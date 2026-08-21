package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Run-scoped rollups for `run report` (docs/tdd/runs-dispatch.md §4.10).
//
// EVERY QUERY HERE ORDERS BY A TOTAL KEY — id, then name, then value — never by
// map iteration (R9). The report is deterministic given the same rows, for the
// same golden-stability reason `referencedSchemas` is ordered: a rollup that
// ranged a Go map would produce a different document on every invocation, and
// an operator diffing two reports would see noise.

// VerdictCount is one subject's pass/fail/unmatched tally.
type VerdictCount struct {
	Name      string `json:"name"`
	Pass      int    `json:"pass"`
	Fail      int    `json:"fail"`
	Unmatched int    `json:"unmatched"`
	// Skipped counts rows that MEASURED NOTHING (DKT-254). Before this column
	// a skipped row landed in none of the three above and vanished from the
	// report entirely — an absence that reads as green, which is the exact
	// inversion of what the verdict means.
	//
	// It partitions with pass/fail/unmatched: every row lands in exactly one
	// of the four. (`Stub` below does not — see its own note.)
	Skipped int `json:"skipped"`
	// Stub counts rows whose authorizing trust entry declared itself a
	// PLACEHOLDER (DKT-265). It is a count of ROWS, not of passes, so it
	// overlaps the three columns above rather than partitioning with them —
	// `pass 3, stub 3` means every one of those passes was hollow.
	//
	// It is zero for actions, which have no trust-entry stub declaration. That
	// is an honest zero and not a missing feature: an action's `builtin` column
	// already says whether core computed it.
	Stub int `json:"stub"`
}

// GateRollup counts a run's gate results per gate name (R4).
func GateRollup(db *sql.DB, runID int) ([]VerdictCount, error) {
	return verdictRollup(db, `gate_results`, `gate`, `stub_entry`, runID)
}

// ActionRollup is the same rollup over `action_results` (R5).
//
// The same shape, deliberately: gates and actions are the two execution seams
// and their results carry the same verdict vocabulary, so a reader who
// understands one section understands the other. Reusing the query rather than
// writing a second is what keeps the two from drifting into different
// definitions of "pass".
//
// `skipped` is counted for actions too, and is always zero there today: no
// action path produces that verdict. That is an honest zero and the right
// shape — the alternative, omitting the column for one of the two seams, is
// how the two definitions of "what outcomes exist" start to drift.
func ActionRollup(db *sql.DB, runID int) ([]VerdictCount, error) {
	// The empty stub column is the shape's one asymmetry, and it is a fact
	// about the tables rather than an omission: only a GATE runs a
	// trust-authorized command that an operator could have declared a stub.
	return verdictRollup(db, `action_results`, `action`, ``, runID)
}

// verdictRollup is the shared body. The table and column names are INTERNAL
// CONSTANTS from the two callers above, never caller data, so the interpolation
// carries no injection surface — and the query is parameterized on everything
// that does come from outside.
func verdictRollup(
	db *sql.DB, table, subject, stubColumn string, runID int,
) ([]VerdictCount, error) {
	// A table with no stub column selects the literal 0 rather than being
	// given a second query. One query means one definition of "pass" for both
	// seams, which is the property the shared body exists to hold.
	stubSum := `0`
	if stubColumn != "" {
		stubSum = fmt.Sprintf("SUM(CASE WHEN %s = 1 THEN 1 ELSE 0 END)", stubColumn)
	}
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %[2]s,
		        SUM(CASE WHEN verdict = 'pass' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN verdict = 'fail' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN verdict = 'unmatched' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN verdict = 'skipped'   THEN 1 ELSE 0 END),
		        %[3]s
		   FROM %[1]s WHERE run_id = ? GROUP BY %[2]s ORDER BY %[2]s`,
		table, subject, stubSum), runID)
	if err != nil {
		return nil, fmt.Errorf("rolling up %s for %s: %w",
			table, model.FormatRunID(runID), err)
	}
	return scanRows(rows,
		fmt.Sprintf("%s rollup for %s", table, model.FormatRunID(runID)),
		func(r *sql.Rows) (VerdictCount, error) {
			var v VerdictCount
			if err := r.Scan(
				&v.Name, &v.Pass, &v.Fail, &v.Unmatched, &v.Skipped, &v.Stub,
			); err != nil {
				return VerdictCount{}, fmt.Errorf("reading a %s rollup row: %w", table, err)
			}
			return v, nil
		})
}

// ResultTrailRow is one step's one result — the per-step trail R4 and R5 carry
// beside their counts.
//
// StepID and Issue joined on (DKT-77): instance names collide across issues in
// one run — two issues running the same workflow both have an `implement@0` —
// so a trail keyed on instance alone was unattributable. Output rides ONLY on
// rows that did not pass, as a bounded tail: a failing gate's diagnosis used
// to require re-running it out-of-band, which is at its most expensive
// exactly where a false failure blocks a security path.
type ResultTrailRow struct {
	Instance string `json:"step"`
	StepID   string `json:"step_id"`
	Issue    string `json:"issue"`
	Name     string `json:"name"`
	Ordinal  int    `json:"ordinal"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason,omitempty"`
	Output   string `json:"output,omitempty"`
}

// trailOutputTail bounds the failure output a trail row carries. The full
// capture stays in the result row itself; the report's job is diagnosis, not
// archival, and an unbounded tail would turn a status check into a log dump.
const trailOutputTail = 2000

func tailOf(s string) string {
	if len(s) <= trailOutputTail {
		return s
	}
	return "…" + s[len(s)-trailOutputTail:]
}

// GateTrail is R4's per-step trail, and ActionTrail is R5's.
func GateTrail(db *sql.DB, runID int) ([]ResultTrailRow, error) {
	return resultTrail(db, `gate_results`, `gate`, runID)
}

func ActionTrail(db *sql.DB, runID int) ([]ResultTrailRow, error) {
	return resultTrail(db, `action_results`, `action`, runID)
}

func resultTrail(db *sql.DB, table, subject string, runID int) ([]ResultTrailRow, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT s.instance, s.id, s.issue_id, r.%[2]s, r.ordinal, r.verdict,
		        r.reason, r.output
		   FROM %[1]s r JOIN steps s ON s.id = r.step_id
		  WHERE r.run_id = ?
		  ORDER BY s.id, r.%[2]s, r.ordinal`,
		table, subject), runID)
	if err != nil {
		return nil, fmt.Errorf("reading the %s trail for %s: %w",
			table, model.FormatRunID(runID), err)
	}
	return scanRows(rows,
		fmt.Sprintf("the %s trail for %s", table, model.FormatRunID(runID)),
		func(r *sql.Rows) (ResultTrailRow, error) {
			var (
				row     ResultTrailRow
				stepID  int
				issueID int
				reason  sql.NullString
				output  sql.NullString
			)
			if err := r.Scan(&row.Instance, &stepID, &issueID, &row.Name,
				&row.Ordinal, &row.Verdict, &reason, &output); err != nil {
				return ResultTrailRow{}, fmt.Errorf("reading a %s trail row: %w", table, err)
			}
			row.StepID = model.FormatStepID(stepID)
			row.Issue = model.FormatID(issueID)
			row.Reason = reason.String
			// A row that PASSED carries no output: the trail's output channel
			// exists for diagnosis, and a passing check's chatter is noise a
			// report reader pays for on every read.
			if row.Verdict != "pass" {
				row.Output = tailOf(output.String)
			}
			return row, nil
		})
}

// MetadataRollup is R7: step `metadata` keys rolled up to their distinct values
// with counts, VERBATIM AND UNINTERPRETED.
//
// THIS IS THE GENERICITY LINE AT ITS THINNEST, so it is worth stating what the
// implementation does and does not do. It groups by key and by value, both as
// OPAQUE STRINGS, and reports counts. It does not know that any particular
// instance puts anything in particular there: it reports `{"tier": {"a": 3,
// "b": 1}}` for exactly the same reason it would report `{"desk": {"front": 3,
// "back": 1}}`.
//
// TestMetadataRollupReadsNoKey asserts the implementation contains no key-name
// literal, which is the mechanical form of that promise — a rollup that
// special-cased one key would be core having an opinion about what a workflow
// author's bag of strings means.
//
// The nesting is key -> value -> count, and both levels are returned SORTED so
// R9's determinism holds through a two-level structure that a naive
// implementation would emit in map order.
func MetadataRollup(db *sql.DB, runID int) ([]MetadataKeyRollup, error) {
	rows, err := db.Query(
		`SELECT metadata FROM steps
		  WHERE run_id = ? AND metadata IS NOT NULL AND metadata != ''
		  ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading step metadata for %s: %w",
			model.FormatRunID(runID), err)
	}
	defer rows.Close()
	out, err := rollupMetadataRows(rows)
	if err != nil {
		return nil, fmt.Errorf("reading step metadata for %s: %w",
			model.FormatRunID(runID), err)
	}
	return out, nil
}

// VoteMetadataRollup is MetadataRollup over CAST VOTES: every metadata bag the
// run's vote-step proposals collected, keys to distinct values with counts,
// verbatim and uninterpreted (DKT-71).
//
// It exists because vote seats are the one spend the ledger cannot see: a
// vote step is never claimed, so nothing accrues usage rows for it, and until
// v13 nothing recorded what model a seat resolved to. The `--metadata` claim
// on `vote cast` closed the write half; this is the run-level read that makes
// routing drift measurable again — the same question the step rollup answers,
// asked of the casts.
//
// The proposals are selected by the caller-supplied idempotency scope and
// prefix — the engine owns that spelling (voteIdempotencyPrefix) and this
// package must not restate it. The same genericity line holds here as in
// MetadataRollup: keys and values are opaque strings, counted, never
// interpreted, and TestMetadataRollupReadsNoKey covers both readers.
func VoteMetadataRollup(db *sql.DB, scope, prefix string) ([]MetadataKeyRollup, error) {
	rows, err := db.Query(
		`SELECT v.metadata FROM votes v
		  WHERE v.metadata IS NOT NULL AND v.metadata != ''
		    AND v.proposal_id IN (
		        SELECT entity_id FROM idempotency_keys
		         WHERE scope = ? AND key LIKE ? ESCAPE '\')
		  ORDER BY v.id`, scope, escapeLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("reading vote metadata: %w", err)
	}
	defer rows.Close()
	out, err := rollupMetadataRows(rows)
	if err != nil {
		return nil, fmt.Errorf("reading vote metadata: %w", err)
	}
	return out, nil
}

// VoteUsageRollup sums the vote_usage ledger per unit over the run's
// vote-step proposals (DKT-95) — UsageByUnit's question, asked of the seats.
// The proposals are selected by the same caller-supplied idempotency scope
// and prefix VoteMetadataRollup reads by, and units stay opaque: summed and
// counted, never interpreted.
func VoteUsageRollup(db *sql.DB, scope, prefix string) ([]UnitTotal, error) {
	rows, err := db.Query(
		`SELECT vu.unit, SUM(vu.quantity), COUNT(*) FROM vote_usage vu
		  JOIN votes v ON v.id = vu.vote_id
		 WHERE v.proposal_id IN (
		       SELECT entity_id FROM idempotency_keys
		        WHERE scope = ? AND key LIKE ? ESCAPE '\')
		 GROUP BY vu.unit ORDER BY vu.unit`, scope, escapeLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("rolling up vote usage: %w", err)
	}
	return scanRows(rows, "vote usage rollup",
		func(r *sql.Rows) (UnitTotal, error) {
			var u UnitTotal
			if err := r.Scan(&u.Unit, &u.Quantity, &u.Rows); err != nil {
				return u, err
			}
			return u, nil
		})
}

// rollupMetadataRows is the one counting loop both rollups share: raw bags in,
// sorted key -> value -> count out.
func rollupMetadataRows(rows *sql.Rows) ([]MetadataKeyRollup, error) {
	counts := make(map[string]map[string]int)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("reading a metadata row: %w", err)
		}

		var bag map[string]any
		if err := json.Unmarshal([]byte(raw), &bag); err != nil {
			// A row whose metadata is not an object is SKIPPED rather than
			// failing the report. The bag is opaque and was never validated as
			// JSON at write time by anything this report can rely on, and a
			// read verb that refused because one row held odd bytes would be
			// useless during exactly the run an operator wants to inspect (R10).
			continue
		}
		for key, value := range bag {
			if counts[key] == nil {
				counts[key] = make(map[string]int)
			}
			counts[key][renderMetadataValue(value)]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]MetadataKeyRollup, 0, len(counts))
	for _, key := range sortedMapKeys(counts) {
		values := counts[key]
		rollup := MetadataKeyRollup{Key: key}
		for _, value := range sortedMapKeys(values) {
			rollup.Values = append(rollup.Values,
				MetadataValueCount{Value: value, Count: values[value]})
		}
		out = append(out, rollup)
	}
	return out, nil
}

// MetadataKeyRollup is one metadata key and every distinct value recorded under
// it, with counts.
type MetadataKeyRollup struct {
	Key    string               `json:"key"`
	Values []MetadataValueCount `json:"values"`
}

// MetadataValueCount is one value of one key, and how many steps carried it.
type MetadataValueCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// renderMetadataValue turns one opaque metadata value into the string the
// rollup groups by.
//
// A non-string value is rendered as its JSON, which keeps `1` and `"1"`
// DISTINCT — they are different values in the bag, and merging them would be
// the rollup deciding they mean the same thing.
func renderMetadataValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(encoded)
}

// sortedMapKeys orders one map level. Both levels of the rollup go through it,
// so R9's determinism holds through a two-level structure a naive
// implementation would emit in map order.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// VoteUsageCoverage is how many of a run's seat-casts reported their spend, and
// how many did not (DKT-257).
type VoteUsageCoverage struct {
	// Casts is every vote cast on this run's vote-step proposals.
	Casts int `json:"casts"`
	// Reported is the subset that recorded at least one vote_usage row.
	Reported int `json:"reported"`
}

// Silent is the gap: seats that deliberated and reported nothing.
func (c VoteUsageCoverage) Silent() int { return c.Casts - c.Reported }

// VoteUsageCoverageFor counts a run's seat-casts and how many reported spend.
//
// It exists because a MISSING report and a FREE panel were the same number
// (DKT-257). The vote_usage ledger has existed since v14 and held zero rows for
// an entire store epoch while 21+ seat-votes did real verification work: the
// table, the writer, and the rollup were all wired, and nothing supplied a
// number, so `vote_usage: 0` read as "this panel cost nothing".
//
// In-wave panels ARE counted, because their seats run as steps — RUN-22
// STEP-379/381 journaled ~39.5k output tokens to step_usage. Conductor-side
// panels of identical shape recorded 287/0. Roughly 40k tokens per panel that
// the run's budget never saw, and the only difference was WHERE the ballot
// executed, not how much work it did.
//
// Core cannot observe a conductor-side seat's spend, so it cannot fix the
// number. What it can do is stop the silence from looking like a zero, which is
// this: a run whose seats all reported reads `12/12`, and one whose seats
// reported nothing reads `0/12` instead of an absent section.
func VoteUsageCoverageFor(db *sql.DB, scope, prefix string) (VoteUsageCoverage, error) {
	var out VoteUsageCoverage
	err := db.QueryRow(
		// COALESCE, because SUM over ZERO ROWS is NULL in SQLite and a run with
		// no panels is the overwhelmingly common case. Without it every report
		// of such a run fails on a Scan — which is how this arrived: the first
		// implementation broke eight report tests at once, all of them runs
		// that had never opened a ballot.
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN EXISTS(
		              SELECT 1 FROM vote_usage vu WHERE vu.vote_id = v.id
		            ) THEN 1 ELSE 0 END), 0)
		   FROM votes v
		  WHERE v.proposal_id IN (
		        SELECT entity_id FROM idempotency_keys
		         WHERE scope = ? AND key LIKE ? ESCAPE '\')`,
		scope, escapeLike(prefix)+"%").Scan(&out.Casts, &out.Reported)
	if err != nil {
		return VoteUsageCoverage{}, fmt.Errorf("counting vote usage coverage: %w", err)
	}
	return out, nil
}
