# Run-scoped review over a run's integrated diff

Status: proposed — 2026-10-09 · design note settling the shape of a review step
that reads the integrated diff of a run's issues once they land on the shared
branch. Every file:line citation below is read at engine commit 3c73f103. The
decisions are proposed, not yet implemented; the spec amendments they require
are collected in §10 and go through docs/design/amendments.md's route.

## 1. Situation

The engine reviews one issue's diff per review step. `issue.diff` resolves per
issue (internal/engine/context.go:1594, `resolveIssueDiff`: "the HIGHEST-ORDINAL,
then highest-id `done`-attributed `issue.diff` artifact for the issue"), every
step row belongs to an issue (internal/engine/activate.go:1917, `expandIssue`
writes `IssueID: issue.ID` on every row), and the run rolls up to `done` the
moment every step is terminal (internal/engine/reconcile.go:583, `reconcileRun`).
Nothing reads the tree the run's issues produced together. `dispatch close`
verifies that each write-class commit reached the shared branch
(internal/engine/dispatch_integration.go:352, `integrationVerdict`), which proves
presence, not correctness: two issues whose diffs each passed review can still
leave the integrated tree broken, and a hand-resolved cherry-pick carries content
no issue's recorded diff ever showed a reviewer.

docs/design/engine-core.md §1.3 already names the shape this note fills in: a step
belongs "to an issue (or to the run itself for run-level steps like planning and
final review)" (line 63). The storage admits it, `steps.issue_id` is nullable
(internal/db/schema.go:563), but no code path writes or reads such a row today.

## 2. Decisions

| # | Question | Decision |
|---|---|---|
| D1 | Workflow-expansion shape | A run-scoped workflow definition: `[pipeline] scope = "run"`. Its steps use the existing `[[step]]` grammar, expand once per run at activation with `issue_id` NULL, and become ready only after every issue of the run is terminal and no dispatch is open (readiness clause R2c). Not a per-step flag inside issue workflows, not an opt-in phase assembled from several bound workflows. |
| D2 | `run.diff` derivation | A fresh read of the integration branch, recorded as a `run.diff` artifact at the claim of each run-scoped step that declares the input: `GitDiff(run.exec_root, run.commit_sha, union of the run's issue scopes)`, with the round record `{base, head, sha256}`. Never the union of issue diffs. |
| D3 | Cross-issue loop entry | No issue's bound governs. A run-scoped step's `fix-loop` enters a run-scoped loop cluster: the counter is the run's (`runs.loop_count`), the bound is the run-scoped definition's own `max_fix_loops` read by the existing rule, and the body is the run-scoped definition's own `loop = true` step. Issue counters never move and completed issues stay `done`. Until the cluster lands (phase 3), `fix-loop` is refused at register time on a run-scoped definition (V48b), so a rejection routes `waiting-human`. |
| D4 | Register-time lint | V48: `[pipeline] scope`, when present, is `"issue"` or `"run"`, and a definition whose scope is `issue` has no step whose `inputs` names `run.diff`. V48a: a definition whose scope is `run` declares no `[match]` table, no step `when`, and no step input of an `issue.*` form. V48b (phase 1 and 2 only): a definition whose scope is `run` has no `loop = true` step, no `threshold`, `on_fail`, or `on_fail_routes` value equal to `fix-loop` or `fix-round`, and no step declaring `on_exhausted` or `serves`. |
| D5 | Activation with no integration point | Activation is refused (`VALIDATION_ERROR`, inside the fat transaction, nothing written) when a run-scoped definition binds, some step of it declares `run.diff` or holds the tree, and the run's `exec_root` or `commit_sha` is empty. The refusal names the run, the definition, the empty field, and the two remedies: start the run from inside the checkout, or retire the run-scoped definition with `workflow deprecate`. |

## 3. Design

### 3.1 D1: a run-scoped workflow definition

A workflow definition gains one optional `[pipeline]` key, `scope`, with values
`"issue"` (the default, and the meaning every existing definition has) and
`"run"`. The field is `omitempty` in the canonical form, so every definition that
never declares it serializes byte for byte as it does today; the repo treats a
canonical-form change on re-register as a CONFLICT (internal/workflow/parse.go:139-144
states the rule for `roster`; internal/workflow/canonical.go:19-25 states why).

Binding. Issue workflows bind per issue through `[match]` under the exactly-one
rule (internal/engine/activate.go:1332, `bindIssue`). A run-scoped definition has
no `[match]` (V48a) and binds per run: at the first activation, among the
bindable (non-retired) definitions whose scope is `run`, zero names means the run
has no run phase; exactly one name binds its highest version; two or more names
is a `VALIDATION_ERROR` listing them, the exactly-one rule's analog. The binding
is pinned with the issue workflows and inherited by re-activations (activate.go's
RA2 comment at line 603 and RA3 comment at line 665).

Expansion. `workflow.Expand` runs once for the bound run-scoped definition with an
empty `Subject`, at ordinal 0, in the same fat transaction that expands the
phase-one issues, and the rows are written with `issue_id` NULL. `Expand` stays
"a pure function of (issue kind, labels, pipeline definitions @ pinned version)"
(internal/workflow/expand.go:53-56 quoting engine-core §2); a run-scoped
definition has no `when` (V48a), so the empty subject never reaches `WhenHolds`
(internal/workflow/match.go:168). Eager expansion over lazy: the rows exist from
activation, `reconcileRun`'s `unfinished` count (reconcile.go:591-600) keeps the
run open for them with no new rollup rule, and `run show` lists the phase before
it opens.

Row identity. Three storage facts decide the implementation:

- `db.Step.IssueID` is `int` and `scanSteps` scans `issue_id` straight into it
  (internal/db/steps.go:424), so a NULL row fails every step read at HEAD. The
  scan moves to `sql.NullInt64`; `IssueID == 0` is the run-scoped reading
  everywhere a step's issue is consulted.
- `InsertStepTx` writes `s.IssueID` directly (internal/db/runs.go:826-838); a zero
  must become NULL, or the `REFERENCES issues(id)` constraint refuses the row.
- `UNIQUE(run_id, issue_id, instance)` (schema.go:597) does not dedupe rows with a
  NULL `issue_id`, since SQLite treats NULLs as distinct in a unique constraint. A
  partial unique index `ON steps(run_id, instance) WHERE issue_id IS NULL` is
  added by migration so a double expansion is refused by the database, not by
  convention.

Readiness. `Ready` reads `s.issues[step.IssueID]` (internal/engine/ready.go:615);
for a run-scoped row `facts` is nil, so R2 and R2b pass vacuously (ready.go:617-627)
and `scopeConflict` returns false before it looks at any holder
(ready.go:1084-1088). `predecessorsDone` is per issue (ready.go:757). A run-scoped
row therefore needs its own clause, R2c, evaluated where R2/R2b are for an issue
row: the step is ready only when every `run_issues` row is expanded, every step
with a non-NULL `issue_id` is terminal (`done`, `skipped`, `superseded`,
`failed-routed`, the set `issueStepsComplete` uses at reconcile.go:437-438), no
step of the run is parked `waiting-human`, and `db.OpenDispatchTx`
(internal/db/dispatch.go:180) finds no open dispatch. Every conjunct is a table
read inside the scheduler's snapshot; none is a git probe. The open-dispatch
conjunct is what makes "post-integration" true in the sanctioned flow: a dispatch
closes only after `integrationVerdict` accepted or vouched for every write-class
head, so a run that uses dispatch cannot open its review before the shared branch
carries the work. A run driven without dispatch has no close to wait for and
opens the phase when its issues finish. R3 through R7 apply unchanged; R4 for a
tree-holding run-scoped step compares the union of the run's issue scopes, read
through the same `loadIssueFacts` path (ready.go:389).

Packet shape. `contextIssue` (context.go:652) reads `run_issues` and errors for a
missing row; a run-scoped assembly skips it and the bundle carries no `issue`
member. In its place the bundle gains `run?: {id, request}` from `runs.request`,
present only on run-scoped rows so every existing bundle is byte-identical.
`next` rows carry `issue` as a string (internal/engine/next.go:607); it is empty
on a run-scoped row, which is the one wire-shape consequence a dispatcher keyed
on the field must tolerate. The staged-ready ordering in
internal/engine/stage.go:79 (`precedesInSet`) requires equal `IssueID`; run-scoped
rows all read 0 and so order among themselves, which is the fixer-then-judges
ordering the run-scoped cluster (D3) needs.

Alternatives weighed.

| Candidate | Why it lost |
|---|---|
| A per-step `scope = "run"` flag inside ordinary issue workflows | A run pins several workflows, so N bound definitions can each declare the step: either N reviews run or a dedupe rule must say whose wins. `after` edges would cross the issue/run boundary, and `Expand`'s per-issue purity contract would have to carve out a once-per-run row. |
| A post-integration phase every issue workflow opts into, assembled by the engine | Needs a definition for the phase's steps anyway, plus an opt-in flag per issue workflow and a merge rule. The opt-in half is deferred (§7) and can be added to D1 later without changing its shape. |
| Keep reviewing per issue and rely on `dispatch close`'s integration check | The check proves commits are present, not that the integrated tree is correct, and a hand-resolved cherry-pick is content no reviewer saw. This is the gap the request names; doing nothing leaves it. |

### 3.2 D2: `run.diff` is a fresh integration-branch read, recorded at claim

Form. `run.diff` is a new engine-produced input form beside `issue.diff` and a new
engine-recorded artifact kind of the same name (context.go:247-258 is where the
constants live). Body: `GitDiff(dir, base, scope)` (internal/engine/saga.go:2510)
with `dir` the run's exec root, `base` the run's pinned starting commit, and
`scope` the union of the run's issues' `scope_globs`. Every property `issue.diff`
bodies have carries over unchanged because it is the same function: untracked
files appear, out-of-scope hunks follow under the marked heading, and an empty
union diffs the whole tree (`rawDiff`, saga.go:2681, "An empty pathspec diffs
the whole tree"). Payload: `{"base": <commit_sha>, "head": <shared HEAD at
claim>, "sha256": <body hash>}`.

Base and tip. `runs.commit_sha` is pinned once at `run start` from the exec root's
HEAD (internal/cli/run_start.go:161-164 via `config.GitHead`); `runDiffBase`
(saga.go:2749) already names it as "the run's PINNED starting commit" for
shared-checkout recordings. The tip is `sharedCheckoutHead(runExecRoot(conn,
runID))`, the read `ClaimStepRendered` already performs before its transaction
(internal/engine/claim.go:345-350) and records as `claim_head`. For a run diff the
pinned commit is the right base for exactly the reason it is the wrong base for a
single writer: everything integrated since the run began is the run's work.

Recording moment. Context assembly reads no live state and `issue.diff` D4 is
explicit that an absent artifact yields an empty diff, "never a live `git diff`"
(context.go:1591-1593); git never runs inside a transaction (`integrationVerdict`
rolls back before probing, dispatch_integration.go:369-372). So `run.diff` is a
recorded artifact, and the recording moment is the claim of a run-scoped step
whose definition declares the input: the claim computes the body before its
transaction opens, at the same point it reads `claim_head`, then inserts the
`run.diff` artifact attributed to the claiming step and binds it into
`step_inputs` inside the claim transaction. Each claim records its own; a fanout's
siblings therefore hold byte-identical artifacts when the tree stood still and
honestly different ones, with differing `head`, when it did not. That is the
property `AssembleRecordedContext` promises (context.go:296-301, "byte-identical
inputs, however far the run has moved"): the packet a step read is the packet
the ledger says it read.

Resolution. A declared `run.diff` resolves to the newest `run.diff` artifact
attributed to a run-scoped step at ordinal at or below the consumer's, highest id
within an ordinal. For the claiming step that is the artifact its own claim just
wrote. A step downstream of the review at the same ordinal (a synthesize step
that declares the input) resolves the review's record rather than taking a new
read, so one round reads one object. With no artifact the input resolves empty,
the D4 rule, which happens only for a definition that declares `run.diff` on a
step whose claim could not run git (the activation refusal in D5 prevents the
common case).

Tree-holding run-scoped steps record a `run.diff` at completion as well, exactly
as `computeIssueDiff` records `issue.diff` for a tree-holding issue step
(saga.go:1646-1685), with the round delta appended by `appendRoundDelta`
(saga.go:3392) from the previous round's recorded head. That record is what the
non-convergence check and `dispatch close`'s candidate collection read in D3.

Alternatives weighed.

| Candidate | Why it lost |
|---|---|
| Union of issue diffs: concatenate each issue's D3-resolved `issue.diff` body | Each body is filtered to one issue's scope and computed against a different base (a worktree's fork point, or the claim head for a shared-checkout writer, saga.go:2749-2775). Concatenating them is not a patch anyone could apply, double-renders files two issues touched, and omits what a hand-resolved cherry-pick changed. It is cheaper and needs no git read, which is its only strength. |
| Record `run.diff` at `dispatch close`, inside `integrationVerdict`'s existing git moment | Binds the review to the dispatch flow; a fixture-driven run or a `next`-driven harness never closes a dispatch, so the input would never exist for them. The claim moment exists for every driver. |
| Record `run.diff` once per ordinal, deduped across sibling claims | Needs a lock around compute-then-insert across processes, or a tolerance for two siblings reading different trees under one recorded object. Per-claim recording keeps the record truthful without a lock; the cost is one artifact per sibling. |

### 3.3 D3: a rejection enters a run-scoped loop; no issue's bound governs

The rule. When a run-scoped step's routing resolves to `fix-loop`, the loop that
opens is the run-scoped definition's own: its `loop = true` bodies instantiate at
run ordinal k (clause 3), its `after_loop` roots and their downstream chain
re-instantiate at k (clause 4), and the unclaimed downstream instances below k are
superseded (clause 2). Clause 1's counter is a new `runs.loop_count` column with
the increment-first semantics `run_issues.loop_count` has today (schema.go:698;
internal/engine/loop.go:1359 `restoreLoopCount`), and the bound is `maxFixLoops`
(loop.go:847) applied to the run-scoped definition, so the ceiling is read off
whichever non-cluster step of that definition declares `max_fix_loops`, exactly as
for an issue. `fix-round` grants, `on_exhausted`, the three loop-history facts, and
the non-convergence refusal apply unchanged, the last over consecutive `run.diff`
sha256 values instead of `issue.diff` ones. No `run_issues.loop_count` moves, no
issue step is superseded or instantiated, and `completeIssue` (reconcile.go:457)
is never reversed: the issues are done; the run is not.

Who fixes. The run-scoped body is a tree-holding executor of the run-scoped
definition. It works in the run's exec root (shared-checkout writers are already
supported, claim.go:345-350) or in a worktree of it, under R4 with the union
scope, and records a `run.diff` with a `head` at completion. `dispatch close`'s
candidate collection (dispatch_integration.go:135-187) adds the `run.diff` kind
beside `issue.diff` so a worktree-recorded run-scoped fix is integration-checked
like any other writer; a shared-checkout fix's head is the branch tip and passes
by ancestry.

Spec consequence. engine-spec.md §11.3 ends "There is no other loop construct"
(line 747) and clause 1 reads "the issue's loop counter increments". The
construct here is the same one, all four clauses with the same grammar
(`loop`, `after_loop`, `serves`, `max_fix_loops`, `on_exhausted`), applied to a
definition whose subject is the run. The wording still needs the amendment in
§10 so that "the issue's counter" reads as "the subject's counter: the issue's
for an issue-scoped definition, the run's for a run-scoped one". This note
proposes that amendment; it does not assume it.

Phasing. The cluster is phase 3 (§6). Until it lands, V48b refuses `fix-loop` and
`loop = true` on a run-scoped definition at register time, so a phase-1 or
phase-2 run-scoped review can route `pass`, `waiting-human`, or interpose a named
step, and a rejection parks `waiting-human` with the findings beside it. The
closed vocabulary's own answer applies in the interval (engine-core §4: "If a
situation isn't expressible, the answer is `waiting-human`, not cleverness").

Alternatives weighed.

| Candidate | Why it lost |
|---|---|
| Re-enter each named issue's loop under that issue's own counter and its bound workflow's `max_fix_loops` | Three defects. The issue is `done` (`completeIssue`, reconcile.go:457-475) and nothing in the engine moves a done issue back; `reflectIssueStatus` (reconcile.go:268) only advances `in_progress` to `review`. Core would have to learn which issues a rejection names, either by reading payload content, which genericity.md forbids, or by re-opening every issue whose scope the diff touches, which re-runs N per-issue review chains over N per-issue diffs for a defect that lives in their intersection. And the run-scoped review itself must run again after the fixes, which is a run-scoped re-instantiation in any case, so this candidate needs the run-level construct and the issue-level reopen both. The question "whose bound governs a rejection spanning several issues" has no answer under it that is not invented: the smallest bound, the largest, or the first-declared are all arbitrary. |
| No loop: a run-scoped rejection routes `waiting-human` only, and follow-up issues carry the fixes | This is phase 1 and 2's behavior, kept deliberately until the cluster lands. As the permanent answer it leaves the integrated tree's known defects to an operator on every run, which is the hand process the request asks to replace. |

### 3.4 D4: V48 and its sub-rules

The rule table (internal/workflow/validate.go:27-38, `RuleIDs`) ends at V47 and
`TestValidationTableIsComplete` asserts set equality over it, so each new rule
below is added to the table and to internal/workflow/validate_test.go together.
V12 is deliberately absent and is not reused.

V48, on `[pipeline] scope` and issue-side inputs. The definition-level condition:
`pipeline.scope` is unset, `"issue"`, or `"run"`; and when the effective scope is
`issue`, no `[[step]]` has an `inputs` entry equal to `run.diff`. Refused with
`Rule: "V48"`, `Field: "pipeline.scope"` for the value, or `Step`, `Field:
"inputs"` for the input, with the message naming the form and the scope it
requires. This is V11's own reasoning (validate.go:1183-1200, the engine-form
vocabulary) carried to a form that resolves only against a run: on an issue-bound
step it would resolve to nothing on every run.

V48a, on a run-scoped definition's issue-only fields. The condition, when the
effective scope is `run`: the definition has no `[match]` table; no step declares
`when`; and no step has an `inputs` entry equal to `issue.body`, `issue.diff`,
`issue.files`, or beginning `issue.latest.` or `issue.linked.`. Refused with
`Rule: "V48a"` and the offending field. Each named field reads an issue that a
run-scoped step does not have: `Matches` takes an issue subject (match.go:53),
`WhenHolds` reads kind and labels (match.go:168), and each `issue.*` form resolves
through `step.IssueID` (context.go:1594, 1202; `contextIssue` at 652).

V48b, the phase gate. The condition, when the effective scope is `run`: no step
has `loop = true`; no `threshold`, `on_fail`, or `on_fail_routes` value is
`fix-loop` or `fix-round`; and no step declares `on_exhausted` or `serves`.
Refused with `Rule: "V48b"`. It is removed in phase 3 and its number is not
reused, the V12 convention.

Two existing rules change shape, not number: V11's engine-form list admits
`run.diff` (validate.go:1092, 1188, 1198), and L4's skip list does too
(internal/workflow/lint.go:236), since `run.diff` names no producer step.

Alternatives weighed. One combined V48 with three clauses was the other shape; the
sub-letter split follows V40/V40a/V40b/V40c and V21a-d so a test can falsify one
clause and the refusal can name it. A lint-stage L5 was rejected because every
condition is a predicate over the parsed fields alone and needs no graph.

### 3.5 D5: no eligible integration point refuses activation

An integration point is the run's recorded shared checkout and starting commit:
`runs.exec_root` and `runs.commit_sha`, "All empty on runs created before v12 or
outside a checkout" (internal/model/run.go:108-115). Without them `run.diff` has
neither a tree to read nor a base to read from, and a tree-holding run-scoped
body has no checkout to write.

The behavior: when a run-scoped definition binds and at least one of its steps
declares `run.diff` or holds the tree (`holds_tree` unset or true), and the run's
`exec_root` or `commit_sha` is empty, `activateTx` returns `validationErr` before
any row is written, so the fat transaction rolls back whole. The message names
the run, the definition as `name@version`, the empty field, and both remedies.
A run-scoped definition with neither a `run.diff` reader nor a tree holder (a
run-level human gate, say) binds and expands regardless; it needs no integration
point.

Precedent. `expandIssue` refuses an unpinned packet entry the same way
(activate.go:1934-1946: "Refusing at expansion, in the fat transaction, leaves
nothing behind"), and engine-spec §11.1's `issue.linked` row has activation fail
"loudly when the relation is missing or no linked issue holds the kind, so the
binding is enforced rather than an issue-body citation".

Alternatives weighed.

| Candidate | Why it lost |
|---|---|
| Expand the run-scoped steps `skipped` and report a `binding_warnings` entry, the `domain_paths` lint's warn-never-refuse stance | A silently absent integrated review is the failure class the step exists to close, and `run report` would show a completed run with no record that its final review never ran. The `domain_paths` lint warns because a cross-domain binding may be deliberate; a run started outside a checkout cannot deliberately want a diff review. |
| A per-run opt-out flag on `run activate` | New surface for a case `workflow deprecate` already covers at the project level. Deferred (§7) until a project needs per-run selection. |

## 4. Paths the implementation touches

Every path not marked new exists at 3c73f103 (verified with `ls` in the checkout).

### Workflow expansion

- internal/workflow/parse.go: `Pipeline.Scope string` with `toml:"scope"
  json:"scope,omitempty"`.
- internal/workflow/expand.go: `Expand` is called with an empty `Subject` for a
  run-scoped definition; no change to its body, a doc-comment update on the
  subject contract.
- internal/workflow/canonical.go: no code change; the `omitempty` tag is what
  keeps existing canonical forms byte-identical, verified by a round-trip test.
- internal/workflow/expand_test.go: expansion of a run-scoped fixture.
- internal/engine/activate.go: run-scoped binding (zero/one/many), eager
  expansion with NULL `issue_id`, the D5 refusal.
- internal/engine/activate_test.go.
- internal/db/runs.go: `InsertStepTx` writes NULL for a zero `IssueID`.
- internal/db/steps.go: `scanSteps` reads `issue_id` through `sql.NullInt64`.
- internal/db/schema.go: migration adding the partial unique index on
  `steps(run_id, instance) WHERE issue_id IS NULL`, and `runs.loop_count` (phase 3).
- internal/engine/ready.go: clause R2c and the union-scope R4 read for run-scoped
  rows.
- internal/engine/ready_test.go.
- internal/engine/next.go: `issue` empty on run-scoped rows.

### Packet input resolution

- internal/engine/context.go: the `run.diff` input form and artifact kind
  constants, `resolveRunDiff` (new function in this file), the `run?` bundle
  member, and skipping `contextIssue` for run-scoped rows.
- internal/engine/claim.go: compute and record `run.diff` before and inside the
  claim transaction, beside the existing `claim_head` read.
- internal/engine/saga.go: `computeRunDiff` (new function in this file) reusing
  `GitDiff`, `runExecRoot`, and `appendRoundDelta` for tree-holding run-scoped
  steps.
- internal/engine/packet.go and internal/engine/render.go: render the run's
  request where the issue section renders today.
- internal/engine/context_test.go.
- docs/design/engine-spec.md: §11.1 inputs row and §11.4 `context` shape (the
  amendment text is in §10; the edit is the amendment's, not this note's).

### Fix-loop routing

- internal/engine/loop.go: `enterLoop` reads and restores the counter through
  the step's subject (issue or run); `maxFixLoops` over the run-scoped
  definition; `newestIssueDiffUpTo` gains a `run.diff` reading for the
  non-convergence check.
- internal/engine/loop_test.go.
- internal/engine/reconcile.go: `reconcileIssueAndRun` skips the issue-status
  mirror for a run-scoped step and still runs `reconcileRun`.
- internal/engine/ready.go: R2c is also what keeps a re-instantiated run-scoped
  chain unready until its same-ordinal body finishes, reusing the loop-body
  closure (`loopClosure`, ready.go:205).
- internal/engine/stage.go: no change; `precedesInSet` already orders run-scoped
  rows among themselves.
- internal/engine/dispatch_integration.go: `integrationCandidatesTx` admits the
  `run.diff` kind.
- internal/engine/event.go: no change; events already carry a NULL `issue_id`
  (`if e.IssueID != 0`, event.go:642).
- internal/db/schema.go: `runs.loop_count` (listed above).

### Lint

- internal/workflow/validate.go: `RuleIDs` gains V48, V48a, V48b; `validatePipeline`
  checks the scope value; `validateInputs` admits `run.diff` under scope `run` and
  refuses it under scope `issue`; a new `validateRunScope` applies V48a and V48b.
- internal/workflow/validate_test.go: one test per rule id, as
  `TestValidationTableIsComplete` requires.
- internal/workflow/lint.go: L4's engine-form skip list admits `run.diff`.
- internal/workflow/lint_test.go.

### Outside the four surfaces

- internal/engine/report.go and internal/cli/run_report.go: list the run-scoped
  steps and their artifacts under a run section rather than an issue section.
- internal/model/run.go: no change; `ExecRoot` and `CommitSHA` already exist.

## 5. What a run-scoped definition looks like

```toml
[pipeline]
name = "integrated-review"
version = 1
scope = "run"

[[step]]
name = "review"
fanout = ["judge-correctness", "judge-architecture"]
emits = "findings"
payload = "findings@13"
inputs = ["run.diff"]
holds_tree = false
gates = [{ name = "tests", pre = true }]

[[step]]
name = "reconcile"
action = "aggregate"
after = ["review"]
inputs = ["review.findings"]
params = { output = "aggregate" }
payload = "aggregate@1"
threshold = { "waiting-human" = "any(severity >= high)" }
```

Under phase 1 and 2 the `waiting-human` routing is the only non-pass outcome.
Phase 3 lets the same definition declare `threshold = { "fix-loop" = ... }`, a
`loop = true` fix body with `after_loop = "review"`, and `max_fix_loops`.

## 6. Implementation phases

Each phase is one issue with its own acceptance criteria; the split follows the
four surfaces plus storage, in dependency order.

Phase 1, grammar and lint. `[pipeline] scope`; V48, V48a, V48b; V11 and L4 admit
`run.diff`; canonical-form round trip. Acceptance: `workflow lint` on the §5
definition reports `new`; the same definition with `[match]` added is refused
naming V48a; an issue-scoped definition declaring `run.diff` is refused naming
V48; every definition in `internal/workflow/testdata` re-registers `unchanged`.

Phase 2, binding, storage, readiness, packet. NULL `issue_id` rows; partial
unique index; run-scoped binding and the D5 refusal; R2c; `run.diff` recorded at
claim; the `run?` bundle member. Acceptance: on a fixture run with two issues,
`next` offers no run-scoped row while any issue step is non-terminal or a
dispatch is open, and offers `review@0#0` and `review@0#1` once both hold; the
claimed bundle's first input is a `run.diff` whose payload `base` equals the
run's `commit_sha` and whose body equals `git diff <base> HEAD -- <union scope>`
plus the untracked trailer, byte for byte; a run started outside a checkout is
refused at activation with the D5 message and leaves zero step rows; every
existing bundle fixture is byte-identical.

Phase 3, run-scoped loop cluster. `runs.loop_count`; `enterLoop` by subject;
non-convergence over `run.diff`; `dispatch close` admits `run.diff` heads; V48b
removed. Acceptance: a `fix-loop` routing from `reconcile@0` instantiates `fix@1`
and supersedes nothing of any issue; `run_issues.loop_count` is unchanged for
every issue and every issue stays `done`; the second entry under `max_fix_loops
= 1` parks `waiting-human` naming `--as fix-round`; a round whose `run.diff`
sha256 equals the previous round's is refused as non-convergent.

Phase 4, reporting. `run report` and `run show` render the run phase. Acceptance:
the report lists each run-scoped step with its artifacts under a run section and
the integration check's `checked` list includes the run-scoped fix's head.

## 7. Non-goals

- Per-issue-workflow opt-in or opt-out of the integrated review, and `run.diff`
  over a subset of the run's issues. Deferred until a project has an issue
  workflow whose issues should be excluded; the owner is whoever files that
  need (unassigned), and D1's shape admits an opt-in key without change.
- Re-opening a `done` issue from a run-scoped rejection. Excluded by D3.
- More than one run-scoped definition bound to one run. Excluded by the
  exactly-one analog; a project composes one definition.
- A run-scoped step before integration (a plan-approval gate at run start).
  R2c makes run-scoped rows post-integration by construction; a pre-integration
  run step is a different readiness rule and a separate design.
- The reference instance's contracts and packet files for the run-scoped judges.
  Instance config lives outside this repository.
- A per-run opt-out flag on `run activate` (§3.5).

## 8. What gets worse

- A run's wall clock grows by the run phase, and the run cannot roll up `done`
  until it finishes; an operator who wants the old behavior retires the
  run-scoped definition.
- `run.diff` is a live read at claim. Two siblings claimed across a shared-branch
  commit read different trees; the artifacts say so through `head`, but the
  panel's verdicts then describe two objects. R2c excludes in-run writers at that
  point; an out-of-engine commit is the remaining source.
- `commit_sha` as base attributes every shared-branch commit since run start to
  the run, including commits from another run on the same branch or an
  operator's own. The body's out-of-scope section discloses paths outside the
  union scope, and nothing else distinguishes them; this is accepted.
- `db.Step.IssueID == 0` becomes a meaningful value. Every reader that formats
  `model.FormatID(step.IssueID)` for a run-scoped row prints a nonsense id until
  it is taught the case; phase 2's review has to grep for the pattern.
- The `next` row's `issue` field is empty on run-scoped rows. A dispatcher that
  indexes by issue must tolerate the empty string.
- Phase 3 adds a second loop subject, so `enterLoop`'s counter reads and writes
  branch on the subject. The branch is one accessor pair, but it is a second
  place a counter can be read wrong.

## 9. Premortem

Hypothetical failures after a quarter of use, each with its mitigation or
acceptance.

- The run phase never opens on dispatch-driven runs because a dispatch stays open
  after the last issue step records. Mitigation: R2c's open-dispatch conjunct is
  reported as its own `ReadyCondition` so `next --explain` names it, and the
  phase-2 acceptance test drives a dispatch to close before asserting the offer.
- Judges receive a 400 KB `run.diff` and the packet blows the context cap.
  Mitigation: `ContextSize` cannot count the body at expansion (it does not exist
  yet; the same uncapped position `issue.files` holds, expand.go:374-377), so the
  claim applies `context.error_bytes` to the assembled bundle and parks the step
  `waiting-human` naming the size rather than spawning. Accepted residual: a
  large integrated change needs an operator's decision.
- The panel finds a defect, routes `waiting-human` under phase 2, and the park sits
  for days because nothing names a remedy. Mitigation: the park reason carries
  the findings count and the two resolutions available at that phase
  (`override-pass`, `abandon`); phase 3 is scheduled before the first production
  workflow declares a run-scoped threshold.
- A run-scoped fix body under phase 3 commits to the shared branch while an
  operator is hand-editing the same checkout. Mitigation: the body runs under
  the tree lock every tree-declaring gate already takes (`RepoPaths.LockPath`,
  internal/engine/repopaths.go:28), and its recorded `head` is what the next
  review reads, so an interleaved operator commit shows up in the diff rather
  than vanishing.
- Someone backfills V12 or renumbers V48b after its removal. Mitigation: the
  `RuleIDs` comment records both as reserved, the way V12's does today.

## 10. Proposed spec amendments

To be filed through docs/design/amendments.md; this note does not edit the spec.

- engine-spec.md §11.1, `[pipeline]`: add `scope?` with values `"issue"` (default)
  and `"run"`; a run-scoped definition has no `[match]` and binds per run under
  the zero/one/many rule of §3.1.
- engine-spec.md §11.1, `inputs` row: add `"run.diff"`, "the engine-computed VCS
  diff of the run's shared checkout from its pinned starting commit over the
  union of its issues' scopes, recorded at the claim of a run-scoped step that
  declares it; valid only in a run-scoped definition (V48)".
- engine-spec.md §11.3, clause 1: "the issue's loop counter increments" becomes
  "the subject's loop counter increments: the issue's for an issue-scoped
  definition, the run's for a run-scoped one"; the closing sentence "There is no
  other loop construct" stands, with "applied per subject" appended.
- engine-spec.md §11.4, `context`: `issue` becomes optional and `run?: {id,
  request}` is added, present only on run-scoped rows; `next row`'s `issue` is
  empty on run-scoped rows.
- engine-core.md §1.3: the parenthetical about run-level steps gains a pointer to
  this note.
- docs/tdd/engine-spine.md §6.3: add the R2c row.

## 11. Verification of this note's premises

Read, not inferred: every citation above was opened at 3c73f103 in the checkout
this note was written in. Two premises are inferences and are marked: SQLite's
treatment of NULL in a unique constraint (§3.1, from the SQLite documentation
rather than from a test in this repository), and the claim that no code path
writes a NULL `issue_id` step today (a search for `IssueID` writers found only
`expandIssue` and the loop instantiation paths, all of which copy an issue's id;
an exhaustive proof is phase 2's test, which asserts every run-scoped row is
readable through `ListRunStepsTx`).
