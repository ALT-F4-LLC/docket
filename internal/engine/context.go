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

// The context bundle — engine-spec §11.4's `context`, and the determinism
// engine-core §8 and §9 item 5 require of it.
//
// ASSEMBLY IS PURE AND SNAPSHOT-PINNED. It reads, and may read, exactly five
// sources (TDD §6.6):
//
//  1. the step row and its definition, from the PINNED workflow's `parsed`
//  2. `run_issues.body_snapshot`   — never the live issue body
//  3. `run_issues.issue_snapshot`  — title/kind/labels/scope as of activation
//  4. recorded `artifacts` bodies, resolved per §6.7's input rule
//  5. `pins` rows (path + hash) — the LIST, not the file contents
//
// It never reads the working tree, never re-reads a file a pin names, and reads
// NO LIVE ISSUE FIELD AT ALL: `id` is the run_issues key and every other member
// of §11.4's `context.issue` comes from a snapshot column.
//
// The scheduler's live read of `issues.scope_globs` (§6.3 R4) is NOT context
// assembly and is deliberately outside this file. Two different questions
// legitimately have two different answers (§5.1.1).
//
// TestContextAssemblyReadsNoLiveState enforces this at the code level; the
// golden bundles (§8.3) are the same property at the CLI level.

// Context is §11.4's `context`, field for field:
//
//	{ step: <next row>, issue: {id, title, body_snapshot, kind, labels, scope},
//	  inputs: [{artifact, kind, producer_step, body}], pins: [{path, sha256}],
//	  loop_entry, metadata }
type Context struct {
	Step   model.StepRow  `json:"step"`
	Issue  ContextIssue   `json:"issue"`
	Inputs []ContextInput `json:"inputs"`
	Pins   []ContextPin   `json:"pins"`
	// LoopEntry is the ordinal k and the routing that entered it, NULL at k=0
	// (§6.4). It is a pointer so the wire shape carries `null` rather than a
	// zero-valued object, which a consumer would have to know to ignore.
	LoopEntry *LoopEntry `json:"loop_entry"`
	// Metadata is the definition's opaque KV, verbatim. Core never reads a key
	// inside it (genericity.md).
	Metadata map[string]any `json:"metadata,omitempty"`
	// PreGates carries the results of the step's `pre = true` gates, in
	// declared order (gates-trust §7.6.3, amendment A5).
	//
	// PRESENT ONLY WHEN THE STEP DECLARES PRE-GATES — absent, not empty,
	// otherwise, which is the rule the v6 `lease` object established. §11.4's
	// `context` line names no member for these while §11.1 requires the results
	// be "included in the context bundle"; the amendment proposes this name.
	PreGates []PreGateResult `json:"pre_gates,omitempty"`
	// TargetSHA and TargetWorktree are the machine-readable target ref
	// (DKT-24): the commit the step's resolved `issue.diff` tree stood at, and
	// the producing record's declared worktree path — good while that checkout
	// is still on disk (it is swept at integration). Lifted from the diff
	// artifact's own round-record payload, so they are exactly as reproducible
	// as the input they describe.
	//
	// Before these, the target commit rode a PROSE convention ("the
	// change-summary's first line carries the sha") and every reviewing
	// consumer re-derived the tree via `git archive | tar -x` — or burned
	// reasoning proving tree-equivalence when integration had already minted a
	// new sha. Both absent when the resolved diff carries no round record.
	TargetSHA      string `json:"target_sha,omitempty"`
	TargetWorktree string `json:"target_worktree,omitempty"`
	// Resolution is the ruling recorded on THIS STEP that sent it back for
	// rework — the routing, and the note whoever decided it wrote (DKT-247).
	//
	// A resolve/approve note was audit-trail only: the packet rendered header,
	// frozen body, inputs, pins, and output spec, and nothing else. So a
	// ruling issued BETWEEN rounds — the operator's answer to the very
	// question that parked the step — could not reach the retry it authorized,
	// and a conductor applied rulings as its own repo commits instead (agw:
	// 3df53c4, b9182fa — both worked, neither sanctioned).
	//
	// SCOPED TO THIS STEP'S OWN ROW. A note on another step is a fact about
	// that step, and rendering it here would put one instance's ruling in
	// another's packet — the collision the instance-label ambiguity already
	// makes easy. Read from `steps.routing`, which holds the CURRENT routing
	// record for this step alone.
	//
	// nil when the step carries no routing record, so a first-round packet is
	// byte-identical to what it always was.
	Resolution *ContextResolution `json:"resolution,omitempty"`
}

// ContextResolution is the routing that sent a step back, and its note.
type ContextResolution struct {
	// Routing is the recorded routing — `retry`, `fix-loop`, `waiting-human`,
	// and so on. It is carried beside the note because "do X" reads very
	// differently under a retry than under an override.
	Routing string `json:"routing"`
	// Note is what the decider wrote, verbatim. Empty when the routing was
	// recorded without one — the routing alone is still worth rendering,
	// since it says why the step is being asked again.
	Note string `json:"note,omitempty"`
	// Gates names the gates that did NOT pass on the attempt this routing
	// ended, with their verdicts and reasons (DKT-261).
	//
	// It exists so a relay can tell an ENVIRONMENTAL failure from a CAPABILITY
	// one without a second query. That distinction is the hard and valuable
	// part of an escalation ladder: all three genuine capability-suspect
	// retries of the RUN-25/26 epoch were in fact environmental, so a naive
	// "gate failed twice, escalate" rule would have escalated three times and
	// helped zero times.
	//
	// DKT-254 is what makes the distinction readable rather than a guess. A
	// gate that COULD NOT MEASURE now records `skipped` and parks the step for
	// an operator instead of routing `on_fail`, so it never reaches a retry at
	// all; `unmatched` says the command was never trusted here; only `fail`
	// means a measurement was taken and the work did not pass it. A ladder that
	// escalates on `fail` alone is the rule the epoch's evidence supports.
	//
	// Empty on a step whose routing had nothing to do with gates, which is most
	// of them.
	Gates []ContextGateOutcome `json:"gates,omitempty"`
}

// ContextGateOutcome is one non-passing gate as the rework packet carries it.
//
// It is the LAST attempt per gate, matching the routing rule (§5.6 F4): a gate
// that failed twice and passed on the third try did not fail, and a packet that
// listed all three would invite a reader to conclude otherwise.
type ContextGateOutcome struct {
	Gate string `json:"gate"`
	// Verdict is `fail`, `unmatched`, or `skipped` — never `pass`, since a
	// passing gate is not why the step came back.
	Verdict string `json:"verdict"`
	// Reason is the recorded diagnosis, verbatim. It is what separates "the
	// trust entry is missing" from "the tree was gone" from a real failure,
	// and it is the field a classifier reads after the verdict.
	Reason string `json:"reason,omitempty"`
}

// ContextIssue is §11.4's `context.issue`. EVERY field but `id` comes from a
// snapshot column — that is what makes §9 item 5's mid-run edit immunity hold,
// and it is why §5.1.1 made title/kind/labels/scope snapshot columns rather
// than a join against `issues`.
type ContextIssue struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	BodySnapshot string   `json:"body_snapshot"`
	Kind         string   `json:"kind"`
	Labels       []string `json:"labels"`
	Scope        []string `json:"scope"`
}

// ContextInput is one resolved input artifact, in §11.4's shape.
type ContextInput struct {
	Artifact string `json:"artifact"`
	Kind     string `json:"kind"`
	// ProducerStep is the rendered instance identity of the step that produced
	// it, or "" for an engine-produced artifact (`issue.diff` at activation).
	ProducerStep string `json:"producer_step"`
	Body         string `json:"body"`
	// Payload is the artifact's STRUCTURED half as stored — JSON text, or ""
	// when the producing step declared none.
	//
	// CARRIED VERBATIM: no re-encoding, no re-validation, no key inspection.
	// Core validated the shape once, at completion (§6.8 stage 0), and the
	// schema register is S5's. Re-parsing here would let assembly reject an
	// artifact the engine already accepted — a packet that fails on data the
	// ledger holds is a worse failure than the one this field fixes.
	//
	// It is `omitempty` so a payload-less input serializes exactly as it did
	// before this field existed: every workflow that does not use payloads sees
	// byte-identical bundles and byte-identical packets.
	//
	// Without it, a step whose contract requires its inputs' structure had only
	// the prose — and recovering the rest meant reading engine storage directly,
	// which voids the pinning, the reproducibility, and §6.6's no-live-state
	// rule that the packet exists to provide.
	Payload string `json:"payload,omitempty"`
}

// ContextPin is §11.4's `pins` element. §11.4 gives pins ONE shape, so a
// workflow pin renders its `path` as `workflow:name@version` rather than
// growing a second shape (§6.4).
type ContextPin struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// LoopEntry records which loop iteration a step belongs to.
type LoopEntry struct {
	Ordinal int    `json:"ordinal"`
	Routing string `json:"routing"`
}

// The engine-produced input forms of §6.7.
const (
	inputIssueBody = "issue.body"
	inputIssueDiff = "issue.diff"

	// inputGateResults is the `<step>.gate-results` suffix (DKT-77): the
	// named step's RECORDED gate results, served from the ledger rather than
	// re-run. Engine-produced like the issue forms, but addressed through a
	// producer step because the results belong to one.
	inputGateResults = "gate-results"

	// ArtifactKindIssueDiff is the kind the engine records a computed VCS diff
	// under (§6.7.1). It is engine-produced rather than step-declared, which is
	// why it is a constant here and not a value read from a definition.
	ArtifactKindIssueDiff = "issue.diff"
)

// AssembleContext builds one step's context bundle inside tx.
//
// It takes a transaction because `step claim` mints the token and assembles the
// bundle in ONE transaction — "one atomic mediation: an unclaimed executor has
// nothing, a claimed one has everything" (engine-core §8).
func AssembleContext(
	tx *sql.Tx, sched *Scheduler, step *db.Step, ttls ttlConfig,
) (*Context, error) {
	def := sched.defs[step.WorkflowID]
	if def == nil {
		return nil, fmt.Errorf("step %s: its pinned workflow is not loaded", step.Instance)
	}
	spec := materializedSpec(def, step, sched.holdTally)
	if spec == nil {
		return nil, fmt.Errorf(
			"step %s: %q is not a step of its pinned workflow", step.Instance, step.StepName)
	}

	// Source 1: the step row, rendered at its effective status.
	row, err := StepRowFor(sched, step, ttls)
	if err != nil {
		return nil, err
	}

	// Sources 2 and 3: the snapshots. NOTHING below reads the `issues` table.
	issue, err := contextIssue(tx, step.RunID, step.IssueID)
	if err != nil {
		return nil, err
	}

	// Source 4: the resolved input artifacts. The run's artifact rows are
	// loaded ONCE and shared with the prior-round pass below — both read the
	// same snapshot, and the table's bodies scale with the run's whole
	// recorded output, so a second load doubled exactly the cost this path
	// pays most of.
	var artifacts []*db.Artifact
	if len(spec.Inputs) > 0 || loopReentryEmitter(sched, step, spec) {
		artifacts, err = db.ListRunArtifactsTx(tx, step.RunID)
		if err != nil {
			return nil, err
		}
	}
	inputs, err := resolveInputs(tx, sched, step, spec, issue.BodySnapshot, artifacts)
	if err != nil {
		return nil, err
	}

	// A materialized held step exists to decide ONE cluster, and its
	// synthesized spec declares no inputs — inline that cluster, or the
	// packet poses a question without its subject (DKT-105).
	if step.Materialized {
		held, err := heldClusterInput(tx, sched, step)
		if err != nil {
			return nil, err
		}
		if held != nil {
			inputs = append(inputs, *held)
		}
	}

	// A loop re-entry step answers the PREVIOUS round — the re-review
	// contract asks whether each prior finding is closed — yet its declared
	// inputs resolve to the loop body's own account of them (DKT-63).
	inputs = append(inputs, previousRoundInputs(sched, step, spec, artifacts)...)

	// Source 5: the pin LIST — paths and hashes, never contents.
	pins, err := contextPins(tx, step.RunID)
	if err != nil {
		return nil, err
	}

	metadata, err := decodeMetadata(step.Metadata)
	if err != nil {
		return nil, err
	}

	sha, worktree := resolveTarget(inputs)

	resolution := resolutionOf(step)
	if err := attachGateOutcomes(tx, step, resolution); err != nil {
		return nil, err
	}

	return &Context{
		Step: row, Issue: *issue, Inputs: inputs, Pins: pins,
		LoopEntry: loopEntryOf(step), Metadata: metadata,
		TargetSHA: sha, TargetWorktree: worktree,
		Resolution: resolution,
	}, nil
}

// attachGateOutcomes fills a resolution's failing-gate list (DKT-261).
//
// It is a SEPARATE pass rather than part of resolutionOf, because that function
// is pure over the step row and this one needs the gate table. Keeping the
// split makes the extra read visibly bounded to steps that have a routing
// record at all — a first-attempt packet performs none.
func attachGateOutcomes(tx *sql.Tx, step *db.Step, res *ContextResolution) error {
	if res == nil || step == nil {
		return nil
	}
	rows, err := db.GateResultsForStepTx(tx, step.ID)
	if err != nil {
		return err
	}

	// LAST attempt per gate, matching F4's routing rule: a gate that failed
	// twice and passed on the third try did not fail.
	last := make(map[string]db.GateResultRow)
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Pre {
			continue // PG4: a pre-gate is an input, not a judgment of the step.
		}
		prev, seen := last[r.Gate]
		if !seen {
			order = append(order, r.Gate)
		}
		if !seen || r.Ordinal >= prev.Ordinal {
			last[r.Gate] = r
		}
	}

	for _, gate := range order {
		r := last[gate]
		if r.Verdict == db.GateVerdictPass {
			continue
		}
		res.Gates = append(res.Gates, ContextGateOutcome{
			Gate: r.Gate, Verdict: r.Verdict, Reason: r.Reason,
		})
	}
	return nil
}

// resolutionOf splits this step's stored routing record back into its routing
// and its note (DKT-247).
//
// The record is written by routingRecord as `<routing>: <note>`, and the
// routing half is always one of a small closed set of hyphenated words — none
// containing ": " — so splitting on the FIRST occurrence recovers both halves
// exactly, however many colons the note itself contains.
//
// nil when the step carries no routing record at all: a step on its first
// attempt has nothing to render, and a packet that grew an empty section for
// every such step would be noise on the overwhelming majority of packets.
func resolutionOf(step *db.Step) *ContextResolution {
	if step == nil || step.Routing == "" {
		return nil
	}
	routing, note, found := strings.Cut(step.Routing, ": ")
	if !found {
		return &ContextResolution{Routing: step.Routing}
	}
	return &ContextResolution{Routing: routing, Note: note}
}

// resolveTarget lifts the round record out of the resolved `issue.diff`
// input's payload (DKT-24). The FIRST diff input wins: §6.7 resolves one per
// declaration, and a bundle carrying several describes one tree recorded at
// one moment — the resolver's ordinal scoping already guarantees that.
func resolveTarget(inputs []ContextInput) (sha, worktree string) {
	for _, in := range inputs {
		if in.Kind != ArtifactKindIssueDiff || in.Payload == "" {
			continue
		}
		var record struct {
			Head     string `json:"head"`
			Worktree string `json:"worktree"`
		}
		if json.Unmarshal([]byte(in.Payload), &record) != nil {
			continue
		}
		return record.Head, record.Worktree
	}
	return "", ""
}

// contextIssue materializes §11.4's `context.issue` from the two snapshot
// columns, and from nothing else.
//
// `id` is the run_issues key — the one field that is not snapshot-derived, and
// it cannot drift because an issue's id never changes. Every other field comes
// from `issue_snapshot`, which activation froze (§5.1.1).
func contextIssue(tx *sql.Tx, runID, issueID int) (*ContextIssue, error) {
	var (
		body     sql.NullString
		snapshot sql.NullString
	)
	err := tx.QueryRow(
		`SELECT body_snapshot, issue_snapshot FROM run_issues
		  WHERE run_id = ? AND issue_id = ?`, runID, issueID,
	).Scan(&body, &snapshot)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("issue %s is not in run %s",
			model.FormatID(issueID), model.FormatRunID(runID))
	}
	if err != nil {
		return nil, fmt.Errorf("reading the issue snapshot: %w", err)
	}

	out := &ContextIssue{
		ID:           model.FormatID(issueID),
		BodySnapshot: body.String,
		Labels:       []string{},
		Scope:        []string{},
	}

	if snapshot.String != "" {
		var frozen struct {
			Title  string   `json:"title"`
			Kind   string   `json:"kind"`
			Labels []string `json:"labels"`
			Scope  []string `json:"scope"`
		}
		if err := json.Unmarshal([]byte(snapshot.String), &frozen); err != nil {
			return nil, fmt.Errorf("reading the issue snapshot for %s: %w",
				model.FormatID(issueID), err)
		}
		out.Title, out.Kind = frozen.Title, frozen.Kind
		if frozen.Labels != nil {
			out.Labels = frozen.Labels
		}
		if frozen.Scope != nil {
			out.Scope = frozen.Scope
		}
	}
	return out, nil
}

// contextPins reads the pin LIST — paths and hashes. It never opens a pinned
// file: §6.6 is explicit that assembly "never re-reads a file a pin names".
// The hash IS the contract; re-reading would make the bundle depend on the
// working tree, which is exactly what the pin exists to prevent.
func contextPins(tx *sql.Tx, runID int) ([]ContextPin, error) {
	rows, err := db.ListPinsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]ContextPin, 0, len(rows))
	for _, p := range rows {
		path := p.Ref
		switch p.Kind {
		case db.PinKindWorkflow, db.PinKindSchema:
			// §11.4 gives pins one shape, so a registered-object pin renders its
			// ref into the `path` slot with its KIND as a scheme, rather than
			// growing a second shape a consumer must branch on (§6.4). A bare
			// `aggregate@1` in a slot named `path` would be indistinguishable
			// from a file someone happened to name that.
			path = p.Kind + ":" + p.Ref
		}
		out = append(out, ContextPin{Path: path, SHA256: p.SHA256})
	}
	return out, nil
}

// previousRoundInputs binds the PREVIOUS round's artifacts of a loop
// re-entry step's own emitted kind (DKT-63): review@k reads review@(k-1)'s
// fanout findings and the reconciled set beside the fix's change-summary,
// so a re-review judge reads the critique it must answer rather than the
// loop body's summary of it. RUN-5's re-review judges had only the fix
// step's own account — the author's summary of the critique of the author's
// work — and could not tell an answered ask from an ask answered as the fix
// characterised it, nor compute finding volume across rounds from one side
// of the exchange.
//
// The rule is structural, read from the pinned definition
// (loopReentryEmitter): the step is at ordinal > 0, inside the loop closure
// (the same merged set blockingLoopBodyAbsent consults), and emits a kind —
// then every DONE same-issue producer at ordinal k-1 contributes its newest
// artifact of that kind (latestPerProducer, f6b28cc's binding rule), in
// §6.7's within-input order (sortArtifacts) — the same collapse and the same
// order every declared input resolves under. "Its own emitted kind" is what
// keeps this generic: the critique a step answers is definitionally the kind
// it produces, and core never reads what the kind means. nil when the step
// is not a re-entry — ordinal 0, an unlooped definition, or a kindless step
// — so every other packet is byte-identical.
//
// `artifacts` is the caller's already-loaded snapshot (AssembleContext loads
// it once for this and the declared-input pass together).
func previousRoundInputs(
	sched *Scheduler, step *db.Step, spec *workflow.Step, artifacts []*db.Artifact,
) []ContextInput {
	if !loopReentryEmitter(sched, step, spec) {
		return nil
	}

	var matched []*db.Artifact
	for _, a := range artifacts {
		p := sched.stepByID[a.StepID]
		if p == nil || p.IssueID != step.IssueID ||
			p.Ordinal != step.Ordinal-1 || !recordedProducer(p.Status) {
			continue
		}
		if a.Kind != spec.Emits {
			continue
		}
		matched = append(matched, a)
	}
	matched = latestPerProducer(matched)
	sortArtifacts(matched, sched.stepByID)
	return artifactInputs(matched, sched.stepByID)
}

// artifactInputs renders artifact rows into the §11.4 bundle's input shape —
// the ONE reading of the artifact -> ContextInput mapping (the "ARTIFACT-%d"
// name, the producer instance, body beside payload), shared by the
// declared-input tail and the prior-round pass so the bundle shape cannot
// drift between them.
func artifactInputs(artifacts []*db.Artifact, producers map[int]*db.Step) []ContextInput {
	out := make([]ContextInput, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, ContextInput{
			Artifact:     fmt.Sprintf("ARTIFACT-%d", a.ID),
			Kind:         a.Kind,
			ProducerStep: producerInstance(producers, a.StepID),
			Body:         a.Body,
			Payload:      a.Payload,
		})
	}
	return out
}

// loopReentryEmitter is previousRoundInputs' applicability rule, named so
// AssembleContext can consult it too — it decides whether the artifact
// table needs loading at all when the step declares no inputs.
func loopReentryEmitter(sched *Scheduler, step *db.Step, spec *workflow.Step) bool {
	if step.Ordinal == 0 || spec == nil || spec.Emits == "" {
		return false
	}
	def := sched.defs[step.WorkflowID]
	return def != nil && sched.loopClosure(step.WorkflowID, def)[step.StepName]
}

// loopEntryOf reports the loop iteration a step belongs to, or nil at ordinal 0
// (§6.4: "`loop_entry` is the ordinal k and the routing that entered it, null
// at k=0").
//
// The routing that ENTERED the loop is recorded on the step at instantiation;
// phase 4 owns loop entry, so at S3 every step is at ordinal 0 and this returns
// nil. It is written now rather than later so the wire shape lands whole and a
// dispatcher consuming bundles today is not broken by the field appearing.
func loopEntryOf(step *db.Step) *LoopEntry {
	if step.Ordinal == 0 {
		return nil
	}
	return &LoopEntry{Ordinal: step.Ordinal, Routing: step.Routing}
}

// resolveInputs is §6.7: a step's declared `inputs`, resolved over `done`
// siblings only, ordered by (declared position, sibling index, artifact id) —
// over an artifact snapshot the caller already loaded (AssembleContext shares
// one load between this and the prior-round pass).
//
// THE ORDER IS NEVER EVENT ORDER. §11.3 (3) says so explicitly for loops and §2
// says so for fanout joins. "Ordered by artifact id" is trivially satisfiable
// by accident when insertion order happens to match, so the ordering here is
// applied explicitly and a test shuffles insertion order to prove it.
func resolveInputs(
	tx *sql.Tx, sched *Scheduler, step *db.Step, spec *workflow.Step,
	bodySnapshot string, artifacts []*db.Artifact,
) ([]ContextInput, error) {
	if len(spec.Inputs) == 0 {
		return []ContextInput{}, nil
	}

	// Which step instance produced each artifact — needed both for the
	// `producer_step` field and for the `done`-only rule. The scheduler's own
	// step index is exactly that mapping.
	producers := sched.stepByID
	def := sched.defs[step.WorkflowID]

	out := make([]ContextInput, 0, len(spec.Inputs))

	// The OUTER loop is the declared position: `inputs` order is the author's
	// order and the packet presents them in it (§6.11).
	for _, declared := range spec.Inputs {
		switch declared {
		case inputIssueBody:
			out = append(out, ContextInput{
				Artifact: inputIssueBody, Kind: inputIssueBody,
				Body: bodySnapshot,
			})
			continue
		case inputIssueDiff:
			input, err := resolveIssueDiff(artifacts, producers, step)
			if err != nil {
				return nil, err
			}
			out = append(out, input)
			continue
		}

		// `<step>.gate-results` (DKT-77): the producer's RECORDED gate
		// results, addressable as an input. Before this form existed, no
		// syntax exposed what the engine had already recorded — so every
		// review step re-ran the same checks independently (44/44 judge steps
		// re-ran the tests in RUN-5), duplicating exactly the work whose
		// results were sitting in the ledger.
		if stepName, kind, ok := splitInput(declared); ok && kind == inputGateResults {
			resolved, err := resolveGateResults(tx, sched, step, stepName)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved...)
			continue
		}

		matched, err := resolveDeclaredInput(artifacts, producers, def, step, declared)
		if err != nil {
			return nil, err
		}
		out = append(out, artifactInputs(matched, producers)...)
	}

	return out, nil
}

// ResolveInputArtifacts is §6.7 resolved to the ARTIFACT ROWS themselves, for a
// consumer that needs a field the §11.4 bundle does not carry.
//
// It exists because the bundle is the WORKER-FACING shape: a `ContextInput` has
// a `body` and no `payload`, since §11.4 gives an executor the artifact's text
// and nothing else. The `aggregate` builtin reduces PAYLOADS (§2), so
// it needs the row.
//
// The RULE is not duplicated — both this and resolveInputs walk the same
// declared-position loop over the same resolveDeclaredInput, so the `done`-only
// filter, the ordinal scoping, and the within-input sort have exactly one
// implementation. What differs is only which columns the caller reads.
//
// The engine-produced forms (`issue.body`, `issue.diff`) resolve to NO ARTIFACT
// here. `issue.body` has no artifact row at all, and neither carries a payload
// — they are prose and a diff, not a JSON array of clusters. A caller wanting
// them as text still has the bundle.
func ResolveInputArtifacts(
	tx *sql.Tx, sched *Scheduler, step *db.Step, spec *workflow.Step,
) ([]*db.Artifact, error) {
	if len(spec.Inputs) == 0 {
		return nil, nil
	}

	artifacts, err := db.ListRunArtifactsTx(tx, step.RunID)
	if err != nil {
		return nil, err
	}
	producers := make(map[int]*db.Step, len(sched.steps))
	for _, s := range sched.steps {
		producers[s.ID] = s
	}
	def := sched.defs[step.WorkflowID]

	var out []*db.Artifact
	for _, declared := range spec.Inputs {
		if declared == inputIssueBody || declared == inputIssueDiff {
			continue
		}
		matched, err := resolveDeclaredInput(artifacts, producers, def, step, declared)
		if err != nil {
			return nil, err
		}
		out = append(out, matched...)
	}
	return out, nil
}

// resolveDeclaredInput is §6.7's rule for ONE `<step>.<kind>` declaration: the
// `done`-only match, the ordinal scoping, and the within-input sort, in order.
//
// It is the single implementation both resolvers above share. A second copy is
// exactly what would drift at the first ordinal that mattered.
//
// `def` carries the loop-rebind rule (DKT-12): a declared input naming a step
// that never re-runs (`implement`) still resolves to its ordinal-0 artifact by
// this rule alone. loopProducerRedirect, applied after, is what makes a step
// downstream of `after_loop` see the LOOP's latest emit instead — see its
// doc comment for why `def` is what makes that decidable.
func resolveDeclaredInput(
	artifacts []*db.Artifact, producers map[int]*db.Step, def *workflow.Definition,
	step *db.Step, declared string,
) ([]*db.Artifact, error) {
	stepName, kind, ok := splitInput(declared)
	if !ok {
		// V11 rejects a malformed input at register time, so reaching here
		// means a definition was written directly into the table.
		return nil, fmt.Errorf(
			"step %s: input %q is neither `<step>.<kind>` nor an engine form",
			step.Instance, declared)
	}

	matched := matchingArtifacts(artifacts, producers, step.IssueID, stepName, kind)
	matched, boundOrdinal := ordinalScoped(matched, producers, step.Ordinal)
	if redirect := loopProducerRedirect(
		artifacts, producers, def, step, stepName, kind, boundOrdinal,
	); redirect != nil {
		matched = redirect
	}
	matched = latestPerProducer(matched)
	sortArtifacts(matched, producers)
	return matched, nil
}

// latestPerProducer collapses superseded emits: when ONE producer instance
// recorded several artifacts of the same kind, only the newest is current and
// the earlier ones are history (DKT-103).
//
// Multi-emit from a single instance happens on every held-cluster resolution
// — resolveHeldPayload re-records the routing step's payload with the
// decision folded in, expressly so "the latest artifact" carries the whole
// story — and on a resolved retry, where the re-claimed step completes a
// second time. Binding every emit fed a downstream vote step the same
// findings payload once per resolution round (RUN-8's security-vote@0
// rendered one 21-cluster payload five times, ~95KB of noise per panelist).
//
// The key is the producer INSTANCE (step id), never the step name: fanned-out
// siblings are distinct instances whose artifacts are all inputs under §2's
// join rule, and this keeps every one of them.
func latestPerProducer(matched []*db.Artifact) []*db.Artifact {
	type emitKey struct {
		stepID int
		kind   string
	}
	latest := make(map[emitKey]*db.Artifact, len(matched))
	for _, a := range matched {
		k := emitKey{a.StepID, a.Kind}
		// Artifact ids ascend with insertion, so the highest id is the newest
		// emit — the same "read starts from the latest artifact" rule the
		// hold-resolution writer relies on.
		if cur, ok := latest[k]; !ok || a.ID > cur.ID {
			latest[k] = a
		}
	}
	if len(latest) == len(matched) {
		return matched
	}
	out := make([]*db.Artifact, 0, len(latest))
	for _, a := range matched {
		if latest[emitKey{a.StepID, a.Kind}] == a {
			out = append(out, a)
		}
	}
	return out
}

// loopProducerRedirect is DKT-12's rebind: on loop re-entry, a declared input
// naming the loop's original producer (e.g. `implement.change-summary`) binds
// to the loop BODY's latest emit (`fix@N`) instead, when the body produced an
// artifact of the same kind at this step's ordinal.
//
// Without it, a re-entered `review@N`'s static `implement.change-summary`
// input resolves through ordinalScoped's ordinary fallback straight to
// `implement@0` — correct for a step that genuinely has no ordinal-N sibling,
// wrong for `review`, whose whole reason for re-entering IS that `fix@N`
// produced a newer answer to the same question. RUN-1 graph-engine measured
// the cost: four re-review judges reconstructed and judged the superseded
// `implement` commit instead of `fix`'s, because their packets' only commit
// reference was `implement`'s stale change-summary.
//
// It fires only when ALL of:
//  1. the consuming step is downstream of `after_loop` — the loop BODY's own
//     inputs (e.g. `fix`'s own `implement.change-summary`) must NOT redirect:
//     `implement` is genuinely upstream of the loop and its ordinal-0 answer
//     is the right one for `fix` to read (§7.4's `fix@1 -> implement@0`
//     example is exactly this row, and redirecting it would feed `fix` its
//     own prior output as if it were `implement`'s);
//  2. the step is at ordinal > 0 — ordinal 0 has no loop body yet to redirect
//     to, and every step is at ordinal 0 the first time through;
//  3. the named producer's own artifact is STALE — bound below this ordinal,
//     which is true for any step that never re-runs;
//  4. a loop-body step (`loop = true`) produced a `done` artifact of the SAME
//     KIND at exactly this ordinal — the redirect matches by kind, not by
//     name, because that is the only thing the two producers share: `fix`
//     does not know it is standing in for `implement`, it only knows it
//     `emits = "change-summary"` too.
//
// nil means no redirect applies; the caller keeps ordinalScoped's answer.
func loopProducerRedirect(
	artifacts []*db.Artifact, producers map[int]*db.Step, def *workflow.Definition,
	step *db.Step, stepName, kind string, boundOrdinal int,
) []*db.Artifact {
	if def == nil || step.Ordinal == 0 || boundOrdinal >= step.Ordinal {
		return nil
	}
	if !afterLoopDownstream(def)[step.StepName] {
		return nil
	}
	// The named producer must itself be one this loop does not re-run — a
	// step declared `loop = true` legitimately has fresher instances the
	// ordinary rule already finds, and redirecting past those would be wrong.
	if named := workflow.StepByName(def, stepName); named != nil && named.Loop {
		return nil
	}

	var out []*db.Artifact
	for _, a := range artifacts {
		if kind != "*" && a.Kind != kind {
			continue
		}
		producer := producers[a.StepID]
		if producer == nil || producer.IssueID != step.IssueID {
			continue
		}
		if !recordedProducer(producer.Status) || producer.Ordinal != step.Ordinal {
			continue
		}
		body := workflow.StepByName(def, producer.StepName)
		if body == nil || !body.Loop {
			continue
		}
		out = append(out, a)
	}
	return out
}

// recordedProducer reports whether an instance in this status has finished the
// work its artifacts describe, and so may feed a downstream input.
//
// §2 says "`done` siblings only", and for every status but one that reading is
// exact: `pending`/`claimed`/`running`/`gated` are in flight, and `skipped`
// and `failed-routed` never recorded. `superseded` is the exception, and
// taking §2 literally over it is DKT-375.
//
// A step reaches `superseded` by exactly two routes, and they are not alike:
//
//  1. the loop's supersede sweep (loop.go), which takes ONLY `pending`
//     instances — steps that never ran, and so own no artifact for this
//     filter to admit or exclude;
//  2. `resolve --as fix-round` (human.go), which supersedes the PARKED
//     instance that asked the question. That instance ran, gated, recorded,
//     and emitted; the operator authorized the next round on the strength of
//     what it emitted. Its artifacts are the freshest answer in the run.
//
// Excluding (2) is silent and expensive: `ordinalScoped` falls back a round
// and the packet reads a superseded set with nothing marking it stale. Harness
// RUN-32 spent an operator-authorized `fix@3` re-reading `reconcile@1`'s
// closed round-1 clusters because `reconcile@2` — the instance whose park
// routed that very round — was `superseded` rather than `done`.
//
// Admitting (1) costs nothing by construction: a swept step has no artifacts.
func recordedProducer(status string) bool {
	return status == db.StepDone || status == db.StepSuperseded
}

// matchingArtifacts selects the artifacts a `<step>.<kind>` or `<step>.*` input
// resolves over: artifacts of the named step, for THIS issue, produced by an
// instance that RECORDED its work (recordedProducer).
//
// The rule (§2, verbatim: "Downstream inputs resolve over `done` siblings
// only") is what keeps a join honest: an in-flight sibling's partial artifact
// is not an input, it is work in progress. `superseded` is admitted beside
// `done` because it is not in-flight — see recordedProducer for why the
// literal `done`-only reading was wrong (DKT-375).
func matchingArtifacts(
	artifacts []*db.Artifact, producers map[int]*db.Step,
	issueID int, stepName, kind string,
) []*db.Artifact {
	var out []*db.Artifact
	for _, a := range artifacts {
		producer := producers[a.StepID]
		if producer == nil || producer.IssueID != issueID {
			continue
		}
		if producer.StepName != stepName {
			continue
		}
		if !recordedProducer(producer.Status) {
			continue
		}
		if kind != "*" && a.Kind != kind {
			continue
		}
		out = append(out, a)
	}
	return out
}

// ordinalScoped is §7.4 — ordinal-scoped input binding, with the fallback that
// is PER INPUT rather than per step.
//
// For a step at ordinal k, one declared input resolves:
//
//  1. among `done` instances of the named step AT ORDINAL k;
//  2. if none, fall back to the HIGHEST ORDINAL < k that has a `done` instance.
//
// THE FALLBACK BEING PER-INPUT IS THE WHOLE CLAUSE, and the fixture's `fix` step
// is why §11.3 (3) says so explicitly. `fix@1` declares
// `["reconcile.findings", "implement.change-summary"]`: `reconcile` re-ran at
// ordinal 1, so its input binds fresh at 1, while `implement` is upstream of
// `after_loop` and never re-runs, so its input binds at 0. A per-STEP rule would
// have to pick one ordinal for both and would be wrong about one of them — it
// would either feed `fix@1` a stale ordinal-0 reconciliation, or find no
// `implement@1` and bind nothing at all.
//
// Applied per declared input by construction: the caller invokes this once per
// entry in `inputs`, over that input's candidates alone.
//
// The second return is the ordinal the result actually bound at (-1 when
// nothing matched). loopProducerRedirect reads it to tell "bound fresh, at
// this step's own ordinal" from "bound stale, at an earlier one" — the
// distinction its whole rebind decision turns on.
func ordinalScoped(
	matched []*db.Artifact, producers map[int]*db.Step, ordinal int,
) ([]*db.Artifact, int) {
	if len(matched) == 0 {
		return matched, -1
	}

	// The best ordinal available at or below this step's: k when the named step
	// re-ran, otherwise the highest earlier one that produced anything.
	best := -1
	for _, a := range matched {
		producer := producers[a.StepID]
		if producer == nil || producer.Ordinal > ordinal {
			// Above this step's ordinal: a LATER loop's artifact is not an input
			// to an earlier instance. Without this, a re-instantiated
			// `synthesize@1` completing while `verify@0` is still in flight
			// would leak ordinal-1 findings into an ordinal-0 bundle — and
			// §9 item 5's byte-identical guarantee would depend on timing.
			continue
		}
		if producer.Ordinal > best {
			best = producer.Ordinal
		}
	}
	if best < 0 {
		return nil, -1
	}

	// Every artifact AT that ordinal — not just one. A fanned-out producer has
	// four `done` siblings at the same ordinal and all four are inputs (§2's
	// join rule), which is what `review.*` resolves to for `synthesize`.
	out := make([]*db.Artifact, 0, len(matched))
	for _, a := range matched {
		if producer := producers[a.StepID]; producer != nil && producer.Ordinal == best {
			out = append(out, a)
		}
	}
	return out, best
}

// sortArtifacts applies §6.7's within-input order: sibling index, then artifact
// id. The declared position is the caller's outer loop.
//
// A nil sibling index (a step that is not fanned out) sorts before any index, so
// the single-instance case is stable against a later fanout of the same step.
func sortArtifacts(artifacts []*db.Artifact, producers map[int]*db.Step) {
	sort.SliceStable(artifacts, func(i, j int) bool {
		si := siblingIndexOf(producers, artifacts[i].StepID)
		sj := siblingIndexOf(producers, artifacts[j].StepID)
		if si != sj {
			return si < sj
		}
		return artifacts[i].ID < artifacts[j].ID
	})
}

func siblingIndexOf(producers map[int]*db.Step, stepID int) int {
	step := producers[stepID]
	if step == nil || step.SiblingIndex == nil {
		return -1
	}
	return *step.SiblingIndex
}

func producerInstance(producers map[int]*db.Step, stepID int) string {
	if step := producers[stepID]; step != nil {
		return step.Instance
	}
	return ""
}

// resolveIssueDiff is §6.7.1 D3 and D4.
//
// D3: the input resolves to the HIGHEST-ORDINAL, then highest-id `done`-
// attributed `issue.diff` artifact for the issue — exactly §7.4's ordinal-scoped
// rule applied to a kind the engine produces rather than a step does. No special
// case, which is what makes the fixture's `fix@1 -> review@1` cycle correct
// without any rule specific to loops: ordinal 1 beats ordinal 0.
//
// D4: with no such artifact, the input resolves to an EMPTY diff — never an
// error, and never a live `git diff`. Assembly reads no live state (§6.6), and
// "empty" is the truthful answer: nothing has changed the tree in this run.
func resolveIssueDiff(
	artifacts []*db.Artifact, producers map[int]*db.Step, step *db.Step,
) (ContextInput, error) {
	var best *db.Artifact
	bestOrdinal := -1

	for _, a := range artifacts {
		if a.Kind != ArtifactKindIssueDiff {
			continue
		}
		producer := producers[a.StepID]

		// An activation-recorded empty diff (D4) has no producing step. It is a
		// legitimate candidate at ordinal 0 — it is what a consumer downstream
		// of no executor step resolves to.
		ordinal := 0
		if producer != nil {
			if producer.IssueID != step.IssueID {
				continue
			}
			if !recordedProducer(producer.Status) {
				continue
			}
			ordinal = producer.Ordinal
		} else if a.StepID != 0 {
			// Attributed to a step that is not loaded: not this issue's.
			continue
		}

		// Highest ordinal wins; within an ordinal, highest id. Written as an
		// explicit first-candidate case rather than folded into the comparison,
		// because `best == nil` makes the id half of the condition unevaluable
		// and a reader should not have to work out that the ordinal half always
		// carries it.
		switch {
		case best == nil, ordinal > bestOrdinal:
		case ordinal == bestOrdinal && a.ID > best.ID:
		default:
			continue
		}
		best, bestOrdinal = a, ordinal
	}

	if best == nil {
		// D4, in its purest form: not even the activation-time empty artifact
		// exists. The empty diff is still the answer.
		return ContextInput{
			Artifact: inputIssueDiff, Kind: ArtifactKindIssueDiff, Body: "",
		}, nil
	}

	return ContextInput{
		Artifact:     fmt.Sprintf("ARTIFACT-%d", best.ID),
		Kind:         best.Kind,
		ProducerStep: producerInstance(producers, best.StepID),
		Body:         best.Body,
		// The round record (DKT-106): `head`, and `round_base` when the body
		// carries a round delta. Absent on pre-record artifacts, and omitempty
		// keeps those bundles byte-identical.
		Payload: best.Payload,
	}, nil
}

// resolveGateResults is the `<step>.gate-results` form (DKT-77): one input per
// `done` producer instance, carrying that instance's recorded gate results as
// a JSON array in the §11.4 `gate result` shape.
//
// The instance selection mirrors artifact resolution exactly — same issue,
// `done` only, ordinal-scoped with the per-input fallback, siblings ordered by
// index — because the question is the same one with a different column: "what
// did the producer record". A producer that recorded no gates resolves to an
// EMPTY ARRAY, not an absent input: "this step ran no checks" is an answer a
// consumer can act on, while an absent input reads as a resolution failure.
//
// The one departure from artifact resolution: the REQUESTING step admits
// itself regardless of status (DKT-12). Its `pre = true` gate rows are
// committed before context assembly runs, so a self-declared
// `<self>.gate-results` is the step reading its own claim-time measurements —
// the done-filter alone would drop the step (`claimed` at assembly) and the
// input would silently resolve absent. Completion-side rows, not yet recorded
// at claim, show up as the empty array the previous paragraph promises.
func resolveGateResults(
	tx *sql.Tx, sched *Scheduler, step *db.Step, stepName string,
) ([]ContextInput, error) {
	var candidates []*db.Step
	best := -1
	for _, s := range sched.steps {
		if s.IssueID != step.IssueID || s.StepName != stepName {
			continue
		}
		if s.Ordinal > step.Ordinal || (!recordedProducer(s.Status) && s.ID != step.ID) {
			continue
		}
		if s.Ordinal > best {
			best = s.Ordinal
		}
		candidates = append(candidates, s)
	}
	if best < 0 {
		return nil, nil
	}

	producers := make([]*db.Step, 0, len(candidates))
	for _, s := range candidates {
		if s.Ordinal == best {
			producers = append(producers, s)
		}
	}
	sort.SliceStable(producers, func(i, j int) bool {
		si, sj := -1, -1
		if producers[i].SiblingIndex != nil {
			si = *producers[i].SiblingIndex
		}
		if producers[j].SiblingIndex != nil {
			sj = *producers[j].SiblingIndex
		}
		if si != sj {
			return si < sj
		}
		return producers[i].ID < producers[j].ID
	})

	out := make([]ContextInput, 0, len(producers))
	for _, producer := range producers {
		rows, err := db.GateResultsForStepTx(tx, producer.ID)
		if err != nil {
			return nil, err
		}
		body, err := encodeGateResults(rows)
		if err != nil {
			return nil, fmt.Errorf(
				"encoding gate results of %s: %w", producer.Instance, err)
		}
		out = append(out, ContextInput{
			Artifact:     inputGateResults,
			Kind:         inputGateResults,
			ProducerStep: producer.Instance,
			Body:         body,
		})
	}
	return out, nil
}

// encodeGateResults renders recorded rows in the §11.4 `gate result` shape —
// the same keys a claim response's `pre_gates` carries, so a consumer parses
// one shape wherever gate results appear.
func encodeGateResults(rows []db.GateResultRow) (string, error) {
	type wire struct {
		Gate       string   `json:"gate"`
		Ordinal    int      `json:"ordinal"`
		Argv       []string `json:"argv"`
		Exit       *int     `json:"exit"`
		DurationMS int64    `json:"duration_ms"`
		Output     string   `json:"output"`
		Truncated  bool     `json:"truncated"`
		Verdict    string   `json:"verdict"`
		Pre        bool     `json:"pre,omitempty"`
		Reason     string   `json:"reason,omitempty"`
	}
	encoded := make([]wire, 0, len(rows))
	for _, r := range rows {
		encoded = append(encoded, wire{
			Gate: r.Gate, Ordinal: r.Ordinal, Argv: r.Argv, Exit: r.Exit,
			DurationMS: r.DurationMS, Output: r.Output, Truncated: r.Truncated,
			Verdict: r.Verdict, Pre: r.Pre, Reason: r.Reason,
		})
	}
	body, err := json.Marshal(encoded)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// splitInput decomposes a `<step>.<kind>` input into its halves. The split is on
// the LAST dot so a kind containing one (`change-summary.v2`) is not truncated;
// step names cannot contain a dot (V3).
func splitInput(declared string) (stepName, kind string, ok bool) {
	i := strings.LastIndex(declared, ".")
	if i <= 0 || i == len(declared)-1 {
		return "", "", false
	}
	return declared[:i], declared[i+1:], true
}

// ContextMeta is `step context --meta`'s per-section byte counts — the
// closure-size record of engine-core §8.
//
// It is a SIBLING object rather than a mutation of `context`, so the golden
// bundles (§8.3) are unaffected by asking for it (§6.4). A `--meta` that
// spliced counts into the bundle would make the goldens depend on a flag.
type ContextMeta struct {
	IssueBytes    int `json:"issue_bytes"`
	InputsBytes   int `json:"inputs_bytes"`
	PinsBytes     int `json:"pins_bytes"`
	MetadataBytes int `json:"metadata_bytes"`
	TotalBytes    int `json:"total_bytes"`
	// TemplatePinned reports whether the template a render would use is pinned.
	// An UNPINNED template is reported so the reproducibility gap is visible
	// rather than assumed (§6.11.1) — a packet rendered through an unpinned
	// file is reproducible only to the extent the operator chose.
	TemplatePinned bool `json:"template_pinned"`
}

// Meta measures a bundle's sections.
func (c *Context) Meta() ContextMeta {
	meta := ContextMeta{
		IssueBytes: len(c.Issue.Title) + len(c.Issue.BodySnapshot) + len(c.Issue.Kind),
	}
	for _, l := range c.Issue.Labels {
		meta.IssueBytes += len(l)
	}
	for _, s := range c.Issue.Scope {
		meta.IssueBytes += len(s)
	}
	for _, in := range c.Inputs {
		// The payload counts. A cap computed over the prose alone would let a
		// bundle exceed its declared limit while reporting that it did not.
		meta.InputsBytes += len(in.Body) + len(in.Kind) + len(in.Artifact) +
			len(in.ProducerStep) + len(in.Payload)
	}
	for _, p := range c.Pins {
		meta.PinsBytes += len(p.Path) + len(p.SHA256)
	}
	for k, v := range c.Metadata {
		meta.MetadataBytes += len(k) + len(fmt.Sprint(v))
	}
	meta.TotalBytes = meta.IssueBytes + meta.InputsBytes + meta.PinsBytes + meta.MetadataBytes
	return meta
}
