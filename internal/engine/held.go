package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Held clusters — docs/tdd/payloads-thresholds.md §7.7.
//
// engine-spec §2 gives this one sentence:
//
//	When `hold_spread` trips, the engine materializes a `type=human` step named
//	`<step>-held` gating the routing step.
//
// "Gating the routing step" is implemented with the machinery that already
// exists and with nothing new: the routing step's saga stops at a `held` stage,
// its status stays `gated` — which is NON-TERMINAL — so every downstream
// successor fails R3 and nothing proceeds. No new status, no synthetic `after`
// edge, no second readiness rule (H8).
//
// The materialized step is the one row in a run that expansion did not write,
// and its spec is a pure function of the PINNED definition plus the reserved
// suffix (H5), so §9 item 5's determinism is untouched.

// holdTally is the instance's configured answer to "who decides a hold?" —
// `vote.hold.rule` and `vote.hold.voters`, resolved for one run's project.
//
// The ZERO VALUE IS THE DEFAULT AND MEANS ONE OPERATOR DECIDES: held steps are
// minted `human` and approve/reject resolve them, exactly as they did before
// this type existed. Nothing in core assumes a panel, for the same reason
// nothing in core assumes a threshold — an unstated policy is not a policy.
//
// BOTH KEYS ARE REQUIRED for a tally, because either alone is incoherent: a
// rule with nobody to cast under it tallies zero votes forever, and a roster
// with no rule has no threshold to be measured against. Configuring one and not
// the other therefore mints `human`, which is the conservative reading — the
// question still gets asked, and an operator still answers it.
type holdTally struct {
	rule   string
	voters []string
	// cost is `vote.hold.cost` (DKT-584): the declared expected_cost a hold
	// minted as `vote` carries, so the panel the engine convenes is visible to
	// the budget floor the way a declared vote step's cost is. 0 — the default
	// — mints exactly the prior row.
	cost float64
}

// configured reports whether held steps are minted as vote steps.
func (t holdTally) configured() bool {
	return t.rule != "" && len(t.voters) > 0
}

// loadHoldTally resolves the configuration for a run's project.
func loadHoldTally(conn *sql.DB, runID int) (holdTally, error) {
	projectID, err := db.RunProjectID(conn, runID)
	if err != nil {
		return holdTally{}, err
	}
	rule, err := db.GetConfig(conn, projectID, db.KeyVoteHoldRule)
	if err != nil {
		return holdTally{}, err
	}
	voters, err := db.GetConfig(conn, projectID, db.KeyVoteHoldVoters)
	if err != nil {
		return holdTally{}, err
	}
	cost, err := db.GetConfig(conn, projectID, db.KeyVoteHoldCost)
	if err != nil {
		return holdTally{}, err
	}
	return holdTally{
		rule: rule.Value, voters: db.SplitNameList(voters.Value),
		cost: parseHoldCost(cost.Value),
	}, nil
}

// loadHoldTallyTx is loadHoldTally inside a caller's transaction, for the
// scheduler's snapshot (see db.GetConfigTx for why a pool read cannot be used
// there). The project id is already loaded by the caller that has the run.
func loadHoldTallyTx(tx *sql.Tx, projectID int) (holdTally, error) {
	rule, err := db.GetConfigTx(tx, projectID, db.KeyVoteHoldRule)
	if err != nil {
		return holdTally{}, err
	}
	voters, err := db.GetConfigTx(tx, projectID, db.KeyVoteHoldVoters)
	if err != nil {
		return holdTally{}, err
	}
	cost, err := db.GetConfigTx(tx, projectID, db.KeyVoteHoldCost)
	if err != nil {
		return holdTally{}, err
	}
	return holdTally{
		rule: rule.Value, voters: db.SplitNameList(voters.Value),
		cost: parseHoldCost(cost.Value),
	}, nil
}

// parseHoldCost reads `vote.hold.cost`'s stored value. `config set` validated
// it as a non-negative number on the way in, so a malformed value can only be
// a hand-edited store — tolerated as 0 rather than failing the saga that is
// materializing a hold, the same tolerance configuredBudgetDefault keeps for
// `budget.default`.
func parseHoldCost(value string) float64 {
	var cost float64
	if _, err := fmt.Sscanf(value, "%g", &cost); err != nil || cost < 0 {
		return 0
	}
	return cost
}

// heldStepCost is the expected_cost a materialized held step is minted with:
// the configured `vote.hold.cost` when the hold convenes a PANEL (kind
// `vote`), and 0 when one operator decides (kind `human`) — an operator's
// decision is not a panel's spend, and charging the floor for it would make
// the configured number mean two different things.
func (t holdTally) heldStepCost() float64 {
	if t.configured() {
		return t.cost
	}
	return 0
}

// heldStepKind is the kind a materialized held step is MINTED as.
//
// H3 says `human` "in every respect the verbs can observe", and that stays the
// default. A configured tally mints `vote` instead, and the kind is what
// PERSISTS: every later read of that step — the spec synthesis, `approve`, the
// vote driver — asks the row rather than the config, so a roster edited or
// removed mid-run cannot retype a question already asked.
func (t holdTally) heldStepKind() string {
	if t.configured() {
		return workflow.TypeVote
	}
	return workflow.TypeHuman
}

// heldStepInstance renders the materialized step's instance identity: the
// routing step's own name plus `-held`, at the routing step's OWN ordinal.
//
// No sibling index, ever (H2). §11.1's exactly-one-of rule means an `action`
// step is never fanned out, so there is no second instance for a hold to belong
// to and an index would encode a distinction that cannot exist.
//
// THIS IS THE WHOLE-HOLD IDENTITY, retained for the resolution read. The
// PER-CLUSTER identity is heldClusterInstance below.
func heldStepInstance(step *db.Step) string {
	return fmt.Sprintf("%s@%d", workflow.HeldStepName(step.StepName), step.Ordinal)
}

// heldClusterInstance renders the materialized step for ONE held cluster:
// the whole-hold identity plus `#<element index>`.
//
// One human step per held cluster, because approve/reject over a set of them
// is a single bit spent on several independent questions. RUN-1's round-2 hold
// carried four clusters in one step; the operator wanted two escalated and two
// accepted, could not say so, and recorded the protest in the approve note.
//
// THE INDEX IS THE ELEMENT'S POSITION IN THE PAYLOAD, not a counter over held
// elements. Position is stable across re-reads of the same immutable artifact,
// so a resumed saga re-derives the same instance for the same cluster; a
// counter would renumber if the held set were ever computed differently, and
// silently reattach an operator's answer to a different cluster.
//
// This uses `#` — the same sibling-index separator fanout uses — deliberately.
// H2 says a hold has no sibling index because an action step is never fanned
// out, and that reasoning is about the ROUTING step having one instance. The
// clusters inside its payload are a different axis, and they are genuinely
// plural, so they get the notation the codebase already uses for "several of
// these, distinguished by position".
func heldClusterInstance(step *db.Step, element int) string {
	return fmt.Sprintf("%s#%d", heldStepInstance(step), element)
}

// enterHeld is H4: the materialized step is created in the SAME TRANSACTION
// that records the aggregate's artifact and enters the `held` stage.
//
// One transaction, or a crash leaves a held payload with nobody able to resolve
// it — an artifact saying "an operator must decide this" and no step for the
// operator to decide it on.
// ONE STEP PER HELD CLUSTER. `held` carries the payload indices whose
// spread tripped, and each becomes its own step, so an operator can
// accept one and escalate another in the same hold.
//
// `tally` is the instance's configured decider (see holdTally). Its zero value
// mints `human` steps, which is the default and the whole of the prior
// behavior.
func enterHeld(tx *sql.Tx, step *db.Step, held []int, tally holdTally, nowMS int64) error {
	for _, element := range held {
		if err := materializeHeldCluster(tx, step, element, tally, nowMS); err != nil {
			return err
		}
	}

	// THE ISSUE MIRROR RUNS (DKT-380). A hold is an issue blocked on a human
	// decision, which is exactly what `review` reports — but this path never
	// routes (H8: the routing step stays `gated` and the decision is DEFERRED),
	// so reconcileIssueAndRun is never called and nothing recomputed the
	// mirror. The whole hold path read `in-progress`: the status for work in
	// flight, of which there is none until someone decides.
	//
	// Only the mirror, not the full reconciliation: there is no routing to
	// reconcile. The routing string is empty for the same reason — it exists
	// on reflectIssueStatus to narrate a `fix-loop` entry, and no routing was
	// decided here.
	if err := reflectIssueStatus(tx, step, "", nowMS); err != nil {
		return err
	}

	// The routing step's saga stops HERE. Its status is already `gated` from
	// stage 1 and stays there.
	if err := db.AdvanceSagaTx(tx, step.ID, db.SagaRouting, db.SagaHeld, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// materializeHeldCluster creates the gate step for one held cluster.
func materializeHeldCluster(
	tx *sql.Tx, step *db.Step, element int, tally holdTally, nowMS int64,
) error {
	instance := heldClusterInstance(step, element)

	// H7: at most one step per (run, issue, step, ordinal, cluster), enforced
	// by the existing UNIQUE(run_id, issue_id, instance) index. A resumed saga
	// that re-enters this branch finds it and does not duplicate.
	exists, err := heldStepExists(tx, step, instance)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	err = db.InsertStepTx(tx, db.StepRow{
		RunID: step.RunID, IssueID: step.IssueID, WorkflowID: step.WorkflowID,
		StepName: workflow.HeldStepName(step.StepName),
		Ordinal:  step.Ordinal,
		Instance: instance,
		// H3: `human` in every respect the verbs can observe — approve and
		// reject apply, `claim` refuses with CONFLICT naming the class, and
		// `next --run` offers it with no executor. A configured tally mints
		// `vote` instead, and the difference is confined to WHO ANSWERS FIRST:
		// `claim` still refuses, `next` still offers it with no executor, and
		// approve/reject still apply once a failed tally parks it.
		Kind:   tally.heldStepKind(),
		Status: db.StepPending,
		// DKT-584: a hold minted as a VOTE step carries the configured
		// `vote.hold.cost` so the panel is visible to the budget floor at
		// materialization; a `human` hold stays at 0, the prior row exactly.
		ExpectedCost: tally.heldStepCost(),
		// H4: the flag that tells a reader a declared question from a
		// computed one.
		Materialized: true,
	}, nowMS)
	if err != nil {
		return err
	}

	// The event names the ONE cluster this step decides, so the log says which
	// question was asked rather than only that a hold occurred.
	return recordEvent(tx, eventRecord{
		Kind: EventStepHeld, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: fmt.Sprintf(`{"held":%s,"step":%q}`, jsonInts([]int{element}), instance),
		AtMS: nowMS,
	})
}

// jsonInts renders the held indices for the event's data field.
func jsonInts(values []int) string {
	if values == nil {
		values = []int{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// heldClusterInput synthesizes the ONE input a materialized held step
// carries: the cluster it exists to decide (DKT-105).
//
// The engine knows both the producer step and the cluster index at hold time
// — the instance name encodes the index, and the routing step's latest
// artifact carries the payload — yet the synthesized spec declares no inputs,
// so the packet used to render header and issue body alone. Every panel then
// re-derived the cluster by fetching the findings artifact out-of-band and
// correlating `#k` to a payload position by hand: an array-order join with
// transposition risk, per concurrent panel.
//
// nil with no error means "nothing to inline" — an unmaterialized step, an
// instance that names no cluster, or a payload this build cannot read. A
// held step must keep rendering in every one of those cases: the packet is
// how the question reaches its decider at all.
func heldClusterInput(tx *sql.Tx, sched *Scheduler, step *db.Step) (*ContextInput, error) {
	link, err := heldClusterLink(tx, sched, step)
	if err != nil || link == nil {
		return nil, err
	}
	return &ContextInput{
		Artifact:     link.Artifact,
		Kind:         "held-cluster",
		ProducerStep: link.ProducerStep,
		Body: fmt.Sprintf(
			"Cluster %d of %d from %s, held for this step's decision. "+
				"Decide THIS cluster alone; the payload below is the cluster itself.",
			link.Cluster, link.Clusters, link.ProducerStep),
		Payload: link.payload,
	}, nil
}

// HeldClusterLink is a materialized held step's provenance: WHICH cluster it
// decides, out of how many, and WHERE that cluster's payload lives.
//
// It exists because a held row named none of it (DKT-239). `step artifacts` on
// a `reconcile-held@0#N` row answered "produced no artifacts" — true, and
// useless: the payload sits on the SYNTHESIZE step's artifact, not the hold's.
// `step show --json` named no cluster and no source artifact. And the `#N`
// suffix is the element's POSITION IN THE PAYLOAD, which reads as a cluster id
// and is not one. A conductor needed 8 calls, 3 tracebacks, and a reading of
// the aggregate's step-recorded event to find and disambiguate one payload.
//
// The payload itself is deliberately NOT on the wire here — `step show` is a
// status check, and inlining a held cluster's body would make it a document
// dump, the same rule the report's artifact index follows. The artifact id is
// one `step context` away for anyone who wants the body.
type HeldClusterLink struct {
	// Cluster is the element's POSITION in the routing step's payload array,
	// zero-based — the number the instance's `#N` suffix carries. Named
	// `cluster_index`, not `cluster`, precisely because it is not an id.
	Cluster int `json:"cluster_index"`
	// Clusters is how many the payload holds, which is what makes the index
	// legible: `#3` alone says nothing about whether that is the last one.
	Clusters int `json:"cluster_count"`
	// Artifact is the ARTIFACT-N the cluster's payload lives in — the answer
	// `step artifacts` on the held row cannot give, because the held row
	// produced nothing.
	Artifact string `json:"artifact"`
	// ProducerStep is the routing step instance that recorded it.
	ProducerStep string `json:"producer_step"`

	// payload is the cluster's own bytes, for the packet's inline input. It is
	// unexported so it cannot reach a status verb's wire format by accident.
	payload string
}

// heldClusterLink resolves a materialized held step to the cluster it decides.
//
// nil with no error means "nothing to link" — an unmaterialized step, an
// instance that names no cluster, or a payload this build cannot read. Every
// one of those must stay renderable: the packet is how the question reaches
// its decider at all, and a status verb must not fail because a hold's
// provenance is unreadable.
func heldClusterLink(
	tx *sql.Tx, sched *Scheduler, step *db.Step,
) (*HeldClusterLink, error) {
	if !step.Materialized {
		return nil, nil
	}
	routingName, ok := workflow.RoutingStepNameOf(step.StepName)
	if !ok {
		return nil, nil
	}
	// The step's own instance is `<held-name>@<ordinal>#<element>`.
	element, ok := heldClusterElementOf(
		step.Instance, fmt.Sprintf("%s@%d", step.StepName, step.Ordinal))
	if !ok {
		return nil, nil
	}

	// The routing step this hold belongs to, from the scheduler's loaded set.
	var routing *db.Step
	for _, s := range sched.steps {
		if s.RunID == step.RunID && s.IssueID == step.IssueID &&
			s.StepName == routingName && s.Ordinal == step.Ordinal {
			routing = s
			break
		}
	}
	if routing == nil {
		return nil, nil
	}

	artifacts, err := db.ListRunArtifactsTx(tx, step.RunID)
	if err != nil {
		return nil, err
	}
	// The newest artifact carries the current payload — each resolution round
	// re-records it whole, which is the same rule resolveHeldPayload reads by.
	var latest *db.Artifact
	for _, a := range artifacts {
		if a.StepID != routing.ID {
			continue
		}
		if latest == nil || a.ID > latest.ID {
			latest = a
		}
	}
	if latest == nil || latest.Payload == "" {
		return nil, nil
	}

	var elements []json.RawMessage
	if err := json.Unmarshal([]byte(latest.Payload), &elements); err != nil {
		return nil, nil
	}
	if element < 0 || element >= len(elements) {
		return nil, nil
	}

	return &HeldClusterLink{
		Cluster:      element,
		Clusters:     len(elements),
		Artifact:     fmt.Sprintf("ARTIFACT-%d", latest.ID),
		ProducerStep: routing.Instance,
		payload:      string(elements[element]),
	}, nil
}

// heldStepExists reports whether the materialized step is already there.
func heldStepExists(tx *sql.Tx, step *db.Step, instance string) (bool, error) {
	var exists bool
	err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM steps
		                WHERE run_id = ? AND issue_id = ? AND instance = ?)`,
		step.RunID, step.IssueID, instance).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("probing for %s: %w", instance, err)
	}
	return exists, nil
}

// heldDecision reports whether the hold on a routing step has been RESOLVED,
// and if so whether it was approved.
//
// "Resolved" is the materialized step reaching a terminal status, which
// approve and reject both produce (H14). "Approved" is `done` with a `pass`
// routing — exactly the state `step approve` writes, and exactly the test
// `guard gate` already applies to a declared human gate. A step that reached
// `done` by any other route did not receive an approval.
// A hold now spans SEVERAL steps, one per cluster, so this reduces
// over all of them:
//
//   - RESOLVED means EVERY cluster step is terminal. A partial answer leaves
//     the saga waiting, because routing on it would route over a cluster
//     nobody decided.
//   - APPROVED means every one of them was approved. Reject is the escalating
//     answer, so one rejection routes the step per `on_fail` — the same
//     outcome a single rejection produced when there was one step, and the
//     conservative reduction: a mixed answer must not silently pass.
//
// The per-cluster dispositions are not lost by this reduction. Each step keeps
// its own status, routing, and note, and resolveHeldPayload marks only the
// approved clusters — so what routes is one decision while the record stays
// per-cluster.
func heldDecision(conn *sql.DB, step *db.Step) (resolved, approved bool, err error) {
	rows, err := heldClusterRows(conn, step)
	if err != nil {
		return false, false, err
	}

	found := false
	allApproved := true
	for _, r := range rows {
		found = true
		if !db.StepTerminal(r.status) {
			return false, false, nil
		}
		if r.status != db.StepDone || !routingIs(r.routing.String, RoutingPass) {
			allApproved = false
		}
	}
	if !found {
		// No materialized step — a loop entry superseded the whole ordinal, or
		// a database was restored without it. Treat the hold as unresolved:
		// the saga waits rather than routing on a question nobody answered.
		return false, false, nil
	}
	return true, allApproved, nil
}

// heldClusterRow is one row of a materialized hold's own steps — the whole-hold
// row (H2's identity) and every per-cluster row (H2's plural sibling), as read
// by heldClusterRows.
type heldClusterRow struct {
	instance string
	status   string
	routing  sql.NullString
}

// heldClusterRows reads every materialized step of one hold — the whole-hold
// instance and its `#<element>` siblings — for heldDecision and
// parkedByRejectedHold, which ask two DIFFERENT questions of the same rows
// (heldDecision reduces "resolved, and unanimously approved?"; parkedByRejectedHold
// asks "is a REJECTION what parked this, specifically?") and so keep their own
// scan loops; only the query and the row shape are shared here.
func heldClusterRows(conn *sql.DB, step *db.Step) ([]heldClusterRow, error) {
	prefix := heldStepInstance(step)
	rows, err := conn.Query(
		`SELECT instance, status, routing FROM steps
		  WHERE run_id = ? AND issue_id = ? AND (instance = ? OR instance LIKE ?)
		  ORDER BY instance`,
		step.RunID, step.IssueID, prefix, prefix+"#%")
	if err != nil {
		return nil, fmt.Errorf("reading the held decision for %s: %w",
			step.Instance, err)
	}
	defer rows.Close()

	var out []heldClusterRow
	for rows.Next() {
		var r heldClusterRow
		if err := rows.Scan(&r.instance, &r.status, &r.routing); err != nil {
			return nil, fmt.Errorf("reading a held decision for %s: %w",
				step.Instance, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the held decision for %s: %w",
			step.Instance, err)
	}
	return out, nil
}

// heldClusterElements reports the payload indices whose cluster steps were
// APPROVED, for the resolution half.
//
// Only approved clusters are marked `operator_resolved` — that is what makes
// the record per-cluster rather than a blanket stamp over everything that was
// ever held.
func heldClusterElements(tx *sql.Tx, step *db.Step) (map[int]bool, error) {
	prefix := heldStepInstance(step)

	rows, err := tx.Query(
		`SELECT instance, status, routing FROM steps
		  WHERE run_id = ? AND issue_id = ? AND instance LIKE ?`,
		step.RunID, step.IssueID, prefix+"#%")
	if err != nil {
		return nil, fmt.Errorf("reading held clusters for %s: %w", step.Instance, err)
	}
	defer rows.Close()

	approved := make(map[int]bool)
	for rows.Next() {
		var (
			instance string
			status   string
			routing  sql.NullString
		)
		if err := rows.Scan(&instance, &status, &routing); err != nil {
			return nil, fmt.Errorf("reading a held cluster for %s: %w", step.Instance, err)
		}
		element, ok := heldClusterElementOf(instance, prefix)
		if !ok {
			continue
		}
		approved[element] = status == db.StepDone && routingIs(routing.String, RoutingPass)
	}
	return approved, rows.Err()
}

// heldClusterElementOf recovers the element index from a per-cluster instance.
func heldClusterElementOf(instance, prefix string) (int, bool) {
	suffix, ok := strings.CutPrefix(instance, prefix+"#")
	if !ok {
		return 0, false
	}
	element, err := strconv.Atoi(suffix)
	if err != nil {
		return 0, false
	}
	return element, true
}

// parkedByRejectedHold reports whether a step is parked BECAUSE its hold was
// rejected, and names the held step that carries the rejection.
//
// It is the read `retry` needs to refuse honestly. `heldDecision` answers
// "resolved, and approved?" for the SAGA, which routes on it; this answers
// "is a rejection what is holding this step down?" for the OPERATOR, who needs
// to know that resetting attempts cannot help.
//
// The predicate is deliberately NARROW. It requires all three of:
//
//   - the step is parked (`waiting-human`) — a step in any other status is not
//     being blocked by anything and `retry` is free to reset it;
//   - a materialized held step EXISTS for this ordinal — a plain gate failure
//     or a bad exit code parks without any hold involved, and retry is exactly
//     the right remedy there;
//   - that held step is TERMINAL and NOT approved — the sticky verdict.
//
// Anything less would refuse `retry` on ordinary parks, which would be a
// worse bug than the one being fixed: it would remove the least destructive
// resolution from the cases where it genuinely works.
func parkedByRejectedHold(conn *sql.DB, step *db.Step) (rejected bool, heldInstance string, err error) {
	if step.Status != db.StepWaitingHuman {
		return false, "", nil
	}

	// A hold spans one step PER CLUSTER, so this scans them all and
	// reports the FIRST rejected one. Any single rejection is what routed the
	// step per `on_fail`, and it is equally sticky however many siblings were
	// approved — re-running the aggregate re-reads the same rejection.
	//
	// The bare whole-hold instance is still matched so a hold materialized
	// before per-cluster steps existed is read correctly.
	rows, err := heldClusterRows(conn, step)
	if err != nil {
		return false, "", err
	}

	for _, r := range rows {
		if !db.StepTerminal(r.status) {
			// The question is still open — the operator can still answer it,
			// and `approve` is the verb that does.
			continue
		}
		if r.status != db.StepDone || !routingIs(r.routing.String, RoutingPass) {
			return true, r.instance, nil
		}
	}

	// Either no hold was ever materialized for this ordinal, or every cluster
	// was approved. Whatever parked this step, it was not a rejected hold.
	return false, "", nil
}

// heldResolution is the CONTENT of one approve decision, carried into the
// resolved payload (DKT-84): the cluster it answers, the operator's note, and
// — when the operator corrected the aggregated value (DKT-42) — the validated
// value and the field it lands on.
//
// Element is the payload index the decided step names; a negative Element
// means the decision predates per-cluster steps (a legacy whole-hold
// instance) and applies to every approved cluster.
type heldResolution struct {
	Element int
	Note    string
	Value   string
	Field   string
}

// applies reports whether this decision's content lands on payload element i.
func (r heldResolution) applies(i int) bool {
	return r.Element < 0 || r.Element == i
}

// resolveHeldPayload is §7.7.3's approve half: a NEW artifact of the same kind
// on the ROUTING step, whose payload is the held one with every
// previously-held element carrying `"operator_resolved": true`.
//
// ARTIFACTS STAY IMMUTABLE (engine-core §1.4). Approval records a new artifact
// rather than annotating the old one, which is the same rule a step re-run
// follows: what the engine computed and what the operator accepted are two
// records, not one overwritten one. The held payload remains addressable
// forever.
//
// THE DECISION'S CONTENT TRAVELS WITH THE FLAG (DKT-84). RUN-5's fixer
// received a cluster whose `alternative` offered two competing options,
// `operator_resolved: true`, and no way to learn which one the operator chose
// — the note that said so was recorded on the held step and never routed. The
// approved element now carries `operator_note`, and a corrected value lands on
// the aggregated field itself with the computed value it replaced in
// `operator_set_from` — so a boolean is never the whole message.
func resolveHeldPayload(
	tx *sql.Tx, routingStep *db.Step, res heldResolution, nowMS int64,
) error {
	var (
		priorID int
		kind    string
		payload sql.NullString
	)
	err := tx.QueryRow(
		`SELECT id, kind, payload FROM artifacts
		  WHERE step_id = ? ORDER BY id DESC LIMIT 1`, routingStep.ID,
	).Scan(&priorID, &kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing to resolve. The routing step will route over whatever it has,
		// which is the honest outcome rather than a fabricated artifact.
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the held payload for %s: %w", routingStep.Instance, err)
	}

	elements, err := parsePayload([]byte(payload.String))
	if err != nil {
		return err
	}

	// PER-CLUSTER: only the clusters whose own step was APPROVED are
	// marked resolved. Previously every held element was stamped whatever the
	// single decision was, so a hold over four clusters recorded one answer
	// four times.
	approvedClusters, err := heldClusterElements(tx, routingStep)
	if err != nil {
		return err
	}

	resolved := false
	for index, element := range elements {
		if held, _ := element[KeyHeld].(bool); !held {
			continue
		}
		// A cluster whose step was rejected keeps `operator_resolved: false`.
		// The rejection routes the step per `on_fail`; recording it as
		// resolved would claim an acceptance that did not happen.
		if !approvedClusters[index] {
			continue
		}
		element[KeyOperatorResolved] = true
		resolved = true

		// The CURRENT decision's content lands on its own cluster; clusters
		// approved by earlier decisions keep the content those decisions wrote
		// (this read starts from the latest artifact, so it is still there).
		if !res.applies(index) {
			continue
		}
		if res.Note != "" {
			element[KeyOperatorNote] = res.Note
		}
		// A corrected value replaces the computed one ON THE FIELD ITSELF, so
		// every threshold and downstream input routes on the number the
		// operator actually endorsed (DKT-42). The value was validated against
		// the schema's declared enum before this transaction opened — never
		// parsed out of the note: refusing to infer a value from prose was
		// always right, and it argued for a STRUCTURED field, not for no field.
		if res.Value != "" && res.Field != "" {
			if prior, ok := element[res.Field]; ok && prior != any(res.Value) {
				element[KeyOperatorSetFrom] = prior
			}
			element[res.Field] = res.Value
		}
	}
	if !resolved {
		return nil
	}

	encoded, err := json.Marshal(elements)
	if err != nil {
		return fmt.Errorf("encoding the resolved payload: %w", err)
	}
	// SUPERSEDES the artifact it was computed from (v15, DKT-70). H13's
	// two-record trail is deliberate and stays; the pointer says which of the
	// two records a computation and which records a decision.
	//
	// The BODY IS REGENERATED from the resolved payload (DKT-112): the prior
	// row's body still counts the cluster this decision just resolved as "held
	// for an operator decision", and carrying it forward recorded a stale
	// sentence as the newest fact about the step. The hash covers body and
	// payload both, so two links of a supersession chain can never share a
	// content address while their payloads differ.
	var body string
	stillHeld, operatorResolved := 0, 0
	for _, element := range elements {
		if held, _ := element[KeyHeld].(bool); !held {
			continue
		}
		if done, _ := element[KeyOperatorResolved].(bool); done {
			operatorResolved++
		} else {
			stillHeld++
		}
	}
	// The recorded count is 0 on purpose: this body is regenerated from the
	// RESOLVED PAYLOAD, and `route_at`'s below-floor clusters were never in
	// it — they live in the aggregate's own `action_results` row, which this
	// supersession does not touch.
	body = aggregateBody(routingStep.Instance, len(elements), stillHeld, 0)
	if operatorResolved > 0 {
		body += fmt.Sprintf(", %d operator-resolved", operatorResolved)
	}
	_, err = db.InsertArtifactTx(tx, db.Artifact{
		RunID: routingStep.RunID, StepID: routingStep.ID, Kind: kind,
		Body:       body,
		Payload:    string(encoded),
		SHA256:     artifactSHA256([]byte(body), encoded),
		Supersedes: &priorID,
	}, nowMS)
	return err
}

// routingStepOf returns the step a materialized `<step>-held@k` belongs to.
func routingStepOf(conn *sql.DB, held *db.Step) (*db.Step, error) {
	name, ok := workflow.RoutingStepNameOf(held.StepName)
	if !ok {
		return nil, validationErr("step %s is not a materialized held step", held.Instance)
	}
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps
		  WHERE run_id = ? AND issue_id = ? AND step_name = ? AND ordinal = ?`,
		held.RunID, held.IssueID, name, held.Ordinal).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundErr(db.ErrStepNotFound,
			"step %s has no routing step %s@%d to resolve",
			held.Instance, name, held.Ordinal)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the routing step for %s: %w", held.Instance, err)
	}
	return db.GetStep(conn, id)
}

// decideMaterializedStep is §7.7.3 — M-c's branch, and the only place
// `approve`/`reject` resolve ANOTHER step's saga.
//
// Both verbs are TOKEN-FREE (H15), per §2: a human gate is resolved by an
// operator who never claimed it.
func (e *Engine) decideMaterializedStep(
	conn *sql.DB, held *db.Step, approve bool, note, value string, nowMS int64,
) error {
	routingStep, err := routingStepOf(conn, held)
	if err != nil {
		return err
	}

	// `--value` accompanies APPROVE: a correction is an acceptance of the
	// cluster at a different number, while reject is the escalating answer and
	// records no artifact for the cluster at all.
	if value != "" && !approve {
		return validationErr(
			"step %s: --value accompanies approve; reject records no artifact "+
				"for the cluster, so there is no value to set", held.Instance)
	}

	// The correction is VALIDATED against the pinned schema's declared enum
	// before anything is written (DKT-42) — a member of the membership set the
	// run agreed to, never a value parsed out of prose.
	res := heldResolution{Element: -1, Note: note, Value: value}
	if element, ok := heldClusterElementOf(held.Instance, heldStepInstance(routingStep)); ok {
		res.Element = element
	}
	if value != "" {
		field, err := heldValueField(conn, routingStep, value)
		if err != nil {
			return err
		}
		res.Field = field
	}

	// H16: approve/reject on a materialized step whose routing step is NOT in
	// `held` — a double approve, a resumed race — is CONFLICT naming both
	// steps. The saga advance below is CAS-guarded on the same stage, so the
	// loser writes nothing even if it gets past this read.
	if routingStep.SagaStage != db.SagaHeld {
		return conflictErr(
			"step %s cannot be decided: its routing step %s is not holding "+
				"(its saga is at %q). The decision it asked for has already been "+
				"made, or the loop moved past it",
			held.Instance, routingStep.Instance, saganame(routingStep.SagaStage))
	}
	if db.StepTerminal(held.Status) {
		return conflictErr("step %s is already %s", held.Instance, held.Status)
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the held decision: %w", err)
	}
	defer tx.Rollback()

	// H14: the materialized step ends `done` on BOTH approve and reject — it
	// recorded a decision, which is what a gate does. The CONSEQUENCE lands on
	// the routing step, which is also why V13's rule is not violated when that
	// step's `on_fail` is `waiting-human`: the park is on a DIFFERENT step,
	// resolvable by `step resolve`, not a step parking on its own decision.
	routing := RoutingPass
	event := EventStepApproved
	if !approve {
		routing = heldRejectRouting
		event = EventStepRejected
	}
	// THIS DECISION IS WRITTEN BEFORE THE PAYLOAD IS RESOLVED.
	// resolveHeldPayload now reads each cluster step's OWN status to decide
	// which elements to mark, so this step must already carry its verdict in
	// the transaction or the cluster being approved reads as undecided and
	// nothing is marked. Under the previous whole-hold shape the order did not
	// matter, because approval was inferred from the caller rather than read
	// back.
	if err := db.SetStepRoutingTx(
		tx, held.ID, routingRecord(routing, note), db.StepDone, nowMS); err != nil {
		return err
	}
	if approve {
		// AFTER the routing above, so this cluster's own verdict is visible to
		// the per-cluster read. Still the same transaction: §7.7.3's rule that
		// approval records its artifact atomically with the decision is
		// unchanged, only the order within the transaction moved.
		if err := resolveHeldPayload(tx, routingStep, res, nowMS); err != nil {
			return err
		}
	}
	// A corrected value rides in the event beside the note, so the feed's
	// account of the decision carries what was decided, not only that a
	// decision happened.
	data := note
	if value != "" {
		encoded, err := json.Marshal(map[string]string{"note": note, "value": value})
		if err != nil {
			return fmt.Errorf("recording the held decision: %w", err)
		}
		data = string(encoded)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: event, RunID: held.RunID,
		Instance: held.Instance, IssueID: held.IssueID, Data: data, AtMS: nowMS,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the held decision: %w", err)
	}

	// The routing step's saga resumes from `held` and routes. It is a SEPARATE
	// transaction on purpose: the routing stage owns its own four-table commit
	// (§2), and folding it into this one would put the decision and its
	// consequences in a transaction whose boundaries neither stage specifies.
	// A crash between the two leaves a resolved question and an unrouted step,
	// which the next `next`/`claim`/`complete` finishes — which is exactly what
	// lazy resume is for.
	return e.ResumeSaga(conn, routingStep.ID, nowMS)
}

// heldValueField validates an operator's corrected value (DKT-42) and returns
// the payload field it lands on: the routing step's declared `params.field`,
// checked for membership in the pinned schema's declared enum.
//
// Every refusal here is a VALIDATION_ERROR while the operator is still typing
// the decision — never a payload that fails downstream. The membership set is
// the AUTHOR'S: docket learns which values exist from the schema the run
// pinned and holds no opinion about what any of them means.
func heldValueField(conn *sql.DB, routingStep *db.Step, value string) (string, error) {
	defs, err := StepDefinitions(conn, routingStep.RunID)
	if err != nil {
		return "", err
	}
	// The ROUTING step is a declared aggregate, never a materialized one, so
	// the tally cannot reach this spec — passed zero rather than loaded, and
	// said here so the omission reads as a fact instead of a shortcut.
	spec := stepSpec(defs, routingStep, holdTally{})
	if spec == nil {
		return "", validationErr(
			"step %s has no pinned definition; --value has nothing to set",
			routingStep.Instance)
	}
	field, _ := spec.Params["field"].(string)
	if field == "" || spec.Payload == "" {
		return "", validationErr(
			"step %s declares no aggregated field with a payload schema; "+
				"--value applies to a hold produced by an aggregate step",
			routingStep.Instance)
	}

	registered, err := pinnedSchema(conn, routingStep.RunID, spec.Payload)
	if err != nil {
		return "", err
	}
	// Membership is answered WHERE THE VALUES LIVE (internal/schema), so the
	// engine never reads a declared enum's values — the same discipline that
	// keeps ordering behind Position.
	if err := registered.ValidateMember(field, value); err != nil {
		return "", validationErr("%v", err)
	}
	return field, nil
}

// heldRejectRouting is what a REJECTED materialized step records for itself.
//
// It is `reject` rather than the routing step's `on_fail`, because the two are
// different facts: this step's own outcome is "the operator declined", and the
// routing that follows belongs to the step the consequence lands on. Recording
// the parent's `on_fail` here would make `guard gate` read a rejected hold as
// having taken a routing it never took.
const heldRejectRouting = "reject"

// saganame renders a saga stage for a message, naming the completed case.
func saganame(stage string) string {
	if stage == "" {
		return "complete"
	}
	return stage
}

// materializedSpec resolves a step's spec, synthesizing one for a materialized
// held step (H5).
//
// Every `spec == nil` site routes through this instead of erroring, because a
// materialized step is REAL — readiness asks about it, the saga's resume asks
// about it, and `approve` asks about it — and it is not in the pinned
// definition by construction.
//
// THE STEP ROW'S OWN KIND DECIDES WHICH FORM IS SYNTHESIZED, never the config
// `tally` carries. Config supplies the roster and the rule; the row supplies
// the question's type, which was fixed when the hold was minted. That split is
// what makes a mid-run config change survivable in both directions: turning a
// tally ON leaves already-minted `human` holds answerable by approve/reject,
// and turning it OFF leaves an already-minted vote hold a vote step — which
// then refuses loudly at the tally (resolveVoteRule names the missing rule)
// rather than silently becoming a question with no verb able to answer it.
func materializedSpec(
	def *workflow.Definition, step *db.Step, tally holdTally,
) *workflow.Step {
	if spec := workflow.StepByName(def, step.StepName); spec != nil {
		return spec
	}
	if !step.Materialized {
		return nil
	}
	if step.Kind == workflow.TypeVote {
		return workflow.MaterializedHeldVoteStep(
			def, step.StepName, tally.rule, tally.voters)
	}
	return workflow.MaterializedHeldStep(def, step.StepName)
}

// HeldResolutionStaleTargets is DKT-414's resolve-time advisory: the SAME
// recorded-target-vs-shared-HEAD ancestry check `dispatch open` runs (DKT-193),
// asked at the moment a materialized held step's resolution commits — because
// that is when the check can still change what happens next.
//
// The gap it closes: under the staged closure, the downstream verify/review
// rows are ALREADY in an open dispatch when an operator resolves a held
// reconcile, so no `dispatch open` runs between the resolution and their
// execution. RUN-26/FLX-141 resolved a held cluster while the shared branch
// HEAD had moved off the recorded target sha; verify and two 3-seat panels
// then consumed packets rendered from a tree the branch no longer carried,
// rejected 3-0, and each seat re-derived by git forensics the divergence the
// engine already knew — the next round's `dispatch open` named exactly it,
// one phase too late to save the ~3-4 cost units already burned.
//
// It REUSES DKT-193's two primitives rather than forking either half:
// staleTargetCandidates collects (the steps the resolution just un-blocked,
// judged over the run's current ready set), and staleTargets judges the shas
// against the shared checkout — so its answer is, by construction, the same
// rows with the same reason the next `dispatch open` would emit. That shared
// judge is also why DKT-424's known false positive (a cherry-pick integration
// mints a new sha, so an ancestry test fails while the TREE is identical)
// reproduces here verbatim — and why its fix, landing in staleTargets, heals
// this advisory with no change needed here.
//
// ADVISORY, NEVER A REFUSAL, exactly stale_targets'/pin_drift's posture: the
// resolution this rides on has already committed, completed steps keep their
// recorded provenance untouched, and a false positive that merely warns costs
// far less than one that blocks. For the same reason every failure inside —
// an unreadable step, a snapshot that cannot load — returns nil rather than
// an error: absence of evidence is not staleness, and a divergence probe must
// not turn a resolution that succeeded into a verb that failed.
//
// nil for any step that is not materialized, so the ordinary resolve/approve
// paths on declared steps stay exactly as they were. A resolution that does
// NOT end the hold (sibling clusters still open, `--as retry`) finds the
// routing step still gated, no downstream step ready, and warns about
// nothing — the advisory fires when packets actually become consumable.
func (e *Engine) HeldResolutionStaleTargets(
	conn *sql.DB, heldStepID int, nowMS int64,
) []StaleTarget {
	if e == nil || e.HeadFn == nil || e.IsAncestorFn == nil {
		return nil
	}
	held, err := db.GetStep(conn, heldStepID)
	if err != nil || !held.Materialized {
		return nil
	}
	defs, err := StepDefinitions(conn, held.RunID)
	if err != nil {
		return nil
	}
	candidates, err := readyStaleTargetCandidates(conn, held.RunID, defs, nowMS)
	if err != nil {
		return nil
	}
	return e.staleTargets(conn, held.RunID, candidates)
}

// readyStaleTargetCandidates collects DKT-193 target candidates over a run's
// CURRENT ready set: one read-only snapshot, rolled back unconditionally, with
// the collection inside it (resolution is snapshot reads) and the git question
// left to the caller — the same inside/outside split OpenDispatch keeps.
//
// Readiness is the scheduler's own predicate, not a re-derivation: the steps a
// held resolution un-blocks are exactly the ones that just became ready, and
// answering with sched.Ready keeps this advisory's "which rows" aligned with
// the offer verbs' rather than a second definition that could drift.
func readyStaleTargetCandidates(
	conn *sql.DB, runID int, defs map[int]*workflow.Definition, nowMS int64,
) ([]targetCandidate, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("reading the post-resolution ready set: %w", err)
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	var ready []*db.Step
	for _, step := range sched.Steps() {
		if ok, _ := sched.Ready(step); ok {
			ready = append(ready, step)
		}
	}
	return staleTargetCandidates(tx, sched, runID, ready)
}

// heldStepsFor lists the materialized held steps of one issue below an ordinal,
// for the loop's supersede sweep (H17).
//
// The sweep walks instance NAMES against the `after_loop` downstream set, and a
// materialized name is not in the definition — so it is mapped back to the
// routing step it belongs to and swept exactly as that step's other downstream
// instances are. An open held question from a superseded ordinal must not
// survive to block ordinal k+1.
func heldStepBelongsToDownstream(name string, downstream map[string]bool) bool {
	routing, ok := workflow.RoutingStepNameOf(name)
	if !ok {
		return false
	}
	return downstream[routing]
}
