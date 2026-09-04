package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `run report` — the ledger rollup (engine-core §1.1, §3.5; TDD §4.10).
//
// THE REPORT WRITES NOTHING (R8). It computes effective status at read —
// engine-spec §2's "read verbs render effective status … no write" — and opens
// a transaction it ROLLS BACK unconditionally. `TestReadVerbsWriteNothing`
// snapshots the database's page-level content before and after, which is the
// mechanical form of that promise: a report that reaped a lease would make "I
// only looked at it" untrue, and would let polling a rollup advance an attempt
// counter.
//
// IT WORKS ON A RUN IN ANY STATUS (R10), including `planning` — everything zero
// — and `abandoned` — the trail up to abandonment. A report that refused on a
// non-terminal run would be useless during exactly the run an operator wants to
// inspect.

// RunBudgetReport is R2: the budget section.
//
// Every number here is BARE. There is no currency, no token, and no rate: what
// the numbers count is the workflow author's business (§1.1's second leak,
// closed), and the report's job is to publish them so an instance can compute
// its own policy — a warn threshold, say — from a read verb (B27, B28).
type RunBudgetReport struct {
	// Cap is the effective cap, and Source is where it came from — R6's line,
	// which exists because B3's pinning property is surprising: a run started
	// before `budget.default` was set stays unlimited after. An operator asking
	// "why didn't it stop?" reads the answer here rather than in the source.
	Cap    float64 `json:"cap"`
	Source string  `json:"cap_source"`
	// Floor is §4.3's SUM over claim events — recomputed here, through the same
	// query enforcement runs, never read from `runs.usage_floor`. The cache
	// exists for the burn-rate line's convenience and a report that read it
	// would be publishing a number no decision was made against.
	Floor float64 `json:"floor"`
	// Reported is per-unit and NEVER summed across units (B19, §4.5): summing
	// {tokens: 4000, seconds: 12} to 4012 would be core asserting those add up.
	Reported []db.UnitTotal `json:"reported,omitempty"`
	// Unit is `budget.unit` — which of the above, if any, the cap counts.
	Unit string `json:"budget_unit,omitempty"`
	// Spend is `max(reported, floor)`: the quantity the cap is compared to.
	Spend float64 `json:"spend"`
	// BurnRate is floor per wall-clock hour. It is published and NOT projected:
	// core does not predict when a run will breach, because that is a policy
	// computation over a published number (§14).
	BurnRate float64 `json:"burn_rate"`
	// BreachReason is set only on a run a budget actually paused.
	BreachReason string `json:"breach_reason,omitempty"`
	// The MEASURED dimension (DKT-238): a second, independent cap over what
	// the ledger recorded rather than what step definitions declared.
	//
	// Beside the numbers above rather than folded into them, because they are
	// not commensurable — a raise tribunal deliberating 280 declared units
	// against a run that measured hundreds of millions of tokens was
	// deliberating over a proxy, and its own security seat said so. All three
	// are absent when the dimension is dormant.
	UsageCap   float64 `json:"usage_cap,omitempty"`
	UsageUnit  string  `json:"usage_budget_unit,omitempty"`
	UsageSpend float64 `json:"usage_spend,omitempty"`

	// VoteUsageNote states, on any run whose panels cast, that the seats'
	// measured spend (the report's `vote_usage` section) is EXCLUDED from
	// `reported` and `spend` — and why (DKT-584).
	//
	// It is a note rather than a fold because `Reported` must remain exactly
	// the rows enforcement's own snapshot sums (the step usage_ledger): the
	// report publishing a "reported" no decision was made against is the
	// two-sources-of-truth failure RunFloorTx's export exists to prevent. The
	// seats' DECLARED cost reaches the budget through the floor instead — a
	// vote step's expected_cost accrues at materialization — so the panel is
	// not invisible; its measured spend is simply accounted in its own
	// section, and this line says so instead of leaving the omission silent.
	VoteUsageNote string `json:"vote_usage_note,omitempty"`
}

// VoteUsageExcludedNote is VoteUsageNote's one value, a constant so the JSON
// document and the rendered report cannot say it differently.
const VoteUsageExcludedNote = "vote_usage is excluded from reported and spend: " +
	"those sum the step usage_ledger the cap enforcement reads, and seat casts " +
	"land in the separate vote_usage ledger; a vote step's declared " +
	"expected_cost accrues to the floor instead"

// RunReport is the whole document — R1 through R7, in that order.
type RunReport struct {
	Run *model.Run `json:"run"`
	// WallClockMS is activation -> now, or activation -> the terminal
	// transition. A run that never activated has none, and the field is absent
	// rather than zero: a field that is not a fact does not appear, which is the
	// v6 `lease` object's rule applied here.
	WallClockMS int64 `json:"wall_clock_ms,omitempty"`

	Budget RunBudgetReport `json:"budget"`

	// PinnedWorkflows is DKT-594's first half: per pinned workflow, how many
	// registered versions the corpus has advanced since this run froze.
	//
	// It rides in the report rather than in `verify-pins` because the question
	// is not about DRIFT. A pinned `ui-change@8` whose file is byte-identical to
	// what the registry holds is perfectly sound and can still be five versions
	// behind, and `verify-pins` — which compares hashes at one ref — is right to
	// call it `ok`. What a post-mortem reader needs before trusting a finding is
	// the other number, and until this section it existed in no read verb: every
	// analyst on RUN-32 recovered it from git by hand.
	PinnedWorkflows []PinnedWorkflowStaleness `json:"pinned_workflows,omitempty"`

	// PinEpochs is DKT-594's second half: the run's pin-agreement timeline,
	// PRESENT ONLY ON A RUN THAT REPINNED.
	//
	// A run whose agreement never moved has one epoch, every step ran under it,
	// and the `pins` table already says what it was — so the section is absent
	// and each step's `pin_epoch` is absent with it, rather than a column of 1s
	// on every report in the store. Where it IS present it is what RUN-39's
	// analysts assembled by hand from event seqs and step ids.
	PinEpochs []PinEpoch `json:"pin_epochs,omitempty"`

	// Steps is R3: the count by EFFECTIVE status, computed at read.
	Steps []model.StatusCount `json:"steps,omitempty"`
	// Attempts is R3's other half: per-step attempts, ordered by instance.
	Attempts []StepAttempt `json:"attempts,omitempty"`

	// Issues is R3's issue-level half (DKT-403): the TERMINAL RULINGS an
	// operator made about whole issues, which the step sections cannot carry.
	//
	// A step's row says how that step ended. It does not say that the question
	// the step parked on was later answered — and for the two abandon paths it
	// cannot: `run abandon --issue` terminalizes every remaining step WITHOUT
	// touching `routing`, so a step parked with "loop 4 would exceed
	// max_fix_loops" keeps rendering that park text as though it still stood,
	// and the operator's actual ruling lived only in an `issue-abandoned`
	// event that no section of this document read. RUN-32 shipped exactly that
	// shape: two resolved gates, both reported as open, and the next session
	// re-asked two decisions the operator had already made.
	Issues []IssueDisposition `json:"issues,omitempty"`

	// Actors is E21 (docs/tdd/runs-dispatch.md §8.7): the per-actor event
	// counts, folded into R3's neighborhood.
	//
	// It publishes §9 item 2's answer as a NUMBER rather than as an argument.
	// "Every transition in events traceable to next/gate/threshold/human input"
	// is checkable from this section alone: the four counts sum to the run's
	// event total, and an event attributable to nothing would make them not.
	// The events read surface is where an auditor goes for the individual rows;
	// this is the rollup that says whether they need to.
	Actors []ActorCount `json:"actors,omitempty"`

	Gates       []db.VerdictCount   `json:"gates,omitempty"`
	GateTrail   []db.ResultTrailRow `json:"gate_trail,omitempty"`
	Actions     []db.VerdictCount   `json:"actions,omitempty"`
	ActionTrail []db.ResultTrailRow `json:"action_trail,omitempty"`

	// Artifacts is R6: the INDEX — id, kind, producer instance, sha256, bytes.
	// NEVER THE BODIES. A rollup that inlined artifact bodies would turn a
	// status check into a document dump, and the bodies are one `step context`
	// away for anyone who wants them.
	Artifacts []ArtifactIndexEntry `json:"artifacts,omitempty"`

	// Metadata is R7, the genericity line at its thinnest: keys to distinct
	// values with counts, verbatim and uninterpreted.
	Metadata []db.MetadataKeyRollup `json:"metadata,omitempty"`

	// VoteMetadata is the same rollup over CAST VOTES (DKT-71): every
	// `--metadata` claim the run's vote-step proposals collected. Vote seats
	// are the one spend the usage ledger cannot see — a vote step is never
	// claimed — so this is where tribunal routing and cost claims become
	// verifiable from the run document.
	VoteMetadata []db.MetadataKeyRollup `json:"vote_metadata,omitempty"`

	// VoteUsage is DKT-95: the seats' own spend reports, summed per unit —
	// UsageByUnit's question asked of the casts, from the vote_usage ledger
	// (usage_ledger cannot key a seat: a vote step's attempt is permanently
	// 0). Beside VoteMetadata, this is what makes tribunal cost measurable
	// from the run document instead of an absent zero awaiting an operator's
	// backfill.
	VoteUsage []db.UnitTotal `json:"vote_usage,omitempty"`

	// VoteUsageCoverage is how many seat-casts reported their spend (DKT-257).
	//
	// It is NOT omitempty, and that is the whole point. `vote_usage` is
	// `omitempty`, so a run whose seats all reported nothing carried no key at
	// all — and an absent section reads as "no panels ran", which is a
	// different and much more comfortable claim than "panels ran and none of
	// them said what they cost". The ledger held ZERO rows for an entire store
	// epoch while 21+ seat-votes did real verification work, and nothing
	// anywhere distinguished that from a run with no panels.
	//
	// Core cannot supply the missing numbers — it cannot observe a
	// conductor-side seat's spend — but it can stop the silence from looking
	// like a zero.
	VoteUsageCoverage db.VoteUsageCoverage `json:"vote_usage_coverage"`

	// SilentVoteSeats is the identity behind VoteUsageCoverage.Silent
	// (DKT-733): each cast that reported no spend — which seat, on which
	// proposal, seated via which path. The count alone told an operator that
	// seats went silent on a run and nothing said WHICH, so `vote
	// backfill-usage` — the verb that exists to close exactly this gap
	// (DKT-115) — could not be aimed without spelunking proposals by hand.
	//
	// `omitempty`: a run whose every seat reported carries no key, because the
	// coverage line already says so and an empty list would restate it.
	SilentVoteSeats []SilentVoteSeat `json:"silent_vote_seats,omitempty"`

	// StepUsage is the ledger row by row — which step, which attempt, which
	// unit, how much, and who measured it. Budget.Reported is the same rows
	// summed per unit; this is the detail behind that headline.
	//
	// It exists because NOTHING exposed per-step usage (DKT-241). A
	// back-fill's duplicate refusal tells an operator to "check `docket run
	// report` before re-running", and that was impossible advice: the report
	// answered only per-unit totals, so the question the refusal raises —
	// WHICH steps already have usage — could be answered from no read verb at
	// all, and conductors hand-filtered batches by trial and error across
	// three sessions. `omitempty`, so a run whose ledger is empty is unchanged.
	StepUsage []db.StepUsageRow `json:"step_usage,omitempty"`
}

// ActorCount is one row of E21's rollup: a cause, and how many of the run's
// transitions it accounts for.
//
// The actor is a string on the wire rather than an enum, matching every other
// count row in this document, and the ORDER is a total one (§4.10 R9) so two
// reports of the same rows are byte-identical.
type ActorCount struct {
	Actor string `json:"actor"`
	Count int    `json:"count"`
}

// StepAttempt is one step's attempt count and how it ended.
//
// `Routing` is the DKT-258 addition, and it is what makes the other three
// readable. A status alone collapses outcomes that need opposite responses:
// `skipped` is a tribunal that never convened AND one whose panel deliberated
// and was then resolved by an operator; `failed-routed` is a step that was
// measured and came back bad AND one that was cascade-terminated by an
// issue-abandon without ever being claimed. The engine records why in the
// step's own `routing`, and the report simply never carried it.
type StepAttempt struct {
	Step     string `json:"step"`
	Instance string `json:"instance"`
	// Issue is the issue this step was expanded for.
	//
	// It rides here for the collision instance labels have everywhere: two
	// issues on one workflow share every instance name, so `reconcile@3` alone
	// does not identify a row. It is also the JOIN KEY a reader needs to carry
	// an issue-level disposition (below) back onto the step that parked on it.
	Issue    string `json:"issue,omitempty"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	// Routing is the step's recorded routing, with its reason when one was
	// given — `abandon-issue: the issue was abandoned...`, `skip: operator
	// selected ...`. Empty for a step that has not been routed yet, which is
	// itself the answer for a `pending` or `claimed` row.
	Routing string `json:"routing,omitempty"`
	// Vote is a vote step's proposal and how it tallied, `DKT-V38 rejected`.
	//
	// A vote step's `attempts` is permanently 0 — it is never claimed — so the
	// count that tells every other row apart from a row that did nothing says
	// nothing here. RUN-22's verify-tribunal rendered `skipped, attempts: 0`
	// after DKT-V38 convened, three seats deliberated for ~39.5k output tokens,
	// and the panel rejected 0-3-0; the `skipped` was the operator's post-tally
	// resolve. Beside `Routing` this carries BOTH facts, which is what the
	// issue asks for: the panel decided, and then a person disposed of it.
	//
	// Empty for every non-vote step, and for a vote step whose proposal was
	// never opened — which is exactly the never-convened case the reader needs
	// to tell apart.
	Vote string `json:"vote,omitempty"`
	// PinEpoch is WHICH PIN AGREEMENT this step's recorded work ran under
	// (DKT-594), indexing RunReport.PinEpochs.
	//
	// Absent unless the run actually repinned, and absent on a step that has not
	// run — see PinEpochs and stepReportsAnEpoch. On a run whose agreement moved
	// mid-flight it is the field that says which bytes a completed step
	// consumed: `pins` holds only the CURRENT agreement, completed steps' rows
	// are never rewritten, and correlating the two was the hand-join RUN-39's
	// post-mortem performed against event seqs (5375/5376 vs STEP-1350/1353).
	PinEpoch int `json:"pin_epoch,omitempty"`

	// Metadata is THIS STEP'S WHOLE BAG, verbatim (DKT-868) — the detail behind
	// RunReport.Metadata exactly as StepUsage is the detail behind
	// Budget.Reported.
	//
	// The rollup answers "which values did this key take, and how often". It
	// cannot answer "which values did two keys take TOGETHER on one step",
	// because grouping by key is precisely what discards the pairing. Any bag
	// whose keys are a REQUEST and its RESOLUTION — the shape the corpus
	// actually writes — is therefore unaggregatable from the report: RUN-51's
	// rollup showed one key with no `low` value and its partner with one, a
	// mismatch on exactly one step that no reader could name. Recovering it
	// meant `step show` per step, and the audit that motivated this ran ~90 of
	// them across 19 runs.
	//
	// It rides on the ATTEMPT ROW rather than in a section of its own so the
	// bag arrives already joined to the four facts that make it interpretable:
	// effective status, routing, attempt count and issue. That is what closes
	// the other half of the gap — a step that FAILED or was reaped carries only
	// what its dispatcher recorded at claim (`step claim --metadata`, DKT-592),
	// and in a rollup that half-bag is indistinguishable from a completed
	// step's, so drift concentrated in failures reads as no drift at all.
	//
	// CORE READS NO KEY HERE, as everywhere (docs/design/genericity.md, R7). It
	// publishes the bag; what a pair of keys MEANS — a tier, a variant, a desk
	// — stays the workflow author's business, and the consumer does the
	// comparison core must not learn how to make.
	Metadata map[string]any `json:"metadata,omitempty"`

	// MetadataUnreadable marks a step whose stored bag exists but does not
	// decode, so an absent `metadata` is never silently read as "the dispatcher
	// recorded nothing" — the exact ambiguity DKT-868 is about. It mirrors
	// model.Vote's field of the same name and the same purpose.
	//
	// The bag is NOT re-validated here: a read verb that refused because one
	// row held odd bytes would be useless during exactly the run an operator
	// wants to inspect (R10), which is why db.MetadataRollup skips such a row
	// too. This row says so out loud rather than skipping silently, because a
	// per-step row IS the row — there are no other rows to carry the fact.
	MetadataUnreadable bool `json:"metadata_unreadable,omitempty"`
}

// DispositionAbandoned is the one issue-level terminal ruling core records as
// an event, and therefore the only one this section can report.
//
// An issue that COMPLETED leaves an activity-log row and a trail comment and no
// event, and it needs none: its steps are `done` and the step sections already
// say so. Abandonment is the asymmetric case — the steps go terminal carrying
// whatever they last said, and the ruling that terminalized them is recorded
// nowhere a run reader looks.
const DispositionAbandoned = "abandoned"

// IssueDisposition is one issue's terminal ruling within this run (DKT-403).
//
// It is a statement about THE RUN'S work on the issue, not about the issue: the
// `abandon-issue` routing deliberately does not force the issue itself to a
// terminal status (see abandonIssue), so this says "this run stopped" and the
// tracker says what became of the issue afterwards.
type IssueDisposition struct {
	Issue string `json:"issue"`
	// Disposition is the ruling. One value today — the constant above — and a
	// field rather than a bare presence flag, so a second issue-level ruling
	// can land here instead of forcing a second section.
	Disposition string `json:"disposition"`
	// By is the step instance whose routing abandoned the issue, or empty when
	// an operator abandoned it from OUTSIDE the graph with `run abandon
	// --issue` — where no step decided anything and naming one would be a
	// fabrication.
	By string `json:"by,omitempty"`
	// Reason is the recorded ruling, VERBATIM and unabridged. Both abandon
	// paths capture it and neither published it: `run abandon --issue` puts it
	// in the event payload, the routing path in the deciding step's own
	// routing note. A renderer may show a head; this carries the whole thing.
	Reason string `json:"reason,omitempty"`
}

// ArtifactIndexEntry is R6's row: what was produced, by whom, and how big —
// never the body.
//
// Executor and Issue attribute the producer (DKT-79): `producer` alone is the
// fanout ordinal (`review@0#2`), which says WHERE in the topology an artifact
// came from and nothing about WHO — the opaque executor hint the definition
// declared is the axis a judge-value question actually groups by. Issue rides
// for the same collision instance labels have everywhere: two issues on one
// workflow share every instance name.
type ArtifactIndexEntry struct {
	Artifact string `json:"artifact"`
	Kind     string `json:"kind"`
	Producer string `json:"producer,omitempty"`
	Executor string `json:"executor,omitempty"`
	Issue    string `json:"issue,omitempty"`
	SHA256   string `json:"sha256"`
	Bytes    int    `json:"bytes"`
	// Supersedes names the artifact this one REVISES, e.g. `ARTIFACT-71`
	// (DKT-70). A held cluster's resolution records a new artifact rather than
	// annotating the old one, and the two share a kind and a sha256 — so this
	// index showed one operator decision as a second unit of work, and ledger
	// mining, which counts artifacts as evidence of work, counted it twice.
	// A rollup counting work should skip entries that carry it.
	Supersedes string `json:"supersedes,omitempty"`
}

// LoadRunReport builds the document. IT WRITES NOTHING.
func LoadRunReport(conn *sql.DB, runID int, nowMS int64) (*RunReport, error) {
	run, err := db.GetRun(conn, runID)
	if err != nil {
		return nil, err
	}

	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}
	report := &RunReport{Run: run}

	// ---- R2's config-and-rollup half, BEFORE any transaction opens. --------
	//
	// internal/db caps the connection pool at ONE connection, so a pool read
	// from inside an open transaction deadlocks permanently rather than
	// failing. TestNoPoolReadsInsideTransactions enforces that lexically —
	// which is the right conservatism: a rollback that silently failed would
	// leave the transaction open and turn the next of these into the deadlock.
	//
	// Nothing here needs the snapshot. These are rollups over append-only
	// result tables that no read can change, so reading them a moment early
	// costs nothing and keeps the discipline unambiguous.
	configDefault, err := configuredBudgetDefault(conn, run.ProjectID)
	if err != nil {
		return nil, err
	}
	reported, err := db.UsageByUnit(conn, runID)
	if err != nil {
		return nil, err
	}
	facts, err := db.RunBudgetFactsFor(conn, runID)
	if err != nil {
		return nil, err
	}
	if report.Gates, err = db.GateRollup(conn, runID); err != nil {
		return nil, err
	}
	if report.GateTrail, err = db.GateTrail(conn, runID); err != nil {
		return nil, err
	}
	if report.Actions, err = db.ActionRollup(conn, runID); err != nil {
		return nil, err
	}
	if report.ActionTrail, err = db.ActionTrail(conn, runID); err != nil {
		return nil, err
	}
	if report.Metadata, err = db.MetadataRollup(conn, runID); err != nil {
		return nil, err
	}
	if report.VoteMetadata, err = db.VoteMetadataRollup(
		conn, db.ScopeVoteCreate, voteIdempotencyPrefix(runID)); err != nil {
		return nil, err
	}
	// DKT-584: the vote-step key family alone missed every panel the run's
	// machinery convened OUTSIDE a vote step — reap-ack ballots, and the
	// conversational gates (activation panels and the like) whose only link to
	// the run is that their text names it. Their casts appeared in NO run
	// section at all. The extra ids widen the usage rollup and its coverage
	// line to those proposals; the vote-step sections above are unchanged.
	extraProposalIDs, err := conversationalRunProposalIDs(conn, runID)
	if err != nil {
		return nil, err
	}
	if report.VoteUsage, err = db.VoteUsageRollup(
		conn, db.ScopeVoteCreate, voteIdempotencyPrefix(runID),
		extraProposalIDs...); err != nil {
		return nil, err
	}
	if report.VoteUsageCoverage, err = db.VoteUsageCoverageFor(
		conn, db.ScopeVoteCreate, voteIdempotencyPrefix(runID),
		extraProposalIDs...); err != nil {
		return nil, err
	}
	if report.SilentVoteSeats, err = silentVoteSeats(
		conn, runID, extraProposalIDs); err != nil {
		return nil, err
	}
	if report.StepUsage, err = db.UsageByStep(conn, runID); err != nil {
		return nil, err
	}
	if report.Artifacts, err = artifactIndex(conn, runID); err != nil {
		return nil, err
	}
	// DKT-594's staleness diff: the run's workflow pins against the registry's
	// current head. Up here with the other pool reads for the reason stated
	// above — it needs no snapshot, and reading it from inside the transaction
	// below would deadlock the one-connection pool.
	if report.PinnedWorkflows, err = pinnedWorkflowStaleness(
		conn, run.ProjectID, runID); err != nil {
		return nil, err
	}

	// ---- R1/R2/R3, over ONE consistent snapshot. ---------------------------
	//
	// The scheduler is loaded for the same reason every other read verb loads
	// it: effective status is a question about a set of rows at one instant, and
	// a report that re-read between sections could count one step twice under
	// two statuses.
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning the report: %w", err)
	}
	// Rolled back unconditionally: this path has nothing to commit, and the
	// rollback is the STRUCTURAL guarantee of R8 rather than a convention
	// someone could edit away.
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}

	report.WallClockMS = wallClockMS(run, nowMS)

	cap, floor, reportedInUnit, spend, _, unit := sched.Budget()
	// AN UNLIMITED RUN'S SNAPSHOT DELIBERATELY QUERIED NOTHING (D1), so the
	// report computes what it needs here instead.
	//
	// This is the one place that wants these numbers regardless of whether
	// anything is enforcing against them: "how much has this run declared it
	// spent, and what would its cap have counted" are fair questions about a run
	// with no cap, and answering them is what lets an instance compute its own
	// policy from a read verb (B27).
	//
	// The `budget.unit` read is NOT the enforcement path re-reading config
	// mid-run (B3): nothing here decides anything. It labels which of the
	// per-unit numbers already in the report a cap would compare, which is R6's
	// job.
	if sched.budget.unlimited() {
		floor, err = RunFloorTx(tx, runID)
		if err != nil {
			return nil, err
		}
		unit, err = db.BudgetUnitTx(tx, sched.run.ProjectID)
		if err != nil {
			return nil, err
		}
		if unit != "" {
			reportedInUnit, err = db.ReportedUsageTx(tx, runID, unit)
			if err != nil {
				return nil, err
			}
		}
		spend = max(floor, reportedInUnit)
	}

	counts, attempts := effectiveStepFacts(sched)
	if err := annotateVoteOutcomes(tx, runID, sched, attempts); err != nil {
		return nil, err
	}
	// DKT-594: which agreement each step's recorded work ran under, in the SAME
	// snapshot as the statuses that decide whether a step ran at all.
	if report.PinEpochs, err = annotatePinEpochs(tx, runID, attempts); err != nil {
		return nil, err
	}
	report.Steps, report.Attempts = counts, attempts

	// The issue-level rulings, read in the SAME snapshot as the steps they
	// dispose of — so a reader joining one onto the other is joining two views
	// of one instant rather than of two.
	if report.Issues, err = issueDispositionsTx(tx, runID); err != nil {
		return nil, err
	}

	// E21: the per-actor rollup, computed in this same read-only transaction so
	// it describes the same instant every other section does.
	actors, err := actorReportTx(tx, runID)
	if err != nil {
		return nil, err
	}
	report.Actors = actors

	usageCap, usageSpend, usageUnit, _ := sched.UsageBudget()

	report.Budget = RunBudgetReport{
		UsageCap:     usageCap,
		UsageUnit:    usageUnit,
		UsageSpend:   usageSpend,
		Cap:          cap,
		Source:       string(BudgetSourceOf(cap, configDefault)),
		Floor:        floor,
		Reported:     reported,
		Unit:         unit,
		Spend:        spend,
		BurnRate:     burnRate(floor, report.WallClockMS),
		BreachReason: facts.BreachReason,
	}
	// The exclusion is stated whenever there is anything to exclude: a cast
	// happened, whether or not its seat reported spend (DKT-584). A run with
	// no panels carries no note — there is nothing being left out.
	if report.VoteUsageCoverage.Casts > 0 || len(report.VoteUsage) > 0 {
		report.Budget.VoteUsageNote = VoteUsageExcludedNote
	}

	// The transaction is rolled back by the deferred call and never committed:
	// there is nothing to commit, and R8's zero-write property is that
	// structural fact rather than a convention.
	return report, nil
}

// wallClockMS is activation -> now, or activation -> the terminal transition.
//
// A run that never activated has NO wall clock rather than a zero one: the
// question "how long has this been running" has no answer for a run that has
// not started, and answering 0 would read as "instantly", which is a different
// claim.
func wallClockMS(run *model.Run, nowMS int64) int64 {
	if run.ActivatedAtMS == nil {
		return 0
	}
	end := nowMS
	if run.Status.Terminal() {
		end = run.UpdatedAtMS
	}
	if end < *run.ActivatedAtMS {
		return 0
	}
	return end - *run.ActivatedAtMS
}

// burnRate is floor per wall-clock HOUR.
//
// Zero when there is no elapsed time, rather than an infinity: a run activated
// this millisecond has not established a rate, and publishing +Inf would be the
// report asserting something about a division it could not perform.
func burnRate(floor float64, wallClockMS int64) float64 {
	if wallClockMS <= 0 {
		return 0
	}
	return floor / (float64(wallClockMS) / float64(3_600_000))
}

// effectiveStepFacts is R3: the count by EFFECTIVE status, and per-step
// attempts.
//
// Effective, not stored. A `pending` step whose predicate holds counts as
// `ready`, and a claimed step whose lease has LAPSED counts as `pending` — the
// v6 discipline, computed at read and never written back, so a report does not
// lie just because nobody ran a scheduling command.
func effectiveStepFacts(sched *Scheduler) ([]model.StatusCount, []StepAttempt) {
	counts := make(map[string]int)
	attempts := make([]StepAttempt, 0, len(sched.Steps()))

	for _, step := range sched.Steps() {
		status := EffectiveStatus(sched, step)
		counts[status]++
		row := StepAttempt{
			Step:     model.FormatStepID(step.ID),
			Instance: step.Instance,
			Status:   status,
			Attempts: step.Attempt,
			Routing:  step.Routing,
		}
		if step.IssueID != 0 {
			row.Issue = model.FormatID(step.IssueID)
		}
		// DKT-868: the step's own bag rides with its status.
		//
		// TOLERANT, NOT SILENT. A stored bag that does not decode leaves
		// `Metadata` nil and sets the flag beside it — the R10 tolerance
		// db.MetadataRollup already applies to the same column (a read verb must
		// not refuse because one row holds odd bytes), without the rollup's
		// freedom to drop the row and let the other rows carry the answer.
		//
		// The decode names no key: it hands over whatever object was stored.
		if bag, err := decodeMetadata(step.Metadata); err != nil {
			row.MetadataUnreadable = true
		} else {
			row.Metadata = bag
		}
		attempts = append(attempts, row)
	}

	// Ordered by a TOTAL key at both levels (R9): statuses by name, attempts by
	// step id. Ranging the map would emit a different document per invocation.
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)

	out := make([]model.StatusCount, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, model.StatusCount{Status: status, Count: counts[status]})
	}
	sort.Slice(attempts, func(i, j int) bool {
		return attempts[i].Instance < attempts[j].Instance
	})
	return out, attempts
}

// issueDispositionsTx reads the run's issue-level terminal rulings (DKT-403).
//
// THE EVENT IS THE RECORD, not the steps. Both abandon paths write one
// `issue-abandoned` event and they agree on nothing else: the routing path
// (`step resolve --as abandon-issue`, and an automatic `on_fail`) records the
// operator's note on the DECIDING STEP's routing and leaves the event payload
// carrying only that step's instance, while `run abandon --issue` records the
// note in the EVENT PAYLOAD and terminalizes every remaining step without
// touching `routing` at all. Reading the event and joining back to its step
// covers both; reading the steps covers only one, and it is the wrong one —
// the path that leaves stale park text standing is exactly the path with
// nothing on the step to read.
//
// IT READS THROUGH THE TRANSACTION, never the pool: internal/db caps the pool
// at one connection, so a pool read from inside the report's open snapshot
// deadlocks permanently rather than failing (the rule LoadRunReport's own
// comment states and TestNoPoolReadsInsideTransactions enforces lexically).
func issueDispositionsTx(tx *sql.Tx, runID int) ([]IssueDisposition, error) {
	rulings, err := scanAbandonRulings(tx.Query(
		abandonRulingSelect+` AND e.run_id = ? AND e.issue_id IS NOT NULL
		  ORDER BY e.seq`,
		EventIssueAbandoned, runID))
	if err != nil {
		return nil, err
	}

	byIssue := make(map[int]IssueDisposition)
	for _, r := range rulings {
		// LAST RULING WINS. Neither path can normally fire twice on one issue —
		// the second is refused, and the first leaves no non-terminal step for
		// a cascade to find — but if a store ever holds two, the one that
		// stands is the later one, not whichever the scan reached first.
		byIssue[r.issueID] = IssueDisposition{
			Issue:       model.FormatID(r.issueID),
			Disposition: DispositionAbandoned,
			By:          r.by,
			Reason:      r.reason,
		}
	}
	if len(byIssue) == 0 {
		return nil, nil
	}

	// A TOTAL order (R9), by issue id, so two reports of the same rows are
	// byte-identical. Event order would do for one run and reshuffle the moment
	// a replay landed the same facts in a different sequence.
	ids := make([]int, 0, len(byIssue))
	for id := range byIssue {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]IssueDisposition, 0, len(ids))
	for _, id := range ids {
		out = append(out, byIssue[id])
	}
	return out, nil
}

// abandonRulingSelect is the ONE read behind every issue-level disposition,
// however it is keyed.
//
// The run report asks it for "every ruling in this run" and `issue show` for
// "the last ruling about this issue, in whatever run made it" (DKT-404) — two
// WHERE clauses over one join, not two queries. The join is what makes the
// row complete: the routing path leaves the operator's note on the DECIDING
// STEP and the run-level path leaves it in the EVENT PAYLOAD, so a reader of
// either column alone recovers half the rulings and silently loses the other
// half's reason.
//
// Callers append their own filter and ORDER BY, and pass the event kind first.
const abandonRulingSelect = `
	SELECT e.issue_id, COALESCE(e.run_id, 0), e.at_ms, e.data, COALESCE(s.routing, '')
	  FROM events e
	  LEFT JOIN steps s ON s.id = e.step_id
	 WHERE e.kind = ?`

// abandonRuling is one `issue-abandoned` event with its note already recovered
// from whichever of the two places the writing path left it.
type abandonRuling struct {
	issueID int
	runID   int
	atMS    int64
	by      string
	reason  string
}

// scanAbandonRulings drains an abandonRulingSelect query, in the order the
// caller asked for.
//
// It takes the query's own (rows, err) pair so a caller reads as one statement
// whether it queries through a transaction or the pool — the two disposition
// readers do one each, and neither may be tempted to reach for the wrong one.
func scanAbandonRulings(rows *sql.Rows, err error) ([]abandonRuling, error) {
	if err != nil {
		return nil, fmt.Errorf("reading issue dispositions: %w", err)
	}
	defer rows.Close()

	var out []abandonRuling
	for rows.Next() {
		var (
			r       abandonRuling
			data    string
			routing string
		)
		if err := rows.Scan(&r.issueID, &r.runID, &r.atMS, &data, &routing); err != nil {
			return nil, fmt.Errorf("reading an issue disposition: %w", err)
		}
		// The payload is core's OWN, written by the two abandon paths — but a
		// malformed one is NOT worth failing a read verb over. R10 says the
		// report works on a run in any state, and a report that refused here
		// would be unavailable during exactly the incident that produced the
		// bad row. An unparseable payload costs the reason line and nothing
		// else: the abandonment itself is the event's existence.
		var payload struct {
			Instance string `json:"instance"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err == nil {
			r.by, r.reason = payload.Instance, payload.Reason
		}
		if r.reason == "" {
			r.reason = abandonNote(routing)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading issue dispositions: %w", err)
	}
	return out, nil
}

// LatestIssueDisposition reads the LAST terminal ruling any run recorded about
// its work on one issue (DKT-404), or nil when no run ever abandoned it.
//
// KEYED BY ISSUE, DELIBERATELY UNBOUNDED BY RUN. The run report answers "what
// did THIS run decide"; a reader of `issue show` is holding an issue and no
// run at all, and the abandonment they need to see is frequently not in the
// newest run — RUN-14 abandoned four issues whose replacements ran two runs
// later, and those four have been sitting at `todo` ever since with the
// disposition reachable only through `events list`.
//
// The LAST ruling, not every one: an issue can be abandoned by a run, replanned
// and abandoned again, and what a reader must not misread is the CURRENT state.
// The earlier rulings stay in the event log, which is the right place for a
// history.
func LatestIssueDisposition(conn *sql.DB, issueID int) (*model.IssueRunDisposition, error) {
	rulings, err := scanAbandonRulings(conn.Query(
		abandonRulingSelect+` AND e.issue_id = ? ORDER BY e.seq DESC LIMIT 1`,
		EventIssueAbandoned, issueID))
	if err != nil {
		return nil, err
	}
	if len(rulings) == 0 {
		return nil, nil
	}
	r := rulings[0]
	return &model.IssueRunDisposition{
		RunID:       r.runID,
		Disposition: DispositionAbandoned,
		By:          r.by,
		Reason:      r.reason,
		AtMS:        r.atMS,
	}, nil
}

// abandonNote recovers the operator's note from a deciding step's routing.
//
// The routing is `routingRecord`'s output — `abandon-issue` alone when no note
// was given, `abandon-issue: <note>` when one was. Anything else belongs to a
// different routing and yields no note rather than a misattributed one.
func abandonNote(routing string) string {
	note, ok := strings.CutPrefix(routing, workflow.OnFailAbandonIssue+":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(note)
}

// artifactIndex is R6: what was produced, never the bodies.
func artifactIndex(conn *sql.DB, runID int) ([]ArtifactIndexEntry, error) {
	artifacts, err := db.ListRunArtifacts(conn, runID)
	if err != nil {
		return nil, err
	}

	type producerRow struct {
		instance string
		executor string
		issueID  int
	}
	producers := make(map[int]producerRow)
	rows, err := conn.Query(
		`SELECT id, instance, executor, issue_id FROM steps WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading artifact producers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id       int
			instance string
			executor sql.NullString
			issueID  int
		)
		if err := rows.Scan(&id, &instance, &executor, &issueID); err != nil {
			return nil, fmt.Errorf("reading an artifact producer: %w", err)
		}
		producers[id] = producerRow{
			instance: instance, executor: executor.String, issueID: issueID,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading artifact producers: %w", err)
	}

	out := make([]ArtifactIndexEntry, 0, len(artifacts))
	for _, a := range artifacts {
		producer := producers[a.StepID]
		entry := ArtifactIndexEntry{
			Artifact: fmt.Sprintf("ARTIFACT-%d", a.ID),
			Kind:     a.Kind,
			Producer: producer.instance,
			Executor: producer.executor,
			SHA256:   a.SHA256,
			// BYTES, not the body. The size is the fact a rollup wants; the
			// content is one `step context` away for anyone who wants it.
			Bytes: len(a.Body),
		}
		if a.Supersedes != nil {
			entry.Supersedes = fmt.Sprintf("ARTIFACT-%d", *a.Supersedes)
		}
		if producer.issueID != 0 {
			entry.Issue = model.FormatID(producer.issueID)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Artifact < out[j].Artifact })
	return out, nil
}

// The two seating paths a run's vote seats are minted through (DKT-733).
// These are the values SilentVoteSeat.Path carries, and they are core's own
// closed vocabulary — derived from HOW the proposal joined the run's
// membership, never from anything a caster asserted.
const (
	// SeatPathVoteStep: the proposal is keyed under the run's vote-step
	// family (voteIdempotencyPrefix) — an engine-minted `type = "vote"` step
	// row, whose panel is seated in-wave.
	SeatPathVoteStep = "vote-step"
	// SeatPathConversationalGate: everything the run's machinery convened
	// OUTSIDE a vote step — a reap-ack ballot keyed under
	// ReapAckProposalKey's family, or a proposal whose text names the run (an
	// activation panel opened with `vote create`). These panels are seated
	// conductor-side.
	SeatPathConversationalGate = "conversational-gate"
)

// SilentVoteSeat is one cast that reported no spend, with the seating path
// that minted its proposal (DKT-733). The proposal id is the argument `vote
// backfill-usage` takes, so each row is an aimable backfill, not just a name.
type SilentVoteSeat struct {
	Proposal string `json:"proposal"`
	Voter    string `json:"voter"`
	Role     string `json:"role,omitempty"`
	Path     string `json:"path"`
}

// silentVoteSeats enumerates the casts the coverage line counts as silent and
// labels each with its seating path. The rows come through the SAME
// membership the coverage count uses; the label is resolved HERE because only
// the engine owns the key-family spellings: a proposal in the run's vote-step
// family is a vote-step seat, and anything else in the membership — reap-ack
// keyed or run-named — is a conversational gate (the extraIDs' two halves,
// per conversationalRunProposalIDs).
func silentVoteSeats(conn *sql.DB, runID int, extraIDs []int) ([]SilentVoteSeat, error) {
	rows, err := db.SilentVoteSeatsFor(
		conn, db.ScopeVoteCreate, voteIdempotencyPrefix(runID), extraIDs...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}

	keyed, err := db.LookupIdempotencyKeys(
		conn, db.ScopeVoteCreate, voteIdempotencyPrefix(runID))
	if err != nil {
		return nil, err
	}
	voteStep := make(map[int]bool, len(keyed))
	for _, id := range keyed {
		voteStep[id] = true
	}

	out := make([]SilentVoteSeat, 0, len(rows))
	for _, r := range rows {
		path := SeatPathConversationalGate
		if voteStep[r.ProposalID] {
			path = SeatPathVoteStep
		}
		out = append(out, SilentVoteSeat{
			Proposal: model.FormatProposalID(r.ProposalID),
			Voter:    r.Voter,
			Role:     r.Role,
			Path:     path,
		})
	}
	return out, nil
}

// conversationalRunProposalIDs resolves the run's CONVERSATIONAL-GATE
// proposals — the ballots the run's machinery convened outside any vote step
// (DKT-584), whose casts otherwise appear in no run section at all:
//
//   - reap-ack ballots, keyed under ReapAckProposalKey's family — positively
//     attributed through the same idempotency table the vote-step family uses;
//   - proposals that NAME the run in their description or rationale (an
//     activation panel opened with `vote create` carries no key and no step,
//     and its text is its only link to the run it gates).
//
// Vote-step proposals are deliberately NOT re-resolved here: the rollups
// already select their key family by prefix, and the membership test is a set
// test, so an overlap would be harmless but a second spelling of that family
// would not.
func conversationalRunProposalIDs(conn *sql.DB, runID int) ([]int, error) {
	keyed, err := db.LookupIdempotencyKeys(
		conn, db.ScopeVoteCreate, reapAckRunPrefix(runID))
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(keyed))
	ids := make([]int, 0, len(keyed))
	for _, id := range keyed {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	named, err := db.ProposalIDsNaming(conn, model.FormatRunID(runID))
	if err != nil {
		return nil, err
	}
	for _, id := range named {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	// A TOTAL order (R9): the ids feed a parameterized IN whose bound values
	// participate in query text equality for no engine, but a deterministic
	// argument list keeps two reports byte-identical in any future trace.
	sort.Ints(ids)
	return ids, nil
}

// configuredBudgetDefault reads `budget.default` for R6's source derivation.
func configuredBudgetDefault(conn *sql.DB, projectID int) (float64, error) {
	entry, err := db.GetConfig(conn, projectID, db.KeyBudgetDefault)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", db.KeyBudgetDefault, err)
	}
	var value float64
	if _, err := fmt.Sscanf(entry.Value, "%g", &value); err != nil {
		// A malformed default is not this read verb's problem to refuse over:
		// `config set` validates on the way in, and a report that failed here
		// would be unavailable during exactly the incident that made someone
		// hand-edit the value.
		return 0, nil
	}
	return value, nil
}

// annotateVoteOutcomes fills each vote step's `Vote` with its proposal and how
// it tallied (DKT-258).
//
// IT READS THROUGH THE TRANSACTION, never the pool. This function runs inside
// the report's snapshot, and internal/db caps the pool at ONE connection — a
// pool read from in here deadlocks permanently rather than failing, which is
// the rule LoadRunReport's own comment states and TestNoPoolReadsInsideTransactions
// enforces lexically. Reading through `tx` is also the more correct answer on
// its own terms: the annotation then describes the same instant as the statuses
// beside it.
//
// TWO QUERIES FOR THE WHOLE RUN, not two per vote step. The idempotency keys
// come back as one prefix scan — the same one VoteMetadataRollup already
// makes — and the statuses as one `IN` over the ids that scan produced. A
// per-step loop would be N round trips against a single connection to answer a
// question one join can.
func annotateVoteOutcomes(
	tx *sql.Tx, runID int, sched *Scheduler, attempts []StepAttempt,
) error {
	voteSteps := make(map[string]*db.Step)
	for _, step := range sched.Steps() {
		if step.Kind == workflow.TypeVote {
			voteSteps[model.FormatStepID(step.ID)] = step
		}
	}
	if len(voteSteps) == 0 {
		return nil
	}

	keys, err := db.LookupIdempotencyKeysTx(
		tx, db.ScopeVoteCreate, voteIdempotencyPrefix(runID))
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		// No proposal was ever opened for any of this run's vote steps, which
		// is the NEVER-CONVENED case. Leaving the annotation empty is the
		// answer, not a failure to produce one.
		return nil
	}

	ids := make([]int, 0, len(keys))
	for _, id := range keys {
		ids = append(ids, id)
	}
	statuses, err := db.ProposalStatusesTx(tx, ids)
	if err != nil {
		return err
	}

	for i := range attempts {
		step, ok := voteSteps[attempts[i].Step]
		if !ok {
			continue
		}
		id, ok := keys[voteIdempotencyKey(step.RunID, step.IssueID, step.Instance)]
		if !ok {
			continue // This step's panel never convened.
		}
		status, ok := statuses[id]
		if !ok {
			continue
		}
		attempts[i].Vote = fmt.Sprintf("%s %s",
			model.FormatProposalID(id), status)
	}
	return nil
}
