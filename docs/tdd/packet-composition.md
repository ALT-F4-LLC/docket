# TDD: packet composition — pinned files reach the packet (DKT-70)

Status: approved-in-shape, revised — 2026-08-05 · design review APPROVED IN
SHAPE with one required revision (**F1**, §1.1.1: the substitution token, since
a step-level literal list cannot serve fanout siblings with divergent
contracts) and two obligations on the frontmatter concession (§1.3.1 corpus
migration and the interim; §1.3.2 malformed-frontmatter timing and errors).
V-rule numbers corrected to V32/V33 — V29–V31 are taken. Implements
docs/design/engine-core.md §8 items 2
and 3 (the pinned file at its pinned version, and the files its frontmatter
declares, inlined) and its Properties clause ("packets read only hash-pinned
… files … same step ⇒ same packet, byte-identical"), under engine-spec.md §2's
layout/content split. Tracker unit: **DKT-70** (BLOCKER, gates M4). Spec of
record is engine-spec.md; deviations become DKT amendment issues per
docs/design/amendments.md, never silent changes.

**The seam is real and the citations check out.** I verified each independently
rather than inheriting them: `packetData` is `*Context + Emits + PayloadSchema`
(`render.go:38-45`) — there is no field a template could reach file content
through, so `--template F` cannot recover it either; `contextPins` returns
paths and hashes and states it "never opens a pinned file" (`context.go:218`);
`autoregister.go:157-167` pins everything non-registry and "never opens the
content again". The `== PINNED` section emits pointers, which inverts §8's
"nothing in it is optional … no pointers, no 'read if needed'".

**The hard part is not reading files — it is deciding which files belong to a
step without core learning what they are.** DKT-70 leaves that question open by
design. This note answers it.

## 1. Mechanism

Three pieces, each generic on its own terms:

1. **A step-level `packet` field** in the workflow grammar — an optional list
   of pinned file references (§1.1).
2. **A declared-includes key the engine follows one level** — the frontmatter
   question, answered (§1.3).
3. **A `packetData.Files` slice** the template composes explicitly, plus a
   default template that renders it (§1.4).

Content is resolved **from the pin record, verified against the recorded
hash** — never from the live tree unverified (§1.2).

### 1.1 `packet` — a declared reference list on the step

```toml
[[step]]
name     = "implement"
executor = "spec-author-api"
emits    = "patch"
packet   = ["contracts/spec-author.md"]      # paths, relative to .docket/config/
```

`packet` is an optional array of file references. The engine reads the named
files (subject to §1.2) and hands their bytes to the template. **Core attaches
no meaning to any entry** — not "contract", not "instructions". They are files
the step declares it needs, in declared order. The field name states the
*mechanism* (what goes into the work packet) rather than the *content* (what
the file says), which is where engine-spec §2 draws the line: packet layout is
core mechanics, packet content is instance data.

**This is why an explicit field beats a convention lookup.** The brief's third
candidate — `contracts/<executor>.md` — requires core to know that a directory
named `contracts/` is special and that an executor hint doubles as a filename.
Both are core learning what instance files mean, with a mapping rule baked into
core that only one instance's layout justifies.

#### 1.1.1 Substitution — how one step serves divergent siblings (F1)

A literal path list alone is **not sufficient**, and the reason is a defect the
design review caught in the first draft of this note. It is recorded here
rather than quietly fixed, because the reasoning is the mechanism.

A `fanout` step is **one** step whose siblings each carry a different executor
hint: `expand.go:151-165` emits one row per hint, in declared hint order,
setting `row.Executor = hint` per sibling. A single literal `packet` entry
therefore serves every sibling **identically** — so a four-way fanout whose
siblings need four different files cannot express that at all. My first draft
answered the `spec-author-<axis>` question by saying "the seven rows each write
`packet = [...]`", which conflated *policy-side* executor rows with *engine-side
workflow steps*. Seven siblings of one fanout step share one `packet` field;
there are no seven rows to write.

**The mechanism: entries admit a substitution token, and core does dumb string
substitution and nothing else.**

```toml
[[step]]
name   = "review"
fanout = ["reviewer-security", "reviewer-perf", "reviewer-api", "reviewer-docs"]
packet = ["contracts/{executor}.md"]     # four siblings -> four files
```

Core replaces the literal token `{executor}` in each entry with **that
sibling's resolved executor hint**, per sibling, and does nothing further: no
directory knowledge, no filename convention, no fallback rule, no knowledge
that the result is a "contract". **The instance authors the mapping rule as
syntax**, in its own workflow file, where it belongs. Core's entire
contribution is `strings.ReplaceAll`.

This covers both of the cases that motivated the field, with no special-casing
of either:

- **Divergent siblings** — `review`'s four siblings resolve to four different
  files via `contracts/{executor}.md`; all four exist and are pinned.
- **Shared contract** — `spec-project`'s seven `spec-author-<axis>` siblings
  share the **literal** entry `contracts/spec-author.md`, with no token. They
  need no per-axis file and no prefix-stripping rule, because the axis identity
  already reaches the rendered packet independently via `target:
  {{.Step.Executor}}` in the template (established by the DKT-64 evidence).
  **That is the generic answer to the `spec-author-<axis>` question** — the
  instance chooses literal-or-token per step, and core never infers which.

Scope of substitution, stated tightly so it stays mechanism:

- **One token, `{executor}`.** It is the only per-sibling identity a step row
  carries that varies across siblings of one step. I deliberately do not add
  `{name}`, `{instance}`, or `{ordinal}`: each would be a speculative surface
  with no motivating case, and every additional token is another chance for
  core to look like it is deriving meaning.
- **Substituted before resolution.** The result is an ordinary path and goes
  through §1.2's ladder unchanged. **A substituted path that resolves to no pin
  refuses exactly like a literal one** — no "the token expanded to nothing, so
  skip it" special case. Fail closed, identically.
- **A token on a step with no executor hint** (an `action`, `human`, or `vote`
  step) is a validation error at register time (V33), not a silent empty
  substitution that would produce `contracts/.md`.
- **Unknown tokens are not substituted and not errors at the string level** —
  `{foo}` stays literal, and then fails resolution as a missing pin with its
  path named. Erroring on unknown braces would make core the arbiter of a
  filename grammar; letting the pin check fail names the exact path the author
  wrote, which is the more useful message.

Genericity note on the name: the gate's banned set is `model|prompt|llm|agent|
brief`. `contract` is *not* banned, and I still avoid it as a field name —
`contract` in a workflow grammar reads as a promise about content. `packet`
matches the existing noun already in core (`RenderResult.Packet`, `packetData`,
`packets/default.tmpl`), so no new vocabulary enters at all.

### 1.2 Resolution is pin-verified, and refuses rather than drifts

For each entry, in declared order:

| State | Outcome |
|---|---|
| pinned, bytes hash-match | inlined |
| pinned, bytes differ | **CONFLICT** (exit 4), both hashes named |
| pinned, file absent | **NOT_FOUND** (exit 2) — the pin says the run needs it |
| **not pinned** | **VALIDATION_ERROR** — see below |

The first three are `templateSource`'s existing ladder (`render.go:136-187`)
applied to a second class of file; reusing that reasoning verbatim is
deliberate, since DKT-70 itself names it as the precedent for doing this
safely.

The fourth row differs from the template's, and the difference is the point. An
unpinned `--template F` renders unverified and *reports* the gap
(`TemplatePinned: false`) because the operator chose that file at render time.
A `packet` entry is chosen at *authoring* time and pinned at *activation*, so an
unpinned entry means the file was not in `.docket/config/` when the run
activated — the run has no snapshot of it, and reading the live tree would
break the byte-identical property outright. Refusing is the only answer
consistent with §8's Properties clause.

This preserves snapshot-pinning exactly: content is admitted only when it
hashes to what activation recorded. Same step ⇒ same packet, mid-run, forever —
or an explicit refusal naming both hashes.

### 1.3 Frontmatter: the engine follows one declared key, and parses nothing else

**The brief's sharpest question.** engine-core §8 item 3 says the fragments
"the contract's frontmatter declares" are inlined — which sounds like core
parsing an instance file's structure, and that is exactly the line genericity.md
draws.

**Decision: the engine parses one thing — a YAML frontmatter block's
`packet_includes:` list — and nothing else.**

```markdown
---
packet_includes:
  - fragments/testing-discipline.md
  - fragments/commit-conventions.md
---
# (body)
```

Justification, and the boundary:

- **What core learns:** that a file may open with a `---`-delimited block, and
  that one key in it, spelled `packet_includes`, holds paths to more files for
  the same packet. That is a *generic declared-includes mechanism* — the same
  idea as an `#include` or an import list — and it carries no vocabulary about
  what any file says.
- **What core does not learn:** every other frontmatter key is **ignored
  entirely**, not validated, not surfaced, not errored on. An instance's
  `title:`, `owner:`, or anything else passes through untouched. Core never
  reads them, so it never has an opinion about them.
- **Depth is exactly one.** Includes do not themselves include. This is a hard
  stop, not a limitation to lift later: unbounded recursion needs cycle
  detection and a depth cap, and every level makes the closure size harder for
  an author to predict at the moment §1.5 needs it to be predictable. One level
  matches §8's structure precisely — the file, and the files it declares.
- **Resolution rules are §1.2's, unchanged.** An include must be pinned and
  hash-match. A dangling include is a refusal, not a silent omission — the
  failure mode this whole patch exists to eliminate.
- **The frontmatter block is stripped from the inlined body**, since it is
  engine-directed metadata rather than content.

#### 1.3.1 `packet_includes` is the ONE engine-read frontmatter key

Obligation carried by the review's approval of this concession, stated so the
corpus wave has an unambiguous target.

`packet_includes` becomes the **only** frontmatter key any engine code reads,
now or later, and it is authored with **explicit paths** — not names to be
expanded against a directory. A second key describing the same relationship is
the drift this patch exists to remove: two sources for one fact, disagreeing
silently.

The M2b corpus currently declares fragment relationships as `fragments: [names]`
— bare names, not paths, resolved by nothing in core. **That key retires**; it
does not coexist. Retirement happens in a small mechanical corpus wave after
this patch ships (sentinel-checked; the reviewer cuts its kickoff), not in this
patch, because this patch is the ONE session authorized to edit engine source
since S7 and rewriting instance prose is neither engine source nor its risk
profile.

**The interim, stated plainly: until that wave lands, packets compose only what
`packet` lists.** A corpus file whose old `fragments:` key names three
fragments contributes **zero** of them — the engine does not read that key, and
will not grow a compatibility path that reads it. Packets in the interim are
therefore correct-but-thin: the declared files themselves inline (which is the
whole of DKT-70's blocker), and their fragments arrive when the corpus is
re-authored. No silent partial behaviour is introduced, because the engine
never claimed to read `fragments:` at all; the thinness is visible as a missing
section rather than a wrong one.

Core reads `packet_includes` and ignores every other key, so a corpus file
still carrying `fragments:` during the interim is neither refused nor
misread — it is simply not consulted for that key.

#### 1.3.2 Malformed frontmatter: where it refuses, and with what

Obligation two. These files are **pinned, not registered**: activation hashes
them and never opens them (`autoregister.go:157-167`), so activation has no
parse step to refuse in and inventing one would mean opening pinned content at
activation purely to validate an instance file's syntax.

**Therefore a malformed `packet_includes` refuses at claim/render time — the
same moment as pin verification**, which is when the bytes are legitimately
opened and hash-checked. Timing is consistent with §1.2's ladder because it is
literally the same pass: verify hash, then parse.

Fail closed, in every case:

| Condition | Outcome |
|---|---|
| frontmatter block opens `---` and never closes | VALIDATION_ERROR, file named |
| block is not parseable YAML | VALIDATION_ERROR, file named, parser message included |
| `packet_includes` present but not a list | VALIDATION_ERROR, file named |
| a `packet_includes` entry is not a string, or is empty | VALIDATION_ERROR, file named |
| an entry escapes the config dir (`..`, absolute) | VALIDATION_ERROR, file named |
| `packet_includes` absent, other keys present | **not an error** — body inlined, keys ignored |
| no frontmatter at all | **not an error** — body inlined as-is |

Never a warning, never a skip, never a partially-composed packet. A packet that
silently omitted a declared include would reproduce DKT-70's exact failure
signature — an instance doing everything right, observing nothing, concluding
it was at fault — one level deeper and harder to find. Each row above is a
named test in §4.2.

**The alternative I rejected: let the instance template compose explicitly**
(no engine parsing; the workflow lists every fragment in `packet`). It is more
generic — core parses nothing — and I rejected it on two grounds. First,
engine-core §8 assigns the declaration to the file's own frontmatter, and 05 §1
assigns assembly to the engine; moving it to the workflow author contradicts
both and pushes a per-file fact into every step that names the file. Second, it
scales badly in the direction the corpus actually grows: a fragment added to a
widely-shared file would require editing every step in every workflow that
references it, and the first one missed is a silently-thinner packet. The
engine following one explicitly-named key is a smaller genericity cost than
that.

If the reviewer disagrees, this is the cleanly separable decision in the note:
drop §1.3 entirely and `packet` still works, with instances listing fragments
explicitly. Nothing else in the design depends on it.

### 1.4 The template sees files; the default renders them

`packetData` gains one field:

```go
// Files are the step's declared packet files, resolved in declared order:
// each entry's path, hash, and verified bytes. Core attaches no meaning to
// any entry; the template decides how they appear.
Files []PacketFile
```

with `PacketFile{Path, SHA256, Body string}` — path and hash retained so
provenance survives into the rendered packet.

Ordering is fully determined: declared `packet` order, and after each entry
immediately its own `packet_includes` in declared order. Deterministic and
stable, which R9 requires and §4's golden tests assert.

De-duplication: a file reachable twice (declared directly and via an include,
or via two includes) is inlined **once**, at its first position. Repeating it
would inflate the closure for no gain.

The default template gains a section rendering each file's body between
delimiters carrying its path and hash. `== PINNED` **stays** — it is still the
honest list of what the run pinned, and now the files that were inlined are
also legible as content rather than only as pointers.

### 1.5 Closure size: counted where the spec says to count it

engine-core §8 records closure size on the step, and §11.1's caps make an
oversized closure "an engine error AT EXPANSION TIME — the fix is a
pipeline/contract change, visible before spend" (implemented at
`activate.go:903-935`).

Inlining bodies makes closures materially larger, so `workflow.ContextSize`
must include the declared files' sizes — otherwise the recorded figure
understates real cost and the caps stop binding, which is precisely the "silent
107KB spawn" §8 forbids. At expansion the bytes are known: activation has just
pinned and hashed them.

**This is the one place the fix reaches beyond render.** It is not scope creep;
it is the difference between the caps meaning something and meaning nothing.

### 1.6 Registration and validation

Two new validation rules. **The numbers are V32 and V33**: V29 and V30 are
taken (the schema-aware `ordered_enum` half) as is V31 (`validate.go:18-27`),
so the next free numbers are V32 onward. Verified against the rule list rather
than assumed.

- **V32** — `packet` entries are non-empty strings, relative, and lexically
  contained within the config directory (no absolute paths, no `..` escape).
  Checked **after** token substitution is accounted for lexically — a token
  cannot be used to smuggle an escape, since `{executor}` values come from the
  workflow's own `fanout`/`executor` fields and those are validated shapes, and
  the substituted result is re-checked for containment at resolution time
  regardless. This is shape validation at register time, in the spirit of V25's
  "SHAPE ONLY at this stage": the file's *existence* is an activation-time
  fact, not a registration-time one, and a workflow must remain registerable
  before the files it names are added.
- **V33** — a `{executor}` token may appear only on a step that has a
  per-sibling executor hint (an `executor` step or a `fanout` step). On an
  `action`, `human`, or `vote` step the token has nothing to resolve to, and
  refusing at register time is better than an empty substitution yielding
  `contracts/.md` and a confusing missing-pin error much later.

`packet` itself is permitted on **any** step class. I considered restricting it
to executor steps and rejected that: a `human` step benefits from the same
composed packet, and `step render` is not executor-only. Only the *token* is
class-restricted, by V33.

At **activation**, an entry naming a file that was not pinned is refused with
the run — the same fat-transaction, leaves-nothing-behind stance as the context
cap. Discovering it at claim time would have already cost a dispatcher a round
trip.

`skills/docket/SKILL.md` gains the `packet` row in its grammar table in the
same PR, per CLAUDE.md — a stale table is drift and blocks review.

## 2. Alternatives considered

**A. Convention lookup, `contracts/<executor>.md` baked into core.** Rejected —
§1.1 and §1.1.1 give the full argument. Core would learn that a directory name
is special and that a routing hint is a filename, and the `spec-author-<axis>`
family would need a prefix-stripping fallback in core to collapse seven hints
onto one file.

Worth being precise about what the substitution token does and does not concede
to this alternative, since they look similar: **the token is the same
*expressive power* with none of the *knowledge*.** Under the convention, core
holds the rule and every instance obeys it. Under the token, the instance
writes the rule as a path and core performs a string replacement — it does not
know `contracts/` from `checklists/`, cannot fall back, and cannot collapse
hints. The `spec-author-<axis>` case is decided by the instance *choosing a
literal entry*, which under a baked-in convention it could not do at all.
Zero-config is the convention's real virtue, and `packet` recovers most of it:
`workflow init --template` ships steps with the field already filled in.

**B. A generic include/compose helper over pinned paths, template-supplied.**
The brief's first candidate. It is attractive — the instance supplies the
template and the template names the files, so core parses nothing. Rejected as
the *primary* mechanism because the step→file association would live in the
template rather than the workflow, which means (i) it cannot vary per step
without a template per step, and (ii) closure size is unknowable at expansion
time, so §1.5's caps cannot bind. A template helper is a fine *addition* later;
it is not a substitute for the declaration.

**C. Store file bodies in the `pins` table at activation.** Rejected, though it
is the most robust option and deserves the record. It would make packets
immune to post-activation deletion (§1.2's NOT_FOUND becomes impossible) and
remove render-time file I/O entirely. Costs: a schema migration (a v11 with a
column sentinel — v6's shape, not v7–v10's table shape, plus DKT-6's index
lesson), and turning `.docket/issues.db` into a content store that duplicates
git-versioned files. The existing hash-verify ladder already gives correct
behaviour without either. **Confirmed rejected for this patch at design review,
and filed as its own tracker unit (DKT-71)** — the migration discipline makes
it a different-sized change than this one.

**D. Inline into the context bundle rather than the packet.** Rejected. It
would put file bodies in `step claim`'s response for every consumer, including
those that never render, and it contradicts `context.go:218`'s explicit
contract. The split engine-core §8 draws is bundle = pointers + artifacts,
packet = the assembled thing. This note keeps that split and changes only the
packet side.

**E. Do nothing in core; have the harness read the files.** Rejected on the
spec: 05 §1 lists packet assembly among "semantics supplied by the engine, not
restated per pipeline", and every harness re-implementing hash verification is
how the unread-pointer hole (upstream E-6) reopens.

## 3. Genericity argument

The rule: core carries zero agent/LLM vocabulary; instance files are opaque.

- **No banned vocabulary.** `packet`, `packet_includes`, `Files`, `PacketFile`
  — the gate's set (`model|prompt|llm|agent|brief`) is untouched, in production
  code, templates, **and tests** (the gate scans `internal/`). Test fixtures use
  neutral filenames (`docs/checklist.md`), never `contracts/spec-author.md`.
- **Core reads bytes and one declared key.** It never interprets a file's
  content, never branches on a filename, never treats any directory as special.
  A file's meaning is entirely between the instance and its worker.
- **The one concession is named and bounded.** §1.3 has the engine parse
  `packet_includes` — a generic declared-includes mechanism, one level deep,
  every other key ignored. It is argued on its merits above, and it is
  separable if the reviewer judges the cost too high.
- **Stranger test.** A two-step docs-review workflow: the `review` step
  declares `packet = ["checklists/docs-review.md"]`, that file's frontmatter
  declares `packet_includes: [style-guide.md]`, and a human reviewer claims the
  step and receives the issue, the checklist, and the style guide as one
  document instead of three paths to go find. Useful, no agents anywhere,
  comprehensible from public docs alone.

## 4. Test plan

Test-first. Go tests in `internal/engine` and `internal/workflow`; CLI proof in
`scripts/qa/` with `XDG_CONFIG_HOME` sandboxed — never the real
`~/.config/docket`, never the real trust store.

### 4.1 Go — resolution ladder (the core of the fix)

| Case | Assertion |
|---|---|
| pinned, hash matches | body inlined verbatim |
| pinned, file edited after activation | CONFLICT (exit 4), **both** hashes in the message |
| pinned, file deleted | NOT_FOUND (exit 2), naming the run |
| present on disk, never pinned | VALIDATION_ERROR (not a silent read) |
| `packet` absent/empty | packet byte-identical to today's output — the no-regression assertion |

### 4.1.1 Go — substitution and fanout siblings (F1)

The case the first draft could not express. Both variants are tested, because
the mechanism's correctness is that it serves them *without special-casing
either*.

- **Divergent siblings:** a four-hint `fanout` step with
  `packet = ["contracts/{executor}.md"]` and four distinct pinned files →
  each sibling's packet inlines **its own** file, and no sibling sees another's
  content. Asserted per sibling, by instance id.
- **Shared literal:** a seven-hint `fanout` step (hints sharing a common
  prefix, in the shape of the `spec-author-<axis>` family) with the literal
  entry `packet = ["contracts/spec-author.md"]` → all seven siblings inline the
  same single file; **no per-axis file is required and no prefix-stripping
  happens anywhere**. Additionally asserted: each sibling's rendered packet
  still carries its own distinct `target:` line, so axis identity survives
  independently of the shared file.
- **Mixed:** one step whose `packet` holds both a literal and a token entry →
  both resolve, in declared order.
- Substitution is `strings.ReplaceAll`-simple: a hint containing no special
  characters substitutes verbatim; the token may appear more than once in one
  entry.
- **Substituted path resolving to no pin → refuses exactly like a literal**
  (VALIDATION_ERROR / NOT_FOUND per §1.2), with the *substituted* path in the
  message, not the template form — the operator needs the path that was
  actually looked for.
- A non-fanout `executor` step substitutes its own single hint.
- Unknown token `{foo}` → left literal, then fails resolution naming the
  literal path (not a brace-grammar error).
- Determinism: sibling order is declared hint order, and two renders of one
  sibling are byte-equal.

### 4.2 Go — includes

- One level resolved, in declared order, frontmatter stripped from the body.
- Non-`packet_includes` frontmatter keys ignored, not validated, not errored.
- A file with no frontmatter → body inlined as-is.
- Dangling include (declared, not pinned) → refusal.
- **A file carrying the retiring `fragments:` key contributes nothing from it**
  and is not refused (§1.3.1's interim, as an explicit test so the interim is a
  proven property rather than a hope).

#### 4.2.1 Malformed frontmatter — fail closed, at render time

One named test per §1.3.2 row, each asserting refusal **at claim/render time**
(never at activation, never a warning, never a partial packet):

| Fixture | Expected |
|---|---|
| unterminated `---` block | VALIDATION_ERROR, file named |
| unparseable YAML | VALIDATION_ERROR, file named, parser message surfaced |
| `packet_includes` a scalar, not a list | VALIDATION_ERROR, file named |
| entry non-string / empty | VALIDATION_ERROR, file named |
| entry escaping the config dir | VALIDATION_ERROR, file named |
| `packet_includes` absent, other keys present | success, body inlined, keys ignored |
| no frontmatter | success, body inlined verbatim |

Plus a timing assertion: a workflow whose packet file has malformed
frontmatter **activates successfully** (activation never parses pinned files)
and refuses on the first `claim --render` — which is the pin-verify moment.
- An include's own `packet_includes` is **not** followed (depth exactly one),
  asserted explicitly so the stop is a tested property rather than an
  implementation accident.
- Diamond: two files including the same third → inlined once, at first
  position.

### 4.3 Go — determinism (R9)

- **Golden packet test:** fixed fixtures render byte-identically across runs.
- Rendering the same step twice → byte-equal.
- Re-ordering entries in `packet` changes output order (order is declared, not
  filesystem or map order).
- Two activations of one config directory pin identically (the existing
  ordering property, extended to this path).

### 4.4 Go — grammar, validation, activation

- V32: absolute path, `..` escape, and empty-string entries each refused at
  register time, with the path named.
- V33: a `{executor}` token on an `action`, `human`, or `vote` step refused at
  register time; the same token on `executor` and `fanout` steps accepted.
- A workflow naming a not-yet-existing file **registers** successfully (shape
  only), then **refuses at activation** — the split V25 established.
- `packet` (without a token) accepted on executor, action, human, and vote
  steps.
- The new rules appear in the rule-list constant alongside V27–V31, so the
  numbering cannot silently collide with the schema-aware half.
- Activation refusing an unpinned entry leaves **no** partial run (the fat
  transaction, asserted by row counts).

### 4.5 Go — closure size

- `ContextSize` grows by the declared files' bytes, includes counted.
- A step whose files push it over `context.error_bytes` is refused **at
  expansion**, naming the instance and both numbers — not at claim, not at
  render.
- The warn cap emits a `ContextWarning` without refusing.

### 4.6 QA section — the CLI proof

1. Sandboxed init; a config dir with a checklist file and one fragment it
   declares via `packet_includes`; activate.
2. `step claim --render` → the rendered packet **contains the checklist's body
   text and the fragment's body text**, not merely their paths. This is the
   literal inverse of DKT-70's observation and the acceptance bar.
3. Edit the checklist post-activation, re-render → exit 4, both hashes shown.
4. Delete it, re-render → exit 2.
5. `step render` and `claim --render` agree byte-for-byte.
6. **Fanout, divergent:** a fanout step with `{executor}` substitution and one
   file per hint → each sibling's packet carries its own file's text, proven
   through the CLI by rendering every sibling and diffing.
7. **Fanout, shared:** a fanout step with a literal shared entry → every
   sibling carries the same file, each with its own `target:` line.
8. Genericity: grep the section's fixtures for the banned words → zero hits.
   Fixture executor hints are neutral (`desk-a`, `desk-b`), never
   `spec-author-*` or `reviewer-*`.

### 4.7 Gates that must stay green

Full `scripts/qa.sh` plus `scripts/qa/genericity.sh`; `go build ./...`;
`go test ./...`. **No schema change and no migration** under the chosen design
(§2.C is the option that would have required one) — `currentSchemaVersion`
stays 10, and that is an assertion this design makes rather than a detail.

The existing stranger-test section (ZH) must stay green **unchanged**: a
workflow declaring no `packet` renders exactly the packet it renders today.
