package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The complete saga — TDD §6.8. §2 verbatim is the specification:
//
//	`complete` is the specified saga — artifact+payload validation → gates
//	one-by-one → routing — with panel-hardened semantics: THE TOKEN RETIRES
//	WHEN THE ARTIFACT RECORDS; from that commit the saga is engine-owned and
//	resumes lazily under any later engine invocation, each stage its own
//	transaction, no subprocess ever inside a transaction, every stage commit
//	refreshing the step's activity clock.
//
// Stages, each its own transaction, each recording `saga_stage` as its resume
// point:
//
//	0      null          authorize holder; validate artifact size/shape;
//	                     validate the payload against the step's declared
//	                     `payload = schema@ver`, from the run's PINNED
//	                     bytes (§4.8)                                   token REQUIRED
//	1      recorded      insert artifact; RETIRE THE TOKEN; status ->
//	                     gated; refresh activity_ms                     last needing it
//	2..N   gate:<name>   one gate: write gate-started, run it OUTSIDE
//	                     the transaction, commit the result             no token
//	N+1    routing       resolve routing; update step, issue mirror,
//	                     run, events — ONE transaction over all four    no token
//	—      null          saga complete
//
// TOKEN RETIREMENT AT ARTIFACT RECORD IS THE HINGE. Before stage 1 commits, a
// non-holder cannot record (AUTH_ERROR) and a stale holder is refused
// (STALE_LEASE). After it commits, the step needs no lease at all: any later
// engine invocation advances the saga from `saga_stage`. That is what makes
// crash-at-any-boundary safe — there is no lease to lose and no owner to wait
// for.

// GapArtifactKind is the auxiliary kind every executor step may record beside
// its declared emit (DKT-72). It is PACKAGING vocabulary, like `held` and
// `operator_resolved`: "work surfaced outside this step's scope" names no
// domain. Before it existed, every rendered brief promised the channel while
// no workflow declared the kind — so agents narrated gaps into findings
// bodies, believed they had recorded them, and the residue evaporated.
const GapArtifactKind = "gap"

// CompleteOptions are `step complete`'s inputs.
type CompleteOptions struct {
	Token string
	// WorkDir is `--worktree`: the checkout the work actually happened in.
	// Persisted on the step at stage 0 and read by the diff stage, so a
	// conductor recording on behalf of an executor in another worktree
	// captures THAT tree rather than its own (G7/G8). Empty means "the
	// invoking checkout", resolved at diff time.
	WorkDir string
	// Artifact is the body from --artifact-file.
	Artifact []byte
	// Payload is the JSON from --payload-file, or nil.
	Payload []byte
	// Usage is `--usage`. It is STORED and ENFORCES NOTHING until S6 — the same
	// reasoning as `--budget`: the wire shape is §11.4's and it lands whole, so
	// the S6 upgrade adds enforcement rather than a flag.
	Usage string
	// Metadata is `--metadata`, opaque KV merged onto the step's own.
	Metadata string
	// Gaps are `--gap-file` bodies (repeatable): out-of-scope problems the
	// worker surfaced. Each records an auxiliary artifact of GapArtifactKind
	// beside the declared emit AND materializes a backlog issue related to the
	// step's own, in the same transaction (DKT-72).
	Gaps [][]byte
	// GapIssues, when non-nil, receives the refs of the issues materialized
	// from Gaps — the caller's channel for telling the operator where the
	// residue landed.
	GapIssues *[]string
	NowMS     int64
}

// Engine carries the two execution seams and drives the saga.
//
// The runners are fields rather than package-level values precisely so S4 and
// S5 swap them by constructing a different Engine — "one constructor call and
// nothing else" (§5.6, §6.13).
type Engine struct {
	Gates   GateRunner
	Actions ActionRunner
	// DiffFn computes an issue's VCS diff over its snapshotted scope (§6.7.1
	// D1). It is a field so a test can supply a deterministic one: the real
	// implementation shells out to git, which is THE ONE declared VCS coupling
	// (engine-spec §7), and a test that invoked it would depend on the working
	// tree it is supposed to prove immunity from.
	DiffFn func(dir, base string, scope []string) (string, error)
	// HeadFn resolves a checkout's current HEAD commit, for the round-delta
	// record (DKT-106). A field for DiffFn's reason exactly: the real one
	// shells out to git. "" means "could not resolve", and every consumer
	// treats that as "record no head, compute no delta".
	HeadFn func(dir string) string
	// IsAncestorFn reports whether `sha` is an ancestor of (or equal to)
	// execRoot's HEAD, and whether the question could be answered at all
	// (DKT-193). A field for DiffFn's reason: the real one shells out to git.
	// `known = false` means every consumer stays silent — an unanswerable
	// question is not evidence of staleness.
	IsAncestorFn func(execRoot, sha string) (ancestor, known bool)
	// PatchContainedFn reports whether execRoot's HEAD carries `sha`'s PATCH —
	// whether some commit on HEAD's side of their merge base is patch-id
	// equivalent to each commit on `sha`'s side (DKT-1033) — and whether that
	// question could be answered at all. A field for DiffFn's reason: the real
	// one shells out to git.
	//
	// It is the FIRST question a disproved ancestry opens, ahead of
	// TreeMatchFn, because it asks about the work rather than the tip: a
	// sibling issue's integration or a conductor's follow-up patch on the same
	// file moves HEAD's tree without touching this patch, and RUN-67 warned on
	// all nine review rows for exactly that. Like TreeMatchFn it may only
	// ACQUIT a sha ancestry already disproved; unlike TreeMatchFn, a
	// definitive `contained = false` is the ONE measurement that lets the
	// advisory call an integration diverged, because it is the one that
	// actually measured the patch.
	PatchContainedFn func(execRoot, sha string) (contained, known bool)
	// TreeMatchFn reports whether execRoot's HEAD still carries `sha`'s TREE
	// on the paths `sha`'s work touched — or, where that question has no
	// evidence to answer with, whether the two carry the same root tree
	// outright (DKT-424, DKT-451) — and whether either could be answered at
	// all. A field for DiffFn's reason: the real one shells out to git.
	//
	// It is IsAncestorFn's ACQUITTAL, never its accuser: staleTargets asks it
	// only about a sha ancestry already disproved, and only a `match = true`
	// changes the outcome. `known = false` therefore leaves DKT-193's verdict
	// exactly as it stood — a probe that cannot answer must not silence a
	// warning it did not disprove, which is the opposite direction from
	// IsAncestorFn's own fail-open and deliberately so.
	//
	// Since DKT-1033 it is PatchContainedFn's FALLBACK: a tree comparison
	// reads any later commit on the same paths as a difference, so its
	// `match = false` is no longer allowed to word the advisory as a
	// divergence — only as content that could not be matched.
	TreeMatchFn func(execRoot, sha string) (match, known bool)
	// ObjectExistsFn reports whether `sha` resolves as a COMMIT OBJECT from
	// execRoot at all — `git cat-file -e <sha>^{commit}` — and whether that
	// question could be answered (DKT-742). A field for DiffFn's reason: the
	// real one shells out to git.
	//
	// It exists because IsAncestorFn's `known = false` conflates two states
	// that read very differently to a packet consumer: "git could not answer"
	// (git absent, not a repository — nothing to warn about) and "the object
	// is not in the shared store at all" (a separate-clone worktree whose
	// objects never reached the shared store, or a pruned+GC'd one). RUN-52's
	// DKT-V253 was the second: all three vote seats ran `git cat-file -t` on
	// the packet's target, found no object anywhere, and burned an
	// investigation each — while the engine, which had asked git about that
	// exact sha at dispatch open, had silently skipped it as unanswerable.
	//
	// staleTargets consults it only where ancestry was UNANSWERABLE, and only
	// a definitive `exists = false, known = true` produces a warning — so the
	// "absence of evidence is not staleness" posture holds for every state
	// where git genuinely could not answer.
	ObjectExistsFn func(execRoot, sha string) (exists, known bool)
}

// NewEngine builds the S5 engine: the REAL gate runner, the REAL action runner,
// and the real git diff.
//
// THIS IS THE CONSTRUCTOR SWAP engine-spine §6.13 promised — "the saga is
// written against the interface, so S5 changes one constructor call and nothing
// else". The promise holds FOR THE SEAM'S INVOCATION: `runRoutingStage` calls
// `e.Actions.Run(...)` outside every transaction with a fully-populated
// ActionSpec, and that call site is where it always was.
//
// docs/tdd/payloads-thresholds.md §6.4 records honestly the four places more
// moves — the seam's return shape (M-a), the routing stage's `held` branch
// (M-b), DecideStep's materialized branch (M-c), and stage 0's schema
// validation with the threshold's resolver (M-d). None deviates from an
// engine-spec line, so the note is not an amendment.
//
// The repo root is resolved HERE rather than passed by every call site, so the
// four `internal/cli` callers are genuinely unchanged. A repo that cannot be
// resolved yields a runner whose gates all report `unmatched` — fail-closed,
// which is the same direction every other unknown in this stage takes.
func NewEngine() *Engine {
	paths := repoPathsFrom(resolvePaths())
	return &Engine{
		Gates:            NewExecRunner(paths),
		Actions:          NewActionRunner(paths),
		DiffFn:           GitDiff,
		HeadFn:           sharedCheckoutHead,
		IsAncestorFn:     gitAncestorOfHead,
		PatchContainedFn: gitPatchContainedInHead,
		TreeMatchFn:      gitTreeMatchesHead,
		ObjectExistsFn:   gitCommitResolvable,
	}
}

// CompleteStep runs the saga from stage 0, or resumes it from wherever it
// stopped.
//
// It is safe to call at any time on any step: a step not in the saga starts it
// (token required), and one already in it resumes (token not required, and not
// consulted). That is what makes a crashed worker's saga finish under the next
// `next`, the next `claim`, or an explicit re-invocation, with no operator
// action.
func (e *Engine) CompleteStep(conn *sql.DB, stepID int, opts CompleteOptions) error {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return err
	}

	// Stage 0 runs only when the saga has not started. A step already past it
	// needs no token — the token retired at stage 1.
	if !step.InSaga() {
		if err := e.stageZero(conn, step, opts); err != nil {
			return err
		}
	}

	return e.ResumeSaga(conn, stepID, opts.NowMS)
}

// RunActionStep enters an action step's saga ENGINE-SIDE — no claim, no token,
// no worker artifact (§6.15 as amended).
//
// It is the other half of `claim` refusing an action step. Action steps are the
// engine's deterministic half (AC-2): the saga computes the builtin from the
// step's declared `inputs`, records the artifact the computation produced, and
// routes. Nothing a worker could supply belongs in that sequence, which is why
// there is no stage-0 equivalent here rather than a stage 0 with its checks
// relaxed — a token to authorize, a body to size-cap, and a payload to validate
// are all facts about a HOLDER, and this step has none.
//
// The stage-1 commit is therefore just the two rows the routing stage needs to
// find: the status, and the resume point. The ARTIFACT IS NOT WRITTEN HERE —
// runRoutingStage writes the action's own, and writing an empty one first would
// leave a crashed action indistinguishable from one that computed nothing.
//
// Idempotent by the same CAS every stage uses: a step already in the saga skips
// straight to the resume, so two concurrent `next` invocations produce one run.
func (e *Engine) RunActionStep(conn *sql.DB, stepID int, nowMS int64) error {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return err
	}
	if step.Kind != workflow.ClassAction {
		return validationErr(
			"step %s is a %s step; the engine runs only `action` steps itself",
			step.Instance, step.Kind)
	}

	if !step.InSaga() {
		if err := e.enterActionSaga(conn, step, nowMS); err != nil {
			if errors.Is(err, db.ErrSagaStageMoved) {
				// Lost the race to a concurrent driver, which is a WIN: the saga
				// is started and the resume below advances whatever is current.
				return e.ResumeSaga(conn, stepID, nowMS)
			}
			return err
		}
	}
	return e.ResumeSaga(conn, stepID, nowMS)
}

// enterActionSaga commits an action step's stage 1: gated, and recorded.
func (e *Engine) enterActionSaga(conn *sql.DB, step *db.Step, nowMS int64) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the action saga: %w", err)
	}
	defer tx.Rollback()

	if err := db.SetStepStatusTx(tx, step.ID, db.StepGated, nowMS, nowMS); err != nil {
		return err
	}
	if err := db.AdvanceSagaTx(tx, step.ID, "", db.SagaRecorded, nowMS); err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepRecorded, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// stageZero authorizes the holder, validates, and commits stage 1.
//
// Stages 0 and 1 are ONE transaction because stage 0 is pure validation with no
// state of its own to record: its resume point IS stage 1's. Splitting them
// would create a committed "validated but not recorded" state that nothing can
// act on and that a resume would have to re-validate anyway.
func (e *Engine) stageZero(conn *sql.DB, step *db.Step, opts CompleteOptions) error {
	// ---- Validation, BEFORE the transaction opens. -------------------------
	//
	// R12: an artifact over the cap is refused by size, naming both numbers. A
	// refusal that happens before anything is locked costs nothing and writes
	// nothing — which is what the version-unchanged assertions of §6.9 check.
	if len(opts.Artifact) > db.ArtifactMaxBytes {
		return validationErr(
			"artifact is %d bytes, over the %d-byte cap; reduce it or record a "+
				"reference instead", len(opts.Artifact), db.ArtifactMaxBytes)
	}

	// The metadata cap, measured on the RAW INPUT
	// (docs/tdd/completion-metadata.md §1.1.1). It sits with R12's artifact cap
	// and for the same reason: a size refusal costs nothing and writes nothing.
	//
	// The ORDER of the two content checks is fixed here as C5 fixes R12's and
	// the schema's — an operator debugging an oversized input should not get a
	// different message depending on which check happened to run first.
	if err := validateMetadataSize(opts.Metadata); err != nil {
		return err
	}

	// Gaps take R12's cap each, and an EMPTY gap refuses: recording "something
	// is out of scope here" with no content is the evaporating-residue failure
	// this channel exists to end (DKT-72).
	for i, gap := range opts.Gaps {
		if len(gap) > db.ArtifactMaxBytes {
			return validationErr(
				"gap %d is %d bytes, over the %d-byte cap; reduce it or record a "+
					"reference instead", i+1, len(gap), db.ArtifactMaxBytes)
		}
		if len(strings.TrimSpace(string(gap))) == 0 {
			return validationErr(
				"gap %d is empty; a gap records the out-of-scope problem itself, "+
					"and an empty one records nothing anyone can pick up", i+1)
		}
	}

	// Shape validation, unchanged from S3: a payload must be a JSON array of
	// objects, which is the shape a threshold aggregates over. For a step that
	// declares NO `payload` this is still the whole of it (§4.8 C1) — no new
	// refusal exists for such a step.
	if _, err := parsePayload(opts.Payload); err != nil {
		return err
	}

	// The metadata SHAPE, still before the transaction opens: a bag that is not
	// a JSON object is refused rather than stored for the rollup to skip later.
	// Parsing here also means the merge inside the transaction cannot fail on
	// caller input — by then the only remaining error is a serialization one.
	if _, err := DecodeMetadataBag(opts.Metadata, "metadata"); err != nil {
		return err
	}

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return err
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if spec == nil {
		return validationErr("step %s: %q is not a step of its pinned workflow",
			step.Instance, step.StepName)
	}

	// C6: AUTHORIZATION PRECEDES CONTENT, ALWAYS. A non-holder must not learn
	// the schema by probing it, so the holder is checked here — before any
	// schema is consulted — and again inside the transaction, which stays the
	// authority. The read is advisory and the write-side check is unchanged;
	// what this adds is only the ORDER of two refusals.
	if err := db.AuthorizeStepRead(conn, step.ID, opts.Token, opts.NowMS); err != nil {
		return err
	}

	// C2-C5: the declared payload schema. Still before the transaction opens,
	// so a refusal writes nothing and leaves `row_version` where it was — the
	// assertion every refusal in §6.9 carries.
	//
	// C5 pins the ORDER of the two content checks: R12's size cap above, then
	// the schema. Both are pre-transaction, and fixing the sequence is what
	// makes the error a 2 MiB invalid payload produces stable — an operator
	// debugging one should not get a different message depending on which check
	// happened to run first.
	if err := validateDeclaredPayload(conn, step, spec, opts.Payload); err != nil {
		return err
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the saga: %w", err)
	}
	defer tx.Rollback()

	// R1-R4 and R9, from the SHARED authorize (§6.6): the same three-way
	// refusal issues use, so the step matrix cannot drift from the issue one.
	// R9 falls out of this for free: a step past stage 1 has no lease, so a
	// second `complete` presents a token that holds nothing — AUTH_ERROR.
	if _, err := db.AuthorizeStepTx(tx, step.ID, opts.Token, opts.NowMS); err != nil {
		return err
	}

	kind := workflow.ArtifactKind(spec)
	if kind == "" {
		return validationErr(
			"step %s produces no artifact and cannot be completed with one",
			step.Instance)
	}

	if _, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: step.RunID, StepID: step.ID, Kind: kind,
		Body:    string(opts.Artifact),
		Payload: string(opts.Payload),
		SHA256:  artifactSHA256(opts.Artifact, opts.Payload),
	}, opts.NowMS); err != nil {
		return err
	}

	// The declared worktree persists WITH the artifact (G8), because the saga
	// resumes lazily: the diff stage may run under a later invocation from a
	// different cwd entirely, and only a persisted fact survives that gap. The
	// in-memory copy keeps THIS invocation's diff correct too.
	if opts.WorkDir != "" {
		if _, err := tx.Exec(
			`UPDATE steps SET work_root = ? WHERE id = ?`, opts.WorkDir, step.ID,
		); err != nil {
			return fmt.Errorf("recording the worktree for %s: %w", step.Instance, err)
		}
		step.WorkRoot = opts.WorkDir
	}

	// Gaps record beside the declared emit, in the SAME transaction (DKT-72):
	// an auxiliary artifact of GapArtifactKind — always emittable, no workflow
	// declaration required, because a promised channel that cannot receive is
	// worse than no channel — plus a backlog issue related to this step's own,
	// so the residue lands where the next planning pass already looks.
	for _, gap := range opts.Gaps {
		if _, err := db.InsertArtifactTx(tx, db.Artifact{
			RunID: step.RunID, StepID: step.ID, Kind: GapArtifactKind,
			Body:   string(gap),
			SHA256: workflow.SHA256(gap),
		}, opts.NowMS); err != nil {
			return err
		}
		// The gap lands in the RUN's project — the backlog the next planning
		// pass over this work will actually look at.
		gapProject, err := db.RunProjectIDTx(tx, step.RunID)
		if err != nil {
			return err
		}
		issueID, err := db.InsertGapIssueTx(tx, gapProject, gapTitle(gap), string(gap), step.IssueID)
		if err != nil {
			return err
		}
		if opts.GapIssues != nil {
			*opts.GapIssues = append(*opts.GapIssues, model.FormatID(issueID))
		}
	}

	// The completing worker's opaque KV bag, merged over the step's own.
	// It lands HERE — with the artifact, inside the one transaction —
	// because a step that recorded an artifact and dropped the bag reported
	// with it is the exact silent half-write this fix exists to remove.
	//
	// Before the saga advances, so the step's own row reaches its final shape
	// before stage zero is recorded as having happened.
	if opts.Metadata != "" {
		merged, err := mergeMetadata(step.Metadata, opts.Metadata)
		if err != nil {
			return err
		}
		if err := db.SetStepMetadataTx(tx, step.ID, merged, opts.NowMS); err != nil {
			return err
		}
	}

	// ---- THE HINGE: the token retires as the artifact records. -------------
	if err := db.RetireStepTokenTx(tx, step.ID); err != nil {
		return err
	}
	if err := db.SetStepStatusTx(tx, step.ID, db.StepGated, opts.NowMS, opts.NowMS); err != nil {
		return err
	}
	if err := db.AdvanceSagaTx(tx, step.ID, "", db.SagaRecorded, opts.NowMS); err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepRecorded, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: opts.Usage,
	}); err != nil {
		return err
	}

	// `--usage` ALSO lands in the ledger (§4.9 B34). The event above keeps
	// carrying the blob — removing it would break the replay property — and
	// this stage adds the rows the report sums and `max(reported, floor)` reads
	// when `budget.unit` names a unit.
	//
	// In THIS transaction, with the artifact and the event, because the three
	// are one fact: a step that recorded an artifact and no ledger row would
	// look to group 2's discrepancy probe exactly like a relay that forgot to
	// report.
	if err := recordUsageTx(tx, step, opts.Usage, opts.NowMS); err != nil {
		return err
	}

	return tx.Commit()
}

// gapTitle derives the materialized issue's title from a gap body: the first
// non-empty line, markdown heading markers stripped, capped so a paragraph
// pasted as line one still yields a scannable title. MECHANICAL extraction
// only — a line boundary and a length are structure, not meaning, so the
// genericity line holds: docket never interprets what the gap says.
func gapTitle(gap []byte) string {
	const maxTitle = 120
	for _, line := range strings.Split(string(gap), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > maxTitle {
			return string(runes[:maxTitle-1]) + "…"
		}
		return line
	}
	return "gap"
}

// gapOnlyCompletion reports whether a step's recorded product is gap
// artifacts alone: at least one gap recorded while the declared emit's body is
// empty (DKT-25).
//
// The declared emit's EMPTINESS is the whole test, not the diff's — a body
// with content is a claim about work done, and docket never interprets what
// it says (the genericity line). What the engine can know structurally is
// that a worker who recorded nothing but gaps produced nothing for the
// pipeline to consume.
func gapOnlyCompletion(conn *sql.DB, step *db.Step, spec *workflow.Step) (bool, error) {
	kind := workflow.ArtifactKind(spec)
	if kind == "" {
		return false, nil
	}

	var gaps int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND kind = ?`,
		step.ID, GapArtifactKind,
	).Scan(&gaps); err != nil {
		return false, fmt.Errorf("counting gap artifacts: %w", err)
	}
	if gaps == 0 {
		return false, nil
	}

	var body string
	err := conn.QueryRow(
		`SELECT body FROM artifacts WHERE step_id = ? AND kind = ?
		  ORDER BY id DESC LIMIT 1`,
		step.ID, kind,
	).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the declared emit: %w", err)
	}
	return strings.TrimSpace(body) == "", nil
}

// ResumeSaga advances a step's saga from its recorded resume point, to
// completion.
//
// RESUME IS LAZY AND IDEMPOTENT. Each stage's transaction is CAS-guarded on the
// expected `saga_stage`, so two concurrent engine invocations resuming the same
// saga produce exactly one advance; the loser matches zero rows, re-reads, and
// either finds the work done or advances what is now current.
func (e *Engine) ResumeSaga(conn *sql.DB, stepID int, nowMS int64) error {
	// A bounded loop rather than `for {}`: the saga has a finite stage count
	// (one per gate, plus routing), and a runaway here would spin against the
	// database. The bound is generous — no definition has thousands of gates —
	// and being reached at all is a bug, so it says so.
	const maxStages = 1000

	for range maxStages {
		step, err := db.GetStep(conn, stepID)
		if err != nil {
			return err
		}
		if !step.InSaga() {
			return nil // Complete.
		}

		advanced, err := e.advanceOne(conn, step, nowMS)
		if err != nil {
			if errors.Is(err, db.ErrSagaStageMoved) {
				// Lost the race to a concurrent resumer. Re-read and continue:
				// either the saga is finished or the next stage is ours.
				continue
			}
			return err
		}
		if !advanced {
			return nil
		}
	}
	return fmt.Errorf("step %s: the saga did not converge in %d stages",
		model.FormatStepID(stepID), maxStages)
}

// advanceOne runs exactly one stage. It reports whether it advanced, so the
// caller's loop terminates on a saga that has nothing left to do.
func (e *Engine) advanceOne(conn *sql.DB, step *db.Step, nowMS int64) (bool, error) {
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return false, err
	}
	// H5: a materialized held step's spec is SYNTHESIZED from the pinned
	// definition rather than found in it, so every `spec == nil` site routes
	// through this resolver instead of erroring.
	tally, err := loadHoldTally(conn, step.RunID)
	if err != nil {
		return false, err
	}
	spec := materializedSpec(defs[step.WorkflowID], step, tally)
	if spec == nil {
		return false, validationErr("step %s: %q is not a step of its pinned workflow",
			step.Instance, step.StepName)
	}

	// H9: `held` is "NO ADVANCE" until the question is answered. Every later
	// `next`/`claim`/`complete` invocation therefore costs one read and changes
	// nothing, rather than spinning against the database.
	if step.SagaStage == db.SagaHeld {
		resolved, _, err := heldDecision(conn, step)
		if err != nil {
			return false, err
		}
		if !resolved {
			return false, nil
		}
		// H10: the threshold is evaluated EXACTLY ONCE, after resolution. The
		// alternative — route now over the unheld subset, re-route after —
		// requires UN-ROUTING, and there is no un-route: a `fix-loop` entry
		// supersedes instances and increments a counter.
		return true, e.runRoutingStage(conn, step, spec, defs, nowMS)
	}

	next, gate := nextSagaStage(step, spec)

	switch {
	case gate != nil:
		// §7.5 A1: a gate whose `gate-started` committed but whose result did
		// not is a STARTED-BUT-UNRECORDED gate — the crash window
		// at-least-once creates. Detectable exactly as A1 says: this stage is
		// the one about to run, and it already has a `gate-started` event with
		// no result row to match.
		//
		// The decision is made BEFORE the spawn, because the whole point is to
		// not spawn a second time.
		interrupted, err := gateWasStartedButNotRecorded(conn, step, gate.Name)
		if err != nil {
			return false, err
		}
		if interrupted {
			return true, e.resolveInterruptedGate(conn, step, *gate, next, nowMS)
		}
		return true, e.runGateStage(conn, step, *gate, next, nowMS)
	case next == db.SagaRouting:
		return true, e.enterRouting(conn, step, nowMS)
	default:
		return true, e.runRoutingStage(conn, step, spec, defs, nowMS)
	}
}

// nextSagaStage decides which stage comes after the step's current one.
//
// The gate ORDER is the definition's declared order, and `pre` gates are
// excluded: §11.1 makes a `pre` gate run at CLAIM with its results in the
// context bundle, not in sequence inside `complete`.
func nextSagaStage(step *db.Step, spec *workflow.Step) (stage string, gate *workflow.Gate) {
	gates := completionGates(spec)

	switch step.SagaStage {
	case db.SagaRecorded:
		if len(gates) > 0 {
			return db.SagaGatePrefix + gates[0].Name, &gates[0]
		}
		return db.SagaRouting, nil
	case db.SagaRouting, db.SagaHeld:
		// Routing is the last stage; the next state is complete. `held` never
		// reaches here — advanceOne handles it before this is consulted — and is
		// listed so a future reader does not have to prove that it cannot.
		return "", nil
	}

	// Mid-gate: find the gate just recorded and take the one after it.
	if name, ok := strings.CutPrefix(step.SagaStage, db.SagaGatePrefix); ok {
		for i, g := range gates {
			if g.Name != name {
				continue
			}
			if i+1 < len(gates) {
				return db.SagaGatePrefix + gates[i+1].Name, &gates[i+1]
			}
			return db.SagaRouting, nil
		}
	}
	// An unrecognized stage — a definition edited under a live saga. Routing is
	// the safe destination: it resolves the step rather than leaving it wedged
	// in a stage nothing will ever advance.
	return db.SagaRouting, nil
}

// completionGates returns the gates that run inside `complete`, in declared
// order — every gate except `pre` ones.
func completionGates(spec *workflow.Step) []workflow.Gate {
	out := make([]workflow.Gate, 0, len(spec.Gates))
	for _, g := range spec.Gates {
		if g.Pre {
			continue
		}
		out = append(out, g)
	}
	return out
}

// runGateStage is stages 2..N. THE TRANSACTION BOUNDARIES ARE THE POINT.
//
// §6's "no subprocess ever executes inside a transaction" is structural here:
// the stage commits its `gate-started` event, CLOSES THE TRANSACTION, invokes
// the runner, then opens a NEW transaction to record the result. At S3 the
// runner does nothing, but the boundaries are already where S4 needs them —
// which is the entire reason for landing the saga now rather than with the
// gates that will use it.
func (e *Engine) runGateStage(
	conn *sql.DB, step *db.Step, gate workflow.Gate, stage string, nowMS int64,
) error {
	// ---- Transaction A: announce the spawn. --------------------------------
	//
	// §2: "a `gate-started` event precedes each spawn". It commits BEFORE the
	// runner is invoked, so the at-least-once discipline is observable: a crash
	// between here and the result leaves a started-but-unrecorded gate, which
	// is exactly the state a resume must handle and can only see if this
	// committed first.
	txA, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the gate stage: %w", err)
	}
	err = recordEvent(txA, eventRecord{
		Kind: EventGateStarted, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID, Data: gate.Name,
	})
	if err != nil {
		txA.Rollback()
		return err
	}
	if err := txA.Commit(); err != nil {
		return fmt.Errorf("committing the gate announcement: %w", err)
	}

	// ---- Outside any transaction: run the gate. ----------------------------
	fences, hashes, err := gateCommands(conn, step, gate)
	if err != nil {
		return err
	}
	spec := GateSpec{
		Name: gate.Name, Source: gate.Source, Pre: gate.Pre,
		Commands: fences, CommandHashes: hashes,
	}
	// The SNAPSHOTTED scope rides along (DKT-63), so a diff-shaped gate can
	// evaluate the change it is gating rather than the whole shared tree.
	scope, err := snapshotScope(conn, step.RunID, step.IssueID)
	if err != nil {
		return err
	}
	// The step's RECORDED worktree rides along (DKT-9), the same resolution
	// the diff stage applies: a completion gate measures the tree the work
	// happened in, not the shared checkout the saga was resumed from. So does
	// the worktree's base commit (DKT-992), resolved per gate rather than once
	// per step because the saga advances stage by stage and may resume from a
	// different invocation — and the fork point never moves for the worktree's
	// lifetime, so every gate of one step resolves the same sha.
	sc := StepContext{
		Instance: step.Instance, RunID: step.RunID, IssueID: step.IssueID,
		Scope: scope, WorkRoot: step.WorkRoot,
		Base: gateBaseSHA(conn, step.RunID, step.WorkRoot),
	}

	rows, err := runGate(e.Gates, spec, sc)
	if err != nil {
		return fmt.Errorf("running gate %s on %s: %w", gate.Name, step.Instance, err)
	}

	// ---- Transaction B: record the result and advance. ---------------------
	txB, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the gate record: %w", err)
	}
	defer txB.Rollback()

	// M-a: results land in `gate_results` (v8), not in the `steps.gate_trail`
	// JSON blob. That migration IS the recorded exit condition, and the
	// trail column stops being written here (G3) while keeping its old bytes.
	if err := recordGateRows(txB, step, gate.Name, rows, nowMS); err != nil {
		return err
	}

	if err := recordGateEvents(txB, step, gate.Name, rows); err != nil {
		return err
	}
	if err := db.AdvanceSagaTx(txB, step.ID, step.SagaStage, stage, nowMS); err != nil {
		return err
	}
	return txB.Commit()
}

// enterRouting moves a step from its last gate to the routing stage.
func (e *Engine) enterRouting(conn *sql.DB, step *db.Step, nowMS int64) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("entering routing: %w", err)
	}
	defer tx.Rollback()

	if err := db.AdvanceSagaTx(tx, step.ID, step.SagaStage, db.SagaRouting, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// runRoutingStage is stage N+1: ONE TRANSACTION SPANNING step, issue mirror,
// run, and events (§2).
//
// The four are one transaction because they are one fact — this step ended this
// way — and a partial commit is a run whose step says `done` while its issue
// says otherwise, which no later pass can reconcile without guessing.
func (e *Engine) runRoutingStage(
	conn *sql.DB, step *db.Step, spec *workflow.Step,
	defs map[int]*workflow.Definition, nowMS int64,
) error {
	// ---- Outside the transaction: the two subprocess-shaped computations. --
	//
	// §6.7.1 D1 puts the diff computation HERE, before the transaction opens,
	// because it is a git subprocess and §6 forbids one inside a transaction.
	// The artifact INSERT is inside.
	var (
		diffBody    string
		diffPayload string
		wantsDiff   bool
	)
	if isExecutorStep(step) && stepHoldsTree(spec) {
		// D1 as amended by DKT-75: recomputed at the completion of every
		// executor step THAT HOLDS THE TREE — the same `holds_tree`
		// declaration scope exclusion consults, and the only step property
		// that says "this step changed what a diff would show". Recomputing at
		// every executor completion was the defect the amendment closes:
		// read-shaped fanout siblings each re-diffed the LIVE tree at their
		// own record time, so a diff taken after the change was committed came
		// out empty and one taken beside a sibling's in-flight probe carried
		// the probe (36 of RUN-5's 71 issue.diff artifacts hashed empty).
		//
		// A non-holding step records nothing and its consumers resolve D3 to
		// the artifact the last HOLDING step recorded — the reviewed object,
		// pinned at the moment the change existed, byte-identical for every
		// sibling. D2 still excludes action/human/vote steps: they change no
		// tree either.
		scope, err := snapshotScope(conn, step.RunID, step.IssueID)
		if err != nil {
			return err
		}
		if e.DiffFn != nil {
			// The tree that gets diffed is the step's RECORDED worktree when
			// one was declared, else the invoking checkout's root — never the
			// bare process cwd (G7): a saga resumed from another directory, or
			// a conductor recording an executor's work, must capture the tree
			// the work touched.
			execRoot := runExecRoot(conn, step.RunID)
			dir := step.WorkRoot
			if dir == "" {
				dir = execRoot
			}
			// DKT-11 / DKT-20 / DKT-42: for a worktree, the base is its FORK
			// POINT; for the shared checkout, the run's PINNED starting
			// commit — see runDiffBase's doc for why.
			base := runDiffBase(conn, step.RunID, dir, execRoot)
			diffBody, err = e.DiffFn(dir, base, scope)
			if err != nil {
				return fmt.Errorf("computing the diff for %s: %w", step.Instance, err)
			}
			if base == "" && dir != execRoot {
				// An empty base is ordinarily the correct default (dir IS
				// the checkout, diff it against its own HEAD) — but here dir is
				// a DIFFERENT tree than execRoot, i.e. a worktree was recorded
				// and the cross-checkout base this diff needed could not be
				// resolved. Silently falling back to dir's own HEAD reproduces
				// the exact empty-diff symptom this issue exists to end, with
				// no way for a reader of the artifact to tell the difference
				// from "nothing changed". Say so in the artifact itself, since
				// that is the one place every downstream consumer already
				// looks (git-show fallback, RUN-8).
				diffBody = "# issue.diff: could not resolve the run's pinned " +
					"base commit; this diff compares " + dir + " against its " +
					"own HEAD instead, which may be empty or incomplete\n" + diffBody
			}
			// DKT-106: record the tree's HEAD beside the diff and, on a loop
			// re-entry, append this ROUND's delta to the cumulative body.
			diffPayload = e.appendRoundDelta(conn, step, dir, execRoot, &diffBody)
		}
		wantsDiff = true

		// DKT-259: AN EMPTY RE-RECORD DOES NOT REPLACE A RECORDED CHANGE.
		//
		// A re-execution — an operator `--as retry`, most often — diffs a tree
		// that ALREADY CONTAINS the work, against a base that already contains
		// it too, so the diff comes back empty. Recording that superseded the
		// issue's real `issue.diff` with 0 bytes, the sha of empty input: the
		// evidence of the change was replaced by a recording of nothing, and
		// every downstream reader that resolves "the latest diff" then reviewed
		// an empty object.
		//
		// The rule is narrow on purpose. An empty diff is not evidence that the
		// change vanished; it is evidence that this measurement had nothing to
		// compare. So it is dropped only when a NON-EMPTY one already exists
		// for the issue — a first empty diff still records, because there a
		// genuine "nothing changed" is exactly what happened and suppressing it
		// would hide a step that produced no work.
		//
		// Read BEFORE the transaction opens, on the pooled connection, for the
		// same reason loadHoldTally is: inside the transaction it would
		// deadlock against the one-connection pool rather than fail.
		if diffRecordsNoChange(diffBody) &&
			issueHasRecordedChange(conn, step.RunID, step.IssueID) {
			wantsDiff = false
		}

		// AND A BYTE-IDENTICAL RE-RECORD IS NOT A SUPERSESSION. Nothing
		// revised, so a new row would say something happened that did not.
		//
		// This is the rule that makes `--as rerun-gates` honest: a gate-only
		// re-run rewinds to `recorded` and walks the routing stage again, which
		// recomputes the diff — over a tree nobody touched in between, so it
		// comes back the same. The verb promises the recorded work is left
		// exactly as it was found, and a second identical artifact breaks that
		// promise in the one place a reviewer looks.
		//
		// It is also DKT-258's third shape, arriving here first because this
		// change created a new way to produce it: RUN-15's reconcile recorded
		// the same 79-byte sha three times and RUN-16 twice, and the artifact
		// chain read as a sequence of revisions when nothing revised.
		if latestIssueDiffBody(conn, step.RunID, step.IssueID) == diffBody {
			wantsDiff = false
		}
	}

	// THE HAND-BACK COMPARISON (DKT-588), for a LOOP BODY at ordinal > 0: the
	// commit this completion is about to hand back (the `head` of the round
	// record above) against the hand-back the SAME loop body recorded at its
	// newest earlier ordinal. Identical shas mean the round moved nothing —
	// RUN-34's fix@2 handed back the unchanged HEAD 64d3336b3d71 and a full
	// 5-judge + synthesize + verify round (~2.9 budget units) still ran over
	// the zero-byte delta, because DKT-340's non-convergence guard fires only
	// at the NEXT loop's entry, after the wasted round has already been paid
	// for. The comparison is scoped to this step's OWN records rather than
	// latestIssueDiffHead's issue-wide newest, because another producer
	// (`implement` at ordinal 0, a downstream committer) may have written the
	// issue's newest head, and the question here is whether THIS body moved
	// the tree since ITS last round.
	//
	// Read here on the pooled connection, before the transaction opens, for
	// loadHoldTally's reason exactly: inside the transaction it would deadlock
	// against the one-connection pool rather than fail.
	//
	// THE MEASUREMENT MUST BE REAL, failing in roundMovedNothing's direction:
	// an unresolvable head on either side ("" — HeadFn could not name a
	// commit, or the prior round recorded none) is not evidence of an
	// unchanged tree, so every degenerate case answers "changed" and the round
	// proceeds. Parking a run on a broken head resolution would turn a diff
	// setup problem into a stalled run, a worse failure than the wasted round
	// this exists to prevent.
	unchangedHandBack := ""
	if spec.Loop && step.Ordinal > 0 {
		if head := handBackHead(diffPayload); head != "" &&
			head == priorRoundHandBack(conn, step) {
			unchangedHandBack = head
		}
	}

	// §5: the order the threshold evaluates under, and the validator an action's
	// output is checked against, both come from the schema the run PINNED. They
	// are resolved ONCE here and threaded into both consumers, so a step cannot
	// aggregate under one document and route under another.
	order, err := stepOrder(conn, step, spec)
	if err != nil {
		return err
	}

	// RESUMING FROM A HOLD, the action does NOT re-run. Its artifact and its
	// `action_results` rows are already recorded, and recomputing would produce
	// a second result — possibly a second hold — for a question an operator has
	// just answered. This is the other half of H10's "exactly once".
	resumedFromHold := step.SagaStage == db.SagaHeld

	// The action seam, also outside every transaction: the real runner may spawn
	// a user-trusted command, and §6's "no subprocess ever executes inside a
	// transaction" is a property of THIS boundary rather than of the runner.
	var action *ActionResult
	if step.Kind == workflow.ClassAction && !resumedFromHold {
		result, err := e.runAction(conn, step, spec, order, nowMS)
		if err != nil {
			return err
		}
		action = result
	}

	// ---- The routing decision. ---------------------------------------------
	payloads, err := stepPayloads(conn, step, spec, action)
	if err != nil {
		return err
	}

	verdict, unmeasured, err := gateVerdict(conn, step.ID)
	if err != nil {
		return err
	}

	// A completion whose only product is gap artifacts parks for the operator
	// (DKT-25). The worker's whole answer was "this work cannot be done here,
	// and here is the residue": the gates measured an unchanged tree, and a
	// threshold routed over that would schedule the issue's entire remaining
	// pipeline over an empty change — a full judge fanout independently
	// verifying "nothing to judge" was the measured cost. `waiting-human`
	// stops the lineage at its source; `step resolve` is the disposition.
	gapOnly := false
	if isExecutorStep(step) {
		gapOnly, err = gapOnlyCompletion(conn, step, spec)
		if err != nil {
			return err
		}
	}

	// evaluate applies the threshold. It is a closure rather than a value
	// because a HELD step must not evaluate it at all (H10) — except on a stale
	// lineage, where the routing is recorded for the ledger and nothing is
	// materialized (H20), and where the evaluation therefore happens inside the
	// transaction that decided the lineage was stale. It is pure, so calling it
	// there costs nothing and races nothing.
	evaluate := func() (string, string, error) {
		result, err := EvaluateThreshold(
			step.Instance, spec.Threshold, ThresholdOrder(spec.Threshold),
			payloads, order)
		if err != nil {
			return "", "", err
		}
		return result.Routing, result.Reason, nil
	}

	// A tripped `hold_spread` DEFERS the routing decision. The threshold is not
	// evaluated here and the loop is not entered; both wait for the operator.
	holding := action != nil && !action.Failed && len(action.Held) > 0 &&
		verdict != VerdictFail

	var (
		routing string
		reason  string
		cover   *batchCover
	)
	switch {
	case gapOnly:
		// Decided before the gate verdict on purpose: with no recorded
		// content, neither a passing gate nor a failing one is a judgment of
		// the (nonexistent) work, and an `on_fail` fix-loop entered here would
		// spawn a fix step exactly as unimplementable as this one was.
		routing = workflow.OnFailWaitingHuman
		reason = "the step recorded only gap artifacts; operator disposes"
	case len(unmeasured) > 0:
		// A GATE THAT MEASURED NOTHING PARKS (DKT-254). It is decided before
		// the fail case because `skipped` is not-pass and would otherwise be
		// swallowed by it, which is how RUN-22 STEP-380 and RUN-27 STEP-467
		// came to route a swept worktree into a 3-seat verify-tribunal: three
		// seats deliberated over an infrastructure artifact instead of the
		// change, because "we could not measure" arrived at the panel wearing
		// the same word as "we measured and it failed".
		//
		// `on_fail` is the wrong destination for the same reason it is wrong
		// for a gap-only completion above: it routes as though a judgment was
		// reached about the work. None was. Nothing about the change is known,
		// and a fix loop entered here would ask a worker to fix a tree that was
		// never read. What IS known is an environment condition an operator can
		// act on — reconstruct the tree, restore the entry, re-run the gate —
		// so the disposition goes to a person, which is what `waiting-human`
		// means everywhere else in this switch.
		//
		// This is the STATED consequence the issue asks for. It is stated here,
		// once, rather than inherited by each caller from whatever the
		// execution verdict happened to be — which is what let RUN-29 STEP-746
		// record `skipped` and silently pass-route.
		routing = workflow.OnFailWaitingHuman
		reason = fmt.Sprintf(
			"gate(s) %s measured nothing — the tree under review could not be "+
				"bound, so no verdict about the change was reached; operator disposes",
			strings.Join(unmeasured, ", "))
	case verdict == VerdictFail:
		// A failed gate routes per `on_fail`, not through the threshold: the
		// threshold asks a question about a RESULT, and a step whose gate
		// failed has no result to ask about.
		//
		// UNLESS a run-scoped batch override grant covers EVERY failed gate's
		// signature (DKT-546): the operator already ruled this exact failure
		// environmental for this run, and re-parking it would re-ask a settled
		// question. The pass it records is the same generic RoutingPass the
		// operator's own override-pass records, attributed to the grant(s) in
		// the routing record and in its own event — never silently. A cover
		// blocked by an interposed threshold target parks as usual, with the
		// block named as the reason.
		routing = spec.EffectiveOnFail()
		if cover, err = batchOverrideCover(conn, step, spec); err != nil {
			return err
		}
		switch {
		case cover == nil:
		case cover.blocked != "":
			reason = cover.blocked
			cover = nil
		default:
			routing, reason = RoutingPass, cover.reason()
		}
	case action != nil && action.Failed:
		// B3: a builtin's bad params or an unorderable value, and a trusted
		// command's non-zero exit or unmatched name, are STEP failures routed
		// per the step's effective `on_fail` — never engine errors that abort
		// the saga. A workflow authoring mistake must not wedge a run.
		routing, reason = spec.EffectiveOnFail(), action.Reason
	case holding:
		// Deferred. Decided inside the transaction, once the lineage is known.
	default:
		if resumedFromHold {
			// §7.7.3: `reject` routes the ROUTING step per its effective
			// `on_fail`, SKIPPING THE THRESHOLD. `approve` falls through to the
			// threshold, which now sees the resolved payload — `stepPayloads`
			// already reads the step's latest artifact, so it arrives with no
			// new read path.
			_, approved, err := heldDecision(conn, step)
			if err != nil {
				return err
			}
			if !approved {
				routing = spec.EffectiveOnFail()
				reason = "the held clusters were rejected by an operator"
				break
			}
		}
		routing, reason, err = evaluate()
		if err != nil {
			return err
		}
	}

	// Who decides a hold, read BEFORE the transaction opens: it is a config
	// read on the pooled connection, which inside the transaction below would
	// deadlock rather than fail. Zero unless this project configured a tally,
	// and unused unless this step is about to hold.
	tally, err := loadHoldTally(conn, step.RunID)
	if err != nil {
		return err
	}

	// ---- ONE transaction over step, issue mirror, run, and events. ---------
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning routing: %w", err)
	}
	defer tx.Rollback()

	// §11.3 (2)'s inert half, evaluated INSIDE the transaction that would apply
	// the effects — the ordinal and the issue's `loop_count` must be read under
	// the same lock that a concurrent loop entry would take, or the check races
	// the thing it is checking.
	stale, err := StaleLineage(tx, step)
	if err != nil {
		return err
	}

	// The action's artifact and per-attempt records land FIRST, before any
	// routing effect in this transaction — H4's "one transaction, or a crash
	// leaves a held payload with nobody able to resolve it" for the holding
	// path, and, for the routing path, what makes the CURRENT round visible to
	// the loop-entry evidence reads (DKT-870): an action step's artifact used
	// to land after applyFixLoop, so routingVerdictUnchanged (DKT-589) and the
	// flat-volume signal read an action-routed round's evidence one round
	// late — an executor's artifact is recorded at `complete`, stages earlier,
	// and the two step classes must measure alike.
	if action != nil {
		if err := recordActionResult(tx, step, action, nowMS); err != nil {
			return err
		}
	}

	if holding {
		if !stale {
			return enterHeld(tx, step, action.Held, tally, nowMS)
		}
		// H20: a STALE-LINEAGE hold is inert exactly as a stale-lineage routing
		// is. The aggregate still records its artifact and its `action_results`
		// row — history is attributed — and it neither materializes a held step
		// nor gates anything, because nothing downstream of it will run.
		routing, reason, err = evaluate()
		if err != nil {
			return err
		}
	}

	status := statusForRouting(routing)

	// AN UNCHANGED HAND-BACK PARKS THE ROUND AT ITS SOURCE (DKT-588), before
	// the review chain downstream spends anything. The park lands on the loop
	// body's OWN row — the gap-only and measured-nothing parks above are the
	// precedent, and "waiting-human stops the lineage at its source" is their
	// exact reasoning — because the downstream chain this ordinal instantiated
	// at loop entry already waits on this body: readiness withholds the
	// `after_loop` closure from every offer while its same-ordinal loop body
	// is non-terminal (blockingLoopBodyAbsent, DKT-48/DKT-61), and the run
	// rollup parks the run with the step. Nothing is superseded, deliberately:
	// `superseded` is terminal, so sweeping the chain would let the issue
	// COMPLETE without its judges ever running the moment an operator resolved
	// the park, and would leave `--as override-pass` — the way out the reason
	// names — releasing a round with nothing left in it to run.
	//
	// Only a routing that would have handed the round downstream is overridden
	// — `pass`, the status-table row that makes the chain claimable. A failed
	// gate's `on_fail` and a threshold's own routing already decided something
	// about this completion and are left alone.
	if !stale && routing == RoutingPass && unchangedHandBack != "" {
		routing = workflow.OnFailWaitingHuman
		reason = fmt.Sprintf(
			"round %d of %s handed back the same commit %.12s its previous "+
				"round recorded: the loop body changed nothing, so the review "+
				"chain would read the identical tree and reach the identical "+
				"verdict; `docket step resolve --as override-pass` runs the "+
				"chain anyway, `--as retry` redoes the round",
			step.Ordinal, model.FormatID(step.IssueID), unchangedHandBack)
		status = statusForRouting(routing)
	}

	// A PASS THAT WOULD LEAVE DECLARED-FLOOR WORK STANDING PARKS INSTEAD
	// (DKT-870). RUN-58's reconcile routed `pass` with all 16 clusters open —
	// six at the order's high position, none held, none operator-resolved —
	// and the loop exited clean: the threshold had read the field its author
	// pointed it at, and nothing read the evidence recorded beside it. When
	// the step declares a `pass_floor`, the pass is measured against the
	// step's OWN recorded payload under the same pinned order the threshold
	// evaluates under, and a pass with standing floor-or-above elements
	// becomes `waiting-human` — see passFloorStanding for the exemptions and
	// the fail-toward-pass discipline.
	//
	// The override takes the unchanged-hand-back park's exact shape and
	// placement, for its reasons: only a `pass` — the one routing that hands
	// the issue onward as settled — is overridden, a failed gate's `on_fail`
	// and a threshold's own `fix-loop` already decided something and are left
	// alone, and the park lands before applyFixLoop so a parked pass never
	// touches the loop counter. `--as override-pass` (below) is the recorded
	// operator exit; `--as fix-round` buys a round instead.
	if !stale && routing == RoutingPass && spec.PassFloor != nil {
		if floor := passFloorStanding(spec.PassFloor, payloads, order); floor.Standing > 0 {
			routing = workflow.OnFailWaitingHuman
			reason = fmt.Sprintf(
				"%d element(s) of %s's recorded payload sit at or above the "+
					"declared pass_floor (%s >= %s), none held and none "+
					"operator-resolved, so a `pass` would exit with that work "+
					"still standing; `docket step resolve --as override-pass` "+
					"exits anyway, `--as fix-round` authorizes another round "+
					"instead",
				floor.Standing, step.Instance, spec.PassFloor.Field,
				spec.PassFloor.At)
			status = statusForRouting(routing)
		}
	}

	// A `fix-loop` routing from a LIVE lineage is the loop entry (§11.3). It
	// runs before the step's own status is written so its outcome — entered, or
	// bounded to `waiting-human` — decides that status. applyFixLoop re-reads
	// StaleLineage inside the same transaction, so the outer guard is only an
	// early-out, never a second opinion.
	if !stale {
		var outcome *LoopOutcome
		routing, outcome, err = applyFixLoop(tx, step, defs[step.WorkflowID], routing, nowMS)
		if err != nil {
			return err
		}
		if outcome != nil {
			reason = outcome.Reason
			status = statusForRouting(routing)
		}
	}

	if wantsDiff {
		if _, err := db.InsertArtifactTx(tx, db.Artifact{
			RunID: step.RunID, StepID: step.ID, Kind: ArtifactKindIssueDiff,
			Body: diffBody, Payload: diffPayload,
			SHA256: workflow.SHA256([]byte(diffBody)),
		}, nowMS); err != nil {
			return err
		}
	}

	// THE ROUTING IS RECORDED EITHER WAY (§7.3 (3): "records the routing on the
	// step for the ledger"). A superseded lineage's step still finished, still
	// decided something, and the ledger still attributes it — what a stale
	// lineage loses is its DOWNSTREAM EFFECT, not its history.
	if err := db.SetStepRoutingTx(tx, step.ID, routingRecord(routing, reason), status, nowMS); err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepRouted, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID, Data: routing, AtMS: nowMS,
	}); err != nil {
		return err
	}
	// A batch-covered pass spends operator authority, so it is attributed like
	// the resolution it stands in for (DKT-546): the covering grants' counters
	// move and the feed records which grants decided this step — in this same
	// transaction, and BEFORE the stale guard, because §7.3 (3) records a stale
	// lineage's routing for the ledger too, and an override the ledger carries
	// must name its authority either way.
	if cover != nil {
		if err := db.CoverGateOverrideGrantsTx(tx, cover.grantIDs); err != nil {
			return err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventStepBatchOverridden, RunID: step.RunID,
			Instance: step.Instance, IssueID: step.IssueID,
			Data: cover.eventData(), AtMS: nowMS,
		}); err != nil {
			return err
		}
	}

	// ---- The inert guard (§7.3 (3), §11.3 (2)). ---------------------------
	//
	// "no supersede, no re-expansion, no issue status change, no loop
	// increment". The first, second, and fourth were skipped above by not
	// entering the loop; the THIRD is skipped here, by not reconciling.
	//
	// Reconciling a stale lineage is the subtle harm: a slow `verify@0`
	// finishing after `fix@1` started would otherwise evaluate the issue's
	// completion and could mark it `done` — completing an issue whose ordinal-1
	// work has not run. The rollup that would do it is the same one that is
	// correct for a live lineage, which is why the guard is here rather than
	// inside it.
	if stale {
		return finishRoutingStage(tx, step, nowMS)
	}

	// The issue mirror and the run rollup, in the same transaction.
	if err := reconcileIssueAndRun(tx, step, spec, routing, nowMS); err != nil {
		return err
	}

	return finishRoutingStage(tx, step, nowMS)
}

// finishRoutingStage closes the saga and commits — the last two operations of
// the routing stage, shared by the live and inert paths so a stale lineage's
// saga completes exactly as a live one's does.
//
// An inert routing that skipped this would leave the step wedged in
// `saga_stage = 'routing'` forever, resumed by every later engine invocation
// and re-recording its routing each time. "Inert" means no downstream effect,
// not unfinished.
func finishRoutingStage(tx *sql.Tx, step *db.Step, nowMS int64) error {
	// The CAS guard is the step's OWN current stage, not the literal `routing`.
	// A step resuming from `held` reaches here with `saga_stage = 'held'`, and a
	// guard that named `routing` would match zero rows and leave the saga
	// wedged in a stage every later invocation would re-run.
	if err := db.AdvanceSagaTx(tx, step.ID, step.SagaStage, "", nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// isExecutorStep reports whether a step's completion recomputes the issue diff
// (§6.7.1 D1/D2). Fanout siblings and `loop = true` instances count: they are
// executor steps that change the tree.
func isExecutorStep(step *db.Step) bool {
	return step.Kind == workflow.ClassExecutor
}

// statusForRouting maps a routing to the step status it produces (§6.2).
func statusForRouting(routing string) string {
	switch routing {
	case workflow.OnFailWaitingHuman:
		return db.StepWaitingHuman
	case workflow.OnFailSkip:
		return db.StepSkipped
	case workflow.OnFailAbandonIssue:
		return db.StepFailedRouted
	case RoutingPass:
		return db.StepDone
	case workflow.OnFailFixLoop:
		// The loop ENTRY is phase 4's (§11.3). At S3 the step itself is done —
		// its work recorded and its routing decided — and what phase 4 adds is
		// the instantiation the routing calls for, not a different status here.
		return db.StepDone
	}
	// A step-name routing interposes that step as a successor gate (§11.2); the
	// routing step itself is done.
	return db.StepDone
}

// routingRecord stores the routing, with a T3 park's reason appended so an
// operator resolving the step can see what could not be decided.
func routingRecord(routing, reason string) string {
	if reason == "" {
		return routing
	}
	return routing + ": " + reason
}

// gateVerdict reads the step's recorded results and reports the overall
// verdict. Any failing gate fails the step: gates are conjunctive.
//
// M-b: this reads `gate_results` (v8) rather than the trail JSON, and it knows
// the `unmatched` verdict, which N3 makes a FAILURE for routing — not a pass
// and not a skip.
//
// PG4: pre-gate rows are EXCLUDED. They are inputs to the step, not judgments
// of it — §11.1 calls them "measure-then-judge steps", and the judging is the
// step's job. S3 already excludes `pre` gates from completionGates; this is the
// read-side counterpart.
//
// It returns the UNMEASURED gates alongside the verdict (DKT-254). `skipped`
// still counts as not-pass, so the verdict is unchanged; what the second result
// buys is that the CALLER can tell "measured and failed" from "measured
// nothing" without re-reading the rows, which is what lets the two route
// differently.
func gateVerdict(conn *sql.DB, stepID int) (string, []string, error) {
	rows, err := db.GateResultsForStep(conn, stepID)
	if err != nil {
		return "", nil, err
	}
	verdict, unmeasured := verdictOverRows(rows)
	return verdict, unmeasured, nil
}

// verdictOverRows reduces a step's recorded rows to a routing verdict, and
// names the gates that measured nothing.
func verdictOverRows(rows []db.GateResultRow) (string, []string) {
	last := lastGateAttempts(rows)

	verdict := VerdictPass
	var unmeasured []string
	for _, r := range last {
		if r.Verdict != db.GateVerdictPass {
			verdict = VerdictFail
		}
		if r.Verdict == db.GateVerdictSkipped {
			unmeasured = append(unmeasured, r.Gate)
		}
	}
	// Sorted so the reason a step parks with is the same sentence on every
	// run over the same rows — map range order is not.
	sort.Strings(unmeasured)
	return verdict, unmeasured
}

// lastGateAttempts reduces a step's recorded rows to the one attempt per gate
// that ROUTES.
//
// Ordinals carry flaky re-runs (§5.6 F3): every attempt is its own row, and F4
// makes the LAST attempt's verdict the one that routes. So the decision is made
// per (gate, ordinal-max), never over every row — otherwise a gate that failed
// twice and passed on the third try would route as a failure, which is exactly
// what declaring it flaky was meant to prevent. Pre-gate rows are excluded per
// PG4: they are inputs to the step, not judgments of it.
//
// It is shared by the routing verdict and by FailedGates so the gates a caller
// REPORTS are, by construction, the rows the saga ROUTED on. Two reductions
// over the same table is how a report comes to contradict the routing beside
// it — which is the class of defect DKT-982 is.
func lastGateAttempts(rows []db.GateResultRow) map[string]db.GateResultRow {
	last := make(map[string]db.GateResultRow)
	for _, r := range rows {
		if r.Pre {
			continue // PG4
		}
		prev, seen := last[r.Gate]
		if !seen || r.Ordinal >= prev.Ordinal {
			last[r.Gate] = r
		}
	}
	return last
}

// FailedGate is one completion gate whose routing attempt did not pass, in the
// shape a caller needs to SAY SO: the name, the verdict word, and the exit code
// the process left behind.
//
// Exit is a pointer for db.GateResultRow's reason exactly — an `unmatched` gate
// never ran, and rendering `exit 0` for a process that does not exist reads as
// a pass (T11).
type FailedGate struct {
	Gate    string `json:"gate"`
	Verdict string `json:"verdict"`
	Exit    *int   `json:"exit"`
	// Reason explains an `unmatched` verdict or a timeout, verbatim from the
	// recorded row.
	Reason string `json:"reason,omitempty"`
}

// FailedGates returns the completion gates that did not pass, sorted by name.
//
// It is the READ SIDE of the routing decision (DKT-982): a step whose gate
// failed parks, and until this existed the verb that parked it could name
// nothing — `step record` printed a success-shaped line with no gate on it, and
// the executor reading that line reported the run green. The engine knew at
// print time; it had no accessor to say it with.
//
// Empty means every completion gate passed, or none ran.
func FailedGates(conn *sql.DB, stepID int) ([]FailedGate, error) {
	rows, err := db.GateResultsForStep(conn, stepID)
	if err != nil {
		return nil, err
	}
	return failedGatesOverRows(rows), nil
}

// failedGatesOverRows is FailedGates over rows already read — pure, so a test
// can weigh the reduction without a store.
func failedGatesOverRows(rows []db.GateResultRow) []FailedGate {
	last := lastGateAttempts(rows)

	failed := make([]FailedGate, 0, len(last))
	for _, r := range last {
		// Not-pass, in the same sense verdictOverRows uses: `fail`, `unmatched`,
		// and `skipped` all route as a failure, and a report that named only the
		// first would be silent about the two the routing acted on.
		if r.Verdict == db.GateVerdictPass {
			continue
		}
		failed = append(failed, FailedGate{
			Gate: r.Gate, Verdict: r.Verdict, Exit: r.Exit, Reason: r.Reason,
		})
	}
	// Sorted for verdictOverRows' reason: the sentence a step parks with must be
	// the same on every run over the same rows, and map range order is not.
	sort.Slice(failed, func(i, j int) bool { return failed[i].Gate < failed[j].Gate })
	return failed
}

// runGate invokes the runner and normalizes its output to rows.
//
// A runner that implements the richer GateExecution shape (the real one)
// reports every attempt; one that implements only the seam's single-result
// interface (the S3 pass-through, and the fakes tests build on it) reports one.
// Both are recorded the same way, which is what lets a test swap in a
// spawn-counting or witness runner without the saga knowing.
func runGate(runner GateRunner, spec GateSpec, sc StepContext) ([]GateResultRow, error) {
	if rich, ok := runner.(gateExecutor); ok {
		ex, err := rich.Execute(context.Background(), spec, sc)
		if err != nil {
			return nil, err
		}
		return ex.Results, nil
	}

	res, err := runner.Run(context.Background(), spec, sc)
	if err != nil {
		return nil, err
	}
	exit := res.Exit
	return []GateResultRow{{
		Gate: spec.Name, Ordinal: 0, Argv: res.Argv, Exit: &exit,
		DurationMS: res.DurationMS, Output: res.Output,
		Truncated: res.Truncated, Verdict: res.Verdict, Stub: res.Stub,
	}}, nil
}

// gateExecutor is the richer seam the real runner implements.
type gateExecutor interface {
	Execute(ctx context.Context, g GateSpec, sc StepContext) (GateExecution, error)
}

// recordGateRows writes one `gate_results` row per attempt, and the trust-cache
// audit row alongside.
//
// Ordinals continue from what the step already recorded, so a resumed gate's
// re-run does not collide with the interrupted attempt's row (§7.5 A3) and the
// UNIQUE(step_id, gate, ordinal) index holds.
func recordGateRows(
	tx *sql.Tx, step *db.Step, gate string, rows []GateResultRow, nowMS int64,
) error {
	base, err := nextGateOrdinalTx(tx, step.ID, gate)
	if err != nil {
		return err
	}

	for i, r := range rows {
		if err := db.InsertGateResultTx(tx, db.GateResultRow{
			RunID: step.RunID, StepID: step.ID, Gate: r.Gate,
			Ordinal: base + i, Argv: r.Argv, Exit: r.Exit,
			DurationMS: r.DurationMS, Output: r.Output, Truncated: r.Truncated,
			Verdict: r.Verdict, Pre: r.Pre, Reason: r.Reason,
			Stub:        r.Stub,
			StubEntry:   r.StubEntry,
			CreatedAtMS: nowMS,
		}); err != nil {
			return err
		}

		// The trust decision is recorded as an AUDIT FACT (§4.5): what this run
		// considered trusted, and when. It is never consulted to authorize a
		// spawn — a cache hit that could authorize would make `trust rm` take
		// effect only after the cache cleared, which is a revocation failure.
		if r.ArgvSHA256 != "" {
			if err := db.InsertTrustCacheTx(tx, step.RunID, db.TrustKindGate,
				r.Gate, r.ArgvSHA256,
				r.TrustEntry, r.Verdict != db.GateVerdictUnmatched, r.Prefix,
				nowMS+int64(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordGateEvents writes the events that accompany one gate's recorded
// results: the per-row exceptions, then the `gate-recorded` that closes it.
//
// §6.4: a refusal to execute gets its OWN event, so an operator following the
// feed sees it as an event rather than as a result they must open and inspect.
// `gate-rerun` marks each flaky re-run (ordinal > 0) for the same at-least-once
// observability reason `gate-started` exists.
//
// IT IS ONE FUNCTION FOR THE SAGA AND THE PRE-GATE PATH BECAUSE THE TWO DRIFTED.
// The pre-gate path recorded its rows and emitted only `gate-unmatched`, so
// every pre-gate that passed announced a `gate-started` and then closed with
// nothing at all — a started-but-never-recorded gate in the feed, which is
// indistinguishable from the crash `gate-started` exists to make visible. §7.6.1
// phase 2 requires a pre-gate commit "its own `gate-started`/result commit
// exactly as the saga's do", and PG1 forbids a pre-gate-specific path anywhere.
// A shared writer is the only shape in which "exactly as the saga's" cannot
// quietly stop being true again.
func recordGateEvents(
	tx *sql.Tx, step *db.Step, gate string, rows []GateResultRow,
) error {
	for _, r := range rows {
		kind := ""
		switch {
		case r.Verdict == VerdictUnmatched:
			kind = EventGateUnmatched
		case r.Ordinal > 0:
			kind = EventGateRerun
		}
		if kind == "" {
			continue
		}
		// NO AtMS: these stamp at EMISSION time, like the `gate-recorded` below
		// them (DKT-66). They used to carry the transaction's `nowMS`, which on
		// the resume path is the time the STEP was recorded — so a re-run
		// announced a minute later landed in the feed stamped a minute before
		// the event preceding it, and the two events closing one gate disagreed
		// about when the gate happened.
		data, err := gateEventData(gate, r.Verdict, r.Exit, r.Pre, r.StubEntry)
		if err != nil {
			return err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: kind, RunID: step.RunID, Instance: step.Instance,
			IssueID: step.IssueID, Data: data,
		}); err != nil {
			return err
		}
	}

	// THE CLOSING EVENT CARRIES THE VERDICT (DKT-63).
	//
	// It used to carry the gate NAME and nothing else, so a gate that failed
	// rendered as `gate-recorded ... detail=build` — character-identical to one
	// that passed. A conductor read both surfaces on 2026-08-16 and reported
	// three failed gates as passes, which is the worst possible failure mode for
	// a feed whose purpose is to say what happened.
	//
	// The verdict is the LAST row's: attempts are recorded in order and a flaky
	// re-run supersedes the attempt before it, so the final row is the outcome
	// the saga itself routes on.
	verdict, exit, pre, stub := gateOutcome(rows)
	data, err := gateEventData(gate, verdict, exit, pre, stub)
	if err != nil {
		return err
	}
	return recordEvent(tx, eventRecord{
		Kind: EventGateRecorded, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID, Data: data,
	})
}

// gateOutcome reduces a gate's attempts to the one that counts: the LAST,
// because a re-run supersedes the attempt before it. `pre` and `stub` ride
// along from the same row: every attempt of one gate is produced by one
// phase, so the last row's markers are the gate's.
//
// `stub` reads StubEntry — the matched trust entry's own `stub` declaration
// (DKT-265) — not the legacy S3-migration `Stub` field. StubEntry is what
// `gate_results` (`step gates --json`) and `run report` already render as
// `stub`, and DKT-983 asks this event to say what those surfaces already say.
func gateOutcome(rows []GateResultRow) (verdict string, exit *int, pre, stub bool) {
	if len(rows) == 0 {
		return "", nil, false, false
	}
	last := rows[len(rows)-1]
	return last.Verdict, last.Exit, last.Pre, last.StubEntry
}

// gateEventData renders a gate event's payload: the gate name in `detail`,
// where it has always been, plus the verdict and the exit code (DKT-63).
//
// A missing exit stays ABSENT rather than rendering as 0 — an unmatched gate
// never ran, and `exit=0` on a gate that was refused execution would read as a
// pass.
//
// `pre` MARKS THE VERDICTS THAT ROUTED NOTHING (DKT-862). A §11.1 pre-gate runs
// at claim as an input to the step, and PG4 keeps its result out of the saga's
// verdict — so `gate-recorded ... verdict=fail` on a pre-gate was reporting a
// failure that never blocked anything, in bytes identical to one that did. On
// RUN-61 three such rows appeared in the feed beside the `step-routed` that
// contradicted them, and nothing on the line said which was which.
//
// It rides as its OWN KEY rather than as an adjective on the verdict, for two
// reasons. `verdict` is a closed vocabulary a program reads, and "fail
// (advisory)" is not in it. And the human line comes from eventDetail, which
// renders `data` as sorted `key=value` pairs and INTERPRETS NOTHING — a
// renderer that special-cased this pair would be the first key core's event
// feed had an opinion about.
//
// It is ABSENT on a blocking gate rather than `pre=false`, so the marker's
// presence is the whole signal and the overwhelmingly common line does not
// grow a column that always says the same thing.
//
// `stub` MARKS A GATE WHOSE PASS WAS NEVER MEASURED (DKT-983). A stub-trusted
// command's pass is already marked stub:true in the trust store, in
// `gate_results` rows (`step gates --json`), and in `run report` — but until
// this, the event stream carried none of it, so `gate-recorded ... verdict=pass`
// for a stub was byte-identical to a real measurement. It is ABSENT rather
// than `stub=false` for the same reason `pre` is: the marker's presence is the
// whole signal. The caller passes StubEntry (the trust entry's own `stub`
// declaration), never the legacy S3-migration `Stub` field — StubEntry is
// what every other stub-aware surface already reads.
func gateEventData(gate, verdict string, exit *int, pre, stub bool) (string, error) {
	fields := map[string]any{"detail": gate}
	if verdict != "" {
		fields["verdict"] = verdict
	}
	if exit != nil {
		fields["exit"] = *exit
	}
	if pre {
		fields["pre"] = true
	}
	if stub {
		fields["stub"] = true
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("encoding the gate event for %s: %w", gate, err)
	}
	return string(out), nil
}

// nextGateOrdinalTx is db.NextGateOrdinal inside a transaction.
func nextGateOrdinalTx(tx *sql.Tx, stepID int, gate string) (int, error) {
	var next sql.NullInt64
	err := tx.QueryRow(
		`SELECT MAX(ordinal) FROM gate_results WHERE step_id = ? AND gate = ?`,
		stepID, gate).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("reading the gate ordinal: %w", err)
	}
	if !next.Valid {
		return 0, nil
	}
	return int(next.Int64) + 1, nil
}

// stepPayloads returns the payload set a threshold aggregates over: the action
// runner's result for an action step, or the recorded artifact's payload for
// any other.
//
// The read is SCOPED TO THE STEP'S DECLARED KIND (DKT-101). A completion that
// files gaps records them beside the declared emit with higher artifact ids, so
// "latest artifact of the step" was the last GAP — whose payload is empty — and
// the threshold evaluated over the empty set: every `any()` arm false, routing
// `pass`, and an interposed escalation gate skipped over a payload that named
// exactly the condition it existed to catch. Scoping by kind still reads a held
// cluster's resolution revision (it records under the same kind), so the
// resumed-from-hold path is unchanged.
func stepPayloads(
	conn *sql.DB, step *db.Step, spec *workflow.Step, action *ActionResult,
) ([]map[string]any, error) {
	if action != nil {
		return parsePayload([]byte(action.Payload))
	}

	// A step with no declared kind records no declared emit; whatever its
	// latest artifact is (there should be none) keeps the pre-DKT-101 read.
	query, args := `SELECT payload FROM artifacts WHERE step_id = ?
		 ORDER BY id DESC LIMIT 1`, []any{step.ID}
	if kind := workflow.ArtifactKind(spec); kind != "" {
		query, args = `SELECT payload FROM artifacts WHERE step_id = ? AND kind = ?
			 ORDER BY id DESC LIMIT 1`, []any{step.ID, kind}
	}

	var payload sql.NullString
	err := conn.QueryRow(query, args...).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the recorded payload: %w", err)
	}
	return parsePayload([]byte(payload.String))
}

// inputPayloads is the builtin action's INPUT CHANNEL (§2, amended): the
// concatenated payloads of the step's declared `inputs` artifacts, resolved per
// §6.7 — `done` instances only, in declared order, ordinal-scoped at loops.
//
// It resolves through ResolveInputArtifacts rather than reimplementing the rule. The
// resolver is the sole authority on which artifacts an input names, and the
// loop-ordinal fallback it applies per input (§7.4) is exactly the machinery an
// aggregate inside a loop depends on; a second implementation would be a second
// answer, and the two would diverge at the first ordinal that mattered.
//
// It reads the artifact ROWS rather than the §11.4 bundle, because the bundle
// is the worker-facing shape: a `ContextInput` carries the artifact's `body`
// and no `payload`, and the builtin reduces payloads. Both go through the same
// resolveDeclaredInput, so there is still exactly one copy of the §6.7 rule.
//
// The transaction is READ-ONLY and rolled back — a scheduler snapshot is what
// the `done`-only rule needs to be consistent, and this path has nothing to
// commit. It runs OUTSIDE the routing transaction, before the runner, for §6's
// no-subprocess-inside-a-transaction reason the context bundle is read here too.
//
// AN INPUT WITH AN EMPTY PAYLOAD CONTRIBUTES NOTHING AND IS NOT AN ERROR. Most
// artifacts carry no payload at all — only a step declaring `payload` records
// one — so an aggregate whose `inputs` name a mix gets the clusters from the
// ones that have them. What it must not do is silently tolerate MALFORMED
// bytes, and parsePayload still refuses those.
func inputPayloads(conn *sql.DB, step *db.Step, nowMS int64) ([]map[string]any, error) {
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning the input read: %w", err)
	}
	// Rolled back unconditionally: resolving an input commits nothing, and the
	// rollback is the structural guarantee of it rather than a convention.
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, step.RunID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	fresh := sched.stepByID[step.ID]
	if fresh == nil {
		return nil, notFoundErr(db.ErrStepNotFound,
			"step %s not found", model.FormatStepID(step.ID))
	}
	spec := materializedSpec(defs[fresh.WorkflowID], fresh, sched.holdTally)
	if spec == nil {
		return nil, validationErr("step %s: %q is not a step of its pinned workflow",
			fresh.Instance, fresh.StepName)
	}

	artifacts, err := ResolveInputArtifacts(tx, sched, fresh, spec)
	if err != nil {
		return nil, err
	}

	var out []map[string]any
	for _, a := range artifacts {
		payloads, err := parsePayload([]byte(a.Payload))
		if err != nil {
			return nil, fmt.Errorf("reading input ARTIFACT-%d of %s: %w",
				a.ID, step.Instance, err)
		}
		out = append(out, payloads...)
	}
	return out, nil
}

// parsePayload is §6.8 stage 0's SHAPE-ONLY validation: a payload must be a
// JSON array of objects, which is the shape a threshold aggregates over.
//
// The SCHEMA — field existence, types, ordered_enum — is S5's per §1's scope
// table. Validating fields here would mean inventing the schema language S5 is
// specified to define, and pinning it before its design.
func parsePayload(raw []byte) ([]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var payloads []map[string]any
	if err := json.Unmarshal(raw, &payloads); err == nil {
		return payloads, nil
	}

	// S3 (§6.3): ONE documented tolerance. An object with exactly the keys
	// `{stub, payload}` is unwrapped and its `payload` used.
	//
	// It exists to READ HISTORY. The S3/S4 stub runner wrapped every action
	// artifact that way, and the migration deliberately does NOT rewrite those
	// bytes — rewriting them would destroy the evidence that a computation did
	// not run, which is the entire reason the marker exists. So the reader
	// tolerates the shape the writer no longer produces.
	if inner, ok := unwrapStubPayload(raw); ok {
		return inner, nil
	}

	if err := json.Unmarshal(raw, &payloads); err != nil {
		return nil, validationErr(
			"payload must be a JSON array of objects: %v", err)
	}
	return payloads, nil
}

// unwrapStubPayload reads the S3/S4 `{"stub":true,"payload":[…]}` wrapper.
//
// EXACTLY those two keys. A wider tolerance would start unwrapping objects a
// worker meant as data, and the shape is historical rather than a format
// anything still writes, so it never needs to grow.
func unwrapStubPayload(raw []byte) ([]map[string]any, bool) {
	var wrapper struct {
		Stub    *bool            `json:"stub"`
		Payload *json.RawMessage `json:"payload"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wrapper); err != nil {
		return nil, false
	}
	if wrapper.Stub == nil || wrapper.Payload == nil {
		return nil, false
	}
	var payloads []map[string]any
	if err := json.Unmarshal(*wrapper.Payload, &payloads); err != nil {
		return nil, false
	}
	return payloads, true
}

// runAction assembles the ActionSpec and invokes the seam.
//
// The §11.4 context bundle is read HERE, before the runner, because assembling
// it needs a transaction and §6 forbids a subprocess inside one. A bundle that
// cannot be assembled is not fatal: a trusted command receives an empty stdin
// and can still run, which is a better outcome than wedging a routing stage over
// a read.
func (e *Engine) runAction(
	conn *sql.DB, step *db.Step, spec *workflow.Step, order OrderResolver, nowMS int64,
) (*ActionResult, error) {
	inputs, err := inputPayloads(conn, step, nowMS)
	if err != nil {
		return nil, err
	}

	a := ActionSpec{
		Name: spec.Action, Params: spec.Params,
		Output: workflow.ArtifactKind(spec),
		Inputs: inputs, Order: order,
		Validate: declaredPayloadValidator(conn, step, spec),
		Context:  actionContext(conn, step, nowMS),
	}
	sc := StepContext{Instance: step.Instance, RunID: step.RunID, IssueID: step.IssueID}

	result, err := e.Actions.Run(context.Background(), a, sc)
	if err != nil {
		return nil, fmt.Errorf("running action %s on %s: %w",
			spec.Action, step.Instance, err)
	}
	return &result, nil
}

// declaredPayloadValidator returns a validator for the step's declared payload
// schema, or nil when it declares none.
//
// It closes over the PINNED bytes, resolved once, so an action that runs for
// minutes is validated against the document its run agreed to rather than
// whatever the registry holds when it finishes.
func declaredPayloadValidator(
	conn *sql.DB, step *db.Step, spec *workflow.Step,
) func([]byte) error {
	if spec.Payload == "" {
		return nil
	}
	registered, err := pinnedSchema(conn, step.RunID, spec.Payload)
	if err != nil {
		// The refusal is deferred into the validator rather than returned:
		// a step whose pinned schema cannot be read must FAIL ITS ACTION (B3),
		// not abort the saga.
		return func([]byte) error { return err }
	}
	return registered.ValidatePayload
}

// actionContext renders the §11.4 context bundle for a trusted command's stdin,
// exactly as `docket step context --json` emits it (§6.2).
//
// It is the SAME assembler `step context` uses, called rather than reproduced:
// two renderings of one bundle would drift, and a command written against the
// documented shape would silently receive a different one.
func actionContext(conn *sql.DB, step *db.Step, nowMS int64) []byte {
	bundle, err := ReadContext(conn, step.ID, nowMS)
	if err != nil {
		return nil
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil
	}
	return encoded
}

// recordActionRows writes one `action_results` row per attempt, and the
// trust-cache audit row alongside — the same discipline recordGateRows follows,
// for the same reason.
//
// Ordinals continue from what the step already recorded, so a routing stage
// re-entered after a crash does not collide with the interrupted attempt's row
// and the UNIQUE(step_id, action, ordinal) index holds.
func recordActionRows(
	tx *sql.Tx, step *db.Step, rows []ActionResultRow, nowMS int64,
) error {
	if len(rows) == 0 {
		return nil
	}
	base, err := db.NextActionOrdinalTx(tx, step.ID, rows[0].Action)
	if err != nil {
		return err
	}

	for i, r := range rows {
		if err := db.InsertActionResultTx(tx, db.ActionResultRow{
			RunID: step.RunID, StepID: step.ID, Action: r.Action,
			Ordinal: base + i, Argv: r.Argv, Exit: r.Exit,
			DurationMS: r.DurationMS, Output: r.Output, Truncated: r.Truncated,
			Verdict: r.Verdict, Builtin: r.Builtin, Reason: r.Reason,
			CreatedAtMS: nowMS,
		}); err != nil {
			return err
		}

		// The trust decision is recorded as an AUDIT FACT (§4.5), never an
		// authorization shortcut: every action consults the live store on every
		// execution, for the revocation reason that rule gives.
		if r.ArgvSHA256 != "" {
			if err := db.InsertTrustCacheTx(tx, step.RunID, db.TrustKindAction,
				r.Action, r.ArgvSHA256, r.TrustEntry,
				r.Verdict != db.ActionVerdictUnmatched, r.Prefix,
				nowMS+int64(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordActionResult writes one action's per-attempt records and, unless the
// action failed, its result artifact — the shared tail of the TWO places
// runRoutingStage records an action's outcome (the holding-and-live branch,
// which always reaches this with `!action.Failed` already true by `holding`'s
// own definition, and the ordinary routing branch, where the action may have
// failed).
//
// Every attempt is recorded whatever the outcome, so an `unmatched` action is
// visible rather than inferable from a routing reason (§6.3). NO ARTIFACT ON
// FAILURE (§6.2): a computation that did not run has not produced a result to
// record, and writing an empty one would make a failed action
// indistinguishable downstream from a successful empty one. S2: the payload
// recorded is the PLAIN ARRAY — the S3/S4 `{"stub":true,…}` wrapper is
// history, never written again, and never rewritten where it already exists,
// which is why the marker rides in `artifacts.stub` (0 on a real result)
// instead.
//
// Called inside the caller's own transaction — H4's "one transaction, or a
// crash leaves a held payload with nobody able to resolve it" holds for both
// callers, holding or not.
func recordActionResult(tx *sql.Tx, step *db.Step, action *ActionResult, nowMS int64) error {
	if err := recordActionRows(tx, step, action.Results, nowMS); err != nil {
		return err
	}
	if action.Failed {
		return nil
	}
	_, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: step.RunID, StepID: step.ID, Kind: action.Kind,
		Body: action.Body, Payload: action.Payload,
		SHA256: artifactSHA256([]byte(action.Body), []byte(action.Payload)),
	}, nowMS)
	return err
}

// snapshotScope reads an issue's SNAPSHOTTED scope — the scope the diff is
// computed over (§6.7.1 D1), frozen at activation (§5.1.1). Reading the live
// scope here would make the recorded diff depend on a mid-run edit.
func snapshotScope(conn *sql.DB, runID, issueID int) ([]string, error) {
	var snapshot sql.NullString
	err := conn.QueryRow(
		`SELECT issue_snapshot FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID,
	).Scan(&snapshot)
	if err != nil {
		return nil, fmt.Errorf("reading the issue snapshot: %w", err)
	}
	if snapshot.String == "" {
		return nil, nil
	}
	var frozen struct {
		Scope []string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(snapshot.String), &frozen); err != nil {
		return nil, fmt.Errorf("reading the snapshotted scope: %w", err)
	}
	return frozen.Scope, nil
}

// subjectFromSnapshot reads the `when`-predicate subject — kind and labels —
// from the issue snapshot activation froze (§5.1.1).
//
// It is the loop's counterpart to snapshotScope, and it reads the snapshot for
// the same reason: a re-instantiation at ordinal k must evaluate `when` against
// the facts activation froze, not against the live issue. Reading live here
// would let a mid-run label edit change ordinal 1's topology while ordinal 0's
// stayed as expanded — two ordinals of one workflow disagreeing about which
// steps exist, and §9 item 5's determinism broken for exactly the runs that
// loop.
func subjectFromSnapshot(snapshot string) (workflow.Subject, error) {
	if snapshot == "" {
		return workflow.Subject{}, nil
	}
	var frozen struct {
		Kind   string   `json:"kind"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(snapshot), &frozen); err != nil {
		return workflow.Subject{}, fmt.Errorf("reading the issue snapshot: %w", err)
	}
	return workflow.Subject{Kind: frozen.Kind, Labels: frozen.Labels}, nil
}

// GitDiff computes a diff over the given path globs. THIS IS THE ONE DECLARED
// VCS COUPLING (engine-spec §7), and it is confined to this function.
//
// It runs OUTSIDE any transaction, always (§6.7.1 D1). A failure is not fatal to
// the saga: a repository that is not a git checkout, or a scope matching
// nothing, yields an empty diff rather than wedging a run. The engine's job is
// to record what the tree says, and "nothing" is a truthful answer.
//
// The D5 hazard engine-spine.md §6.7.1 recorded — "a process recording a step
// on behalf of work done somewhere else captures ITS OWN tree, silently" — is
// closed by the dir argument: the saga passes the step's recorded worktree
// (`--worktree` at complete/record), falling back to the invoking checkout's
// root. An empty dir keeps the old cwd behavior for callers that have neither.
//
// base is the commit-ish `dir`'s tree is compared against, empty meaning
// `HEAD` (dir's own tip). DKT-11: a `--worktree`-recorded step commits its
// change IN THAT WORKTREE, so `dir`'s working tree and `dir`'s own HEAD are
// identical by the time this runs — `git -C dir diff HEAD` then reports
// nothing, even though the worktree's HEAD carries a real commit the shared
// checkout does not have. Passing the shared checkout's HEAD as base makes
// the diff "what did this worktree add", not "is this worktree's tree dirty".
// Worktrees share one object database, so `base` resolves from `dir` even
// though it names a commit reachable only via another checkout's ref.
func GitDiff(dir, base string, scope []string) (string, error) {
	if base == "" {
		base = "HEAD"
	}
	out, err := rawDiff(dir, base, scope)
	if err != nil {
		// Not a checkout, or git absent. An empty diff is the truthful record.
		return "", nil
	}

	// UNTRACKED FILES ARE PART OF THE CHANGE (E-9). `git diff HEAD` cannot see
	// them by construction, so a NEW file was invisible to every consumer of
	// `issue.diff` — and a new file is exactly the class of change a review most
	// needs to see. RUN-3 recorded a review whose entire subject was a new test
	// file and the adaptation it covered, neither of which appeared in the diff
	// the reviewer was handed.
	body := out + untrackedDiff(dir, scope)

	// A SCOPED DIFF DISCLOSES WHAT IT OMITS (DKT-43 / DKT-44), AND SHOWS IT
	// (DKT-86). The pathspec above is the attribution mechanism — in a shared
	// tree it is what keeps a sibling issue's work out of this issue's
	// record — but it also silently swallowed real work: a fix whose commit
	// touched four files rendered a one-file diff because the issue's
	// declared scope named only one (ARTIFACT-114). DKT-43/44 made the
	// truncation named (a names-only trailer); DKT-86 found that still hid a
	// file the issue's own REQUEST mandated editing from every judge — an
	// issue's declared scope and its request text can disagree, and when
	// they do, the filter silently won. So the excluded hunks now FOLLOW the
	// in-scope diff under a clearly marked heading, capped: the scope filter
	// still decides what leads and what the diff claims as the issue's own,
	// but a change the review needs to see can no longer hide behind a
	// narrow scope. The heading says what the section is, because with a
	// shared-checkout cumulative base these hunks can also carry sibling
	// issues' work.
	if outside := outOfScopeNames(dir, base, scope); len(outside) > 0 {
		var b strings.Builder
		b.WriteString(body)
		fmt.Fprintf(&b, "# issue.diff: %d changed file(s) fall outside this "+
			"issue's declared scope and are excluded from the diff above:\n", len(outside))
		for _, path := range outside {
			fmt.Fprintf(&b, "#   %s\n", path)
		}
		b.WriteString(
			"# === outside declared scope: their hunks follow (DKT-86) ===\n" +
				"# A file the change touched must not hide behind a narrow scope;\n" +
				"# with a shared-checkout base these can also carry other issues'\n" +
				"# work since the run began — read them as evidence, not as this\n" +
				"# issue's own claim.\n")
		b.WriteString(outOfScopeDiff(dir, base, outside))
		body = b.String()
	}
	return body, nil
}

// outOfScopeDiffCap bounds the out-of-scope section's bytes. The packet's
// budget belongs to the in-scope diff; a shared-checkout cumulative base can
// drag arbitrary sibling churn here, and a capped section that says so beats
// an artifact nobody can load.
const outOfScopeDiffCap = 128 << 10

// outOfScopeDiff renders the hunks of the named out-of-scope paths (DKT-86):
// tracked changes through one pathspec'd diff against the same base the main
// body used (rawDiff — the same implementation), untracked files through the
// same --no-index device untrackedDiff uses. Failures are swallowed for the
// trailer's reason — this is best-effort enrichment of a record, never a
// reason to wedge a run.
func outOfScopeDiff(dir, base string, outside []string) string {
	out, err := rawDiff(dir, base, outside)
	if err != nil {
		out = ""
	}
	body := out + untrackedDiff(dir, outside)
	if len(body) > outOfScopeDiffCap {
		body = body[:outOfScopeDiffCap] +
			"\n# === out-of-scope hunks truncated at the cap; the file list above is complete ===\n"
	}
	return body
}

// rawDiff runs one pathspec'd `git diff` against base — the tracked half both
// GitDiff and outOfScopeDiff render, kept as ONE implementation so a change
// to how a diff body is produced reaches both. An empty pathspec diffs the
// whole tree: an issue that declared nothing has not narrowed what it may
// touch, so neither does the diff. Each caller keeps its own error policy.
func rawDiff(dir, base string, paths []string) (string, error) {
	args := gitDirArgs(dir, "diff", base, "--")
	if len(paths) == 0 {
		args = args[:len(args)-1]
	} else {
		args = append(args, paths...)
	}
	out, err := exec.Command("git", args...).Output()
	return string(out), err
}

// runExecRoot resolves the checkout `sharedCheckoutHead` should read HEAD
// from — see GitDiff's doc for the DKT-11 rationale this exists to close one
// level deeper: the RUN's own exec root, captured once at `run start` (G8,
// db.RunContext), never `resolvePaths().ExecRoot` (this invocation's own
// cwd, which under `step record --worktree` is the worktree itself — the
// same tree `dir` names). A run whose exec root was never recorded (started
// before this field existed, or outside a checkout) falls back to the
// invoking process's own exec root, the pre-fix behavior.
func runExecRoot(conn *sql.DB, runID int) string {
	run, err := db.GetRun(conn, runID)
	if err == nil && run.ExecRoot != "" {
		return run.ExecRoot
	}
	return resolvePaths().ExecRoot
}

// runDiffBase resolves the commit GitDiff's base should compare against for
// step's run: the run's own commit_sha, PINNED ONCE at `docket run start`
// (G8, db.RunContext, populated via config.GitHead at internal/cli/run_start.go)
// — never a live HEAD read taken at record time.
//
// DKT-20: a live read drifts with the checkout it reads. Late in a long run
// it either goes empty (the shared checkout fast-forwarded onto this very
// step's own commit, so base once again equals dir's own HEAD) or renders a
// SIBLING issue's unrelated commits as this step's own deletions (the
// checkout advanced past the fork point on another issue's work in the
// meantime). The commit recorded at run start predates every step's work by
// construction and never moves, so it stays a valid comparison point for the
// run's whole lifetime — closing both failure modes rather than only the
// second.
//
// DKT-42 narrows the pin's reach to the tree it is actually valid for. A
// WORKTREE is created at the shared checkout's CURRENT head, not at the
// run's pinned commit, so everything sibling issues landed between run start
// and the worktree's creation is already in `dir`'s own history — and a
// pinned base attributes all of it to this issue (measured on RUN-2: 32
// files, 1539 insertions of siblings' work inside one issue's diff, with an
// empty scope the only thing between it and the packet). When `dir` is a
// distinct tree from the run's exec root, the base is therefore the
// worktree's FORK POINT — `git merge-base` of the worktree's HEAD and the
// shared checkout's — which predates this step's own commits by
// construction, never moves for the worktree's lifetime, and excludes
// inherited commits. A rebase onto newer shared work moves the fork point
// forward with the inherited history, which is exactly right. The pinned
// commit remains the base for shared-checkout recordings, where there is no
// fork point and the run's cumulative work is the correct record.
//
// A run started before commit_sha was recorded, or outside a checkout, falls
// back to sharedCheckoutHead's live read of the run's exec root — the prior
// fix's behavior — preserved for that case only.
//
// `execRoot` is the run's already-resolved exec root (runExecRoot): the
// caller resolved it to default `dir`, and passing it in keeps this compare
// and that defaulting reading ONE value rather than two resolutions that
// could disagree.
func runDiffBase(conn *sql.DB, runID int, dir, execRoot string) string {
	if dir != "" && dir != execRoot {
		if fork := worktreeForkPoint(dir, execRoot); fork != "" {
			return fork
		}
	}
	run, err := db.GetRun(conn, runID)
	if err == nil && run.CommitSHA != "" {
		return run.CommitSHA
	}
	return sharedCheckoutHead(execRoot)
}

// gateBaseSHA resolves the base commit a completion gate's child is told
// about via DOCKET_GATE_BASE (DKT-992): for a worktree-recorded step, the
// worktree's fork point — the commit the worktree was created from, the SAME
// resolution the diff stage's runDiffBase applies to `dir` — so a gate can
// scan exactly the committed range the recorded issue.diff describes, instead
// of guessing (`git diff HEAD~1`, wrong for multi-commit steps) or scanning
// the working tree an executor already committed to (always clean, so
// RUN-66's secret-scan passed 8/8 write steps having scanned zero lines).
//
// "" — the var UNSET — everywhere a step's committed range is not knowable,
// and deliberately NOT runDiffBase's pinned-commit fallback:
//
//   - a shared-checkout step ("" or workRoot == the run's exec root) has no
//     fork point, and the pinned run commit is not this STEP's base — sibling
//     issues' work lands between it and the step's own commits, so exporting
//     it would attribute their range to this step. The acceptance choice here
//     is UNSET for non-worktree steps, not "equal to HEAD": a live HEAD read
//     is a value docket cannot vouch for as a range endpoint, and absence is
//     the honest encoding (the same convention as DOCKET_SCOPE).
//   - a worktree whose fork point cannot be resolved exports nothing rather
//     than a guess; a range-shaped gate finding the var absent over a clean
//     tree fails closed, which is the correct direction for a control.
func gateBaseSHA(conn *sql.DB, runID int, workRoot string) string {
	if workRoot == "" {
		return ""
	}
	execRoot := runExecRoot(conn, runID)
	if workRoot == execRoot {
		return ""
	}
	return worktreeForkPoint(workRoot, execRoot)
}

// worktreeForkPoint resolves the merge-base of a worktree's HEAD and the
// shared checkout's — DKT-42's base for worktree-recorded steps. "" when
// either side cannot be resolved, which sends runDiffBase to its pinned
// fallback; worktrees share one object database, so the shared head resolves
// from `dir`.
func worktreeForkPoint(dir, execRoot string) string {
	sharedHead := sharedCheckoutHead(execRoot)
	if sharedHead == "" {
		return ""
	}
	out, err := exec.Command("git",
		gitDirArgs(dir, "merge-base", "HEAD", sharedHead)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// roundDeltaBase resolves the commit a loop re-entry's ROUND DELTA diffs from
// (DKT-171, amended by DKT-409).
//
// The previous round's recorded head is the default. But when this round ran
// in a worktree distinct from the exec root, that worktree was created at the
// shared checkout's head — AFTER the conductor integrated the previous round
// and, possibly, sibling issues' work. prev..HEAD then carries every one of
// those inherited commits as if they were this round's own change (measured on
// RUN-14/HRN-25: five sibling files across 31 diff headers in one issue's
// round delta). The fork point excludes them by construction, so it wins
// whenever it DESCENDS from prev — the commits between the two are inherited
// shared-branch history, never this round's work.
//
// The fork point ALSO wins when the two histories have DIVERGED — prev is
// neither behind the fork point nor ahead of it (DKT-409). That is what a
// cherry-pick integration looks like: the conductor landed the previous round
// as NEW commits, so the recorded prev sits on a line the shared branch no
// longer carries, and prev..HEAD renders sibling issues' integrated work and
// the integration's own reshaping as if this round wrote them (RUN-35: three
// rounds of review packets carrying sibling-issue diff, judged out of scope).
// DKT-171 kept prev here as the conservative answer; RUN-35 measured what
// that conserves. The fork point is the base actually shared with the current
// branch state — an ancestor of HEAD by construction — and fork..HEAD is this
// round's own commits alone, which is the scoping the round delta promises.
//
// prev survives in exactly two shapes. A worktree that persisted across
// rounds has its fork point BEHIND prev — advancing to it would re-attribute
// the previous round's own work to this round — and that holds whether or not
// the integration diverged, because the round's own commits still stack on
// prev in place. And the shared checkout has no fork point at all.
func roundDeltaBase(dir, execRoot, prev string) string {
	if dir == "" || dir == execRoot {
		return prev
	}
	fork := worktreeForkPoint(dir, execRoot)
	if fork == "" || fork == prev {
		return prev
	}
	if isAncestor(dir, prev, fork) {
		// Verbatim integration: prev..fork is inherited shared-branch history.
		return fork
	}
	if isAncestor(dir, fork, prev) {
		// Persisted worktree: the fork point predates the previous round's
		// own work.
		return prev
	}
	// Diverged: integration minted new commits, and prev is no longer part of
	// the branch's history. The fork point is the shared base (DKT-409).
	return fork
}

// isAncestor reports whether `anc` is an ancestor of `desc` in dir's history.
// Any failure — unresolvable commit, not a repository — reads as "no", which
// sends every caller to its conservative default.
func isAncestor(dir, anc, desc string) bool {
	return exec.Command("git",
		gitDirArgs(dir, "merge-base", "--is-ancestor", anc, desc)...).Run() == nil
}

// gitAncestorOfHead is IsAncestorFn's real implementation (DKT-193): git's
// three-valued answer preserved as two booleans. Exit 0 is "an ancestor"
// (equal counts), exit 1 is "definitively not", and anything else — git
// absent, not a repository, a GC'd or never-fetched object — is "the question
// could not be answered", which no caller may treat as staleness. Fail-open
// here matches sharedCheckoutHead's "" convention: these reads enrich records,
// they never wedge a verb.
func gitAncestorOfHead(execRoot, sha string) (ancestor, known bool) {
	if execRoot == "" || sha == "" {
		return false, false
	}
	err := exec.Command("git",
		gitDirArgs(execRoot, "merge-base", "--is-ancestor", sha, "HEAD")...).Run()
	if err == nil {
		return true, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// gitCommitResolvable is ObjectExistsFn's real implementation (DKT-742): does
// this sha resolve as a commit object from execRoot at all? It is exactly the
// probe a packet consumer runs by hand — `git cat-file -e <sha>^{commit}` —
// asked once at dispatch time instead of once per seat mid-wave.
//
// The three-valued mapping needs TWO probes, because cat-file's peel form
// exits 128 for both "no such object" and "not a repository" — one a
// definitive absence worth warning on, the other an unanswerable question
// that must stay silent:
//
//   - `cat-file -e <sha>^{commit}` exit 0: the object exists and peels to a
//     commit — (true, true).
//   - otherwise `cat-file -e <sha>` (no peel) exit 1: git ran, looked, and
//     found NO OBJECT — a definitive (false, true). Exit 0 here means the
//     object exists but is not a commit, which is equally definitive: a
//     recorded target sha naming a blob or tree does not resolve for any
//     consumer either.
//   - anything else — git absent, not a repository — (false, false), which no
//     caller may treat as absence.
func gitCommitResolvable(execRoot, sha string) (exists, known bool) {
	if execRoot == "" || sha == "" {
		return false, false
	}
	if exec.Command("git",
		gitDirArgs(execRoot, "cat-file", "-e", sha+"^{commit}")...).Run() == nil {
		return true, true
	}
	err := exec.Command("git",
		gitDirArgs(execRoot, "cat-file", "-e", sha)...).Run()
	if err == nil {
		// Present but not peelable to a commit: definitively unresolvable as
		// the commit the packet records.
		return false, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// gitPatchContainedInHead is PatchContainedFn's real implementation (DKT-1033):
// does the shared checkout's HEAD carry the PATCH this target recorded — is
// every commit on the target's side of their merge base patch-id equivalent to
// some commit on HEAD's side?
//
// THE DEFECT IT CLOSES. DKT-424's tree comparison asked whether HEAD's TREE
// still equalled the target's on the paths the work touched. That is the wrong
// question about a moving branch tip: a sibling issue integrated into the same
// file, or a conductor's follow-up patch to the same test, changes HEAD's tree
// on those paths while leaving this work's every hunk intact — and RUN-67
// warned "integration diverged" on all nine review rows of a run whose three
// integrations were plain `git cherry-pick -x`, two of them into
// non-overlapping regions of one Makefile. Tree equality is a statement about
// the tip; the question worth asking is a statement about the work, which is
// what patch-id equivalence measures.
//
// TWO PROBES, THE SECOND REFINING THE FIRST.
//
//   - `git cherry HEAD <sha>` (probe one) marks every commit in HEAD..<sha>
//     with `-` when <sha>..HEAD carries a commit of the same patch-id, `+`
//     otherwise. All `-` is the common case and answers in one subprocess.
//     Its patch-id hashes three lines of context around each hunk, so a
//     clean cherry-pick lands as `+` whenever a sibling's change sits within
//     those three lines — the same false positive one commit closer, which
//     is why a `+` is not yet a verdict.
//   - Zero-context patch-ids (probe two): `git log -p -U0 | git patch-id
//     --stable` over each side, compared as sets. Hunk headers carry no
//     hashed line numbers and -U0 emits no context, so the identity is the
//     removed and added lines alone, per path — a neighbour's edit cannot
//     perturb it, and a hunk edited during conflict resolution cannot match
//     it. A squashed integration is covered by the target line's combined
//     diff as one more candidate.
//
// WHY NOT THE `(cherry picked from commit <sha>)` TRAILER. `-x` writes it on a
// conflicted pick exactly as on a clean one, so its presence proves the pick
// happened and nothing about what the resolution kept; the case this must
// still catch — a hunk edited during conflict resolution — carries the
// trailer. Patch-id equivalence answers both and needs no cooperation from
// the integrator's flags.
//
// Three-valued like the other probes. `known = false` — git absent, an
// unresolvable object, nothing off HEAD to test, or a target whose commits
// carry no patch content at all (an empty commit, DKT-451's shape) — hands the
// question to TreeMatchFn. `contained = false` with `known = true` is the one
// verdict in this family that measured the work itself, and the only one the
// advisory may word as a divergence.
func gitPatchContainedInHead(execRoot, sha string) (contained, known bool) {
	if execRoot == "" || sha == "" {
		return false, false
	}
	out, err := exec.Command("git",
		gitDirArgs(execRoot, "cherry", "HEAD", sha)...).Output()
	if err != nil {
		return false, false
	}
	tested, upstream := false, true
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			tested = true
		case strings.HasPrefix(line, "+ "):
			tested = true
			upstream = false
		}
	}
	if !tested {
		// Nothing in HEAD..sha to test: the target is on HEAD's history after
		// all (ancestry answers that, not this), or its line is merges only.
		return false, false
	}
	if upstream {
		return true, true
	}

	want, ok := gitZeroContextPatchIDs(execRoot, "HEAD.."+sha)
	if !ok || len(want) == 0 {
		return false, false // no patch content on the target's side to match
	}
	have, ok := gitZeroContextPatchIDs(execRoot, sha+"..HEAD")
	if !ok {
		return false, false
	}
	carried := make(map[string]bool, len(have))
	for _, id := range have {
		carried[id] = true
	}
	missing := false
	for _, id := range want {
		if !carried[id] {
			missing = true
			break
		}
	}
	if !missing {
		return true, true
	}
	// A squashed integration: the target line's whole diff, landed as one
	// commit, matches no single commit of the line but does match the range.
	if squash, ok := gitZeroContextRangePatchID(execRoot, sha); ok && carried[squash] {
		return true, true
	}
	return false, true
}

// gitZeroContextPatchIDs lists the zero-context patch-id of every non-merge
// commit in a range, as `git patch-id --stable` prints them: one id per commit
// that carries patch content, none for a commit that does not.
//
// The text is an identity, so everything that could make it read differently
// on the two sides is pinned: renames off (gitPathsTouchedSince's reason),
// colour off, inter-hunk fusing off (a config that fuses close hunks would
// pull a neighbour's lines back in as context), and --format keeps `commit
// <sha>` as each entry's first line — the boundary patch-id splits on —
// without the message body a default format prints between it and the diff.
func gitZeroContextPatchIDs(execRoot, rangeSpec string) (ids []string, ok bool) {
	out, err := exec.Command("git", gitDirArgs(execRoot,
		"log", "--no-color", "--format=commit %H", "-p", "-U0",
		"--inter-hunk-context=0", "--no-merges", "--no-renames", rangeSpec)...).Output()
	if err != nil {
		return nil, false
	}
	return gitPatchIDsOf(execRoot, out)
}

// gitZeroContextRangePatchID is the patch-id of the target line's combined
// diff — `git diff HEAD...<sha>`, everything since the merge base as one
// patch — which is what a squashed integration commits.
func gitZeroContextRangePatchID(execRoot, sha string) (id string, ok bool) {
	out, err := exec.Command("git", gitDirArgs(execRoot,
		"diff", "--no-color", "--no-ext-diff", "--no-renames", "-U0",
		"--inter-hunk-context=0", "HEAD..."+sha)...).Output()
	if err != nil {
		return "", false
	}
	ids, ok := gitPatchIDsOf(execRoot, out)
	if !ok || len(ids) != 1 {
		return "", false
	}
	return ids[0], true
}

// gitPatchIDsOf feeds patch text through `git patch-id --stable` and returns
// the ids it prints, in order. --stable makes an id independent of the order
// files appear in, so two renderings of one change cannot disagree on it.
func gitPatchIDsOf(execRoot string, patch []byte) (ids []string, ok bool) {
	cmd := exec.Command("git", gitDirArgs(execRoot, "patch-id", "--stable")...)
	cmd.Stdin = bytes.NewReader(patch)
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if id, _, _ := strings.Cut(line, " "); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, true
}

// treeMatchBatch caps how many pathspecs ride on one `git diff` argv, so a
// commit touching thousands of files cannot overflow the platform's argument
// limit. The comparison is split across batches and every batch must come back
// quiet; a chunked answer is the same answer.
const treeMatchBatch = 256

// gitTreeMatchesHead is TreeMatchFn's real implementation (DKT-424): does the
// shared checkout's HEAD still carry the tree this target recorded, on the
// paths the target's own work touched?
//
// SINCE DKT-1033 IT IS THE FALLBACK, NOT THE VERDICT. A tree comparison on a
// moving branch tip reads any later commit on the same paths — a sibling
// issue's integration, a conductor's follow-up patch — as a difference, so its
// `match = false` says nothing about this work. staleTargets asks
// gitPatchContainedInHead first and words this probe's negative as content
// that could not be matched, never as a divergence; its `match = true` still
// acquits, because a tree identical on the work's own paths carries the work.
//
// THE DEFECT IT CLOSES. The sanctioned integration flow — an executor commits
// in an isolated worktree, a conductor `git cherry-pick`s that commit onto the
// shared branch — MINTS A NEW SHA for identical content. So the recorded
// target can never again be an ancestor of the shared HEAD, and DKT-193's
// ancestry test fired on every judge row of every worktree-isolated run. A
// warning that is always false on the correct flow trains its reader to wave
// it through on the day it is true, which is the whole cost.
//
// WHY THE COMPARISON IS PATH-SCOPED. The shared HEAD is a moving branch tip
// that carries OTHER issues' integrated work; an unscoped `git diff <sha>
// HEAD` therefore goes non-empty the moment any unrelated file changes
// anywhere, which would reintroduce the same false positive one commit later.
// The question worth asking is narrower and stable: on the paths THIS work
// touched, is the branch's content still the content the packet renders?
//
// THE PATH SET IS TAKEN FROM THE MERGE BASE, not from the target commit's own
// parent, so a worktree that recorded several commits is covered whole — the
// union of everything the target added since it left the shared line.
//
// THE SCOPED QUESTION IS ASKED FIRST, AND WHOLE-TREE EQUALITY ANSWERS THE CASES
// IT CANNOT (DKT-451). Two shapes leave the scoped comparison with no evidence
// to acquit on: no merge base at all (unrelated histories), and a target that
// touched no path relative to the base it does have. RUN-37 sat next to that
// seam — its recorded target and the shared HEAD carried the SAME root tree
// object, f36e7b157a5d, and the row warned anyway while the conductor spent
// ~50s running `git rev-parse <sha>^{tree}` on both sides by hand. Two commits
// whose root trees are one object carry identical content everywhere, which is
// a stronger acquittal than the scoped one rather than a weaker one, so it is
// the fallback whenever the scoped half cannot answer.
//
// Three-valued like gitAncestorOfHead, and read by staleTargets in the
// acquittal direction only: `known = false` — git absent, a GC'd object, and
// the two unanswerable shapes above where the root trees ALSO differ — leaves
// the ancestry verdict standing.
func gitTreeMatchesHead(execRoot, sha string) (match, known bool) {
	if execRoot == "" || sha == "" {
		return false, false
	}
	if match, known := gitTouchedPathsMatchHead(execRoot, sha); known {
		return match, known
	}
	return gitWholeTreeMatchesHead(execRoot, sha)
}

// gitTouchedPathsMatchHead is the scoped half of TreeMatchFn: does HEAD still
// carry this target's content on the paths the target's own work touched?
func gitTouchedPathsMatchHead(execRoot, sha string) (match, known bool) {
	base, ok := gitMergeBaseWithHead(execRoot, sha)
	if !ok {
		return false, false
	}
	paths, ok := gitPathsTouchedSince(execRoot, base, sha)
	if !ok || len(paths) == 0 {
		return false, false
	}
	for start := 0; start < len(paths); start += treeMatchBatch {
		end := min(start+treeMatchBatch, len(paths))
		// --literal-pathspecs: a real file may begin with a character git
		// would otherwise read as pathspec magic, and these paths came from
		// git itself — they are names, not patterns.
		args := gitDirArgs(execRoot, "--literal-pathspecs",
			"diff", "--quiet", sha, "HEAD", "--")
		args = append(args, paths[start:end]...)
		err := exec.Command("git", args...).Run()
		if err == nil {
			continue // this batch is identical on both sides
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, true // a real difference on the work's own paths
		}
		return false, false
	}
	return true, true
}

// gitWholeTreeMatchesHead answers the scoped comparison's unanswerable cases
// (DKT-451) by the bluntest evidence git has: are the two commits' root tree
// objects the same object?
//
// EQUALITY IS THE ONLY THING IT REPORTS. Two different trees say nothing about
// whether THIS work's paths diverged — a shared branch carries other issues'
// integrated commits, and reading their presence as this row's divergence is
// the very false positive DKT-424's path scoping exists to avoid. So a
// difference here is `known = false`, "the tree question went unanswered", and
// the ancestry warning stands with the wording that says so.
func gitWholeTreeMatchesHead(execRoot, sha string) (match, known bool) {
	target, ok := gitTreeHash(execRoot, sha)
	if !ok {
		return false, false
	}
	head, ok := gitTreeHash(execRoot, "HEAD")
	if !ok || head != target {
		return false, false
	}
	return true, true
}

// gitTreeHash resolves a commit-ish to its root tree object. An unresolvable
// revision — git absent, not a repository, a GC'd or never-fetched object, a
// repository with no commits — reports "not answered" rather than "".
func gitTreeHash(execRoot, rev string) (tree string, ok bool) {
	out, err := exec.Command("git", gitDirArgs(execRoot,
		"rev-parse", "--verify", "--quiet", rev+"^{tree}")...).Output()
	if err != nil {
		return "", false
	}
	tree = strings.TrimSpace(string(out))
	return tree, tree != ""
}

// gitMergeBaseWithHead resolves the commit the target and the shared HEAD last
// shared. Unrelated histories and unresolvable objects report "not answered",
// which is TreeMatchFn's silent case.
func gitMergeBaseWithHead(execRoot, sha string) (base string, ok bool) {
	out, err := exec.Command("git",
		gitDirArgs(execRoot, "merge-base", sha, "HEAD")...).Output()
	if err != nil {
		return "", false
	}
	base = strings.TrimSpace(string(out))
	return base, base != ""
}

// gitPathsTouchedSince lists every path whose content differs between two
// commits, NUL-delimited so a path containing a newline survives the read.
//
// Rename detection stays OFF: a rename read as one path would hide half the
// pair from the comparison, and the delete/add reading names both sides —
// which is exactly the pair the tree check has to look at.
func gitPathsTouchedSince(execRoot, base, sha string) (paths []string, ok bool) {
	out, err := exec.Command("git", gitDirArgs(execRoot,
		"--literal-pathspecs", "diff", "--no-renames", "--name-only", "-z",
		base, sha)...).Output()
	if err != nil {
		return nil, false
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}

// sharedCheckoutHead resolves a checkout's own HEAD commit, read directly
// from execRoot rather than trusted from any caller-supplied string — the
// fallback runDiffBase uses when no pinned commit_sha is available. Reuses
// config.GitHead (internal/config/gitctx.go), which internal/engine already
// imports (repopaths.go) and which docs/spec/architecture.md already
// sanctions as a subprocess site, rather than re-running `git rev-parse`
// here. A checkout that cannot be resolved (empty execRoot, not a git
// repository) yields "", and GitDiff treats that exactly like no base was
// named — falling back to `HEAD`, i.e. dir's own tip, which is the pre-fix
// behavior and still correct whenever dir IS the checkout being diffed.
func sharedCheckoutHead(execRoot string) string {
	if execRoot == "" {
		return ""
	}
	_, commit := config.GitHead(execRoot)
	return commit
}

// appendRoundDelta is DKT-106's round record, in two halves.
//
// HALF ONE, every tree-holding completion: the checkout's HEAD commit is
// recorded in the artifact's payload as `{"head": "..."}`, naming the exact
// commit the diff's tree stood at — the reference judges previously
// reconstructed by reading commit logs ("delta reconstructed as commit
// c0a157e, since issue.diff spanned the whole issue range").
//
// HALF TWO, at loop re-entry (ordinal > 0): the previous round's recorded
// head becomes the base of a SECOND diff, appended to the cumulative body
// under a marked trailer. The cumulative issue-range diff stays first and
// byte-identical in shape; what re-review judges lacked — the round's own
// change — follows it. The delta is computed UNSCOPED, because the round
// asks "what did the fix touch" and the scope filter blinded exactly the
// judge-testing lens the re-review runs (an issue whose scope excluded test
// files rendered a diff with every test file omitted).
//
// The payload it returns is "" whenever HEAD cannot be resolved, and every
// failure inside is silent-by-design for GitDiff's own reason: the record is
// evidence, and "nothing" is a truthful answer where a tree has no commit.
func (e *Engine) appendRoundDelta(conn *sql.DB, step *db.Step, dir, execRoot string, diffBody *string) string {
	record := map[string]string{}

	// The DECLARED worktree rides in the payload beside the head (DKT-24):
	// while the path is still on disk — it is swept at integration — a
	// consumer reads the target tree in place instead of reconstructing it
	// from the diff. The shared checkout is deliberately not recorded: its
	// tree keeps moving under a mid-wave reader, which is the confusion a
	// structured ref exists to end.
	if step.WorkRoot != "" {
		record["worktree"] = step.WorkRoot
	}

	var head string
	if e.HeadFn != nil {
		head = e.HeadFn(dir)
	}
	if head != "" {
		record["head"] = head
		if step.Ordinal > 0 {
			prev := latestIssueDiffHead(conn, step.RunID, step.IssueID)
			if prev != "" && prev != head {
				// DKT-171/DKT-409: `prev` predates whatever integration landed
				// on the shared branch between rounds. A fresh worktree forked
				// AFTER that integration inherits it in prev..HEAD — sibling
				// issues' cherry-picked commits rendered as this issue's own
				// round work. The base therefore advances to the worktree's
				// fork point whenever prev is not strictly ahead of it —
				// verbatim integration puts prev behind the fork, a cherry-
				// pick integration leaves it on a superseded line beside it —
				// so fork..HEAD is exactly "this round's work alone".
				base := roundDeltaBase(dir, execRoot, prev)
				delta, err := e.DiffFn(dir, base, nil)
				if err == nil {
					record["round_base"] = base
					if delta == "" {
						delta = "# (no tree change this round)\n"
					}
					*diffBody += fmt.Sprintf(
						"\n# === round delta: changes since %.12s — this round's work alone, unscoped ===\n",
						base) + delta
				}
			}
		}
	}

	if len(record) == 0 {
		return ""
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// latestIssueDiffHead reads the `head` the issue's newest issue.diff artifact
// recorded, "" when none did — a run started before the round record existed,
// or a tree whose HEAD could not be resolved at the time.
func latestIssueDiffHead(conn *sql.DB, runID, issueID int) string {
	var payload sql.NullString
	err := conn.QueryRow(
		`SELECT a.payload FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, ArtifactKindIssueDiff).Scan(&payload)
	if err != nil {
		return ""
	}
	return handBackHead(payload.String)
}

// priorRoundHandBack is the hand-back head THIS step's own name recorded at
// its newest earlier ordinal — the sha the same loop body handed back last
// round — or "" when it never recorded one (DKT-588).
//
// BELOW, not AT, the step's ordinal, for newestIssueDiffUpTo's exact reason:
// the engine suppresses a byte-identical or empty re-record (DKT-258/DKT-259),
// so a round may leave no artifact of its own, and the newest earlier record
// is still the last commit this body actually handed back. Excluding the
// step's OWN ordinal is what keeps a re-completion at the same ordinal — an
// operator `--as retry` — from comparing against its own first record.
//
// Filtered to the step's NAME, unlike latestIssueDiffHead's issue-wide read:
// the issue's newest head may belong to a different producer entirely, and
// this comparison is only meaningful between two hand-backs of the same body.
func priorRoundHandBack(conn *sql.DB, step *db.Step) string {
	var payload sql.NullString
	err := conn.QueryRow(
		`SELECT a.payload FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND s.step_name = ?
		    AND s.ordinal < ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 1`,
		step.RunID, step.IssueID, step.StepName, step.Ordinal,
		ArtifactKindIssueDiff).Scan(&payload)
	if err != nil {
		return ""
	}
	return handBackHead(payload.String)
}

// handBackHead decodes the `head` out of a round record payload —
// appendRoundDelta's `{"head": "..."}` — "" when the payload carries none or
// cannot be read. "" is the degenerate answer every consumer must treat as
// "no measurement", never as a comparable value.
func handBackHead(payload string) string {
	if payload == "" {
		return ""
	}
	var record struct {
		Head string `json:"head"`
	}
	if json.Unmarshal([]byte(payload), &record) != nil {
		return ""
	}
	return record.Head
}

// gitDirArgs prepends `-C dir` when a directory is named, so every git call in
// a diff resolves against the SAME tree. An empty dir keeps the process cwd.
func gitDirArgs(dir string, rest ...string) []string {
	if dir == "" {
		return rest
	}
	return append([]string{"-C", dir}, rest...)
}

// untrackedDiff renders each untracked file as an addition, appended after the
// tracked diff.
//
// IT NEVER TOUCHES THE INDEX. The obvious alternative — `git add
// --intent-to-add` before diffing — makes untracked files visible by STAGING
// them, which leaves the operator's staging area different from how it was
// found. Computing a diff is a read; a read that mutates is a defect rather
// than a side effect, and `TestGitDiffDoesNotMutateTheIndex` pins it.
//
// `--exclude-standard` is what makes this usable at all: without it every
// ignored build artifact in the tree would land in the review. The same
// pathspec the tracked half used is applied here, so scope means the same thing
// on both halves.
//
// Failures are swallowed exactly as the tracked half's are: this is a best-
// effort enrichment of a record, and a tree that cannot be listed yields the
// tracked diff alone rather than wedging a run.
func untrackedDiff(dir string, scope []string) string {
	args := gitDirArgs(dir, "ls-files", "--others", "--exclude-standard", "--")
	if len(scope) == 0 {
		args = args[:len(args)-1]
	} else {
		args = append(args, scope...)
	}

	listed, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}

	var b strings.Builder
	for _, path := range strings.Split(strings.TrimSpace(string(listed)), "\n") {
		if path == "" {
			continue
		}
		// `--no-index` diffs two paths without consulting or writing the index,
		// so /dev/null against the file yields the `new file` diff git would
		// have produced had it been staged. It exits 1 when the two differ,
		// which is the ordinary case here, so the output is used regardless of
		// the exit status and only a truly empty result is skipped.
		// `/dev/null` is git's own spelling for "the empty side" and is what it
		// writes into the diff header on every platform it supports, so the
		// literal is correct here rather than os.DevNull. The `-C` matters
		// here too: ls-files listed paths RELATIVE TO dir, so the diff must
		// resolve them from the same place.
		out, _ := exec.Command("git",
			gitDirArgs(dir, "diff", "--no-index", "--", "/dev/null", path)...).Output()
		b.Write(out)
	}
	return b.String()
}

// outOfScopeNames lists every changed path the declared scope keeps OUT of
// the rendered diff: tracked changes against base plus untracked files over
// the whole tree, minus the same two listings under the scope's own pathspec
// — the exact filters the two diff halves applied, so "omitted" here means
// omitted there. An empty scope filters nothing and discloses nothing.
//
// Failures are swallowed exactly as untrackedDiff's are: the trailer is a
// best-effort enrichment of a record, and a tree that cannot be listed yields
// the diff alone rather than wedging a run.
func outOfScopeNames(dir, base string, scope []string) []string {
	if len(scope) == 0 {
		return nil
	}

	list := func(args []string) map[string]bool {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			return nil
		}
		names := map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				names[line] = true
			}
		}
		return names
	}

	all := list(gitDirArgs(dir, "diff", "--name-only", base, "--"))
	for path := range list(gitDirArgs(dir, "ls-files", "--others", "--exclude-standard")) {
		all[path] = true
	}
	if len(all) == 0 {
		return nil
	}

	inScope := list(append(
		gitDirArgs(dir, "diff", "--name-only", base, "--"), scope...))
	for path := range list(append(
		gitDirArgs(dir, "ls-files", "--others", "--exclude-standard", "--"), scope...)) {
		inScope[path] = true
	}

	var outside []string
	for path := range all {
		if !inScope[path] {
			outside = append(outside, path)
		}
	}
	sort.Strings(outside)
	return outside
}

// gateCommands returns the harvested fence lines for a `fence:` gate, or nil
// for a trusted gate.
//
// The commands are read from `run_fences`, which activation harvested and
// hashed FROM THE SNAPSHOT (§5.3 stage 5) — so what runs is what the operator
// approved, and a post-activation body edit cannot inject (engine-spec §4).
// It returns the stored SHA-256 alongside each command so the runner can
// re-verify it before spawning (§7.3 step 3): S3's snapshot closes "the body
// cannot inject", and the hash check closes "the stored row cannot be swapped".
func gateCommands(
	conn *sql.DB, step *db.Step, gate workflow.Gate,
) (commands, hashes []string, err error) {
	tag, ok := strings.CutPrefix(gate.Source, "fence:")
	if !ok {
		return nil, nil, nil
	}

	rows, err := conn.Query(
		`SELECT command, sha256 FROM run_fences
		  WHERE run_id = ? AND issue_id = ? AND tag = ? ORDER BY ordinal`,
		step.RunID, step.IssueID, tag)
	if err != nil {
		return nil, nil, fmt.Errorf("reading harvested commands for gate %s: %w", gate.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var command, sum string
		if err := rows.Scan(&command, &sum); err != nil {
			return nil, nil, fmt.Errorf("reading a harvested command: %w", err)
		}
		commands = append(commands, command)
		hashes = append(hashes, sum)
	}
	return commands, hashes, rows.Err()
}

// diffRecordsNoChange reports whether a computed diff body carries no change
// at all (DKT-259).
//
// Comment lines are stripped first, and that is the load-bearing part: this
// file writes two kinds of `#`-prefixed annotation into a diff body — the
// unresolvable-base warning and the out-of-scope-files note — and a body
// consisting only of those is a body that measured nothing while being far from
// zero bytes. Testing `len(body) == 0` would let exactly the annotated cases
// through, which are the ones most likely to be empty for a bad reason.
func diffRecordsNoChange(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return false
	}
	return true
}

// latestIssueDiffBody is the issue's newest recorded `issue.diff` body, or ""
// when it has none.
//
// It returns "" on a read failure too, which is indistinguishable from "no diff
// yet" and is the right collapse: both mean "no reason to suppress", and a
// guard that suppresses writes must fail toward recording. An extra artifact is
// noise; a lost one is evidence.
func latestIssueDiffBody(conn *sql.DB, runID, issueID int) string {
	var body string
	err := conn.QueryRow(
		`SELECT a.body FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, ArtifactKindIssueDiff).Scan(&body)
	if err != nil {
		return ""
	}
	return body
}

// issueHasRecordedChange reports whether the issue already has an `issue.diff`
// artifact that carries content.
//
// It scans the issue's diffs newest-first and stops at the first one with
// content, so the common case — the previous record was a real diff — costs one
// row. The bound exists because a pathological issue could otherwise scan a
// long history of empty records; the number is generous rather than tuned,
// since the answer only changes behavior when it is false.
//
// A read failure answers FALSE, which permits the record. That is the right
// direction for a guard that suppresses writes: refusing to record because a
// query failed would lose a real diff over a transient error, while recording
// an extra empty one is the pre-DKT-259 behavior and merely noisy.
func issueHasRecordedChange(conn *sql.DB, runID, issueID int) bool {
	rows, err := conn.Query(
		`SELECT a.body FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 50`,
		runID, issueID, ArtifactKindIssueDiff)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return false
		}
		if !diffRecordsNoChange(body) {
			return true
		}
	}
	return false
}
