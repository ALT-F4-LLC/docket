# Stage review: M1.S5 range engine-s4..6c55871 — verdict

Reviewer: design-context session · 2026-08-03 · Scope: the four recorded
judgment calls examined in code, plus targeted reads of the aggregate, held,
threshold, and action-exec surfaces. CI green; qa 1393/1393; sweep 36/36.

## Verdict: ACCEPT WITH ONE REWORK — the aggregate's input seam

Everything else stands: the lower-median tie rule ships with its
direction-agnostic argument in code; U3 landed on the real tracker (the §2
trap, predicted and survived a third time); the held machinery's ZG28
assertions (blocks downstream, blocks stop, approve → ordered routing) prove
H1–H20 as revised; SplitStdout's stderr-cannot-corrupt-the-document rationale
is a genuine security property; the §4.8 action exemption was necessary; the
step-held event kind is correctly enumerated.

## F1 (HIGH) — the aggregate is fed by hand, and production has no hand

Judgment call #1 resolved §7.2's "input payload" as THE STEP'S OWN RECORDED
PAYLOAD (`runAction` → `stepPayloads(conn, step, nil)`). But an action step
that has never run has no recorded payload — so ZG28 makes the flow work by
CLAIMING AND COMPLETING `reconcile@0` with the clusters (test_zg_workflow.sh
~3139), exploiting the fact that `claim.go`'s §6.15 branch refuses human and
vote kinds but not action. Two consequences:

1. **In production, nothing records that payload.** The fixture's `reconcile`
   would reduce over nil — empty output, nothing held, threshold vacuously
   pass. The QA proves a flow only the QA can produce.
2. **The implied production fix is dispatcher glue** — a relay that claims
   the action step and copies the predecessor's payload onto it. That is
   reconcile.py reborn as a claim+complete shim, which D13 exists to forbid:
   action steps are the engine's deterministic half (AC-2), and S3's stub ran
   them engine-side with no claim.

The judgment call's cited argument conflates two different questions: V29
rejected the predecessor's SCHEMA as the ORDER source (correct, kept); it
says nothing about the predecessor's ARTIFACTS as the DATA source — which is
what `inputs` resolution (engine-spine §6.7) exists to provide, ordinal-aware
and deterministic.

**The fix, scoped small:**
- `runAction`: the builtin's input = the concatenated payloads of the step's
  declared `inputs` artifacts, resolved per §6.7's rules (done instances,
  declared order, ordinal-scoped at loops). Trusted-command actions are
  UNCHANGED — their input channel is the stdin context, which already carries
  input artifact bodies.
- `claim.go`: `action` joins the §6.15 unclaimable branch ("resolved by the
  engine, not by a worker"); refusal-matrix row + test.
- V31: `action = "aggregate"` requires non-empty `inputs` (an aggregate with
  no input can never compute — V29's spirit). Update RuleIDs set equality.
- Fixture: `reconcile` gains `inputs = ["synthesize.findings"]` (upstream 05
  mirrored by the operator's reviewer, with DKT-27's payload line).
- ZG28: DELETE the reconcile hand-complete and the "recorded on synthesize
  AND on reconcile" comment — the engine now feeds it, and the test becomes
  the proof of the engine-side flow it was meant to be. Assertions unchanged.
- Spec: the applied DKT-23 sentence gains its source clause (both copies —
  applied by the reviewer with this verdict): the input arrives from the
  step's declared `inputs` artifacts. File DKT-28 recording it.

## The other three judgment calls — endorsed

- §4.8's action exemption: necessary, correctly implemented (without it V29
  makes the fixture's reconcile uncompletable by anything).
- Stdin/SplitStdout: endorsed; the split is a security property and the gate
  capture's C1 interleaving is unchanged where it matters.
- step-held event kind (30 → 31): correctly enumerated, closed-set guard
  updated.

## Close-out (after the fix session)

Tag engine-s5; close DKT-5, DKT-22, DKT-23, DKT-24, DKT-25, DKT-26, DKT-27,
DKT-28; upstream 05 mirror (payload + inputs on reconcile) applied by
reviewer; 09 ticked with the stage summary.
