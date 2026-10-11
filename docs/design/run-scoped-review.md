# Run-scoped review over a run's integrated diff

Status: proposed — 2026-10-10 · design note settling the shape of a review step
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
| D1 | Workflow-expansion shape | A run-scoped workflow definition: `[pipeline] scope = "run"`. Its steps use the existing `[[step]]` grammar, expand once per run at activation with `issue_id` NULL, and become ready only after every issue of the run is terminal and no dispatch is open (readiness clause R2c). Readiness is not the integration proof: the claim of a run-scoped step that reads `run.diff` or holds the tree runs the integration check (§3.1, "Integration at claim"), driver-independent, and parks `waiting-human` instead of claiming when any write-class head is not on the shared branch. Not a per-step flag inside issue workflows, not an opt-in phase assembled from several bound workflows. |
| D2 | `run.diff` derivation | A committed read of the integration branch, recorded as a `run.diff` artifact at the claim of each run-scoped step that declares the input: the claim resolves the shared checkout's HEAD to one commit sha once, and a dedicated call computes `git diff <run.commit_sha> <head_sha> -- <union of the run's issue scopes>` between those two commits, with the round record `{base, head, sha256}` whose `head` is that same sha. Uncommitted changes and untracked files in `run.exec_root` are not part of the body. Never the union of issue diffs. |
| D3 | Cross-issue loop entry | No issue's bound governs. A run-scoped step's `fix-loop` enters a run-scoped loop cluster: the counter is the run's (`runs.loop_count`), the bound is the run-scoped definition's own `max_fix_loops` read by the existing rule, and the body is the run-scoped definition's own `loop = true` step. Issue counters never move and completed issues stay `done`. Until the cluster lands (phase 3), `fix-loop` is refused at register time on a run-scoped definition (V51b), so a rejection routes `waiting-human`; the resolutions that park supports, and the reason it carries, are decided in §3.3 and owned by phase 2. |
| D4 | Register-time lint | V51: `[pipeline] scope`, when present, is `"issue"` or `"run"`, and a definition whose scope is `issue` has no step whose `inputs` names `run.diff`. V51a: a definition whose scope is `run` declares no `[match]` table, no step `when`, and no step input of an `issue.*` form. V51b (phase 1 and 2 only): a definition whose scope is `run` has no `loop = true` step, no `threshold`, `on_fail`, or `on_fail_routes` value equal to `fix-loop` or `fix-round`, and no step declaring `on_exhausted` or `serves`. |
| D5 | Activation with no integration point | Activation is refused (`VALIDATION_ERROR`, inside the fat transaction, nothing written) when a run-scoped definition binds, some step of it declares `run.diff` or holds the tree, and the run's `exec_root` or `commit_sha` is empty. The refusal message is the fixed template in §3.5, with the run, the definition as `name@version`, and the empty field(s) substituted. The checkout remedy is to start a new run from inside the checkout; the refused run stays in `planning`, is replaced rather than repaired, and `docket run abandon <run> --reason <reason>` ends it. The deprecate remedy is to retire every non-retired version of the definition's name, listed by `docket workflow list --name <name>`, with `docket workflow deprecate <name>@<version>` per version; retiring only the top version lets binding fall back to the next. |

## 3. Design

### 3.1 D1: a run-scoped workflow definition

A workflow definition gains one optional `[pipeline]` key, `scope`, with values
`"issue"` (the default, and the meaning every existing definition has) and
`"run"`. The field is `omitempty` in the canonical form, so every definition that
never declares it serializes byte for byte as it does today; the repo treats a
canonical-form change on re-register as a CONFLICT (internal/workflow/parse.go:139-144
states the rule for `roster`; internal/workflow/canonical.go:19-25 states why).

Binding. Issue workflows bind per issue through `[match]` under the exactly-one
rule (internal/engine/activate.go:1332, `bindIssue`). Issue binding admits only
definitions whose effective scope is `issue`: `bindableDefinitions`
(activate.go:1290) drops a definition whose scope is `run` beside the retired
ones, before the highest-version reduction, and `bindIssue` (activate.go:1336)
and `lintDomainScopeMismatch` (activate.go:1588) both take their candidate set
from it, so a registered run-scoped definition is never a candidate for any
issue's exactly-one match and never a workflow the domain lint names. The filter
is what makes a run-scoped definition registerable at all: it has no `[match]`
(V51a), and `(*Match).Matches` returns true for a nil match (match.go:53-56), so
without the filter every registered run-scoped definition would match every
issue and the exactly-one count would refuse every activation in the project.
It therefore lands in phase 1 with the grammar (§6), before any run-scoped
binding exists. A run-scoped definition binds per run: at the first activation,
among the non-retired definitions whose scope is `run` (a reduction of its own,
phase 2, not `bindableDefinitions`), zero names means the run has no run phase;
exactly one name binds its highest version; two or more names is a
`VALIDATION_ERROR` listing them, the exactly-one rule's analog. The binding is
pinned with the issue workflows and inherited by re-activations (activate.go's
RA2 comment at line 603 and RA3 comment at line 665).

The filter changes which zero-match refusal `bindIssue` reports when no
issue-scoped definition is bindable (`candidates` empty). The deciding
condition is the count of registered issue-scoped versions, retired ones
included, not `len(definitions)`. A count above zero means every registered
issue-scoped version is retired: the all-retired message, whose `<N>` is that
count, so with retired issue-scoped versions and a run-scoped definition both
registered, `<N>` equals the number of retired issue-scoped versions and the
run-scoped rows are not counted. A count of zero means no issue-scoped
definition is registered: the only-run-scoped message when at least one
run-scoped version is registered, and the none-registered message when nothing
is. Today `bindIssue` tests `len(definitions) > 0`, so a project whose only
registration is a run-scoped definition falls into the all-retired branch,
which claims deprecated versions and offers `--restore` when nothing is
retired; phase 1 replaces that test with the issue-scoped count and adds the
only-run-scoped branch. Each message is one line, in the style of the engine's
other activation refusals.

Every registered issue-scoped version is retired (today's message, unchanged
except that `<N>` counts issue-scoped versions only):

```
issue <issue> matches no registered workflow: all <N> registered version(s) are deprecated, so none can bind. Restore one with `docket workflow deprecate <name>@<version> --restore`, or register a new version
```

Only run-scoped versions are registered (new in phase 1; its remedy is
registering an issue-scoped workflow, and it neither says any version is
deprecated nor offers `--restore`):

```
issue <issue> matches no registered workflow: no issue-scoped workflow is registered, and the <M> registered run-scoped version(s) never bind an issue. Register an issue-scoped workflow with `docket workflow register <file.toml>`
```

Nothing is registered (today's message, unchanged):

```
issue <issue> matches no registered workflow: none is registered
```

Substitutions: `<issue>` is the issue's display id (`DKT-12`); `<N>` is the
number of registered issue-scoped versions; `<M>` is the number of registered
run-scoped versions, retired ones included; `<name>@<version>` and
`<file.toml>` are literal text the operator fills in. Output for the two
states phase 1 tests, for issue `DKT-12`. Only the §5 definition registered:

```
issue DKT-12 matches no registered workflow: no issue-scoped workflow is registered, and the 1 registered run-scoped version(s) never bind an issue. Register an issue-scoped workflow with `docket workflow register <file.toml>`
```

Two retired versions of an issue-scoped workflow plus the §5 definition
registered:

```
issue DKT-12 matches no registered workflow: all 2 registered version(s) are deprecated, so none can bind. Restore one with `docket workflow deprecate <name>@<version> --restore`, or register a new version
```

Expansion. `workflow.Expand` runs once for the bound run-scoped definition with an
empty `Subject`, at ordinal 0, in the same fat transaction that expands the
phase-one issues, and the rows are written with `issue_id` NULL. `Expand` stays
"a pure function of (issue kind, labels, pipeline definitions @ pinned version)"
(internal/workflow/expand.go:53-56 quoting engine-core §2); a run-scoped
definition has no `when` (V51a), so the empty subject never reaches `WhenHolds`
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
read inside the scheduler's snapshot; none is a git probe. R2c therefore decides
when the phase may be offered, not whether the shared branch carries the work:
the open-dispatch conjunct orders the phase after a dispatch's close, and in a
run that uses dispatch that close has already run `integrationVerdict` over
every write-class head, but no table the scheduler reads records that a
worktree commit was integrated, and a run driven without dispatch (a fixture
run, a `next`-driven harness) never writes a close at all. Integration is
established for every run, whether or not a dispatch exists, by the
integration check at claim (detailed below): `integrationVerdict`
(dispatch_integration.go:352), the gate `dispatch close` runs, is called at the
claim's pre-transaction git moment, before the `claim_head` read
(claim.go:350), over every write-class issue step's recorded `issue.diff` head
against `run.exec_root`; a run-scoped step that R2c offers is claimed only when
that call accepts every head, and parks `waiting-human` otherwise. A
run driven without dispatch therefore reaches R2c when its issues finish and
still cannot open the phase until its writers' commits are on the shared
branch. R3 through R7 apply unchanged; R4 for a tree-holding run-scoped step
compares the union of the run's issue scopes, read through the same
`loadIssueFacts` path (ready.go:389).

Integration at claim. The check is `integrationVerdict`
(dispatch_integration.go:352), `dispatch close`'s own gate, called with no skip
reason from `claimStepWithGates` for a run-scoped step whose definition
declares `run.diff` or holds the tree (D5's predicate, so the step that needs
an integrated tree is the step that is checked; a run-scoped step with neither
is claimed as today). It runs at the claim's pre-transaction git moment, the
point `sharedCheckoutHead` reads `claim_head` (claim.go:350), and before that
read, so every head it accepts is an ancestor of the tip `run.diff` is then
computed from. The verdict is the one `dispatch close` would reach: candidates
are every terminal write-class step's most recent `issue.diff` head
(`integrationCandidatesTx`, dispatch_integration.go:135; skipped steps and
abandoned issues excluded; `run.diff` heads admitted in phase 3, §3.3), judged
by ancestry then patch-equivalence through the same `IsAncestorFn`,
`PatchContainedFn` and `StepPatchContainedFn` seams (`checkIntegration`,
dispatch_integration.go:262), and a head an earlier close of the run accepted
or vouched for is honored from that close's `integration.checked` record
(`priorIntegrationsTx`, dispatch_integration.go:220) rather than asked of git
again. That last rule is what keeps the check inert in the sanctioned flow: a
dispatch-driven run reaches R2c with a close behind every writer, so each
candidate carries a prior, the claim asks git only about a head no close has
judged (none, when the last close followed the last writer), and it records
nothing new. A run with no close has no
priors, so each candidate is asked of git once per claim; a shared-checkout
writer's head is the branch tip or behind it and passes by ancestry, and a
worktree writer's cherry-picked head passes by patch. A hand-resolved pick on
such a run passes only after `step annotate STEP-N --integrated-sha`
(annotate_integration.go:16, 104) re-records the head as the commit the shared
branch carries, the remedy `dispatch close` already relies on for the same
case. A `cherry-error` verdict (git could not answer) counts as unintegrated,
as it does at close.

The failure case. When `integrationVerdict` returns a non-empty unintegrated
list, the claim does not happen: no lease is minted, no token is returned, no
attempt is spent, no `step_inputs` are bound, and no `run.diff` artifact is
recorded (the diff is computed only after the check accepts). The git
questions are asked before transaction A, like `claim_head`; the verdict is
consumed inside A, after the lazy reap (claim.go:371-402) and the
readiness check, at the point the CAS claim would otherwise be written: the
claim writes the park there instead, through `SetStepRoutingWithParkReasonTx`
(internal/db/steps.go:814) with routing `waiting-human`, a new park class
`integration-unverified`, and a reason naming every unintegrated step and its
head in `UnintegratedReason`'s shape (dispatch_integration.go:406), records a
`step-routed` event carrying the routing record (the routing stage's own
event, saga.go:1556; no new event kind), commits A, and returns a CONFLICT
whose text is the park reason. Consuming the verdict inside A is what keeps
the claim's exclusion rule single: the same CAS state that would have minted
the lease decides the park, a reaped-then-re-offered row is parked in the one
transaction that reaped it, and no second transaction re-reads the row. The
candidates were read in `integrationVerdict`'s own read-only transaction
before A, so a head re-recorded between the two is the same window
`claim_head` already has, and the next claim reads it. The park follows `step
hold`'s shape, the one existing park of a READY `pending` row outside the
routing stage (hold.go:81-95 at the pinned commit). The reason, under the
`<class>: <prose>` form §3.3 uses:

```
integration-unverified: {step} {instance}: sha {sha12} ({how}) in {worktree}[; ...]; cherry-pick each commit onto the shared branch, or `step annotate {step} --integrated-sha` for a hand-resolved pick, then resolve with --as retry
```

`{step} {instance}: sha {sha12} ({how}) in {worktree}` is `UnintegratedReason`'s
rendering of one finding, `; `-joined, with ` in {worktree}` present only when
the record names one and `{how}` either `unintegrated` or `cherry-error`.
Rendered for a fixture whose second writer recorded in a worktree:

```
integration-unverified: STEP-12 implement@0: sha 3f9a2c1b7e04 (unintegrated) in /repo/.claude/worktrees/wf_ab12; cherry-pick each commit onto the shared branch, or `step annotate STEP-12 --integrated-sha` for a hand-resolved pick, then resolve with --as retry
```

The park stops the phase: the "no step of the run is parked `waiting-human`"
conjunct of R2c holds every other run-scoped row unready, so a fanout's second
sibling is not offered while the first is parked, and `next --explain` names
the condition. `retry` is a real remedy on this park, unlike the §3.3 sticky
cause: it returns the row to `pending` (human.go:783-791) and the next claim
re-runs the check against the branch as it then stands. `skip` and
`override-pass` stay supported with the consequences §3.3 gives them; the run
then rolls up `done` with its integrated review recorded as skipped or passed
over a tree the engine never verified, which the routing record says.
`abandon-issue` is refused by §3.3's subject rule. Two sibling claims that run
the check concurrently read the same branch and reach the same verdict unless a
commit lands between them; each consumes its own verdict in its own transaction
A, so the second parks or passes on the tree it saw, and its own `step-routed`
record says which. The check is not recorded as an
acceptance: nothing at the claim writes a `dispatch-closed` event (the only
writer of `integration.checked` is the close, dispatch.go:2088 and 2115, and
its no-manifest branch exists for usage acceptance alone, dispatch.go:2073-2079),
so a later claim of the same run asks git again. §8 states the cost.

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

Display. A run-scoped row has no issue, and every operator surface shows it
the same way: the `issue` field is the empty string `""` in `--json` output,
and a text issue column shows `-`. The row's run id, which each of these
surfaces already prints, names its subject. The rule covers `next`, `step
list`, `step show`, `events list` and every other event feed, and
`run report`. Message text that names a step's subject, such as an error or a
park reason, names the run (`RUN-N`) where an issue row names `DKT-N`. No
surface prints `<PREFIX>-0`. Two surfaces already follow the rule: an event
with a NULL `issue_id` leaves `Issue` empty (internal/engine/events_read.go:244)
and `events list` prints `-` for it (internal/cli/events.go:279), and the
`StepAttempt` rows of `run report` set `Issue` only when `IssueID != 0`
(`effectiveStepFacts`, internal/engine/report.go:902).
Phase 2 applies the same guard where a row is built from `step.IssueID`:
`next` (internal/engine/next.go:519), the `StepListEntry` that `step list` and
`step show` share (next.go:678), and the `step list` text column
(internal/cli/step.go, which prints `row.Issue` with no dash today).
`model.FormatID` is unchanged: 0 is not an issue id (`ParseID` refuses
`DKT-0`), and a formatter that returned `""` for it would turn every missed
call site into a silent blank instead of a case the phase 2 criterion catches.

Alternatives weighed.

| Candidate | Why it lost |
|---|---|
| A per-step `scope = "run"` flag inside ordinary issue workflows | A run pins several workflows, so N bound definitions can each declare the step: either N reviews run or a dedupe rule must say whose wins. `after` edges would cross the issue/run boundary, and `Expand`'s per-issue purity contract would have to carve out a once-per-run row. |
| A post-integration phase every issue workflow opts into, assembled by the engine | Needs a definition for the phase's steps anyway, plus an opt-in flag per issue workflow and a merge rule. The opt-in half is deferred (§7) and can be added to D1 later without changing its shape. |
| Keep reviewing per issue and rely on `dispatch close`'s integration check | The check proves commits are present, not that the integrated tree is correct, and a hand-resolved cherry-pick is content no reviewer saw. This is the gap the request names; doing nothing leaves it. |
| Refuse `scope = "run"` at register time until phase 2 binds it, instead of filtering issue binding in phase 1 | Phase 1's acceptance (`workflow lint` reports `new` on the §5 definition) would be unmeetable, and phase 2 would still need the filter, since the run-scoped reduction and the issue reduction read the same registered rows. The filter is one clause in `bindableDefinitions` and is needed in every later phase. |
| Filter in `bindIssue` and `lintDomainScopeMismatch` separately, leaving `bindableDefinitions` untouched | Two call sites enforce one invariant by convention; a third consumer of `bindableDefinitions` would inherit the defect. The filter inside `bindableDefinitions` keeps the lint's documented promise that it names only workflows the issue could have bound. |

Alternatives weighed for the integration check (where it runs and what it
asks).

| Candidate | Why it lost |
|---|---|
| Leave integration to the open-dispatch conjunct and state the no-dispatch limitation in §8, covered by D5's refusal | This note's first draft. D5 refuses a run with no integration point, not a run whose writers' commits never reached it; a fixture run and a `next`-driven harness are supported drivers (§3.2 rejected the close-time `run.diff` record for exactly their sake), and on them R2c would open the phase, `run.diff` would omit the unintegrated work, and the panel could pass a tree lacking it with no signal. The gap the request names, re-opened by driver convention. |
| An R2c conjunct over a recorded "integrated" fact | Keeps readiness a table read, which is its strength. The only recorder of integration today is the close (`integration.checked`), so without dispatch nothing would ever write the fact; the recorder would have to be a git probe somewhere, and the one git moment every driver shares is the claim. The claim check is that recorder with the park in place of the fact. |
| Verify at each write-class step's completion, beside `computeIssueDiff` | The commit is still in the worktree when the step records; the sanctioned flow cherry-picks it afterwards, so the probe would fail every worktree writer by construction. |
| Raw `IsAncestorFn`/`PatchContainedFn` probes per recorded head at claim, without `integrationVerdict` | Smaller call, which is its only strength. It drops the prior-close record, so a hand-resolved pick that a close already accepted (never an ancestor, never patch-equivalent; dispatch_integration.go:70-73) would park every dispatch-driven run that resolved a conflict, and it drops the skipped and abandoned exclusions. `integrationVerdict` is the gate `dispatch close` runs; a second verdict would be a second answer to one question. |
| A readiness-time git probe inside R2c | Puts a subprocess in the scheduler's snapshot, which §6 forbids and which `integrationVerdict` itself avoids by rolling back before probing (dispatch_integration.go:369-372); every `next` would pay the probes for every run-scoped row. |
| A new event kind for the park | Names the decision in the feed, which is its strength. The routing record and the park class already carry it, and a kind costs a closed-set change (`TestEventKindsAreAClosedSet`) for a fact two columns state. |
| Record the claim's verified result as a `dispatch-closed` acceptance so later claims honor it | Saves one probe pair per candidate on later claims. It would write a close event with no close, and every reader of `dispatch-closed` (`run report`, the prior-close record) would have to learn a third author. Re-probing is cheap and truthful; §8 names the cost. |

### 3.2 D2: `run.diff` is a committed integration-branch read, recorded at claim

Form. `run.diff` is a new engine-produced input form beside `issue.diff` and a new
engine-recorded artifact kind of the same name (context.go:247-258 is where the
constants live). Body: a dedicated call, `committedRangeDiff(dir, base, head,
scope)` (new in internal/engine/saga.go), not `GitDiff`. It computes `git diff
<commit_sha> <head_sha> -- <union scope>` between two commits, with `dir` the
run's exec root, `base` the run's pinned starting commit, `head` the sha the
claim resolved (below), and `scope` the union of the run's issues'
`scope_globs`. Both ends are commits, so the body describes committed history
only: uncommitted changes and untracked files in `run.exec_root` are not part of
it. `GitDiff` is the wrong call for this object: its `rawDiff` runs `git diff
<base> -- <paths>` (saga.go:2681-2690), which compares base to the working
tree, and it appends `untrackedDiff` (saga.go:2526), so on a dirty checkout its
body would carry content no commit on the branch holds and no recorded `head`
names. The dedicated call keeps the two properties of `issue.diff` that do hold
for a commit range, the precedent being `GitCommitPatch` (saga.go:2598), the
committed read that already carries them with no untracked half: paths the range
touched outside the union scope are named and their hunks follow under the marked
heading, capped as in `outOfScopeDiff`, both computed over the same two commits;
and an empty union diffs the whole range, `rawDiff`'s rule ("An empty pathspec
diffs the whole tree"). Payload: `{"base": <commit_sha>, "head": <head_sha>,
"sha256": <body hash>}`.

Base and tip. `runs.commit_sha` is pinned once at `run start` from the exec root's
HEAD (internal/cli/run_start.go:161-164 via `config.GitHead`); `runDiffBase`
(saga.go:2749) already names it as "the run's PINNED starting commit" for
shared-checkout recordings. The tip is resolved once per claim:
`sharedCheckoutHead(runExecRoot(conn, runID))`, the read `ClaimStepRendered`
already performs before its transaction (internal/engine/claim.go:345-350) and
records as `claim_head`, returns the shared HEAD as one commit sha. The claim
passes that same sha to `committedRangeDiff` as the diff tip and writes it as the
payload `head`; no second git read of HEAD occurs. The payload `head` therefore
names the exact commit the body was computed against, and a commit landing on the
shared branch between the resolution and the diff cannot enter the body, because
the diff reads the resolved sha, not the moving ref. When the resolution yields no
sha (no commit, or not a checkout), the claim records no `run.diff` and the input
resolves empty under the D4 rule below. For a run diff the pinned commit is the
right base for exactly the reason it is the wrong base for a single writer:
everything integrated since the run began is the run's work.

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

Tree-holding run-scoped steps record a `run.diff` at completion as well, at the
moment `computeIssueDiff` records `issue.diff` for a tree-holding issue step
(saga.go:1646-1685), through the same committed read: the completion resolves
HEAD to one sha once and uses it as both the diff tip and the payload `head`,
with the round delta appended by `appendRoundDelta`
(saga.go:3392) from the previous round's recorded head. That record is what the
non-convergence check and `dispatch close`'s candidate collection read in D3.

Alternatives weighed.

| Candidate | Why it lost |
|---|---|
| Union of issue diffs: concatenate each issue's D3-resolved `issue.diff` body | Each body is filtered to one issue's scope and computed against a different base (a worktree's fork point, or the claim head for a shared-checkout writer, saga.go:2749-2775). Concatenating them is not a patch anyone could apply, double-renders files two issues touched, and omits what a hand-resolved cherry-pick changed. It is cheaper and needs no git read, which is its only strength. |
| Keep `GitDiff`'s working-tree read and record it as such: a dirty flag or a tree hash in the payload, the untracked trailer kept | It would show the panel uncommitted work, which is its strength. But the object would be a working tree no commit names, so `head` could not identify it, `dispatch close` and the phase-3 round delta key on commits, and a fanout's siblings would differ whenever anyone touched the checkout. The run's deliverable is what is on the shared branch; a committed read reviews exactly that, and §8 states what it leaves out. |
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

Phasing. The cluster is phase 3 (§6). Until it lands, V51b refuses `fix-loop` and
`loop = true` on a run-scoped definition at register time, so a phase-1 or
phase-2 run-scoped review can route `pass`, `waiting-human`, or interpose a named
step, and a rejection parks `waiting-human` with the findings beside it. The
closed vocabulary's own answer applies in the interval (engine-core §4: "If a
situation isn't expressible, the answer is `waiting-human`, not cleverness").

Resolving the phase-2 park. `step resolve` accepts six values,
`--as retry|rerun-gates|skip|abandon-issue|override-pass|fix-round`
(internal/cli/step.go:942, 1085). Two facts about the parked row decide each
value's stance, and the engine reads them separately: the park's cause and the
row's subject. The stances below are decided for a run-scoped (NULL `issue_id`)
step parked `waiting-human` with class `threshold-routed` under phase 2, and
each row says which fact it keys on.

The cause. The §5 `reconcile` step is an action step (`action = "aggregate"`;
`Step.Action`, internal/workflow/parse.go:116; `ClassAction`, parse.go:375). Its
payload is a function of its inputs, the terminal `review.findings` artifacts,
and its threshold is evaluated over that payload (saga.go:1210-1219,
`stepPayloads`). When an arm matches `waiting-human`, `EvaluateThresholdOverDiff`
returns the routing with `Parked` false (threshold.go:204); when an ordered
comparison cannot be decided, T3 returns `waiting-human` with `Parked` true and a
reason of its own (threshold.go:194-200). Both reach `ParkClassThresholdRouted`
through one branch (saga.go:1374-1378), so the class alone does not separate
them. The sticky cause this note keys on is the first: an action step whose
threshold matched a `waiting-human` arm. Re-running it re-reads the same inputs,
recomputes the same payload, and matches the same arm. The cause does not depend
on the step's scope; an issue-scoped `aggregate` parked this way is parked for
the same reason.

The subject. A run-scoped row has `issue_id` NULL (§3.1). `abandon-issue`'s one
job is to stop a run's work on an issue: `abandonIssue` (reconcile.go:495-560)
cascades over `WHERE run_id = ? AND issue_id = ?`, calls
`SetIssueResolutionTx(tx, step.IssueID, …)` and `releaseAbandonedIssueTx(tx,
step.IssueID, …)`, and comments through `commentEngineEvent(tx, step.IssueID,
…)`. Its stance follows the subject and nothing about the park.

| `--as` | Stance | Keyed on | Reason |
|---|---|---|---|
| `override-pass` | Supported | Neither | It records a generic `pass` on the step row (human.go:796-797) and reads no issue; the run then rolls up `done` with the park answered. On a step whose threshold interposes other steps it needs `--drop-interposed` (human.go:668-680), as on any step. |
| `skip` | Supported | Neither | It routes the step `skipped` (human.go:792-793). Three issue-keyed reads sit on its reconcile path, and phase 2 teaches each the NULL row as it does every other `IssueID == 0` reader (§3.1): `targetRoutings` and `pendingTargetInstances` (reconcile.go:174-180, 221-226) and `ListIssueStepsTx` under `propagateAfterFiredSkips` (internal/engine/after_fired.go:72-78; internal/db/steps.go:374). Skip also terminalizes any step the threshold interposes, through `skipUnroutedTargets` (reconcile.go:49, 126-166), with no acknowledgment flag; the §5 definition interposes nothing, and the asymmetry against `override-pass`'s `--drop-interposed` is deferred (§7). The run rolls up `done` with its integrated review recorded as skipped. |
| `retry` | Refused on this park | Cause | Retry resets the attempt budget and re-runs the step (human.go:783-791); an action step re-run over the same terminal inputs matches the same arm and parks here again. The engine already refuses retry for the two other sticky causes it can read, a rejected hold (human.go:604-621; `parkedByRejectedHold`, internal/engine/held.go:606) and a decided vote (human.go:583-602; `ReadStepVoteOutcome`), and each refusal inspects the cause, not the row's scope. `retry` stays supported on a T3 park (its remedy is to register the order and re-run), on an executor step's matched arm (a re-execution records a new payload), and on every other park of a run-scoped step: a gate failure, the §9 size park. |
| `rerun-gates` | Refused on this park | Cause | It re-runs the gates and re-evaluates the same threshold over the same recorded payload, the same inert outcome as `retry`, so the same test refuses it with the same message. That refusal runs before the existing no-gates refusal (human.go:682-690), whose message points at `retry`; on the §5 step, which declares no gates, the operator therefore reads the two values that move the step rather than a pointer to a value the next command refuses. |
| `abandon-issue` | Refused | Subject | On a NULL row each of `abandonIssue`'s writes is a no-op or a write against a nonexistent issue, because there is no issue for this run to stop working on. It stays supported, as today, on an issue-scoped row parked the same way. |
| `fix-round` | Refused until phase 3 | Neither | V51b forbids the loop body it would enter, and `GrantLoopTx` keys the grant on `step.IssueID` (human.go:803). Phase 3 makes it the remedy §6 names. |

How the refusal reads the cause. `resolveStep` re-runs the routing stage's own
evaluation, `EvaluateThresholdOverDiff` over `stepPayloads` and `stepOrder`
(saga.go:1219, 1195; internal/engine/order.go:67) with nil `DiffFacts`, for a
step whose `Kind` is `ClassAction` and whose status is `waiting-human` with class
`threshold-routed`. A result whose `Routing` is `waiting-human` and whose `Parked`
is false is the sticky cause; any other result (a T3 park, a payload that has
since changed) leaves `retry` and `rerun-gates` accepted. The evaluation is pure
(saga.go:1244-1249: "It is pure, so calling it there costs nothing and races
nothing"); nil facts are exact for an action step because V45 refuses a `diff.*`
predicate on any step that is not a tree-holding executor or fanout
(internal/workflow/validate.go:1431-1451); and reading the cause with the same
call the park was decided by keeps the refusal and the park from disagreeing,
the argument the interposed-targets refusal makes for reusing
`skipUnroutedTargets`' read (human.go:655-667). The reason's text is never
consulted: the spec forbids moving a row between classes by reading it
(engine-spec.md §2, "Park class"), and a refusal keyed on prose would be fragile
for the same reason.

The park reason. A matched `waiting-human` arm returns no reason today
(threshold.go:204), so `SetStepRoutingTx` stores the class word alone
(internal/db/steps.go:823) and every threshold-routed park that is not T3 reads
`threshold-routed`, naming no remedy. Phase 2 composes a reason for the sticky
cause above, in two halves that follow what each side knows:

- `evaluate` and `evaluateOrdered` report their match count beside the bool they
  return today (T1 counts at threshold.go:301-311, T2 at 341-365, both through
  `aggregate(p, count, len(payloads))`, 470-480), and `ThresholdResult`
  (threshold.go:133-144) gains `Matched` and `Total`. For a matched
  `waiting-human` arm, `EvaluateThresholdOverDiff` sets `Reason` to the cause
  clause, as it already does for T3: `{count} of {total} payload
  {element|elements} matched "{predicate}"`.
- The saga's `evaluate` closure returns the `ThresholdResult` instead of
  narrowing it to `(routing, reason)` (saga.go:1250-1257), and the branch that
  assigns `ParkClassThresholdRouted` (saga.go:1374-1378) composes the stored
  reason for a `ClassAction` step whose result is `waiting-human` with `Parked`
  false, under the `<class>: <prose>` shape the other engine park reasons use
  (`attemptsExhaustedParkReason`, human.go:1290; `loopBoundParkReason`,
  human.go:1302):

```
threshold-routed: {count} of {total} payload {element|elements} matched "{predicate}"; resolve with {resolutions}
```

Placeholders. `{count}` is the number of payload elements the arm's predicate
held for and `{total}` the payload set's size, both as the threshold saw them
(`aggregate@1` carries one element per input cluster, so on the §5 definition
both count clusters). `{predicate}` is the arm's source text. The noun is
`element` when `{total}` is 1 and `elements` otherwise. `{resolutions}` is the
step's supported set, rendered from the subject: `--as override-pass (accept the
step as passing) or --as skip (route it skipped)` on a NULL row, and `--as
override-pass (accept the step as passing), --as skip (route it skipped), or
--as abandon-issue (drop the issue from this run)` on an issue row, the three
glosses the rejected-hold refusal uses today (human.go:618-621). Nothing else is
listed: `retry` and `rerun-gates` are refused on this park, `fix-round` until
phase 3.

Rendered for the §5 definition, three clusters of which two reduce to `high` or
above:

```
threshold-routed: 2 of 3 payload elements matched "any(severity >= high)"; resolve with --as override-pass (accept the step as passing) or --as skip (route it skipped)
```

One cluster, at `high`:

```
threshold-routed: 1 of 1 payload element matched "any(severity >= high)"; resolve with --as override-pass (accept the step as passing) or --as skip (route it skipped)
```

An `all(...)` arm over an empty payload, which T4 decides true without consulting
the predicate (threshold.go:279-281):

```
threshold-routed: 0 of 0 payload elements matched "all(severity >= high)"; resolve with --as override-pass (accept the step as passing) or --as skip (route it skipped)
```

Where the composer stops. A T3 park keeps the reason the evaluator already
composes; its remedy is the schema registration that text names. The
stale-lineage branch (saga.go:1429-1433) evaluates so the ledger is attributed
and materializes nothing, so the composer is not applied there and the result's
own reason is stored as today. An executor step's matched `waiting-human` arm
keeps the class-word reason: its re-execution can change the payload, so neither
the sticky refusal nor this remedy list is right for it (§7).

Consequence for issue-scoped rows. Because the refusal and the composer both key
on the cause, an issue-scoped action step parked by a matched `waiting-human`
arm gains the same reason, with `abandon-issue` in its `{resolutions}`, and
loses `retry` and `rerun-gates` on that park. That is a behavior change on both
scopes, listed in §8. It is decided here because the alternative, one park class
carrying a policy that differs by scope, is the shape the table below rejects.

The refusals, as templates. Each is a `validationErr` in `resolveStep`, in the
shape of the rejected-hold refusal (human.go:609-618). `{as}` is the value the
operator gave; `{resolutions}` is the subject's list above, from the renderer the
park reason uses.

Sticky cause, for `retry` and `rerun-gates`:

```
step {instance} cannot be resolved with --as {as}: it is parked because its threshold matched "{predicate}" over {count} of {total} payload {element|elements}, and an action step re-run over the same inputs would route here again. Use {resolutions}
```

Subject, for `abandon-issue` on a NULL row, whatever the park:

```
step {instance} belongs to the run, not to an issue, so --as abandon-issue has nothing to abandon. Use --as override-pass (accept the step as passing) or --as skip (route it skipped)
```

The second names the two values supported on every run-scoped park; whether
`retry` applies is the first refusal's question, asked when the operator asks it.
The two existing sticky refusals end "or --as abandon-issue to drop the issue
from this run" (human.go:598-601, 618-621); on a NULL row phase 2 drops that
clause, since a run-scoped definition may declare a vote or a hold (V51b forbids
neither) and the message would otherwise name a value the next command refuses.

Rendered for the §5 definition after `step resolve reconcile@0 --as retry`:

```
step reconcile@0 cannot be resolved with --as retry: it is parked because its threshold matched "any(severity >= high)" over 2 of 3 payload elements, and an action step re-run over the same inputs would route here again. Use --as override-pass (accept the step as passing) or --as skip (route it skipped)
```

Alternatives weighed, for the park's resolution first and the loop after it.

| Candidate | Why it lost |
|---|---|
| Key the refusal and the composer on scope: a run-scoped row with class `threshold-routed` (this note's first draft) | Too broad and too narrow at once. It refuses `retry` on a run-scoped T3 park, whose remedy is to register the order and re-run, and overwrites T3's own reason; it keeps `retry` accepted on an issue-scoped aggregate parked by the same matched arm, where it is just as inert. One park class would carry two policies split by scope across human.go and saga.go, and each new scope would need a table of its own. The cited precedents inspect the cause, not the row's scope. |
| Key on the cause by recording it: split `threshold-routed` into a matched-arm class and an undecidable class | Enforces the distinction by construction rather than by a re-evaluation, which is its real strength. It changes a closed enum the spec lists (engine-spec.md §2, "Park class"), re-labels what every existing `threshold-routed` row means, and makes every conductor that routes on the class learn two values, for a distinction one pure function call makes at resolve time. |
| Accept `retry` on the matched-arm park, as the engine does today, and name it in the park reason | The resolution would be accepted and inert: the aggregate re-reads the same findings and parks again with the same reason, so the park reason would offer a remedy that does not remedy. A supported value the park reason omits is the same defect from the other side. |
| Compose the whole reason in saga.go from `Matched`, `Total`, and a new `Predicate` field on `ThresholdResult` | One composer instead of two halves, which is its strength. It adds a field that exists only to be printed and re-derives in saga.go a sentence the evaluator already composes for T3. The two halves follow a seam that exists: the evaluator states the cause (T3's `Reason`), the routing branch states the class and the remedy (`loopBoundParkReason`). |
| Drop `{count}` from the template, so no evaluator change is needed | Removing the evaluator change is its only strength. The count is what tells an operator whether one cluster or every cluster crossed the line, and the issue's acceptance criteria require it. |
| Re-enter each named issue's loop under that issue's own counter and its bound workflow's `max_fix_loops` | Three defects. The issue is `done` (`completeIssue`, reconcile.go:457-475) and nothing in the engine moves a done issue back; `reflectIssueStatus` (reconcile.go:268) only advances `in_progress` to `review`. Core would have to learn which issues a rejection names, either by reading payload content, which genericity.md forbids, or by re-opening every issue whose scope the diff touches, which re-runs N per-issue review chains over N per-issue diffs for a defect that lives in their intersection. And the run-scoped review itself must run again after the fixes, which is a run-scoped re-instantiation in any case, so this candidate needs the run-level construct and the issue-level reopen both. The question "whose bound governs a rejection spanning several issues" has no answer under it that is not invented: the smallest bound, the largest, or the first-declared are all arbitrary. |
| No loop: a run-scoped rejection routes `waiting-human` only, and follow-up issues carry the fixes | This is phase 1 and 2's behavior, kept deliberately until the cluster lands. As the permanent answer it leaves the integrated tree's known defects to an operator on every run, which is the hand process the request asks to replace. |

### 3.4 D4: V51 and its sub-rules

The rule table (internal/workflow/validate.go:26-39, `RuleIDs`) ends at V50 and
`TestValidationTableIsComplete` asserts set equality over it, so each new rule
below is added to the table and to internal/workflow/validate_test.go together.
V12 is deliberately absent and is not reused.

V51, on `[pipeline] scope` and issue-side inputs. The definition-level condition:
`pipeline.scope` is unset, `"issue"`, or `"run"`; and when the effective scope is
`issue`, no `[[step]]` has an `inputs` entry equal to `run.diff`. Refused with
`Rule: "V51"`, `Field: "pipeline.scope"` for the value, or `Step`, `Field:
"inputs"` for the input, with the message naming the form and the scope it
requires. This is V11's own reasoning (validate.go:1183-1200, the engine-form
vocabulary) carried to a form that resolves only against a run: on an issue-bound
step it would resolve to nothing on every run.

V51a, on a run-scoped definition's issue-only fields. The condition, when the
effective scope is `run`: the definition has no `[match]` table; no step declares
`when`; and no step has an `inputs` entry equal to `issue.body`, `issue.diff`,
`issue.files`, or beginning `issue.latest.` or `issue.linked.`. Refused with
`Rule: "V51a"` and the offending field. Each named field reads an issue that a
run-scoped step does not have: `Matches` takes an issue subject (match.go:53),
`WhenHolds` reads kind and labels (match.go:168), and each `issue.*` form resolves
through `step.IssueID` (context.go:1594, 1202; `contextIssue` at 652).

V51b, the phase gate. The condition, when the effective scope is `run`: no step
has `loop = true`; no `threshold`, `on_fail`, or `on_fail_routes` value is
`fix-loop` or `fix-round`; and no step declares `on_exhausted` or `serves`.
Refused with `Rule: "V51b"`. It is removed in phase 3 and its number is not
reused, the V12 convention.

Two existing rules change shape, not number: V11's engine-form list admits
`run.diff` (validate.go:1092, 1188, 1198), and L4's skip list does too
(internal/workflow/lint.go:236), since `run.diff` names no producer step.

Alternatives weighed. One combined V51 with three clauses was the other shape; the
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
any row is written, so the fat transaction rolls back whole. The message is the
template below, a single line in the style of the engine's other activation
refusals. It offers two remedies, worded as fixed text. The checkout remedy is
to start a new run from inside the checkout: the refused run stays in
`planning` with nothing written and cannot gain an integration point, so it is
replaced, not repaired, and an operator who wants it ended runs
`docket run abandon <run> --reason <reason>`. The deprecate remedy is to retire
every non-retired version of the run-scoped definition's name, not only the
bound one: find the versions with `docket workflow list --name <name>` and run
`docket workflow deprecate <name>@<version>` for each. Retiring only the top
version lets binding fall back to the next version, which can be an older
run-scoped version that refuses again.

```
run <run> binds run-scoped workflow <name>@<version>, which reads run.diff or holds the tree, but the run has no integration point (<empty-fields>); start a new run from inside the checkout (this run stays in planning; end it with `docket run abandon <run> --reason <reason>`), or retire every non-retired version of <name> (list them with `docket workflow list --name <name>`, then run `docket workflow deprecate <name>@<version>` for each; retiring only the top version lets binding fall back to the next)
```

Substitutions: `<run>` is the run's display id (`RUN-7`); the first
`<name>@<version>` is the bound run-scoped definition; `<name>` is that
definition's name, repeated in the three places the remedy uses it; `<reason>` and the
`@<version>` in the deprecate invocation are literal text the operator fills in,
one version per invocation; `<empty-fields>` is one of three fixed strings
chosen by which of the two fields is empty. Output for each branch, for run
`RUN-7` bound to `final-review@1`:

`exec_root` empty, `commit_sha` set:

```
run RUN-7 binds run-scoped workflow final-review@1, which reads run.diff or holds the tree, but the run has no integration point (exec_root is empty); start a new run from inside the checkout (this run stays in planning; end it with `docket run abandon RUN-7 --reason <reason>`), or retire every non-retired version of final-review (list them with `docket workflow list --name final-review`, then run `docket workflow deprecate final-review@<version>` for each; retiring only the top version lets binding fall back to the next)
```

`commit_sha` empty, `exec_root` set:

```
run RUN-7 binds run-scoped workflow final-review@1, which reads run.diff or holds the tree, but the run has no integration point (commit_sha is empty); start a new run from inside the checkout (this run stays in planning; end it with `docket run abandon RUN-7 --reason <reason>`), or retire every non-retired version of final-review (list them with `docket workflow list --name final-review`, then run `docket workflow deprecate final-review@<version>` for each; retiring only the top version lets binding fall back to the next)
```

Both empty:

```
run RUN-7 binds run-scoped workflow final-review@1, which reads run.diff or holds the tree, but the run has no integration point (exec_root and commit_sha are empty); start a new run from inside the checkout (this run stays in planning; end it with `docket run abandon RUN-7 --reason <reason>`), or retire every non-retired version of final-review (list them with `docket workflow list --name final-review`, then run `docket workflow deprecate final-review@<version>` for each; retiring only the top version lets binding fall back to the next)
```

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
- internal/engine/activate.go, phase 1: the issue-binding scope filter.
  `bindableDefinitions` (line 1290) admits only definitions whose effective
  scope is `issue` (unset or `"issue"`), so `bindIssue` and
  `lintDomainScopeMismatch` never see a run-scoped definition.
- internal/engine/activate.go, phase 2: run-scoped binding (zero/one/many) over
  the non-retired `run`-scoped definitions, eager expansion with NULL
  `issue_id`, the D5 refusal.
- internal/engine/activate_test.go: phase 1 asserts that registering the §5
  definition leaves an issue's binding unchanged; phase 2 covers the run-scoped
  binding and the D5 refusal.
- internal/db/runs.go: `InsertStepTx` writes NULL for a zero `IssueID`.
- internal/db/steps.go: `scanSteps` reads `issue_id` through `sql.NullInt64`.
- internal/db/schema.go: migration adding the partial unique index on
  `steps(run_id, instance) WHERE issue_id IS NULL`, and `runs.loop_count` (phase 3).
- internal/engine/ready.go: clause R2c and the union-scope R4 read for run-scoped
  rows.
- internal/engine/ready_test.go.
- internal/engine/next.go: `issue` empty on run-scoped `next` rows and on the
  `StepListEntry` that `step list` and `step show` read (§3.1 display rule).
- internal/cli/step.go: the `step list` text column prints `-` for an empty
  issue.

### Packet input resolution

- internal/engine/context.go: the `run.diff` input form and artifact kind
  constants, `resolveRunDiff` (new function in this file), the `run?` bundle
  member, and skipping `contextIssue` for run-scoped rows.
- internal/engine/claim.go: the integration check (§3.1, "Integration at
  claim"): for a run-scoped step under D5's predicate, call
  `integrationVerdict` with no skip reason before the `claim_head` read; on a
  non-empty unintegrated list, consume the verdict inside transaction A after
  the lazy reap, where the CAS claim would be written: park the row
  `waiting-human` with class `integration-unverified` and the §3.1 reason,
  record the `step-routed` event, commit A, and return CONFLICT with no lease
  and no `run.diff`.
  Then compute `run.diff` before the claim transaction from
  the existing `claim_head` sha, which is the diff tip and the payload `head`,
  and record it inside the transaction.
- internal/engine/dispatch_integration.go: `integrationVerdict` gains the
  claim as a second caller; a doc-comment change only, since the function
  already takes `conn`, the run, the definitions and a skip reason, and
  already splits its read-only transaction from its probes. (The phase-3
  `run.diff` admission in `integrationCandidatesTx` is listed under "Fix-loop
  routing".)
- internal/db/steps.go: `ParkClassIntegrationUnverified ParkClass =
  "integration-unverified"`, beside `ParkClassHeld` (line 124), produced by
  the claim's park alone.
- internal/engine/claim_test.go: the no-dispatch unintegrated-head park and its
  retry (§6, phase 2), and a dispatch-driven run whose claim asks git nothing.
- docs/design/engine-spec.md §2, "Park class" table: the
  `integration-unverified` row (the amendment text is in §10; the edit is the
  amendment's, not this note's).
- internal/engine/saga.go: `committedRangeDiff` (new function in this file), the
  two-commit `git diff <base> <head> -- <scope>` with the out-of-scope disclosure
  over the same range and no untracked half; `computeRunDiff` (new) uses it with
  `runExecRoot` and `appendRoundDelta` for tree-holding run-scoped steps.
  `GitDiff` is not reused, because it reads the working tree.
- internal/engine/packet.go and internal/engine/render.go: render the run's
  request where the issue section renders today.
- internal/engine/context_test.go.
- docs/design/engine-spec.md: §11.1 inputs row and §11.4 `context` shape (the
  amendment text is in §10; the edit is the amendment's, not this note's).

### Park resolution (phase 2)

- internal/engine/threshold.go: `evaluate` and `evaluateOrdered` report their
  match count; `ThresholdResult` gains `Matched` and `Total`;
  `EvaluateThresholdOverDiff` sets `Reason` to the §3.3 cause clause for a
  matched `waiting-human` arm. `evaluateDiff` is unchanged: V45 keeps `diff.*`
  off the action steps the composer serves.
- internal/engine/threshold_test.go: the counts and the matched-arm reason for
  `any`, for `all` over an empty set, and for `count>=n`; a T3 park's reason
  unchanged.
- internal/engine/saga.go: the `evaluate` closure returns the `ThresholdResult`;
  the branch that assigns `ParkClassThresholdRouted` composes the §3.3 park
  reason for a `ClassAction` step whose result is `waiting-human` and not
  `Parked`; the stale-lineage branch does not.
- internal/engine/human.go: `resolveStep` refuses `retry` and `rerun-gates` on
  the sticky cause by re-evaluating the threshold, ahead of the no-gates
  refusal; refuses `abandon-issue` on a NULL row; drops the `abandon-issue`
  clause from the decided-vote and rejected-hold refusals on a NULL row; one
  renderer for `{resolutions}`, shared with the saga's composer (§3.3).
- internal/engine/human_test.go: each refusal's message on the §5 fixture, the
  issue-scoped rendering, and `retry` still accepted on a T3 park.
- internal/engine/reconcile.go: `reconcileIssueAndRun` skips the issue-status
  mirror for a run-scoped step and still runs `reconcileRun`; `targetRoutings`
  and `pendingTargetInstances` take the NULL `issue_id` reading so
  `skipUnroutedTargets` works on a run-scoped step.
- internal/engine/after_fired.go and internal/db/steps.go:
  `propagateAfterFiredSkips` and `ListIssueStepsTx` take the NULL reading so a
  `--as skip` cascades over the run-scoped successors (§3.3).

### Fix-loop routing

- internal/engine/loop.go: `enterLoop` reads and restores the counter through
  the step's subject (issue or run); `maxFixLoops` over the run-scoped
  definition; `newestIssueDiffUpTo` gains a `run.diff` reading for the
  non-convergence check.
- internal/engine/loop_test.go.
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

- internal/workflow/validate.go: `RuleIDs` gains V51, V51a, V51b; `validatePipeline`
  checks the scope value; `validateInputs` admits `run.diff` under scope `run` and
  refuses it under scope `issue`; a new `validateRunScope` applies V51a and V51b.
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

Phase 1, grammar, lint, and the issue-binding scope filter. `[pipeline] scope`;
V51, V51a, V51b; V11 and L4 admit `run.diff`; canonical-form round trip; the
issue-binding scope filter, `bindableDefinitions` admits only definitions whose
effective scope is `issue`, so `bindIssue` and `lintDomainScopeMismatch` never
consider a run-scoped definition (§3.1). The filter ships in this phase, not
phase 2, because the phase makes a run-scoped definition registerable and a
registered one with no `[match]` would otherwise match every issue. Acceptance:
`workflow lint` on the §5 definition reports `new`; the same definition with
`[match]` added is refused naming V51a; an issue-scoped definition declaring
`run.diff` is refused naming V51; every definition in `internal/workflow/testdata`
re-registers `unchanged`; with the §5 definition registered, an issue that bound
exactly one workflow before it was registered binds that same workflow and its
activation succeeds, and `lintDomainScopeMismatch` reports nothing it did not
report before; with only the §5 definition registered and no issue-scoped
workflow, activation refuses with the only-run-scoped message (the §3.1
template, rendered with `<M>` = 1) byte for byte, which neither says any
version is deprecated nor offers `--restore`; with two retired versions of an
issue-scoped workflow plus the §5 definition registered, activation refuses
with the all-retired message (the §3.1 template, rendered with `<N>` = 2) byte
for byte, so `<N>` equals the retired issue-scoped versions and excludes the
run-scoped one.

Phase 2, binding, storage, readiness, packet, and the park's resolution. NULL
`issue_id` rows; partial unique index; run-scoped binding and the D5 refusal;
R2c; the integration check at claim and its `integration-unverified` park
(§3.1); `run.diff` recorded at claim; the `run?` bundle member; and everything §3.3
decides about the `waiting-human` park, because this phase is what makes that
park reachable: the matched-arm park reason, the sticky-cause refusal of `retry`
and `rerun-gates`, the subject refusal of `abandon-issue`, the NULL readings on
`skip`'s reconcile path, and the `abandon-issue` clause dropped from the two
existing refusals on a NULL row (the §4 "Park resolution" group). Acceptance: on
a fixture run with two issues, `next` offers no run-scoped row while any issue
step is non-terminal or a dispatch is open, and offers `review@0#0` and
`review@0#1` once both hold; the claimed bundle's first input is a `run.diff`
whose payload `base` equals the run's `commit_sha` and whose body equals `git
diff <base> <head> -- <union scope>` byte for byte, where `<head>` is the
payload's recorded `head`, with no untracked section, in a fixture whose
changes all lie inside the union scope; with an uncommitted modification to an
in-scope tracked file and an untracked in-scope file present in the exec root
at claim, neither appears in the recorded `run.diff` body; for the integration
check, on a fixture run driven with no dispatch (no `dispatch-opened` or
`dispatch-closed` event in the run) whose two issues are `done` and whose
second write-class step recorded its `issue.diff` with a `head` committed in a
worktree and never cherry-picked onto the shared branch, `step claim
review@0#0` returns CONFLICT, the row is `waiting-human` with `park_class`
`integration-unverified` and a `park_reason` equal to the §3.1 rendering for
that step and that head byte for byte, the run has no `run.diff` artifact, the
row has no lease (`owner` empty, `attempt` unchanged), and `next` offers no
run-scoped row; after the commit is cherry-picked onto the shared branch and
`step resolve review@0#0 --as retry`, the next `step claim review@0#0` succeeds
and its `run.diff` body contains the picked change; on the same fixture with
the second writer's head hand-resolved onto the branch (no patch match),
`step annotate STEP-N --integrated-sha <branch sha>` followed by `--as retry`
also lets the claim succeed; and on a dispatch-driven fixture whose close
recorded every head in `integration.checked`, the claim's `IsAncestorFn` and
`PatchContainedFn` seams are called zero times; a
run started outside a checkout is refused at activation with the D5 message (the §3.5 template, rendered for the
empty field(s)) and leaves zero step rows; every existing bundle fixture is byte-identical; no
operator surface renders `<PREFIX>-0`, or any `-0` issue id, for a run-scoped
row: on that fixture `next`, `step list`, `step show`, `events list`, and `run report` each show `review@0#0` under the §3.1 display rule,
`""` in the `--json` `issue` field and `-` in a text issue column. For the
park: with `review.findings` recorded so that two of three clusters reduce to
`high`, `reconcile@0` parks with `park_reason` equal to the §3.3 rendering byte
for byte; `step resolve reconcile@0 --as retry`, `--as rerun-gates`, and `--as
abandon-issue` are each refused with the §3.3 message for that value; `--as
skip` routes `reconcile@0` `skipped`, skips its run-scoped `after_fired`
successors, and the run rolls up `done`; `--as override-pass` routes it `done`
and the run rolls up `done`; an issue-scoped `aggregate` parked by a matched arm
in the same fixture renders the template with `--as abandon-issue` in its list
and refuses `--as retry`; a T3 park on either scope keeps its own reason and
accepts `--as retry`.

Phase 3, run-scoped loop cluster. `runs.loop_count`; `enterLoop` by subject;
non-convergence over `run.diff`; `dispatch close` admits `run.diff` heads; V51b
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
  R2c and the claim's integration check make run-scoped rows post-integration
  by construction; a pre-integration run step is a different readiness rule
  and a separate design.
- The reference instance's contracts and packet files for the run-scoped judges.
  Instance config lives outside this repository.
- A per-run opt-out flag on `run activate` (§3.5).
- A composed park reason and a sticky-cause refusal for an executor step's
  matched `waiting-human` arm. A re-execution can change its payload, so `retry`
  is a real remedy there and the class-word reason stands. Deferred, unassigned,
  triggered when an operator reads one.
- Extending `--drop-interposed` to `--as skip`. The interposed-targets refusal
  guards `override-pass` alone (human.go:668-680) and the flag is refused on any
  other value (human.go:450-458), while `skip` reaches the same
  `skipUnroutedTargets` with no acknowledgment, on both scopes today. The §3.3
  skip row states the consequence; the guard is a separate change, unassigned,
  triggered by the first run-scoped definition that declares an interposed
  target beside its `waiting-human` arm.

## 8. What gets worse

- A run's wall clock grows by the run phase, and the run cannot roll up `done`
  until it finishes; an operator who wants the old behavior retires the
  run-scoped definition.
- `run.diff` is read at claim. Two siblings claimed across a shared-branch
  commit read different commits; the artifacts say so through `head`, but the
  panel's verdicts then describe two objects. R2c excludes in-run writers at that
  point; an out-of-engine commit is the remaining source.
- Every claim of a run-scoped step under D5's predicate runs the integration
  probes over every write-class head the run recorded: on a run with no close,
  one `git merge-base --is-ancestor` per candidate and one `git cherry` per
  worktree head, repeated on each sibling's claim and on each retry, because
  the claim records no acceptance. A dispatch-driven run pays nothing here.
- A run driven without dispatch whose writer hand-resolved a cherry-pick
  cannot open its run phase until an operator runs `step annotate
  --integrated-sha` for that head; before this note such a run opened the
  phase with no operator action. The park names the step, the head and the
  remedy.
- `integration-unverified` is a new value in the park-class enum
  (engine-spec §2, amended in §10); a conductor or panel that routes on the
  class learns one more word.
- `run.diff` reads committed history only. Uncommitted or untracked work in the
  exec root at claim is not reviewed by the run-scoped panel: an operator's
  hand edit, or a writer's output left uncommitted in the shared checkout, is
  absent from the body, and nothing in the artifact flags that the checkout was
  dirty. Work reaches the integrated review by being committed to the shared
  branch before the claim.
- `commit_sha` as base attributes every shared-branch commit since run start to
  the run, including commits from another run on the same branch or an
  operator's own. The body's out-of-scope section discloses paths outside the
  union scope, and nothing else distinguishes them; this is accepted.
- `db.Step.IssueID == 0` becomes a meaningful value. Every reader that formats
  `model.FormatID(step.IssueID)` must apply the §3.1 display rule (`""` in
  JSON, `-` in a text column, the run named in messages) before it can show a
  run-scoped row, and phase 2's acceptance holds every operator surface to it.
- The `next` row's `issue` field is empty on run-scoped rows. A dispatcher that
  indexes by issue must tolerate the empty string.
- Phase 3 adds a second loop subject, so `enterLoop`'s counter reads and writes
  branch on the subject. The branch is one accessor pair, but it is a second
  place a counter can be read wrong.
- Every action step parked by a matched `waiting-human` arm, on either scope,
  refuses `retry` and `rerun-gates` and carries the §3.3 reason (§3.3,
  "Consequence for issue-scoped rows"). An operator or test that retried such a
  park got an identical re-park before and now gets a refusal naming the values
  that move the step; existing tests that retry a parked aggregate need the new
  expectation, and `resolveStep` performs one threshold evaluation it did not
  perform before.

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
  for days because nothing names a remedy. Mitigation: the park reason is the
  §3.3 template, carrying the match count and the two resolutions supported on a
  run-scoped step parked by a matched arm (`override-pass`, `skip`); `retry` and
  `rerun-gates` are refused there by the sticky-cause test and `abandon-issue`
  by the subject test, each with the §3.3 message naming the same two; §6 places
  the reason and the refusals in phase 2's scope and acceptance, so the park
  cannot ship without its remedy; phase 3 is scheduled before the first
  production workflow declares a run-scoped threshold.
- A run-scoped fix body under phase 3 commits to the shared branch while an
  operator is hand-editing the same checkout. Mitigation: the body runs under
  the tree lock every tree-declaring gate already takes (`RepoPaths.LockPath`,
  internal/engine/repopaths.go:28), and its recorded `head` is what the next
  review reads, so an interleaved operator commit shows up in the diff rather
  than vanishing.
- Someone backfills V12 or renumbers V51b after its removal. Mitigation: the
  `RuleIDs` comment records both as reserved, the way V12's does today.
- A fixture-driven run parks `integration-unverified` on every claim because
  its writers record in worktrees that the harness never integrates, and the
  phase is resolved with `--as skip` by habit. Mitigation: the park reason
  names the cherry-pick and the annotation as the remedies ahead of any
  resolution, the phase-2 acceptance drives the cherry-pick-then-retry path,
  and a skipped run-scoped review is recorded as skipped in the routing record
  and `run report`, so the habit is visible. Accepted residual: an operator who
  chooses to skip has chosen an unreviewed integration, and the record says so.

## 10. Proposed spec amendments

To be filed through docs/design/amendments.md; this note does not edit the spec.

- engine-spec.md §11.1, `[pipeline]`: add `scope?` with values `"issue"` (default)
  and `"run"`; a run-scoped definition has no `[match]` and binds per run under
  the zero/one/many rule of §3.1.
- engine-spec.md §11.1, `inputs` row: add `"run.diff"`, "the engine-computed VCS
  diff from the run's pinned starting commit to the shared checkout's HEAD commit
  resolved at claim, over the union of its issues' scopes, excluding uncommitted
  and untracked content, recorded at the claim of a run-scoped step that
  declares it; valid only in a run-scoped definition (V51)".
- engine-spec.md §11.3, clause 1: "the issue's loop counter increments" becomes
  "the subject's loop counter increments: the issue's for an issue-scoped
  definition, the run's for a run-scoped one"; the closing sentence "There is no
  other loop construct" stands, with "applied per subject" appended.
- engine-spec.md §11.4, `context`: `issue` becomes optional and `run?: {id,
  request}` is added, present only on run-scoped rows; `next row`'s `issue` is
  empty on run-scoped rows.
- engine-spec.md §2, "Park class" table: add `integration-unverified`,
  "the claim of a run-scoped step that reads `run.diff` or holds the tree found
  a write-class head the shared branch does not carry
  (`integrationVerdict`'s unintegrated list, asked at claim); the row parks
  before any lease is minted, and `step resolve --as retry` re-asks". The
  enum is closed and each value names one producer; this is a new producer
  with no existing word, which is why §3.3 could decline a new class for a
  distinction one pure call derives and this note cannot.
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
readable through `ListRunStepsTx`). The §3.3 park-resolution citations added in
the 2026-10-10 revision were read at 3c73f103 through `git show`, since the
checkout had moved on; where HEAD differs (human.go, saga.go, db/steps.go have
shifted by a few lines) the pinned numbers stand, per the header. The §3.1
"Integration at claim" citations added in the same day's second revision
(claim.go, dispatch_integration.go, dispatch.go, hold.go, annotate_integration.go,
db/steps.go, saga.go:1556) were likewise read at 3c73f103 through `git show`;
claim.go in particular has shifted by four lines at HEAD (the `claim_head` read
is at 354 there and 350 at the pin). One premise of that section is an
inference and is marked: that the only writer of `integration.checked` is the
close (a search for `EventDispatchClosed` writers outside tests found the two
sites in dispatch.go cited there and no other).
