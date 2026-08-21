# TDD: input payloads reach the packet (H-12), and the E-11 falsification

Status: approved as written — 2026-08-05 · design review APPROVED, every
citation verified against source; implement per §4's test plan unchanged. The
E-11 verdict in Part II was CONFIRMED and upgraded by the reviewer against
RUN-3 production data (§7.1). Tracker unit: **DKT-78** (engine-spec §11.4
shape amendment). Implements
docs/design/engine-spec.md §11.4's context-bundle `inputs` shape for the
structured half of an artifact, closing H-12 from the M4 findings register.
Also records the **repro verdict on E-11**, which the register marked PROBABLE
and instructed be confirmed before any design: it did not reproduce, and no fix
is proposed. Precedent: DKT-70 (packet composition), whose `Files` field was
added for the identical reason and whose §1 sentence — "the field whose absence
made the corpus inert" — describes this defect exactly. Spec of record is
engine-spec.md; deviations become DKT amendment issues per
docs/design/amendments.md, never silent changes.

## Part I — H-12: inputs carry bodies but not payloads

### 1. Mechanism

An artifact has two halves. `body` is prose; `payload` is the structured half,
validated for shape at completion and stored in its own column
(`internal/db/steps.go:600-609`). Both are recorded, both are resolved, and
**only one reaches the packet.**

`ContextInput` (`internal/engine/context.go:78-86`) declares four fields —
`Artifact`, `Kind`, `ProducerStep`, `Body`. There is no payload field.
`ResolveInputArtifacts` already returns whole `*db.Artifact` values with
`Payload` populated (`context.go:339-367`), so the data is in hand at assembly
time and is dropped on the floor when the bundle is built.

The consequence RUN-3 recorded (ledger 27): a `synthesize` step whose contract
requires clusters to carry their input severities **unchanged** received judge
prose with the severities stripped. The executor recovered them by opening a
copy of the database directly — and nearly failed to, because the local SQLite
could not open a file written by a newer one. **That cannot be the path.** An
executor reaching around the packet into engine storage voids every property
the packet exists to provide: the pinning, the byte-identical reproducibility,
and the §6.6 guarantee that assembly reads no live state. A packet that forces
that reach is not merely incomplete; it is unsound.

**The fix is one field, carried the same way `Body` is.**

```go
type ContextInput struct {
    Artifact     string `json:"artifact"`
    Kind         string `json:"kind"`
    ProducerStep string `json:"producer_step"`
    Body         string `json:"body"`
    // Payload is the artifact's structured half as stored — JSON text, or ""
    // when the producing step declared none. Carried VERBATIM: core validated
    // its shape at completion and attaches no meaning to its keys.
    Payload string `json:"payload,omitempty"`
}
```

`omitempty` so an input with no payload serializes exactly as it does today —
the golden packets for payload-less steps stay byte-identical, and the change
is invisible to every workflow that does not use payloads.

### 1.1 Verbatim, and why that is the whole contract

The payload is copied from the column to the field with **no re-encoding, no
re-validation, and no key inspection.** Core validated the shape once, at
completion (§6.8 stage 0), and the schema register is S5's business. Re-parsing
here would let assembly reject an artifact the engine already accepted — a
packet that fails on data the ledger holds is a worse failure than the one
being fixed.

This is also what keeps H-12's fix generic: `severity` is a key in an
instance's schema. Core carries the bytes and never learns the word.

### 1.2 Byte budget

`ContextMeta.InputsBytes` (`context.go:624`) sums the input sizes for the
context cap. It must add `len(in.Payload)`, or a bundle could exceed its
declared cap while reporting that it did not. This is a one-line change in the
same commit and is not optional — the cap's honesty is the reason the field
exists.

### 1.3 The default template

The embedded packet template renders inputs. It gains a conditional payload
block — rendered only when the payload is non-empty, so packets for
payload-less steps are unchanged byte-for-byte. Per DKT-70's rule, **the
template decides how it appears; core only carries it.**

### 2. Alternatives, with verdicts

**(A) Executors call `docket artifact show` for payloads — REJECTED.** It is
the reach-around with a CLI in front of it. The packet stops being the entire
contract (02 §8), the executor needs a second round trip and its own error
handling, and nothing pins what the second call returns — the artifact could
change between the packet and the fetch. It also cannot work for the case that
motivated it: the executor would still need to know *which* artifacts to fetch,
which is what the bundle is for.

**(B) Inline the payload into `Body` at assembly — REJECTED.** It needs no new
field, but it destroys the body/payload distinction the schema deliberately
draws, corrupts prose with JSON for every consumer that wanted prose, and makes
the concatenation irreversible. Two halves recorded separately must arrive
separately.

**(C) A new `payloads` array beside `inputs` — REJECTED.** It carries the same
bytes while breaking the one-to-one correspondence between an input and its
data: consumers would have to re-join two lists on artifact identity, and the
join has no key that is guaranteed unique. The payload belongs to the input.

**(D) Template-only fix via `--template F` — IMPOSSIBLE, and the precedent
matters.** A template cannot render a field the data does not carry. This is
verbatim DKT-70's finding about `Files`, and it is why that fix was a struct
field rather than a template change. Recording it here so the same option is
not re-litigated a third time.

### 3. Migration discipline

**None.** `currentSchemaVersion` stays 10. No table, column, or index changes:
the column exists, the DAO already reads it (`steps.go:688`), and the resolver
already returns it. This is plumbing between two points that both already hold
the data.

### 4. Test plan

- an input whose producing step emitted a payload arrives with `payload`
  populated **and byte-identical** to what was recorded;
- an input with no payload omits the key entirely, and the rendered packet for
  a payload-less run is **byte-identical to today's golden** (the regression
  that protects every existing workflow);
- a payload containing characters that would corrupt the body if concatenated
  survives intact — falsifies alternative (B) rather than merely rejecting it
  in prose;
- `InputsBytes` accounts for payload bytes; a bundle whose payloads push it
  past the cap is refused rather than silently over-cap;
- the fanout case H-12 actually hit: a step consuming `review.*` receives every
  sibling's payload, each attributed to its own producer;
- assembly still reads no live state (§6.6): the existing
  `TestContextAssemblyReadsNoLiveState` extended to mutate the payload column
  between two assemblies and require identical output.

## Part II — E-11: REPRO FIRST, and the verdict is NEGATIVE

The register recorded E-11 as PROBABLE with a named suspect: all four DKT-75
review briefs carried DKT-76's change-summary, "likely keyed on the shared
instance name `implement@0`". The instruction was to confirm the mechanism
before designing. **I built the probe. It does not reproduce, and the suspected
mechanism is not present in the code.**

### 5. What was tested

A probe (`TestE11CrossIssueInputBleed`, run against clean HEAD and since
removed) built RUN-3's exact shape: **one run, two issues**, each with its own
`implement@0` step producing its own `change-summary` artifact, then asked what
issue A's `review` step resolved. Both paths were exercised — `ReadContext`,
and `RenderStep`, which is the one the executor actually travels
(wave.js issues `docket step claim <row.step> --render`).

Result: issue A's review received `SUMMARY-FROM-A` on both paths. The rendered
packet contained issue A's summary and did **not** contain issue B's.

### 6. Why it cannot be keyed on the instance name

`matchingArtifacts` (`context.go:400-422`) filters candidates on
`producer.IssueID != issueID` **before** comparing step names. Resolution is
issue-scoped by construction, and the instance name is matched only within an
issue. The harness side is keyed the same way: wave.js interpolates
`row.step` — the unique step id — into the claim, and `RenderStep(conn, id, …)`
takes that id. **There is no point in the chain where a bare `implement@0`
selects an artifact.** The hypothesis is falsified at the code level, not only
empirically.

### 7. What is real — the ambiguity that likely produced the report

One real thing sits underneath the observation. `ContextInput.ProducerStep`
renders as the bare instance name — `implement@0` — with no issue
qualification (`producerInstance`, `context.go:505-510`). In a multi-issue run,
**four briefs correctly carrying four different issues' summaries all label
their producer `implement@0`.** A reader comparing briefs across issues sees
one name on four different contents and reasonably concludes the wrong content
was delivered. The register's own words for E-4's parallel defect apply exactly
— "ambiguous with same-named instances across issues".

This is a **reporting** defect, not a resolution defect, and it is a plausible
and sufficient explanation for E-11 as observed. I am not proposing a fix under
this TDD: changing the rendered producer identity alters every packet's bytes
and every golden, which deserves its own round rather than a ride-along. It is
also possible the RUN-3 observation had an instance-side cause this probe
cannot see (a stale rendered wave.js, per H-3's stale-registry finding).

**Recommendation: reclassify E-11 from PROBABLE to NOT REPRODUCED, and open a
separate low-severity item for the unqualified `producer_step` rendering.** No
engine fix is proposed here. If the reviewer has RUN-3 evidence contradicting
the probe — particularly the actual rendered briefs — that evidence should
reopen it, and the probe above is the thing to re-run against it.

### 7.1 Reviewer's production-data verdict (2026-08-06): CONFIRMED, root cause found

The reviewer ran the check invited above against RUN-3's actual data, and it
exonerates the engine more strongly than the probe does:

- **`step_inputs`: all four DKT-75 review steps (12-15) consumed ARTIFACT-3 —
  DKT-75's OWN implement artifact.** Resolution was issue-correct in
  production, exactly as §6 argues it must be.
- **ARTIFACT-3's recorded BODY is DKT-76's change summary.** The corruption is
  *in the row*. It predates input resolution entirely.
- **Root cause (instance-side):** STEP-11 (DKT-75 implement) and STEP-21
  (DKT-76 implement) both completed with `--artifact-file` pointing at the
  identical session-scoped path `…/scratchpad/change-summary.md`. Concurrent
  same-kind writers shared one filename and cross-wired. The engine recorded
  the bytes it was handed — content is opaque to core, as it should be.

My alternate suspicion in §7 (a stale rendered wave.js, per H-3) is **retired**:
this is the instance-side cause the probe could not see. Register disposition:
E-11 → NOT REPRODUCED (engine exonerated on production data); new **H-13** =
shared scratchpad artifact path, instance-side, reviewer-owned, lands in the
dotfiles corpus before RUN-4; new **E-13** (low) = the unqualified
`producer_step` rendering of §7, deferred to its own round on golden-churn
grounds.

Worth stating plainly, because it is the lesson rather than the fix: the engine
faithfully recording a wrong byte-stream is not an engine defect, and the
packet's guarantees stop at the artifact boundary. Nothing in this TDD changes
that, and nothing should.

## 8. Surface documentation

`skills/docket/SKILL.md` needs no verb or flag change for Part I — the field is
additive inside an existing response shape. The context-bundle shape is
documented in engine-spec.md §11.4; that table gains the `payload` row in the
same PR.
