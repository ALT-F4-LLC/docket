# Stage review: M1.S3 range engine-s2..f677a69 — conformance verdict

Reviewer: design-context session · 2026-08-03 · Scope: structured conformance
review of the stage's risk surfaces (not line-by-line); CI carries the test
evidence (10/10 checks green, qa 1208/1208, sweep 36/36).

## Verdict: ACCEPT

Zero blocking findings. Two clarifying amendments recommended — both codify
semantics the implementation already carries and this review judges correct;
neither changes behavior.

## Risk surfaces examined

**1. The two group-4 interpretation corners — both sound.**
- *Ordinal predecessors (the R3 fix).* `predecessorInstances` resolves a
  predecessor at the consumer's ordinal, falling back to the highest earlier
  ordinal — mirroring §7.4's per-input rule. Issue completion
  (`issueStepsComplete`) filters to the highest ordinal PER STEP NAME, with the
  correct argument recorded in-code for why an issue-wide maximum is wrong
  (it would skip `implement@0` entirely after one loop entry). This is the only
  coherent reading of §11.3; recommend codifying (amendment A below).
- *min_siblings default (the quorum fix).* `MinSiblings` is nil-when-unset;
  `quorumMet` treats nil or ≥ len(fanout) as J1 (plain join), applying quorum
  semantics only to a declared value below the sibling count. This is the only
  reading under which J3 (inputs over done siblings), J4 (on_fail per sibling),
  and J5 (quorum miss routes the step) are mutually coherent — an all-quorum
  default would make J4 meaningless. Recommend codifying (amendment B below).

**2. Saga discipline — conforms to §6.8 exactly.** Stages 0+1 share one
transaction (validation writes nothing); the token retires in the same commit
that records the artifact (`RetireStepTokenTx` beside the insert — "the
hinge"); post-artifact resumption requires no token; each gate stage commits
`gate-started`, closes the transaction, invokes the (pass-through) runner,
reopens to record; routing is one transaction spanning step, issue mirror
(`reconcileIssueAndRun`), run, and events. Inert stale-lineage routing still
finishes its saga stage — so a superseded lineage cannot resume forever.
Bonus found: `txdiscipline_test.go`, a static-analysis guard born from a real
single-connection deadlock (a pool query inside an open transaction hangs
permanently under SetMaxOpenConns(1)); the discipline is now enforced
structurally, not remembered.

**3. Threshold evaluation and the action stub — as revised-TDD specified.**
T4-before-T3 implemented with the derivation argued in comments (empty-set
aggregation short-circuits before the predicate is consulted — never guesses
an order); the S3 action stub emits kind = params.output with an EMPTY payload
array (exactly the input T4 short-circuits over) and stub:true on every
result, same as gates.

**4. Context determinism — F2's resolution is real.** Assembly reads the five
pinned sources; nothing below the snapshot line touches the `issues` table;
`issue_snapshot` supplies title/kind/labels/scope; the scheduler's live
`scope_globs` read (R4) is explicitly distinguished from context's snapshot
read, in code comments, so neither gets "fixed" into the other. Goldens
committed (`scripts/qa/fixtures/context/step-*.golden`) with the sensitivity
check.

**5. Genericity — holds at every user-visible surface.** The embedded
templates are genuinely stranger-first (standard-dev's comments describe
people, not automation; executor hints explained as "a role name, a team name,
or a person's name"). genericity.sh scans string literals, struct tags, and
templates — the actual surface — with argued exclusions (internal/model as MVC;
`review` handled as a pre-existing core issue status). A literal-string sweep
of workflow/engine/cli files for banned vocabulary returns only Go import
paths. The sentinel rewind guard probes every v7 table with
TestRewindGuardProbesEverySentinel asserting the probe list matches the DDL.

**6. Dormancy — mode-switch and sweep evidence accepted.** test_x_next.sh
untouched since before the stage (mtime pre-dates S3) and green; byte-compat
sweep 36/36 vs engine-s2; the repo's own tracker survived two live upgrade-path
rescues (group 1→2 and 3→4) with data intact — the strongest possible field
evidence for the guard.

## Recommended amendments (file as one DKT issue, two edits, both copies)

**A — §11.3 addendum:** "Predecessor satisfaction and issue completion resolve
per step name over instances at the highest existing ordinal ≤ the consumer's
(mirroring input binding); re-instantiation never spans steps outside the
`after_loop` chain."

**B — §11.1 `min_siblings` row:** default "all" means the plain join (J1) —
quorum semantics (J5's on_fail routing) apply only when `min_siblings` is
declared below the sibling count.

## Dispositions on acceptance

- `git tag engine-s3` (local only); close DKT-3.
- DKT-15: apply the §11.4 `instance` field edit to both spec copies; close.
- DKT-17: close (spec edits applied 2026-08-03).
- DKT-16: stays open — S4 migrates `gate_trail` → `gate_results`.
- File the A+B clarification issue; operator applies the two edits (both
  copies); close on commit.
- Tick M1.S3 in the 09 unit table with the stage summary.
