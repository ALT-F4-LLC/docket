package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	// Pre counts rows recorded by the PRE-GATE phase (DKT-862). Like Stub it
	// counts ROWS and overlaps the four verdict columns rather than
	// partitioning with them, because it describes WHEN the row was produced,
	// not what it decided.
	//
	// It matters because a pre-gate NEVER ROUTES: §11.1 runs it at claim, with
	// its results carried into the step's context bundle, and PG4 excludes it
	// from the saga's verdict. Without this count `ac-commands: pass 0, fail 1`
	// read identically whether the failure blocked the step or was an advisory
	// input to it — RUN-61 had three such rows and a conductor nearly reported
	// a fix round as burned on one of them.
	//
	// It reads the SAME `pre` column `step show`'s `[pre]` marker reads, so the
	// two surfaces cannot disagree about which gates were advisory. It is zero
	// for actions, which have no pre phase.
	Pre int `json:"pre"`
}

// GateRollup counts a run's gate results per gate name (R4).
func GateRollup(db *sql.DB, runID int) ([]VerdictCount, error) {
	return verdictRollup(db, `gate_results`, `gate`, `stub_entry`, `pre`, runID)
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
	// The empty stub and pre columns are the shape's one asymmetry, and they
	// are a fact about the tables rather than an omission: only a GATE runs a
	// trust-authorized command that an operator could have declared a stub, and
	// only a gate has a pre phase.
	return verdictRollup(db, `action_results`, `action`, ``, ``, runID)
}

// verdictRollup is the shared body. The table and column names are INTERNAL
// CONSTANTS from the two callers above, never caller data, so the interpolation
// carries no injection surface — and the query is parameterized on everything
// that does come from outside.
func verdictRollup(
	db *sql.DB, table, subject, stubColumn, preColumn string, runID int,
) ([]VerdictCount, error) {
	// A table with no stub column selects the literal 0 rather than being
	// given a second query. One query means one definition of "pass" for both
	// seams, which is the property the shared body exists to hold. `pre` is
	// absent from `action_results` for the same reason and takes the same
	// treatment.
	stubSum, preSum := `0`, `0`
	if stubColumn != "" {
		stubSum = fmt.Sprintf("SUM(CASE WHEN %s = 1 THEN 1 ELSE 0 END)", stubColumn)
	}
	if preColumn != "" {
		preSum = fmt.Sprintf("SUM(CASE WHEN %s = 1 THEN 1 ELSE 0 END)", preColumn)
	}
	rows, err := db.Query(fmt.Sprintf(
		`SELECT %[2]s,
		        SUM(CASE WHEN verdict = 'pass' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN verdict = 'fail' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN verdict = 'unmatched' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN verdict = 'skipped'   THEN 1 ELSE 0 END),
		        %[3]s,
		        %[4]s
		   FROM %[1]s WHERE run_id = ? GROUP BY %[2]s ORDER BY %[2]s`,
		table, subject, stubSum, preSum), runID)
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
				&v.Pre,
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
//
// extraIDs widens the selection to proposals the caller attributed to the run
// some OTHER way than the vote-step key family (DKT-584): reap-ack ballots and
// conversational-gate proposals that name the run in their text. Membership is
// a set test per vote row, so an id also matched by the prefix counts once.
func VoteUsageRollup(db *sql.DB, scope, prefix string, extraIDs ...int) ([]UnitTotal, error) {
	clause, args := proposalMembership(scope, prefix, extraIDs)
	rows, err := db.Query(
		`SELECT vu.unit, SUM(vu.quantity), COUNT(*) FROM vote_usage vu
		  JOIN votes v ON v.id = vu.vote_id
		 WHERE `+clause+`
		 GROUP BY vu.unit ORDER BY vu.unit`, args...)
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

// proposalMembership builds the WHERE fragment that selects a run's vote
// casts: the idempotency-key family, plus any explicitly attributed proposal
// ids. One builder for both vote_usage readers, so the rollup and its
// coverage line cannot disagree about which casts belong to the run.
//
// The placeholder list is built from the COUNT of ids, never their values,
// and every id is bound — the discipline ProposalStatusesTx states.
func proposalMembership(scope, prefix string, extraIDs []int) (string, []any) {
	clause := `v.proposal_id IN (
	       SELECT entity_id FROM idempotency_keys
	        WHERE scope = ? AND key LIKE ? ESCAPE '\')`
	args := []any{scope, escapeLike(prefix) + "%"}
	if len(extraIDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(extraIDs)), ",")
		clause = "(" + clause + ` OR v.proposal_id IN (` + placeholders + `))`
		for _, id := range extraIDs {
			args = append(args, id)
		}
	}
	return clause, args
}

// ProposalIDsNaming returns the ids of proposals whose description or
// rationale names `token` as a whole word, ordered by id (DKT-584).
//
// It exists for the conversational-gate ballots a conductor opens with `vote
// create` — an activation panel, say — which carry no idempotency key and no
// step, and whose ONLY link to the run they gate is that their text names it
// ("activation panel for RUN-40"). The LIKE narrows candidates in SQL; the
// word-boundary check runs in Go because LIKE '%RUN-4%' would also match
// RUN-40, and a run must never inherit another run's panels.
func ProposalIDsNaming(db *sql.DB, token string) ([]int, error) {
	if token == "" {
		return nil, nil
	}
	pattern := "%" + escapeLike(token) + "%"
	rows, err := db.Query(
		`SELECT id, description, rationale FROM proposals
		  WHERE description LIKE ? ESCAPE '\' OR rationale LIKE ? ESCAPE '\'
		  ORDER BY id`, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("finding proposals naming %q: %w", token, err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var (
			id                     int
			description, rationale string
		)
		if err := rows.Scan(&id, &description, &rationale); err != nil {
			return nil, fmt.Errorf("reading a proposal naming %q: %w", token, err)
		}
		if namesToken(description, token) || namesToken(rationale, token) {
			out = append(out, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding proposals naming %q: %w", token, err)
	}
	return out, nil
}

// namesToken reports whether text contains token as a whole word: not
// preceded by an ASCII letter or digit, and not followed by a digit — so
// "RUN-4" never matches inside "RUN-40" or "XRUN-4", while "(RUN-4)" and
// "RUN-4," match.
func namesToken(text, token string) bool {
	for from := 0; ; {
		at := strings.Index(text[from:], token)
		if at < 0 {
			return false
		}
		start := from + at
		end := start + len(token)
		beforeOK := start == 0 || !isWordByte(text[start-1])
		afterOK := end == len(text) || !(text[end] >= '0' && text[end] <= '9')
		if beforeOK && afterOK {
			return true
		}
		from = start + 1
	}
}

// isWordByte is namesToken's boundary alphabet: ASCII letters and digits.
func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
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
			counts[key][RenderMetadataValue(value)]++
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

// RenderMetadataValue turns one opaque metadata value into the string the
// rollup groups by.
//
// A non-string value is rendered as its JSON, which keeps `1` and `"1"`
// DISTINCT — they are different values in the bag, and merging them would be
// the rollup deciding they mean the same thing.
//
// EXPORTED for DKT-868's per-step section, which renders the same column in the
// same document: the rollup spells a value one way and a report that spelled the
// per-step copy another would show a reader `1` in one section and `"1"` in the
// next and invite them to conclude the two came from different bags. One
// definition, so the two cannot disagree.
func RenderMetadataValue(v any) string {
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
// extraIDs widens the count to explicitly attributed proposals exactly as
// VoteUsageRollup's does (DKT-584), through the same membership builder — so
// a cast counted in the rollup is always counted in its coverage line.
func VoteUsageCoverageFor(db *sql.DB, scope, prefix string, extraIDs ...int) (VoteUsageCoverage, error) {
	clause, args := proposalMembership(scope, prefix, extraIDs)
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
		  WHERE `+clause,
		args...).Scan(&out.Casts, &out.Reported)
	if err != nil {
		return VoteUsageCoverage{}, fmt.Errorf("counting vote usage coverage: %w", err)
	}
	return out, nil
}

// SilentVoteSeatRow is one cast VoteUsageCoverageFor counts as silent: which
// seat, on which proposal, reported no spend at all (DKT-733).
//
// The coverage COUNT told an operator that 12 of 57 seats went silent and
// nothing anywhere said WHICH twelve — so the backfill verb that exists
// precisely to close the gap (`vote backfill-usage`, DKT-115) could not be
// aimed. This row is the identity behind the count.
//
// The seating path is deliberately NOT here: this package selects by an
// opaque scope and prefix and cannot know which key family or run-naming rule
// minted a proposal. The engine, which owns those spellings, labels each row
// (engine.SilentVoteSeat).
type SilentVoteSeatRow struct {
	ProposalID int
	Voter      string
	Role       string
}

// SilentVoteSeatsFor enumerates the casts VoteUsageCoverageFor counts as
// silent, through the SAME membership builder — so the list and the count it
// explains cannot disagree about which casts belong to the run. Ordered by
// (proposal, voter): a total key, per R9.
func SilentVoteSeatsFor(db *sql.DB, scope, prefix string, extraIDs ...int) ([]SilentVoteSeatRow, error) {
	clause, args := proposalMembership(scope, prefix, extraIDs)
	rows, err := db.Query(
		`SELECT v.proposal_id, v.voter_name, v.voter_role
		   FROM votes v
		  WHERE `+clause+`
		    AND NOT EXISTS(SELECT 1 FROM vote_usage vu WHERE vu.vote_id = v.id)
		  ORDER BY v.proposal_id, v.voter_name`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing silent vote seats: %w", err)
	}
	return scanRows(rows, "silent vote seats",
		func(r *sql.Rows) (SilentVoteSeatRow, error) {
			var row SilentVoteSeatRow
			if err := r.Scan(&row.ProposalID, &row.Voter, &row.Role); err != nil {
				return row, err
			}
			return row, nil
		})
}
