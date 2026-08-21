# TDD: claims, leases, and capability tokens (stage 2)

Status: draft — 2026-08-02 · implements docs/design/engine-spec.md §2 ("Steps,
claims, capabilities"), §4 (token transport), §11.4 (claim wire shape), and
engine-core.md §5 ("Claims are capabilities"; "Leases"), with §9 items 3 and 4 as
the acceptance bar. Tracker unit: DKT-2. Spec of record is engine-spec.md;
deviations are DKT amendment issues per docs/design/amendments.md, never silent
changes.

## 1. Scope

engine-spec.md §10 stage 2, verbatim:

> 2. **Claims/leases + capability tokens** on issues and steps-to-be.

"on issues and steps-to-be" is the whole of the stage's shape: the mechanism
lands on **issues** now, in the field layout stage 3's `steps` table reuses
verbatim. Nothing here is issue-specific — a lease is a lease.

In scope:

1. Schema **v6**: lease fields on `issues` (`owner`, `token_hash`, `expires_ms`,
   `attempt`), shaped for reuse by S3's steps.
2. `docket issue claim|heartbeat|release` — the issue-level analog of §1's
   `step claim|heartbeat|complete|fail` verb pattern.
3. Capability tokens: minted at claim, returned exactly once, stored only as a
   hash, transported via `DOCKET_TOKEN` env or stdin — never argv (§4).
4. Token-checked refusals on lease-mediated mutating verbs, per the §9.3 matrix.
5. Lazy expiry: re-claimable at `claim` with `attempt++`; read verbs compute
   *effective* status with **zero writes** (§2 "Runs, report, events", §6).
6. `docket config set|get` over the meta table, covering the engine defaults
   §1 names.

Out of scope, explicitly: everything in §10 stages 3–7. No step, run, workflow,
artifact, gate, trust, dispatch, or event surface lands here. **Schema stops at
v6.** `complete`/`fail`, the context bundle, and `--render` belong to steps and
therefore to stage 3; this stage ships the lease primitives they will consume.

**Genericity check (CLAUDE.md PR bar).** Every noun introduced — `claim`,
`heartbeat`, `release`, `lease`, `owner`, `token`, `attempt`, `expires_ms`,
`class`, `ttl` — is generic mutual-exclusion and liveness vocabulary. No agent,
model, prompt, brief, node, severity, or review concept appears. The stranger
test holds: a team of humans running a shared on-call runbook wants exactly
this — take the item, hold it while you work, lose it if you go dark. The
`executor class` knob is a bare string key for TTL lookup; core never interprets
its value.

## 2. Schema version span

This stage ships **v6**, the second of the v5–v10 span fixed in
docs/tdd/reliability-delta.md §2. That table is authoritative and is not
re-litigated here; this stage occupies its v6 row:

| Schema | Stage (§10) | Contents |
|---|---|---|
| v5 | 1 — reliability delta | shipped |
| **v6** | 2 — claims/leases + capability tokens | **this stage**; see §2.1 |
| v7 | 3 — workflows, steps, `next`, activation/pinning | later |
| v8–v10 | 4–6 | later |

### 2.1 v6 contents, exactly

Four columns on `issues`, added via the existing migrations-map pattern with the
`hasColumn` probe (so the migration is re-runnable, matching v5):

| Column | Type | Default | Meaning |
|---|---|---|---|
| `owner` | TEXT | `NULL` | lease holder's identity string; `NULL` = unclaimed |
| `token_hash` | TEXT | `NULL` | SHA-256 hex of the capability token; **never the token** |
| `expires_ms` | INTEGER | `NULL` | lease expiry, epoch milliseconds |
| `attempt` | INTEGER | `0` NOT NULL | claims made against this issue, ever |

Plus `CREATE INDEX idx_issues_expires_ms ON issues(expires_ms)` — the reap
predicate's index, and the one query shape S3's `next` will run at step scale.

These four names are the S3 contract: the `steps` table will carry them
verbatim, so the lease helpers written here are reused rather than reimplemented.

**Never-mutate rule (inherited, reliability-delta §2.1).** `expires_ms` is
milliseconds because it is a **new** column in a table whose *existing* columns
stay RFC3339 TEXT forever. `issues.created_at`/`updated_at` are untouched. New
columns may use ms; existing column formats are never mutated — that constraint
is what keeps every existing verb byte-compatible (§9 item 8).

**Dormancy.** All four columns are NULL/0 on every pre-existing row and on every
row created by an unmodified `issue create`. A repo that never claims has
`owner IS NULL` everywhere, and every read verb's output is byte-identical to
v5's. The columns are additive; `ADD COLUMN ... DEFAULT` backfills with no table
rewrite.

### 2.2 The defensive rewind guard

`Migrate` carries a per-version guard (stamped >= N but the structure is
missing ⇒ rewind and re-run). v6 gets the same: stamped >= 6 but
`issues.owner` absent ⇒ rewind to 5. The v6 migration is idempotent, so
re-running is safe.

## 3. Verb surface

Pinned by mirroring engine-spec §1's step-verb pattern down to the issue level.
§1 reads:

```
docket step     claim STEP-N        # atomic; mints a capability token
docket step     heartbeat|complete|fail   # token via DOCKET_TOKEN env or stdin
```

The issue-level surface is the same three ideas: take it, hold it, let go.

| Verb | Token required | Effect |
|---|---|---|
| `docket issue claim <id> --owner NAME [--ttl DUR] [--class C]` | no | CAS-atomic; mints a token; sets owner/expires_ms; `attempt++` |
| `docket issue heartbeat <id>` | **yes** | extends `expires_ms` by the TTL; does not touch `attempt` |
| `docket issue release <id>` | **yes** | clears owner/token_hash/expires_ms; leaves `attempt` intact |
| `docket issue close <id>` | if leased | ends the lease as a side effect of terminal status |

`claim`, `heartbeat`, and `release` are subcommands of `issue`, registered in
`internal/cli/issue_claim.go`, `issue_heartbeat.go`, `issue_release.go` — one
file per verb, matching the existing `issue_move.go` layout.

**Why `release` and not `complete`/`fail`.** `complete`/`fail` in §2 carry the
saga — artifact validation, gates, routing — none of which exists before stage
3. Naming an issue-level verb `complete` now would either promise that saga or
redefine it. `release` says exactly what this stage does: end the lease. The
saga verbs land on steps, in stage 3, where their semantics are specified.

**`close` ends the lease.** §1's step model has terminal status retire the
token ("the token retires when the artifact records"). At issue level the
terminal transition is `issue close`, so closing a leased issue clears the lease.
The holder may always close; a non-holder closing a live lease is refused per
§4's matrix — otherwise a bystander could silently evict a working holder.

### 3.1 Claim response shape

engine-spec §11.4 specifies:

```
claim response  { step, token, lease_expires_ms, context }
```

The issue-level analog, under `--json`:

```json
{"ok":true,"data":{"issue":"DKT-42","token":"<64 hex chars>","lease_expires_ms":1754161200000}}
```

`context` is absent: it is defined in §11.4 as `{step: <next row>, issue: {...},
inputs, pins, loop_entry, metadata}` — a bundle of step, artifact, and pin
concepts that do not exist until stage 3. Emitting a degenerate `context` now
would freeze a shape stage 3 must then change. The three fields that *are*
meaningful at issue level are carried verbatim.

**Nominal deviation (field name), recorded per the work order.** §11.4 names the
subject key `step`; at issue level it is `issue`. This is the same field under
the same semantics — the identity of the leased entity — renamed for the entity
it identifies. No semantic change: token minting, one-shot delivery, and expiry
are exactly as specified. Filed as **DKT-14**, citing engine-spec.md §11.4
line `claim response  { step, token, lease_expires_ms, context }`.
Recorded here; the slice proceeds per the nominal-deviation allowance.

Under `--json=v2` the payload additionally carries the issue's CAS `version`,
per reliability-delta §6.3 — a claim is a mutation and advances it.

**The token is returned exactly once.** It appears in the claim response and
nowhere else, ever: not in `issue show`, not in `issue list`, not in the
activity log, not in any error message. Only its SHA-256 is stored.

### 3.2 Token transport (§4)

> Tokens pass via env/stdin, never argv

Accepted, in precedence order:

1. `DOCKET_TOKEN` environment variable.
2. Stdin, when `DOCKET_TOKEN` is unset and stdin is not a TTY — the whole of
   stdin, trimmed of surrounding whitespace.

There is **no `--token` flag**, on any verb. Not a deprecated one, not a hidden
one. argv is world-readable through `ps` on a shared host, so a flag would
defeat the capability model at the transport layer. A verb that needs a token
and finds none in either channel fails `VALIDATION_ERROR` (exit 3) naming both
accepted channels.

A guard test asserts no `--token`-shaped flag exists anywhere in the tree, so a
later PR cannot reintroduce one.

### 3.3 `docket config set|get`

`docket config` today is read-only and `skipDB`-annotated. It gains two
subcommands; the bare verb's output and its `skipDB` annotation are unchanged,
so its existing QA section (C) keeps passing untouched.

`set|get` **do** need the DB (values live in the `meta` table, per the binding
repo facts), so the subcommands carry no `skipDB` annotation while the parent
keeps it. Cobra resolves annotations per-command, so this is exactly expressible.

Keys, covering the engine defaults §1 names ("lease TTLs per class, attempt
caps, budget default, context caps"):

| Key | Type | Default | Spec basis |
|---|---|---|---|
| `lease.ttl.default` | duration | `15m` | §1 "lease TTLs per class" |
| `lease.ttl.<class>` | duration | (falls back to default) | §11.1 `[limits]` per-class TTL |
| `attempt.max` | int ≥ 1 | `3` | §1 "attempt caps"; §11.1 `max_attempts` |
| `budget.default` | number ≥ 0 | `0` (unlimited) | §1 "budget default" |
| `context.warn_bytes` | int ≥ 0 | `65536` | §1 "context caps" |
| `context.error_bytes` | int ≥ 0 | `131072` | §1 "context caps" |

**Defaults are core's, not the reference instance's.** §8 is explicit that its
numbers ("read 15m / write 45m / research 20m", "warn 60% / pause 100%",
"max_attempts 2") are *instance data* and that "core ships with no opinions
here." So the defaults above are chosen as neutral engineering values, not
copied from §8:

- `lease.ttl.default = 15m` — long enough that a working holder is not reaped
  mid-task, short enough that a dead one is recovered within a coffee break.
  Per-class TTLs exist precisely so an instance overrides this; there is no
  core-side `write`/`read` class, only whatever string a caller passes.
- `attempt.max = 3` — one try plus two retries, the conventional bound. §8's
  instance value is 2; core is not §8.
- `budget.default = 0` = unlimited. Core enforces no cap unless asked; a
  default cap would break the stranger test (a team not tracking cost would hit
  a limit they never set). Budget *enforcement* is stage 6; this stage only
  stores the default, so the key exists before the verbs that read it.
- `context.warn_bytes = 65536` / `error_bytes = 131072` — 64KiB/128KiB. These
  match §8's numbers, which is a coincidence of both being the obvious binary
  round numbers; they are recorded here as core's own.

Unknown keys are `VALIDATION_ERROR` (exit 3) naming the key and listing the
known set — a typo'd `docket config set lease.tt1.default 30m` must not
silently store a key nothing reads. Values are validated per type at `set`
time, not at read time, so a bad duration fails where the user can see it.

`docket config get <key>` on an unset key returns the **default**, with
`"source":"default"` distinguishing it from `"source":"set"`. `get` with no key
lists every key with its effective value and source.

## 4. Refusal matrix (§9.3)

engine-spec §9 item 3 is the acceptance bar, verbatim:

> 3. **Capability proofs:** an unclaimed worker cannot record (AUTH_ERROR);
>    duplicates lose at claim (CONFLICT); late completes refused (STALE_LEASE);
>    racing dispatchers cause no duplicate execution or lost updates.

Mapped to this stage's verbs. Every row is proven by a test (§7):

| # | Situation | Verb | Code | Exit |
|---|---|---|---|---|
| R1 | no token supplied | heartbeat/release | `VALIDATION_ERROR` | 3 |
| R2 | token supplied, issue unclaimed | heartbeat/release | `AUTH_ERROR` | 5 |
| R3 | token supplied, wrong value | heartbeat/release | `AUTH_ERROR` | 5 |
| R4 | correct token, lease expired | heartbeat/release | `STALE_LEASE` | 6 |
| R5 | claim against a live lease | claim | `CONFLICT` | 4 |
| R6 | N concurrent claims on one unclaimed issue | claim | 1 × exit 0, N−1 × `CONFLICT` | 4 |
| R7 | non-holder closes a live-leased issue | close | `AUTH_ERROR` | 5 |
| R8 | claim against an expired lease | claim | **succeeds**, `attempt++` | 0 |

**R2 and R3 are both AUTH_ERROR, deliberately.** "This issue is unclaimed" and
"your token is wrong" are the same answer to the caller — you do not hold this
lease — and distinguishing them leaks whether a lease exists to a caller holding
no capability.

**R4 is STALE_LEASE, not AUTH_ERROR.** The token is *right*; time ran out. That
distinction is the entire value of a separate code: a holder seeing
`STALE_LEASE` knows to re-claim (its work may be redone), while `AUTH_ERROR`
means it never held the lease at all. This is §9.3's "late completes refused
(STALE_LEASE)" applied to the verbs that exist at this stage.

**R7 is the "unclaimed worker cannot record" proof at issue level.** `close` is
the terminal, recording-shaped verb available now; refusing a non-holder is the
same guarantee §9.3 states for `complete`.

**Refusals are checked before the mutation, in the same transaction as it.** A
refusal never writes — including never bumping `attempt` and never touching the
CAS `version`. Proven by asserting the version is unchanged after each refusal.

### 4.1 Unleased issues stay unleased

An issue with `owner IS NULL` is not "claimed by nobody who must be checked" —
it is outside the mechanism. `issue edit`, `issue move`, and every other
existing verb behave exactly as they do at v5 on such an issue: no token, no
refusal, no change. **The token check fires only when a live lease exists.**
This is the dormancy guarantee at the semantic level, and it is what makes §9
item 8 hold for a repo that never claims. The lease-mediated verbs are exactly
`heartbeat`, `release`, and `close`; `edit`/`move` are deliberately *not*
lease-mediated at this stage — gating ordinary editing behind a token would
change existing-verb behavior for a claimed issue, which is a stage-3 policy
decision (scope-based mutual exclusion, engine-core §5), not this stage's.

## 5. The claim transaction (CAS)

One transaction, one predicate. The whole of mutual exclusion:

```sql
UPDATE issues
   SET owner = ?, token_hash = ?, expires_ms = ?,
       attempt = attempt + 1, version = version + 1
 WHERE id = ?
   AND (owner IS NULL OR expires_ms <= ?)   -- unclaimed, or lease lapsed
```

`RowsAffected() == 1` ⇒ this caller won, and the token it minted is the one that
matters. `0` ⇒ re-probe for existence: absent ⇒ `NOT_FOUND` (2); present ⇒ a
live lease held by someone else ⇒ `CONFLICT` (4). This mirrors
`CheckAndBumpVersion`'s existing zero-rows re-probe, for the same reason:
collapsing the two would report a live conflict as a missing entity.

**The predicate is the entire concurrency argument.** `owner IS NULL OR
expires_ms <= now` is evaluated by SQLite inside the UPDATE's write
transaction, so exactly one of N concurrent writers can satisfy it — the losers
see the winner's committed row and match zero rows. There is no read-then-write
window because there is no read. This is why the mutation-test in §7 targets
this predicate specifically: it is the load-bearing line of the stage.

**Expiry is handled lazily, here, and only here** (§6: "lazy lease reaping
confined to `next`/`claim`; reads never write"). There is no reaper, no
background pass, no write from any read path. An expired lease is reaped as a
side effect of the next claim that wants it — the `expires_ms <= now` disjunct
*is* the reaping. `attempt` increments on that claim, which is precisely §9.4's
"attempt trail is complete": every claim ever made is counted, including the one
whose holder died.

`attempt` is never decremented and never reset. It counts claims against the
issue, for all time — a monotonic trail, not a live retry counter.
(Stage 3's `step resolve --as retry` resets attempts *for a step instance*;
that is a different counter on a different entity, and nothing here presumes it.)

### 5.1 Effective status on reads

Read verbs compute effective lease state from `expires_ms` and never write
(§2 "Read verbs render *effective* status (lease expiry computed at read time,
no write)"; §6 "reads never write"). Concretely, `issue show --json=v2` reports:

```json
"lease": {"owner":"ci-runner-7","expires_ms":1754161200000,"attempt":1,"live":false}
```

`live` is computed as `expires_ms > now`, per read, in Go — not stored, never
written back. An expired lease therefore *reads* as dead the instant it lapses,
even though the row still carries the stale `owner` until someone claims. Status
never lies just because nobody called `claim`.

The `lease` object is **v2-only and omitted entirely when `owner IS NULL`**, so
v1 output and unclaimed-issue output are byte-identical to v5. That is the
§9-item-8 obligation discharged at the field level.

This is proven at the strongest available level (QA `ZF4b`): the database file
is snapshotted, every read verb runs against an **expired** lease — the case
that would tempt an implementation to reap — and the file must come back
byte-identical. The check's sensitivity is itself verified: a known write to the
same database is detected by the same comparison, so a pass is not vacuous.
(WAL sidecars are checkpointed when the process exits, so a write cannot hide in
one.) `internal/db.TestGetIssueLeaseNeverWrites` covers the same property at the
unit level via the CAS version.

## 6. Token minting and hashing

- **Mint:** 32 bytes from `crypto/rand`, hex-encoded to 64 characters.
  `crypto/rand` specifically — a token from `math/rand` is derivable from the
  seed, and engine-core §5 requires "not derivable from ids".
  A `crypto/rand` read error is a hard failure; there is no fallback path,
  because a fallback is exactly how a weak token gets minted in production.
- **Store:** SHA-256 hex of the token. The plaintext token is never written to
  the DB, the activity log, or any log line.
- **Verify:** `subtle.ConstantTimeCompare` of the SHA-256 of the presented token
  against the stored hash. Constant-time because a timing oracle on a 64-hex
  comparison is a real, if slow, byte-at-a-time recovery path — and the
  correct primitive costs one import.

A plain SHA-256 is right here, and a password KDF (bcrypt/argon2) is not: the
token is 256 bits of uniform entropy, so there is no dictionary to attack and
no work factor to raise. Hashing exists so a stolen DB file does not yield live
capabilities — which SHA-256 of a 256-bit random value achieves completely.

## 7. Test plan

**Go unit tests** (`internal/db/leases_test.go`):

- claim on an unclaimed issue: token minted, hash stored, `attempt` 0→1.
- the plaintext token is absent from the DB file (scan the row: no column equals
  the token).
- claim against a live lease ⇒ `ErrLeaseHeld`; row unchanged; `attempt` unchanged.
- claim against an expired lease ⇒ succeeds, `attempt` increments, new token,
  old token no longer verifies.
- heartbeat: extends `expires_ms`, leaves `attempt` and `owner` alone.
- heartbeat/release with wrong token ⇒ `ErrNotHolder`; with expired lease ⇒
  `ErrLeaseExpired`.
- release clears the lease, preserves `attempt`.
- **CAS mutation test** (as S1 did): a test that rewrites the claim predicate to
  the unguarded `WHERE id = ?` and asserts the concurrency test then **fails**.
  A guard that cannot fail is not a guard; this proves the predicate is what
  produces the exclusion.
- in-process concurrency: N goroutines claim one issue; exactly one wins.

**Go unit tests** (`internal/db/migrate_v6_test.go`): v5→v6 adds the four
columns and the index; the migration is idempotent; a v6 DB's pre-existing rows
have `owner IS NULL` and `attempt = 0`.

**Go unit tests** (`internal/cli`): token from `DOCKET_TOKEN`; token from stdin;
env beats stdin; neither ⇒ `VALIDATION_ERROR`; guard test asserting no
`--token` flag exists in the tree.

**QA section** (`scripts/qa/test_zf_claims.sh`, `SECTIONS` entry
`ZF:test_zf_claims`):

- The full §9.3 refusal matrix, R1–R8, each proven by exit code **and** by the
  `.code` field, and each followed by a version-unchanged assertion.
- **The concurrency race (R6):** N backgrounded `docket issue claim` processes
  against one unclaimed issue, `wait`-ed, then: exactly one exit 0, exactly N−1
  exit 4, exactly one distinct token, and `attempt == 1` — the last being the
  lost-update proof, since a lost update would show `attempt > 1` with a single
  winner. Separate *processes*, not goroutines: `db.Open` caps the pool at one
  connection, so in-process concurrency would not exercise SQLite's cross-process
  locking, which is the thing under test.
- **§9.4-half — kill a claimer mid-work:** claim with a 1s TTL from a
  subshell that is then killed (`kill -9`) without releasing; assert the lease
  reads `live:false` after expiry with **no intervening command**; re-claim
  succeeds with no operator action beyond the claim itself; `attempt` is 2. This
  is "lease expiry ALONE re-readies it, attempt trail complete", exactly.
- Token never appears in `issue show`/`issue list` output at either JSON version.
- `config set|get`: round-trip; defaults with `source:default`; unknown key ⇒
  exit 3; bad duration ⇒ exit 3; bare `docket config` output unchanged.
- Dormancy: an issue that is never claimed has byte-identical `issue show`
  output at v1 and v2 versus the pre-migration baseline.

**Fixture proof (§9.8), re-run per the v4 protocol:** the committed
`v4-baseline.db` opens, migrates **4→6** in one pass, and golden-diffs. The v6
structures are asserted present *before* the diff is trusted — a golden diff
against a DB that failed to migrate passes vacuously and proves nothing. This
extends section ZE's existing ZE1 rather than duplicating it.

**Byte-compat sweep:** every existing verb, run against the fixture under
`--json` by a binary built from the `engine-s1` tag and by the current binary,
diffed byte-for-byte. Zero diffs required. Non-empty output asserted, for the
same vacuity reason.

## 8. Security documentation

`docs/spec/security.md` (new) lands in the same commit as the surface, per
CLAUDE.md's same-PR rule, and records the token transport contract as
implemented: env/stdin only, hash-at-rest, tokens never in argv/`ps`, one-shot
delivery, constant-time verification, and the explicit non-goal (a lease is
mutual exclusion between cooperating callers with DB write access — it is not
an authorization boundary against an adversary who can write the DB file
directly).

## 9. Delivery

Direct commits on `feature/graph-engine` per the operator decision recorded in
reliability-delta §11 (unchanged here): no branches, no PRs, no tags. Draft
PR #33 provides CI on every push. Every commit leaves the branch green.

1. TDD (this file).
2. v6 migration + `internal/db/leases.go` + `internal/model/lease.go` + unit
   tests, including the mutation test.
3. CLI verbs (`claim`/`heartbeat`/`release`), token transport, `close`
   lease-ending, effective status on reads.
4. `config set|get`.
5. QA section ZF + the 4→6 fixture proof + the byte-compat sweep.
6. `skills/docket/SKILL.md` tables and `docs/spec/security.md`, in the same
   commits as the surface changes they document.
