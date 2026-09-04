# TDD: gates, the execution trust model, and the exec runner (stage 4)

Status: draft, revised per security review — 2026-08-03; §3.6 amended per DKT-81 — 2026-08-08
(docs/tdd/gates-trust-review.md, verdict SOUND WITH FIXES — F1–F5 folded in; see
that file's response table for the per-finding
mapping) · implements docs/design/engine-spec.md **§4 (whole)**
and the gate semantics of §2, §11.4's `gate result` shape, and engine-core.md §6
(votes driven as gates; fenced-command execution), with **§9 items 6, 10, and 1**
as the acceptance bar. Tracker unit: DKT-4. Spec of record is engine-spec.md;
deviations are DKT amendment issues per docs/design/amendments.md citing the
exact line, never silent changes.

**This document is reviewed with a security lens before any code exists.** §1 is
the threat model and is written to be read first. Everything after it is the
mechanism that closes each entry.

## 1. Scope

engine-spec.md §10 stage 4, verbatim:

> 4. **Gates + trust model** (§4).

In scope:

1. `internal/trust` — the user-level allowlist at
   `$XDG_CONFIG_HOME/docket/trust.toml`, its entry shape, repo binding, the TOFU
   `trust add` flow, and `trust list|rm`. Pure; no engine dependency.
2. `internal/exec` — resolved argv, no shell, cwd repo root, env allowlist,
   timeout with process-group kill, capture with explicit truncation,
   flaky-declared re-runs recorded individually. Pure; unit-testable with no
   database and no engine.
3. Schema **v8**: `gate_results` per §11.4's shape, the `steps.gate_trail` →
   `gate_results` migration (closing DKT-16), and the trust cache.
4. Saga wiring: the real `GateRunner` replacing `PassThroughRunner` — trust
   matching, at-least-once with the `re-runnable` decision on resume, and the
   per-repo tree mutex for `tree = true` gates.
5. **Pre-gates** (`pre = true`): executed at claim, results into the context
   bundle.
6. **Vote-step execution** (deferred from S3 by its §1 scope table): `type="vote"`
   steps drive Docket's existing proposal/vote machinery, unchanged.
7. `docket trust add|list|rm`, incl. `trust add --yes` (the conversational
   posture, upstream D14).

Out of scope, explicitly, each with the stage that owns it:

| Deferred | Owner | Why not here |
|---|---|---|
| Threshold **field** validation against payload schemas; `schema register`; ordered enums; the builtin `aggregate` computation | S5 | §10 stage 5; the S3 action seam (engine-spine §6.13) is untouched by this stage |
| Budget enforcement and the floor; `run report`; `dispatch open\|close\|verify\|abandon`; `guard spawn\|record`; `events list --since` | S6 | §10 stage 6 |
| The **write-class reap-acknowledgment half of §9 item 10** ("a reaped write-class step cannot gain a successor until the reap is acknowledged") | S6 | it is dispatch mechanics — `guard spawn` surfaces the `reaped` event (§2), and `guard spawn` is stage 6's. DKT-4's own AC text says so: "gate re-run half only". **This stage proves the gate half in full** (§9.2) |
| `events --follow`, `events prune` | S7 | §10 stage 7 |
| Worktree-isolated parallel writes (upstream D9) | never core | engine-core §5 names it "an optional instance optimization"; the tree mutex (§7.4) is the always-available baseline |

### 1.1 Genericity check (CLAUDE.md PR bar, docs/design/genericity.md)

Per the S3 TDD's §1.1 pattern, over **every** noun this stage introduces into core
surface — flag names, JSON keys, column names, error strings, help text, TOML keys
in `trust.toml`, and event kinds:

`trust`, `entry`, `argv`, `argv-hash`, `prefix`, `repo`, `global`, `re-runnable`,
`tree`, `gate`, `fence`, `tag`, `unmatched`, `verdict`, `exit`, `output`,
`truncated`, `timeout`, `capture`, `env allowlist`, `process group`, `mutex`,
`pre-gate`, `flaky`, `re-run`, `proposal`, `vote`, `voter`, `vote_rule`, `tally`,
`quorum`, `threshold config`.

Every one is **execution, authorization, or process vocabulary**. No model,
prompt, brief, node, severity, review, agent, or LLM concept appears anywhere in
the surface. The three places instance meaning could leak are closed by
construction:

- **A gate name is an opaque string.** The engine looks it up in the trust store
  and never interprets it. There is no registry of known gates, no gate whose
  name has behavior, and no default gate. `build`, `tests`, `secret-scan` in the
  committed fixture are an *instance's* names for its own checks, exactly as
  `executor` hints are (engine-spine §1.1).
- **A fence tag is an opaque string.** `source = "fence:<tag>"` harvests blocks
  whose info string is `<tag>`. Core never knows that the reference instance
  writes ```` ```ac ````; the shipped `standard-dev` template uses ```` ```checks ````
  precisely because a human-only team reads that word and knows what it means.
- **`trust.toml` entries name commands, not roles.** An entry is
  `{name, argv, repo, …}` — a command an operator approved. Nothing in the file
  format has a slot for who or what will run it.

**`vote`, `voter`, `proposal`, `verdict`, `quorum`, `threshold` are pre-existing
Docket surface**, shipped since v2 and documented in SKILL.md's voting section.
This stage drives that machinery from a workflow step; it introduces no new
vocabulary there and changes none of its semantics (§8). A team voting on a
documentation change reads every one of those words exactly as written — the
stranger test holds because the feature *predates* the engine entirely.

**`flaky` is the one word worth arguing.** §4 uses it verbatim ("flaky-declared
re-runs recorded individually"), and it names a property of a *command* — that it
does not always produce the same exit code on the same input — which is a fact
about processes, not about models or reviews. It is declared by the operator in a
trust entry, never inferred. A team whose integration test suite is occasionally
flaky wants exactly this field.

### 1.1.1 Re-check over the security-review revision (F1–F5)

The revision that folded the security review's findings into this document
(§5.2.1, §5.7, §7.6.1.1, §7.4 L7, §3.5.1, T17, T18, and the §13.1 rows) is put
through the same check, because a check run once and not re-run at each revision
is a check that decays. The nouns it introduces into core surface:

`resolved path`, `repo root`, `containment`, `symlink`, `lockfile`, `lock`,
`escape`, `control character`, `render`, `terminal`, `lease refresh`,
`expires_ms`, `regular file`.

Every one is **filesystem, process, terminal, or lease vocabulary** — the same
three families §1.1 already covers, with no new family and no new concept. None
of them is a model, prompt, brief, node, severity, review, agent, or LLM notion,
and none of them requires the reader to know what an LLM is. Specifically:

- **`escape` / `control character` / `render` / `terminal`** are about **bytes
  going to a TTY**. The stranger reading §8.2 of security.md is a person who has
  seen a terminal get garbled by `cat`-ing a binary; that is the entire required
  background.
- **`resolved path` / `containment` / `symlink`** are `PATH` resolution, which is
  a property of every command any team has ever run.
- **`lease refresh` / `expires_ms`** are **pre-existing** Docket surface — the
  lease model shipped in the v6 schema and `lease.ttl.<class>` is already in the
  engine-configuration table. §7.6.1.1 adds no key and no field; it changes *when
  an existing field is written*.
- **`lockfile` / `regular file`** are the vocabulary §3.2 and §7.4 already
  established in this same document.

**No new flag, config key, column, event kind, or `trust.toml` key** is
introduced by the revision — §6.4's four event kinds and §3.1's entry shape are
unchanged, so §9 item 2's closed-set check and the surface guards are unaffected.
The one new **error string** family (the T17 refusal and the L7/W3 non-regular-file
refusals) reuses §3.2's existing `VALIDATION_ERROR` language verbatim.

**The stranger test (§9 item 1) is this stage's headline QA script**, not an
aspiration: §9.4 specifies `test_zh_stranger.sh` — the `standard-dev` template,
a fenced check, one `trust add`, and a run to `done`, with no agents anywhere.
S3 shipped the template and could not run its gate; this stage makes the demo
real.

## 2. THREAT MODEL

**This is the section the security review reads first.** Docket executes
registered commands at workflow transitions — §4 calls it "the one place it runs
anything." Everything below is an attack on that one place. Each row names the
attack, the mechanism that closes it, and the test that proves the closure.

The **trust boundary** is inherited from docs/spec/security.md §2 and restated
because every entry depends on it: *Docket is a local-first tool operating on a
file in the user's own repository; file permissions are the trust boundary.* An
adversary with write access to `~/.config/docket/trust.toml` or to the docket
binary has already won and is out of scope. The adversary this model defends
against is **hostile repository content** — a cloned repo, a pulled branch, a
malicious issue body, a workflow definition written by someone else — reaching
an operator who runs `docket` in it.

| # | Threat | Closed by | Proof |
|---|---|---|---|
| T1 | **Malicious-clone execution** (§9 item 6). A cloned repo ships a workflow whose gate runs `curl … \| sh`, or an issue body whose fenced block does. Cloning and running `docket run activate` + a full run must execute nothing. | Trust entries are **user-level only** (§3.1) and never read from the repo. An unmatched gate is **reported `unmatched`, never executed** (§7.3). Repo binding (§3.4) means an entry approved in repo A does not fire in repo B. | §9.2 `TestMaliciousCloneExecutesNothing` + QA `ZH` — the §9 item 6 script, including fenced harvesting |
| T2 | **Argv injection.** A fenced command or gate argv containing `; rm -rf ~`, `$(…)`, backticks, `&&`, a newline, or a glob must not become multiple commands or expand. | **No shell, ever** (§5.1). `exec.Command(argv[0], argv[1:]...)` with `Args` set explicitly; no `sh -c`, no `os/exec` shell wrapper, no `SHELL` env consultation. Tokenization happens **once**, at `trust add` and at harvest, by a POSIX-ish splitter that performs **no expansion of any kind** (§5.2) | §9.1 `TestNoShellEverInvoked` (argv table incl. every metacharacter) + a source-level guard test (§9.1) asserting `internal/exec` references no shell binary |
| T3 | **Post-activation fence injection.** An operator approves an issue body at activation; an attacker (or a well-meaning teammate) edits the body afterward to add or alter a fenced command; the gate runs the *new* command. | **Already closed by S3** and this stage depends on it rather than re-solving it: activation snapshots the body, harvests fences from the **snapshot**, and stores each command with its SHA-256 in `run_fences` (engine-spine §5.3 stage 5). §5.4 states exactly how S4 relies on it: the runner receives `GateSpec.Commands` **from the `run_fences` rows**, and **re-verifies each row's stored hash against its stored command before spawning** | §9.2 `TestPostActivationBodyEditCannotInject` — edit the body between activation and the gate, assert the *snapshotted* command runs; plus §9.2's tamper case: a `run_fences` row mutated in the DB is refused, not run |
| T4 | **TOCTOU between the trust check and the spawn.** The trust file is checked, then the process spawns; a concurrent `trust rm`, a trust-file rewrite, or a symlink swap between the two makes an unapproved command run. | The trust store is **read once into an immutable in-memory snapshot** at the start of the gate stage, and the **matched entry's own argv is what is executed** (§7.2). Matching does not produce a *permission* that is later applied to a command read from elsewhere; it produces **the argv itself**. There is no window because there are not two reads of two different things | §9.1 `TestMatchReturnsTheExecutedArgv` — a match returns the argv, and the runner has no other argv source; §9.2 `TestTrustFileRewriteMidGateDoesNotChangeWhatRuns` |
| T5 | **Env leakage into gate children.** A gate command inherits the operator's whole environment and exfiltrates it, or — worse — inherits `DOCKET_TOKEN` and uses a live capability to record a forged artifact. | **Allowlist, not denylist** (§5.3): the child environment is constructed from an enumerated set (§5.3's table), plus explicitly-set `DOCKET_*` context vars. **`DOCKET_TOKEN` is excluded by name in addition to being absent from the allowlist** — a belt-and-braces check with its own test, because it is the one variable whose leak converts a code-execution foothold into engine authority | §9.1 `TestGateChildEnvIsExactlyTheAllowlist` (asserts set equality, not membership) + `TestDocketTokenNeverReachesAGateChild` (sets it in the parent, asserts absent in the child) |
| T6 | **Output-capture bombs.** A gate emits gigabytes on stdout, or never stops emitting, exhausting memory or disk. | Capture is **bounded at a byte cap** (§5.5), read through a counting limited reader that stops consuming after the cap and sets `truncated: true`. The cap is a constant, not config, at this stage. The process is killed by the timeout independently (§5.4) | §9.1 `TestCaptureIsCappedAndFlagged` — a generator writing far past the cap; memory asserted bounded, `truncated` true, the recorded output exactly cap-sized |
| T7 | **Process escape.** A gate spawns children (a build spawning compilers, a test runner spawning servers); killing the gate on timeout leaves them running, holding the tree, ports, or the mutex's intent. | The child is started in **its own process group** (`Setpgid`), and the timeout kills **the group** (`syscall.Kill(-pgid, …)`), SIGTERM then SIGKILL after a grace period (§5.4) | §9.1 `TestTimeoutKillsTheWholeProcessGroup` — a gate that spawns a long-lived grandchild; assert the grandchild is gone after the timeout |
| T8 | **trust.toml integrity.** The trust file is repo-shipped, world-writable, or a symlink into the repo — any of which lets repo content grant itself execution. | The store is **user-owned and never repo-shipped** (§3.1): it lives under `$XDG_CONFIG_HOME` (default `~/.config`), is created `0600` in a `0700` directory, and **`docket trust` refuses to operate on a path that is a symlink, is group- or world-writable, or is not owned by the calling user** (§3.2). `docket` never reads a trust file from inside a repository — there is no repo-local trust path, no `--trust-file` flag, and no env override of the store location other than `XDG_CONFIG_HOME` itself | §9.1 `TestTrustStoreRefusesUnsafePermissions` (table: symlink, 0666, 0640, wrong owner, 0777 parent dir) + §9.1's surface guard asserting no flag or config key names a trust-file path |
| T9 | **Self-trust by a misbehaving session** (upstream D14, the accepted residual). A session that has been prompt-injected runs `docket trust add --yes …` to approve its own payload. | **Accepted and bounded, not closed** — §4 says so explicitly. The bounds are: (a) full-argv hashing, so the approved thing is exactly one command line, not a prefix; (b) the **harness's own command-permission prompt** is the human backstop — `docket trust add` is a command the harness gates like any other; (c) repo binding, so the grant does not travel; (d) `trust list` makes the grant **visible and auditable after the fact**, and `trust add` is **event-logged** (§3.6) so a run's trail shows a grant that happened mid-run. §2.1 argues the residual honestly | §9.1 asserts (a), (c), (d) mechanically; (b) is a property of the harness, stated not tested |
| T10 | **Trust-entry over-authorization via `--prefix`.** A prefix entry for `make` authorizes `make anything`, including a target that a repo defines to do something hostile. | Prefix entries are **explicit opt-in only** (§3.3): full-argv hashes are the default, `--prefix` is a separate flag that **prints an over-authorization warning naming what it authorizes**, the entry records `prefix = true`, and **a prefix entry matches only when the entry opted in** — a full-argv entry never matches by prefix (§7.2 M3) | §9.1 `TestPrefixMatchingRequiresOptIn` + the warning asserted by substring |
| T11 | **Gate result forgery / silent stubbing.** A run shows green gates that never ran. | Every recorded result carries its **real `argv`, `exit`, `duration_ms`, and captured `output`** (§6.1). The S3 `stub: true` field is **absent on every result this stage produces** (§6.2), and the migration marks S3-era trail results so the two are distinguishable forever. An `unmatched` gate records a distinct **`verdict = "unmatched"`**, never `pass` | §9.2 `TestRealResultsCarryNoStubField`; QA asserts `stub` is absent post-migration on new results and present on migrated S3 rows |
| T12 | **Double execution of a non-idempotent gate on resume** (§9 item 10). A crash between `gate-started` and the result record; resume re-runs a gate that committed, deployed, or charged something. | §2's at-least-once rule, implemented exactly: a started-but-unrecorded gate re-runs **only if its trust entry is flagged `re-runnable`**, else the step **parks `waiting-human`** (§7.5). The flag is per-**entry**, i.e. the operator's declaration about their own command — core never infers idempotence | §9.2 `TestCrashAtGateBoundaryNeverDoubleRunsNonRerunnable` — every saga boundary, both flag values |
| T13 | **Concurrent tree mutation.** Two gates that touch the working tree (`tree = true`) run concurrently from parallel read-step completions and race a build. | An **engine-held per-repo mutex** (§7.4), which is **not** the database transaction — gates run outside transactions by construction (§6 of engine-spec: "No subprocess ever executes inside a transaction"). The mechanism is pinned in §7.4: an OS-level advisory lock on a lockfile in the repo's `.docket/` directory | §9.2 `TestTreeGatesSerialize` — two concurrent `tree=true` gates, each recording entry/exit timestamps, asserted non-overlapping; and a crash-holding-the-lock case |
| T14 | **Pre-gate as a bypass.** Pre-gates run at claim (§11.1), earlier in the lifecycle and on a different code path than the saga's gates — an implementation could reasonably "simplify" them into a trusted path. | **Same trust model, no exceptions** (§7.6). Pre-gates resolve through the identical matcher, the identical runner, the identical env allowlist and timeout, and an unmatched pre-gate is reported `unmatched` and does **not** execute. The only differences are *when* they run and *where the result goes* | §9.2 `TestPreGatesUseTheSameTrustPath` — an untrusted pre-gate command does not execute and the claim still succeeds with the result recorded `unmatched` |
| T15 | **Path resolution hijack.** `argv[0]` is `make`; a repo ships `./make`, or `PATH` contains `.`, so a repo-controlled binary runs. | Resolution is `exec.LookPath` against the **allowlisted `PATH`** (§5.3), which is inherited from the operator's environment and never modified by docket; **the current directory is never prepended** and a relative `argv[0]` containing no separator is **not** resolved against cwd. A trust entry may name an absolute path, which resolves to itself. §5.2 records the residual: an operator whose own `PATH` contains `.` is already exposed everywhere, and docket does not repair that | §9.1 `TestArgv0IsNotResolvedAgainstTheWorkingDirectory` — a `./make` planted in the repo root is not executed |
| T16 | **Fenced command harvested from an issue body an attacker can write.** Not a clone — a *live* repo where an attacker can file an issue. | The gate still needs a **matching trust entry** (T1's mechanism), so filing an issue grants nothing. §2 (engine-spec) adds the operator-facing half: activation "surfaces what activation will bind — including every harvested fenced command, verbatim". §7.7 makes that concrete at this stage: `run activate` **prints every harvested command and its trust-match status** (matched / unmatched), so an operator sees `unmatched` commands before the run, not after | §9.2 `TestActivationReportsFenceTrustStatus`; QA asserts the verbatim print |
| T17 | **Repo-resident `PATH` resolution.** T15 closes `./make` and `PATH`-contains-`.`, but not a `PATH` **entry that points inside the repo**. An allowed `.envrc` (this repository ships one; direnv is in live use) or any checked-in `bin/` on `PATH` prepends `$PWD/bin`; a trusted argv `["make","test"]` then resolves `argv[0]` to a **repo-controlled** binary. The entry names the command; the repo supplies the executable — trust by name is silently transferred to repo content. | **Post-resolution containment check** (§5.2): after `LookPath`, the resolved path is symlink-resolved and compared against the symlink-resolved repo root. A resolved `argv[0]` **under the repo root is refused** — `verdict = "unmatched"`, `reason` naming the resolved path and the rule — **unless the trust entry's own `argv[0]` is that absolute path**. Running repo-owned scripts stays possible, but only by trusting the absolute path explicitly, never by name resolution. Note the direnv vector is *not* a defect in direnv: a repo-resident `PATH` is legitimate dev tooling, and the fix is to stop name resolution from inheriting it | §9.1 `TestArgv0NeverResolvesIntoTheRepoByName` — a repo-resident `bin/` on `PATH`, a by-name entry refused with the reason, an absolute-path entry for the same binary permitted |
| T18 | **Terminal-escape injection into the operator's approval.** Every verbatim print is attacker-influenced bytes going to a TTY: §7.7 prints harvested fence commands "verbatim", `trust add` prints the argv it is about to trust, and `reason` strings carry fence content. A command containing `\x1b[2K\r…` (or `\x1b[1A`, or a lone `\r`) **visually rewrites the very line the operator is approving** — the terminal shows `make test` while the bytes say otherwise. D14's whole backstop is *displayed == approved*, so this defeats the human confirmation without touching the trust file. | **Escape-on-render** (§5.7): every human-mode rendering of an argv, a fence command, or a `reason` passes through one function that emits **Go `%q` form** — printable ASCII and printable Unicode survive; every C0/C1 control, `\r`, `\n`, `\x1b`, and every non-printable becomes a visible escape. There is exactly one renderer and it is used by `trust add`'s disclosure, `trust list`, §7.7's activation report, and every `unmatched`/timeout reason. **JSON mode is inherently safe** and is left alone — `encoding/json` already escapes controls, and the consumer is a program | §9.1 `TestOperatorFacingRenderingEscapesControlBytes` — a fence command carrying `\x1b[2K\r` and a bare `\n` renders escaped in the §7.7 report and in `trust add`'s disclosure; plus a surface guard that no human-mode print of these three value classes bypasses the renderer |

### 2.1 The residual risks, stated honestly

A threat model that closes everything is a threat model that is lying. Three
residuals are accepted, and each is accepted for a stated reason:

1. **A trusted command is trusted.** Once an operator approves
   `["make", "test"]` in this repo, a repo that redefines the `test` target
   redefines what runs. Full-argv hashing bounds *which command line* runs, not
   *what that command line does* — and it cannot, because the command's meaning
   lives in files the repo owns. **The mitigation is disclosure, not
   prevention**: `trust add` prints the argv verbatim and names the repo it is
   binding to; §7.7's activation report shows which commands a run will invoke.
   An operator who trusts `make test` in a repo has decided to run that repo's
   build, which is the same decision they make by typing `make test`.
2. **Self-trust by a misbehaving session** (T9, upstream D14). Accepted by the
   design record; bounded as T9 lists. The one thing this stage adds beyond D14
   is **auditability**: a `trust-added` event (§3.6) means the run's own trail
   records the grant, so a retro can find it.
3. **A gate can read everything the operator can.** The env allowlist controls
   what is *handed* to a gate, not what a process running as the operator can
   open. A trusted gate can read `~/.ssh`. This is inherent to running a command
   at all and is why the trust decision is the security boundary. Docket does not
   sandbox gates in v1; §10 records the deliberate non-goal.

### 2.2 What this stage does NOT change about the existing security posture

Stated so the review can bound its own scope:

- **Token transport is unchanged** (docs/spec/security.md §1.2): env or stdin,
  never argv. This stage adds one rule on top — `DOCKET_TOKEN` is stripped from
  gate children (T5) — and no others.
- **A lease is still not an authorization boundary** (security.md §2). Gates do
  not change that; a gate result is a fact the engine produced, recorded in a
  database anyone with file access can edit.
- **Read verbs still never execute** (§4, verbatim: "Read verbs never execute").
  This stage adds no execution to any read path: `step show`, `step context`,
  `run status`, `trust list`, and `workflow show` spawn nothing. §9.1 carries a
  guard test asserting the exec package is unreachable from the read verbs'
  call graph.

---

# 3. `internal/trust` — the user-level allowlist (§4)

Commit group 1, with §5. Pure: no database, no engine, no CLI. A file, a parser,
a matcher, and a writer.

## 3.1 The store: location and shape

§4, verbatim: *"Executable argv templates live in `~/.config/docket/trust.toml`
(per-user, never repo-shipped), managed by `docket trust`."*

Resolution order for the store path:

| # | Source | Value |
|---|---|---|
| 1 | `$XDG_CONFIG_HOME` set and non-empty | `$XDG_CONFIG_HOME/docket/trust.toml` |
| 2 | otherwise | `$HOME/.config/docket/trust.toml` |

**There is no third source.** No `--trust-file` flag, no `DOCKET_TRUST_FILE`
env var, no repo-local path, no config key. This is T8's mechanism and it is
enforced by a surface guard test (§9.1) that walks the Cobra tree and the config
key registry asserting nothing names a trust path. The reason is direct: every
additional way to point docket at a trust file is another way for repo content —
a checked-in `.envrc`, a direnv hook, a Makefile — to point it at a file the repo
controls.

`$XDG_CONFIG_HOME` is honored because it is the platform convention and because
**the tests need it** (§9.5): every test and every QA section sets
`XDG_CONFIG_HOME` to a sandbox directory, so no test can read or write the
operator's real trust store. That requirement is absolute and is §9.5's own
section.

The file format, one `[[entry]]` per trusted command:

```toml
# ~/.config/docket/trust.toml — written by `docket trust add`.
# Hand-editing is supported; docket re-reads the file on every use.

version = 1

[[entry]]
name       = "tests"                      # the gate name a workflow references
argv       = ["make", "test"]             # resolved argv; NEVER a shell string
argv_sha256 = "9f2c…"                     # hash of the canonical argv (§3.3)
repo       = "/Users/x/src/docket"        # repo binding (§3.4); absent when global
global     = false                        # true = this entry applies in any repo
prefix     = false                        # true = prefix match, explicit opt-in (§3.3)
re_runnable = true                        # safe to re-run after a crash (§7.5)
tree       = false                        # touches the working tree (§7.4)
flaky      = false                        # re-runs are recorded individually (§5.6)
timeout    = "5m"                         # per-entry override of the default
network    = ["vuln.go.dev"]              # hosts this command must reach (§3.7)
added_at_ms = 1754…
```

### 3.7 `network` — declaring what a gate must reach *(amended 2026-08-07, DKT-31)*

A gate runs as a subprocess of `docket step complete`, which runs inside
whichever executor claimed the step. A gate that needs the network therefore
succeeds or fails on a property of **the claimant**, not of the gate — and the
failure lands two process layers down, after the step is recorded, where the
executor never sees it and the step simply parks. Observed three times in one
run, each resolved by an out-of-band scan plus `override-pass`.

`network` is the operator's declaration that a command needs specific hosts. It
is a **declaration, not a grant**: core cannot widen a sandbox it is itself
running inside, and nothing here opens a hole in one. What the declaration
buys:

- **Proxy variables reach the child.** `HTTP(S)_PROXY`, `NO_PROXY`, and
  `ALL_PROXY` — in both casings, because the convention is genuinely split —
  are forwarded only to a declaring gate. They are absent from §5.3's allowlist
  because most gates have no business making connections, which previously left
  a proxied environment unusable by the gates that legitimately needed one.
- **`DOCKET_GATE_NETWORK`** carries the declared hosts, so a check can assert
  its own reachability up front instead of surfacing whatever its underlying
  tool prints on a DNS error.
- **A failing declaring gate says so.** Its recorded `reason` names the hosts
  and reachability as a candidate cause. Core does **not** classify the failure
  — it cannot know why a command exited non-zero. It reports the fact the
  operator stated, which is what makes reachability the first question asked
  rather than the last.

The hostnames are **opaque to core**: nothing resolves, validates, or connects
to them. Like `re_runnable` and `flaky`, this is the operator's assertion about
their own command. A passing gate is never annotated, and a gate that declares
nothing sees exactly the environment it saw before.

**TOML, not JSON**, matching workflow definitions and the repo's existing config
idiom. **`version = 1`** at the top level so a future format change is a
migration rather than a guess; an unknown `version` is a hard refusal naming the
file and the version, never a best-effort parse.

**Unknown keys are a hard error**, exactly as workflow parsing is strict
(engine-spine §4.2). A typo'd `re_runable` silently defaulting to `false` would
turn a re-runnable gate into a `waiting-human` park; a typo'd `global` defaulting
to `false` is the safe direction, but a typo'd `prefix` is not, and a parser that
is strict about one key and lax about another is a parser nobody can reason
about.

## 3.2 Store integrity (T8)

Checked on **every** read and every write, before parsing:

| # | Check | Failure |
|---|---|---|
| I1 | the trust file, if it exists, is a **regular file** — not a symlink, not a FIFO, not a directory | `VALIDATION_ERROR` (exit 3) naming the path and what it is |
| I2 | the trust file's mode is **`0600`** — no group or world bits, read or write | `VALIDATION_ERROR` naming the mode and the required mode |
| I3 | the trust file is **owned by the calling uid** | `VALIDATION_ERROR` naming both uids |
| I4 | the containing directory is **owned by the calling uid** and not group- or world-**writable** | `VALIDATION_ERROR` naming the path and mode |
| I5 | on create: the directory is created `0700` and the file `0600`, with the file created **`O_EXCL`** and written via a temp-file-plus-rename in the same directory | — |

I2 is stricter than "not world-writable" on purpose. A trust file readable by
group is an inventory of the operator's approved commands and the repos they
work in — modest, but there is no reason to publish it. The refusal message
states the fix (`chmod 600`) rather than only the complaint.

**A missing trust file is not an error.** It is an empty allowlist: every gate is
`unmatched`, nothing executes, and a run of a gate-bearing workflow reports what
it would have needed. That is the correct default for a tool a stranger just
installed, and it is the state §9 item 6's proof starts from.

## 3.3 Entry shape: matching, and why the default is a full-argv hash

§4, verbatim: *"Trust entries default to **full-argv hashes**; prefix entries are
explicit opt-in (`trust add --prefix`, with an over-authorization warning)."*

**Canonical argv**, so a hash means something stable: the argv is a list of
strings; the canonical form is the JSON encoding of that list with no
whitespace (`["make","test"]`), and `argv_sha256` is the SHA-256 of those bytes,
hex-encoded. JSON rather than a delimiter-joined string because **no delimiter is
safe**: an argument containing a NUL is impossible, but one containing a newline,
a space, or any chosen separator is trivial, and a join-then-hash scheme makes
`["a b"]` and `["a","b"]` collide. That collision is an argv-injection primitive
in the matcher itself, which is exactly the class of bug §2's T2 is about.

Two matching modes, and the difference between them is the whole of §4's
over-authorization warning:

| Mode | Entry has | Matches |
|---|---|---|
| **full-argv** (default) | `prefix = false` | the candidate argv's canonical hash **equals** `argv_sha256` — an exact, element-wise identical argv |
| **prefix** (opt-in) | `prefix = true` | the entry's `argv` is an **element-wise prefix** of the candidate argv (`["make"]` matches `["make","test"]` and `["make","anything"]`; it does **not** match `["makefoo"]` — the comparison is per element, never per character) |

**Element-wise, never string-prefix**, is stated because the wrong reading is
both easy and dangerous: a string-prefix comparison would let `["make"]` match
`["make-release", "--prod"]`, authorizing a command the operator never saw.

`trust add --prefix` prints, to stderr, before writing:

```
warning: prefix entry — this authorizes ANY command beginning with:
  make
in repo /Users/x/src/docket. A workflow or issue in this repo may run
`make <anything>` without further approval. Use a full argv instead unless you
need this.
```

The warning names the repo because the blast radius is repo-scoped, and it says
what to do instead. Under `--json` it rides in the response as a `warnings`
array; it is never suppressed by `--yes` (§3.5) — suppressing the warning is
exactly the thing that would make the conversational posture unsafe.

## 3.4 REPO IDENTITY — what binds an entry to "this repo"

§4 requires: *"each entry binds to the repo it was approved in unless `--global`
is explicitly chosen — an argv trusted for one project does not execute in a
malicious clone of another."* The spec does not say what "the repo it was
approved in" **is**, and that definition is the load-bearing decision of this
whole section — get it wrong and T1 reopens. It is pinned here.

**The rule: a repo's identity is the absolute, symlink-resolved filesystem path
of the directory containing its `.docket/` store.**

Precisely:

| # | Clause |
|---|---|
| P1 | The identity is computed as `filepath.EvalSymlinks(filepath.Abs(D))` where `D` is `config.Resolve`'s Identity: for an env- or locally-resolved store, the store's parent (the rule as originally ratified); under the shared `~/.docket` store, the git common directory with a trailing `/.git` stripped — the one path every worktree of a repository shares, still a filesystem path and not repo content, so the anti-forgery argument below is unchanged. Amendment (operator request, 2026-08-09): worktrees of one repository share entries; a repository, not a checkout, is what an operator trusts. |
| P2 | An entry matches the current repo iff `entry.repo` **string-equals** the current identity, **or** `entry.global` is true. |
| P3 | `global = true` requires the explicit `--global` flag at `trust add`; there is no way to get it implicitly, and `trust list` renders global entries distinctly. |
| P4 | An entry with neither `repo` nor `global` is a **malformed file** — `VALIDATION_ERROR` naming the entry — never "matches everything". A missing binding failing open would make a hand-edited file a bypass. |

### 3.4.1 Why the path, and the argument against the malicious clone

The candidates, and why each was rejected or chosen:

| Candidate | Rejected because |
|---|---|
| **git remote URL** | Repo-controlled. `.git/config` is a file the clone ships or the attacker edits; a hostile clone sets its `origin` to the victim's trusted URL and inherits every entry. It also fails for repos with no remote — a local-only repo would be untrustable — and for the same repo cloned twice from the same remote, where the operator plausibly wants *different* answers. **This is the candidate that reopens T1, so it is named here to keep it rejected.** |
| **git root commit hash** | Also repo-controlled (a clone has the same root commit — that is the point of a clone) and therefore has the same failure: `git clone` a trusted project, add a hostile workflow, and the entries match. It is *worse* than the remote URL because it looks cryptographic. |
| **`.docket/issues.db` content or a stored repo UUID** | The database is repo content: it is committed in this very repository. A clone carries the UUID. Same failure again. |
| **absolute filesystem path** *(chosen)* | Not repo-controlled. A clone lives somewhere else on the operator's disk; the path differs; the entries do not match. |

**The argument against the malicious clone, stated directly.** An operator trusts
`["make","test"]` in `/Users/x/src/docket`. An attacker publishes a fork with a
hostile `standard-dev.toml` whose `tests` gate is that same argv, and the
operator clones it to `/Users/x/src/docket-fork`. At the gate:

1. The runner computes the current repo identity: `/Users/x/src/docket-fork`.
2. It matches candidate entries: the `tests` entry has
   `repo = "/Users/x/src/docket"`. P2 fails. `global` is false. No match.
3. The gate is recorded `verdict = "unmatched"`, **the process table is never
   touched**, and the step routes per `on_fail`.

The clone executes nothing without a *new*, explicit `trust add` performed by the
operator while standing in the fork — which is exactly what §9 item 6 requires,
and it is `TestMaliciousCloneExecutesNothing`'s script verbatim.

**Symlink resolution (P1) is why this holds under the obvious dodge.** Without
`EvalSymlinks`, an attacker who can create a symlink in the operator's home
(`ln -s ~/src/docket-fork ~/src/docket`) makes the fork *report* the trusted
path. Resolving both sides to their real paths defeats it. The residual — an
attacker who can move or replace the trusted directory itself — is the
file-permission boundary again (§2), where it belongs.

**The known cost, recorded rather than discovered later:** moving a repository
(`mv ~/src/docket ~/work/docket`) invalidates its trust entries. That is the
correct direction of failure — a moved repo re-earns trust; a hostile clone never
inherits it — and `trust list` shows the stale `repo` path so the operator can
see why a gate went `unmatched`. **The refusal message says so**, because
"unmatched" without an explanation sends an operator hunting: the `unmatched`
reason names the gate, the argv, and, when an entry with the same argv exists for
a *different* repo, says the entry is bound elsewhere and names that path.

## 3.5 The TOFU add flow, and `--yes`

§4, verbatim: *"unknown names require an explicit one-time `trust add` (TOFU,
argv-hash recorded)"* and, on the posture: *"in this solution the session
proposes, the human approves in-chat, and the session runs `trust add --yes` —
the harness's own command-permission prompt is the human-confirmation backstop.
The residual risk … is accepted and bounded by full-argv hashing plus the
permission layer (**upstream D14**)."*

| Verb | Flags | Effect |
|---|---|---|
| `docket trust add <name> -- <argv…>` | `--global`, `--prefix`, `--re-runnable`, `--tree`, `--flaky`, `--timeout D`, `--yes`, `--json[=v2]` | writes one entry, binding to the current repo unless `--global` |
| `docket trust list` | `--global`, `--all`, `--json[=v2]` | entries for this repo (plus globals); `--all` shows every repo's. A `Collection`, so v2 renders `{items,total,truncated}` |
| `docket trust rm <name>` | `--global`, `--json[=v2]` | removes the entry bound to this repo (or the global one) |

**`--` separates docket's flags from the trusted argv**, and everything after it
is taken **verbatim as argv elements** — no splitting, no globbing, no expansion.
This is the mechanism that makes T2 closed at the point of entry: the operator's
own shell has already tokenized, and docket stores those tokens. A trailing
`--prefix` after the `--` is part of the trusted command, not a docket flag, and
the help text says so.

**Interactive by default; `--yes` skips the prompt.** Without `--yes` and with a
TTY, `trust add` prints the argv, the repo binding, and every flag's effect, then
asks for confirmation (the repo's existing `huh` idiom, SKILL.md's interactive
forms section). With `--yes`, or with no TTY, it writes without prompting — and
`--yes` is what a session runs, per D14.

**`--yes` suppresses the prompt, never the disclosure.** The argv, the repo
binding, and the `--prefix` over-authorization warning are printed on **every**
add, including `--yes` ones, to stderr in human mode and into the JSON response
under `--json`. The reason is the whole of D14's bound: the harness's permission
prompt shows the human the *command line* `docket trust add --yes -- curl … | sh`,
so what makes that backstop work is that the argv is visible in the command
itself. Anything docket prints is defense in depth on top of that; anything
docket *hides* would erode it.

**Idempotence and conflict:**

| Situation | Behavior |
|---|---|
| `trust add` of a name+repo that does not exist | insert; exit 0 |
| identical argv and flags at an existing name+repo | idempotent success, nothing written; exit 0 |
| **different argv or flags** at an existing name+repo | `CONFLICT` (exit 4) naming both argvs and instructing `trust rm` first |

The last row is the important one. A silent overwrite means a trusted name's
meaning can change without the operator seeing the old value — the same
reasoning that makes a re-register with differing bytes a `CONFLICT`
(engine-spine §4.1), applied to a security-relevant file.

### 3.5.1 The read-modify-write is locked (F5)

I5's temp-file-plus-rename makes each **write** atomic — a reader never sees a
half-written store, and a crash mid-write leaves the old file intact. It does not
make the **read-modify-write** atomic, and `trust add` and `trust rm` are exactly
that: read the whole file, add or remove one entry, write the whole file back.
Two concurrent `trust add`s of different names therefore interleave as
read-A / read-B / write-A / write-B, and **B's write silently drops A's entry**.
The operator sees two successful adds and has one.

This is not a hypothetical for this design: D14's posture is that a *session*
runs `trust add --yes`, and a run with parallel steps can have more than one
session doing so, in a repo where an operator may also be typing one by hand.
Losing an entry is not a security hole — it fails closed, and the lost gate goes
`unmatched` — but it is a **silent** failure of an authorization operation the
operator was told succeeded, and the debugging story ("I definitely trusted
that") is bad.

| # | Clause |
|---|---|
| W1 | `trust add` and `trust rm` hold an **exclusive `flock(2)`** (`syscall.Flock`, `LOCK_EX`) across the **whole** read-modify-write: acquire, re-read the store from disk, apply the change, temp-write, `rename`, release. The lock is released by fd close, so a crashed or killed `docket trust` leaves no stale lock — the same property §7.4 L1 chooses `flock` for. |
| W2 | **The lock is on a sibling `trust.toml.lock`**, created `0600` in the same `0700` directory, not on `trust.toml` itself. Locking the store file directly does not work with rename-based writes: the `rename` replaces the inode, so the lock the writer holds and the lock the next writer takes are on **different files** and exclude nothing. The sibling's inode is stable. |
| W3 | The lockfile is opened with §3.2's integrity discipline — `O_NOFOLLOW`, refuse a non-regular existing file, owner and mode checked as I1–I4 check the store. It lives in the user-owned config directory, so the exposure is smaller than §7.4 L7's, but the rule is the same rule and stating it once here means an implementer does not have to decide. |
| W4 | **Reads are not locked.** A gate's trust read (§7.2 M1) takes the store as a single atomic snapshot via one `read()` of a rename-published file, which I5 already guarantees is a complete and consistent version. Adding a shared lock on the read path would make every gate contend with every other for no correctness gain, and — worse — would let a hung `trust add` block gate execution. The read path stays lock-free. |
| W5 | Acquisition **blocks**, bounded by a short timeout (**5s**, a package constant); exceeding it is a `CONFLICT` (exit 4) naming the lock path and saying another `docket trust` is in progress. A `trust add` is a human-scale operation and 5s is far beyond its honest duration, so a timeout means something is genuinely stuck rather than merely contended. |

`TestConcurrentTrustAddsBothLand`: N goroutines (and, as a second case, N
subprocesses, since the flock's real job is cross-process) each `trust add` a
distinct name into one sandbox store; assert **every** entry is present
afterwards and the file parses. The subprocess case is the one that fails against
an implementation that only takes an in-process mutex — the same reasoning §7.4
L5 applies in the other direction.

### AMENDMENT (DKT-265) — a stub declares itself

An entry may carry `stub = true`, set by `docket trust add --stub`. It declares
that the argv is a PLACEHOLDER rather than the check the entry's name implies —
an `echo`, a `/usr/bin/true`, a script that exits 0 without looking at anything.

**The problem it solves.** RUN-17's GIT-50 recorded `build`, `secret-scan`, and
`tests` all passing, every one an echo stub; RUN-19's `secret-scan` and
`ac-commands` passed via `/usr/bin/true`. Nothing in `gate_results`, the run
report, or the review packet distinguished those rows from a real scan finding
nothing. A run whose entire assurance rested on stubs was legible only by
opening the trust store and reading argvs.

**Stubs stay legitimate.** They are how a repo without a scanner installed
exercises a workflow's shape, and forbidding them would push the same
placeholder into a script with a more convincing name — the same argument §3.3
makes about `--prefix`. The fix is not refusal; it is that hollow green stays
visibly hollow.

**It is DECLARED, not detected**, on the same footing as `re_runnable`, `tree`,
`flaky`, and `network`. Core cannot inspect an argv and tell a real check from a
convincing one; a heuristic would miss `./scan.sh` whose body is `exit 0` and
would flag a legitimate `true` guard. The operator writing the entry knows.

**It changes NO execution behavior.** A stub entry matches, spawns, and records
exactly as any other, and the marker travels with the verdict rather than
gating it. It rides in §3.6's event for the reason `tree` and `network` do — it
is what the grant is really granting — and flipping it on a re-add is a
`CONFLICT`, since a silent flip would convert hollow assurance into assurance
that reads as real, or the reverse, while the store showed only a re-approval.

`gate_results.stub_entry` (v21) carries it onto every row that names a trust
entry, including unmatched, skipped, and failing ones: the field describes the
ENTRY, not the outcome, and a reader should not have to wonder whether its
absence on a failing row means "not a stub" or "only recorded when it passes".
It is deliberately NOT the pre-existing `gate_results.stub`, which marks a row
migrated from an S3 `gate_trail` — that is a fact about which era produced the
row, and one column carrying both would answer neither question.

### AMENDMENT (DKT-607) — a stub records its reason

A stub entry may carry `stub_reason`, set by `docket trust add --stub
--stub-reason "<why; tracking issue>"`. It records the DECISION behind the
placeholder: why no real check exists yet and which issue tracks replacing it
(e.g. `"no scanner selected yet; removal tracked by DKT-607"`).

**The problem it solves.** DKT-265 made hollow green visible; it did not make it
EXPLAINED. Two tribunal seats on DKT-V196 independently rediscovered the same
corpus stubs (`secret-scan`, `sdet-abuse`) because the decision that they remain
stubs lived only in tribunal transcripts. The project's stub-gate policy
requires every stub to have a removal-tracking issue; this field is where that
reference becomes discoverable from the surfaces an operator actually reads.

**Where it surfaces.** The activation gate preflight prints it under the stub's
own line (and carries it as `stub_reason` in the JSON row); a stub with NO
recorded reason gets a remedy line naming `--stub-reason`. `trust list` renders
it inside the `stub(no-real-check: …)` marker, and it rides §3.6's event beside
`stub`.

**Constraints.** It only makes sense alongside `stub = true`: a reason on a
non-stub entry is refused at parse and at add, the closed direction. It is
OPTIONAL on a stub — every pre-DKT-607 stub entry has none and keeps loading
with an empty reason. Changing or erasing it on a re-add is a `CONFLICT`, since
the reason is the documented decision and a silent rewrite would swap one
decision for another under a re-approval.

## 3.6 Trust changes are event-logged (T9)

`trust add` and `trust rm` write a **`trust-added` / `trust-removed` event** into
the events table when invoked inside a repo whose `.docket/issues.db` exists.
Data: the gate name, the argv hash (**never the argv itself in the event body**,
so a trusted command's arguments do not land in an event feed a run report
renders), the repo binding, the flags, and — per the DKT-263 amendment below —
the actor and the cwd.

**AMENDMENT (DKT-263): the event records WHO.** T9's residual was written as an
auditability question and answered with a timestamp, which turned out to be its
smaller half: a run whose trail brackets the event is a run during which the
grant happened, but two concurrent sessions on one machine bracket identically,
and this repo's retro twice had to recover by-whom by correlating against
session logs. `actor` (the git identity, falling back to the OS username and
then to `unknown`) and `cwd` (the invocation's working directory) close it.

Both are resolved at the CLI call site, never inside the recording helper: the
engine has no business shelling out to `git config`, and a helper that resolved
its own actor could attribute an event to whoever happened to be running the
process that called it. Both are written unconditionally, empty string included,
for the same reason the false flags are — an omitted key is byte-identical to
one from a writer that never had the field, so a reader could not tell an
unresolvable actor from a build that records no actors.

Neither is **authenticated**, and the amendment does not claim otherwise:
events_read's actor CLASS (`human`) is a classification of the kind, while these
are the invoker's own account of themselves. That is the footing step metadata
already stands on, and it is the right one — a grant is the one act that widens
what may execute, so its trail should not require a join against a clock.

Two consequences: a run's trail records a grant made mid-run, which is what makes
T9's residual auditable rather than invisible; and `events` gains two kinds,
which extends the closed set (engine-spine §7.6) — §6.4 lists every event kind
this stage adds, so §9 item 2's closed-set check keeps passing.

Recording is **mandatory-or-fail** inside a repo (DKT-81): the event is written
before the store, and a recording failure fails the verb with the store
untouched, so the ledger and the allowlist cannot silently diverge. An
**idempotent re-add emits no event** — an event proves a change, never mere
repetition.

`trust list` and `trust rm` outside a repo work and write no event; the trust
store is user-level and does not require a repo to manage. `add` and `rm`
outside a repo say on stderr (and in the JSON warnings) that nothing was
recorded.

---

# 4. Schema v8

## 4.1 The version span, unchanged

reliability-delta §2's mapping is authoritative and is not re-litigated. This
stage occupies its **v8** row ("gate_results, trust-cache").

| Schema | Stage (§10) | Contents |
|---|---|---|
| v5–v7 | 1–3 | shipped |
| **v8** | 4 — gates + trust model | **this stage**: `gate_results`, `trust_cache`; the `gate_trail` migration |
| v9–v10 | 5–6 | later |

**v8 lands as one migration function** (`migrateV7ToV8`), sliced across this
stage's two commit groups exactly as v7 was across four (engine-spine §2): the
stamp moves to 8 in **group 2** — group 1 is pure Go with no DDL at all — and
does not move again.

**The v8 rewind guard follows the established pattern and probes the full v8
sentinel set** (`gate_results`, `trust_cache`), as `v8Sentinels` next to the
migration, with `TestRewindGuardProbesEverySentinel` extended to assert one entry
per table the v8 DDL creates. The guard's shape is the v7 one, verbatim, and for
the same reason — see §4.4 for the specific proof this stage owes.

## 4.2 `gate_results` — §11.4's shape as a table (closes DKT-16)

§11.4, verbatim:

```
gate result     { step, gate, argv, exit, duration_ms, output, truncated, verdict }
```

```sql
CREATE TABLE IF NOT EXISTS gate_results (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id       INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
    gate          TEXT    NOT NULL,          -- the gate name, opaque
    ordinal       INTEGER NOT NULL DEFAULT 0,-- re-run index (§5.6): 0, 1, 2 …
    argv          TEXT,                      -- JSON array; NULL when unmatched
    exit          INTEGER,                   -- NULL when unmatched (nothing ran)
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    output        TEXT    NOT NULL DEFAULT '',
    truncated     INTEGER NOT NULL DEFAULT 0,
    verdict       TEXT    NOT NULL,          -- 'pass' | 'fail' | 'unmatched'
    pre           INTEGER NOT NULL DEFAULT 0,-- a pre-gate result (§7.6)
    stub          INTEGER NOT NULL DEFAULT 0,-- 1 ONLY on rows migrated from S3
    reason        TEXT,                      -- why unmatched / timed out (§6.3)
    created_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_gate_results_step ON gate_results(step_id);
CREATE INDEX IF NOT EXISTS idx_gate_results_run ON gate_results(run_id);
```

Field-shape notes, each a decision rather than an accident:

- **`step` in the wire shape is the step id**; the table stores `step_id` and the
  emitter renders `STEP-N`, plus the `instance` identity alongside per DKT-15's
  established pattern (engine-spine §10 A1). No new amendment: DKT-15 already
  covers "the rendered instance identity rides alongside the id".
- **`stub` is absent on real results** — the work order's explicit requirement.
  In the table it is a `DEFAULT 0` column; **in the JSON it is `omitempty`**
  (the existing `GateResult.Stub` field already carries `json:"stub,omitempty"`),
  so a result this stage produces serializes with **no `stub` key at all**. A
  migrated S3 row carries `stub: true` forever, which is what makes the S3→S4
  window auditable after the fact.
- **`argv` and `exit` are NULL on an `unmatched` result**, not `[]` and `0`. A
  zero exit code on a gate that never ran is exactly the confusion T11 exists to
  prevent; NULL is the honest encoding of "no process existed".
- **`ordinal`** carries §4's *"flaky-declared re-runs recorded individually"*:
  each re-run is its own row (§5.6), never an overwrite and never an aggregate.
- **`reason`** is an addition to §11.4's listed fields, recorded as an amendment
  (§11 A6) rather than slipped in: an `unmatched` verdict with no reason forces
  an operator to guess between "no entry", "bound to another repo", and "prefix
  not opted in" (§3.4's stale-path case).

## 4.3 The `gate_trail` → `gate_results` migration (DKT-16's closure)

DKT-16 recorded the storage-location deviation and its exit condition: *"S4
creates `gate_results` (v8) and migrates the trail into it."* This is that
migration.

| # | Clause |
|---|---|
| G1 | `migrateV7ToV8` creates `gate_results`, then reads **every** `steps` row with a non-empty `gate_trail`, parses the JSON array of §11.4-shaped results, and inserts one `gate_results` row per element, in array order, with `ordinal` = array index, `step_id`/`run_id` from the step, and `created_at_ms` = the step's `updated_at_ms` (the trail carries no per-result timestamp; using the step's is the honest approximation and is documented in the column comment). |
| G2 | **Every migrated row is stamped `stub = 1`.** Every result in a `gate_trail` was produced by S3's `PassThroughRunner`, which is the only thing that ever wrote one — so the stamp is a fact, not a guess. This is what keeps T11's audit true across the version boundary. |
| G3 | **`steps.gate_trail` is retained, not dropped.** The never-mutate rule (reliability-delta §2.1) forbids destructive column changes; SQLite's `DROP COLUMN` would rewrite the table; and the column is the migration's own evidence. It stops being **written** at v8 — the runner writes `gate_results` — and `TestGateTrailIsNotWrittenAtV8` asserts that. Readers move to `gate_results` in the same commit. |
| G4 | The migration is **idempotent**: re-running it must not duplicate rows. Guarded by a `UNIQUE(step_id, gate, ordinal)` index created before the backfill, with the backfill using `INSERT OR IGNORE`. Idempotence is not optional here — the v8 rewind guard (§4.1) can re-run this migration against a partially-migrated database by design. |
| G5 | A `gate_trail` that fails to parse is **not** a migration failure: the row is skipped, and one `gate_results` row is written with `verdict='unmatched'`, `stub=1`, and `reason` naming the parse failure. A migration that aborts on one malformed JSON blob makes a database unopenable; the failure is recorded where an operator will see it. |

### 4.4 The upgrade-path proof against a v7-stamped DB (this repo's tracker, again)

engine-spine §2 made this obligation concrete for v7: this repository's own
`.docket/issues.db` is dogfooded across the stage, so it is *stamped at the
version under development* while that version's DDL is still growing. The same
shape recurs here and the same proof is owed.

**The v8-specific shapes that must upgrade cleanly**, each a test case in
`internal/db/migrate_v8_test.go`:

| # | Starting database | Required outcome |
|---|---|---|
| U1 | **This repo's tracker**: stamped **7**, all v7 sentinels present, `steps` empty (the tracker has issues but no runs) | migrates to 8; `gate_results` and `trust_cache` created; zero rows backfilled; every existing verb byte-identical (§9.3's dormancy proof) |
| U2 | stamped 7 with a **populated** `gate_trail` (the S3 QA fixture's completed loop run) | every trail element becomes a `gate_results` row, in order, `ordinal` ascending from 0, every row `stub=1`; the trail column still holds its original bytes (G3) |
| U3 | stamped **8** but `gate_results` **absent** — the group-2-partial dogfood shape, exactly the v7 trap | the rewind guard rewinds to 7 and re-runs; the database ends complete |
| U4 | stamped 8, `gate_results` present, **`trust_cache` absent** | same rewind; both tables present after |
| U5 | migration run **twice** against U2's database | no duplicate rows (G4); counts identical |
| U6 | **`scripts/qa/fixtures/v4-baseline.db`** | migrates **4→8 in one pass**; v8 structures asserted present **before** the golden diff is trusted (engine-spine §3's rule — a golden diff against a database that failed to migrate passes vacuously) |
| U7 | stamped 7 with a **malformed** `gate_trail` | G5: migration succeeds, one `unmatched`/`stub` row with the parse failure in `reason` |

U3 is the row that exists because of the v7 lesson. It is not hypothetical: this
stage's group 1 ships no DDL, group 2 ships all of it, and the operator's own
tracker is migrated by whatever binary happens to be built between the two.

## 4.5 `trust_cache` — what it is, and what it is not

reliability-delta §2's v8 row names a "trust-cache". Its scope is pinned here
because a cache of a security decision is a dangerous thing to leave undefined.

```sql
CREATE TABLE IF NOT EXISTS trust_cache (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    gate          TEXT    NOT NULL,
    argv_sha256   TEXT    NOT NULL,      -- the argv that was matched
    entry_name    TEXT    NOT NULL,      -- which trust entry matched
    matched       INTEGER NOT NULL,      -- 1 = matched, 0 = unmatched
    prefix        INTEGER NOT NULL DEFAULT 0,
    at_ms         INTEGER NOT NULL,
    UNIQUE(run_id, gate, argv_sha256, at_ms)
);
```

**It is an audit record, never an authorization shortcut.** Every gate consults
the **live trust store** on every execution (§7.2); nothing is ever executed
because a cache row says a previous run matched. The table answers "what did this
run consider trusted, and when" for a retro and for `run report` (S6) — the same
role the pins table plays for versions.

Stated explicitly because the opposite implementation is the tempting one: if a
cache hit could authorize a spawn, then `trust rm` would not take effect until
the cache cleared, which is a revocation failure and a T4 variant with a much
wider window.

---

# 5. `internal/exec` — the runner mechanics (§4)

Commit group 1, with §3. **Pure and unit-testable without the engine**: it takes
an argv, an environment policy, a timeout, and a working directory, and returns
an exit code, captured output, a truncation flag, and a duration. It has no
database handle, no knowledge of steps or gates, and no dependency on
`internal/engine` — which is what makes §9.1's table tests possible without
building a run.

§4's mechanics sentence is the specification, and each clause below maps to one
of its phrases:

> Mechanics: resolved argv, no shell interpolation, cwd repo root, env allowlist,
> timeout with process-group kill, captured output with explicit truncation,
> flaky-declared re-runs recorded individually.

## 5.1 Resolved argv, no shell interpolation (T2)

```go
// Spec is one command to run. There is no Shell field, no Command string, and
// no way to express one — the type makes the unsafe thing unrepresentable.
type Spec struct {
    Argv    []string      // argv[0] is the program; never a shell string
    Dir     string        // repo root (§5.2)
    Env     []string      // the constructed allowlist (§5.3); never os.Environ()
    Timeout time.Duration
}
```

**`internal/exec` never constructs a shell command**, and the guard is
structural rather than a convention: the package has no `Command string` field
to fill in, so producing one would require adding an API. §9.1's source guard
asserts the package's source references none of `sh`, `bash`, `zsh`, `cmd.exe`,
`/bin/sh`, or `os/exec`'s shell idioms — a grep-level check in the same spirit as
the `--token`-flag guard (security.md §1.2), and for the same reason: the rule is
easy to state and easy to violate accidentally three refactors later.

Consequently `["echo", "a; rm -rf ~"]` runs `echo` with **one** argument whose
text contains a semicolon. Nothing splits it, nothing expands `~`, and no glob
matches. The full metacharacter table is `TestNoShellEverInvoked`'s cases (§9.1).

## 5.2 Tokenization happens once, at the boundary

An argv is a list of strings everywhere inside docket. The only places a *string*
becomes an argv are:

| # | Boundary | Rule |
|---|---|---|
| K1 | `docket trust add … -- make test` | the operator's shell already tokenized; the elements after `--` are the argv **verbatim**, no further processing |
| K2 | a harvested fence line (`run_fences.command`, one line of a fenced block) | tokenized **once, at execution**, by a POSIX-ish splitter that handles single quotes, double quotes, and backslash escapes **and performs no expansion whatsoever** — no variable substitution, no command substitution, no globbing, no tilde, no brace, no word splitting on anything but unquoted whitespace |

**K2's splitter is the one piece of new parsing this stage adds to a security
path**, so its non-goals are pinned as hard as its goals: a `$` is a literal
dollar sign; a `` ` `` is a literal backtick; `*` is a literal asterisk;
`$(whoami)` is a five-token-free single argument containing those characters. A
line the splitter cannot tokenize (an unterminated quote) is **not executed** —
it records `verdict='unmatched'` with `reason` naming the parse failure, which is
the same fail-closed direction as every other unknown in this stage.

`TestFenceSplitterExpandsNothing` is a table over every metacharacter class, each
asserting the character survives into the argv as a literal.

**`argv[0]` resolution** (T15): `exec.LookPath` against the allowlisted `PATH`,
never against `Dir`. Go's `exec.Command` historically resolved a bare name
against the working directory on some platforms; `exec.ErrDot` exists precisely
for this, and the runner treats it as a hard refusal rather than executing.
Absolute paths resolve to themselves.

### 5.2.1 Resolved `argv[0]` may not land inside the repo (T17)

`LookPath` searching a clean `PATH` is not sufficient, because **a `PATH` entry
can itself point into the repository**. The concrete vector: an operator
direnv-allows a repo's `.envrc` for legitimate dev tooling and it prepends
`$PWD/bin`; or the repo simply ships a `bin/` the operator added to `PATH` once.
From then on, a trust entry naming `make` authorizes whatever executable the repo
places at `bin/make`. The trust decision was about a command name; the repo
supplies the code. T15's mechanism does not see this at all — the `PATH` is the
operator's own, unmodified by docket, and contains no `.`.

The rule, applied after resolution and before the spawn:

| # | Clause |
|---|---|
| R1 | After `LookPath` returns a path, the runner computes `filepath.EvalSymlinks(filepath.Abs(resolved))` — the **symlink-resolved** path of the binary that would actually execute. Resolving symlinks is required, not cosmetic: a `PATH` directory outside the repo containing `make → <repo>/bin/make` is the same attack with one indirection. |
| R2 | It computes the repo root identity the same way §3.4 P1 does: `filepath.EvalSymlinks(filepath.Abs(repoRoot))`. **The same function on both sides**, so the comparison cannot be defeated by a symlinked checkout. |
| R3 | If the resolved binary lies **at or under** the repo root — a path-component-wise containment test (`filepath.Rel` yielding a result that does not escape via `..`), never a string-prefix test, since `/src/docket-evil` must not count as under `/src/docket` — the gate is **refused**: no spawn, `verdict = "unmatched"`, and `reason` naming the gate, the resolved absolute path, and the rule ("`argv[0]` resolved to a path inside the repository; trust the absolute path explicitly if this is intended"). |
| R4 | **The one exception**: the matched trust entry's own `argv[0]` is an **absolute path** and, after the same `EvalSymlinks` normalization, string-equals the resolved path. Then the operator trusted *that file*, not a name that happened to resolve to it, and it executes. Repo-owned scripts therefore remain usable — `docket trust add build -- /Users/x/src/docket/scripts/build.sh` works and always did the right thing. |
| R5 | The check is **unconditional** — it applies to named gates, fence gates, and pre-gates alike, in `internal/exec`, so there is no path that skips it. It costs one `EvalSymlinks` per spawn. |

**Why refuse rather than sanitize `PATH`.** Stripping repo-resident entries out
of the child's `PATH` was the alternative and is worse: it silently changes what
the operator's own tooling resolves to (a direnv-managed toolchain is often
exactly what the check is *supposed* to use), and it fails open on the next way a
`PATH` entry can reach repo content (a symlink farm, a `$HOME`-relative path the
repo can write). Refusing at the resolved path is a single check at the one place
that matters, and it fails **closed**, with a reason that tells the operator the
exact remedy: name the absolute path in the trust entry.

**What this deliberately does not close.** A repo that supplies a *library*, a
Makefile target, a compiler plugin, or any other input to a trusted binary still
influences what that binary does — that is §2.1 residual 1, and no resolution
rule reaches it. R1–R5 close the narrower and sharper hole: the **executable
itself** being repo content chosen by name.

**Residual, recorded (T15):** an operator whose own `PATH` contains `.` is
exposed by every tool they run, and docket does not repair their `PATH`. It does
not *add* to the exposure: it never prepends the repo, never appends `Dir`, and
never consults a repo-provided `PATH`. T17's rule now additionally means that
even when their `PATH` does reach repo content, docket's own execution refuses to
follow it.

## 5.3 The environment allowlist (T5)

**Allowlist, never denylist.** The child environment is *constructed*, so a
variable is present only if this table names it. A denylist would fail open on
the next environment variable anyone invents.

**The default allowlist**, enumerated as the work order requires:

| Variable | Why |
|---|---|
| `PATH` | argv[0] resolution and the child's own tool lookups; without it almost nothing runs |
| `HOME` | toolchains resolve caches, config, and credentials-free defaults from it; absent, many tools write to `/` or fail |
| `USER`, `LOGNAME` | tools that stamp output with a user name |
| `SHELL` | **passed but never consulted by docket** — some tools read it for informational output. Docket spawns no shell regardless (§5.1) |
| `LANG`, `LC_ALL`, `LC_CTYPE` | text encoding; absent, tools produce mojibake or refuse non-ASCII paths |
| `TZ` | timestamp rendering in a build's own output |
| `TMPDIR` | scratch space; absent, tools fall back to `/tmp` which may be unwritable in a sandbox |
| `TERM` | set to **`dumb`**, not inherited — see below |
| `SSL_CERT_FILE`, `SSL_CERT_DIR` | tools that make TLS connections need the trust store; absent, a check that fetches fails confusingly |
| `XDG_CACHE_HOME` | build caches; absent, a rebuild-from-scratch on every gate |

**Set by docket, not inherited:**

| Variable | Value | Why |
|---|---|---|
| `TERM` | `dumb` | a gate's output is captured, not displayed. An inherited `TERM` makes tools emit ANSI escapes into `output`, which then pollute a run report and a golden diff |
| `CI` | `1` | the near-universal convention for "non-interactive"; it makes tools skip prompts and progress spinners without docket having to know each tool |
| `DOCKET_GATE` | the gate name | so a check can behave differently under docket if its author wants; opaque to core |
| `DOCKET_REPO` | the repo root | the same value as `Dir`, for tools that need it in an env |
| `DOCKET_GATE_BASE` | the step's base commit sha, **worktree-recorded completion gates only** | so a range-shaped check can scan exactly the step's committed change — `DOCKET_GATE_BASE..HEAD` of the tree it runs in — see below *(added 2026-09-01, DKT-992)* |
| `DOCKET_STEP` | the step's reference, `STEP-N` | so a gate can ask the engine for its **own inputs** — the identity `docket step context` and `docket step artifacts` take — instead of re-deriving which step it is from `DOCKET_ISSUE` plus an instance-name convention; see below *(added 2026-09-03, DKT-1186)* |
| `GOLANGCI_LINT_CACHE`, `STATICCHECK_CACHE` | a scratch directory deleted with the tree, **gates measuring a reconstruction only** | both tools cache issues by package content while storing the absolute path each was found at, and re-open that path to find the `//nolint` that suppresses it. A reconstruction outlives neither, so its entries must not either — see §7.6's DKT-1166 amendment *(added 2026-09-03, DKT-1166)* |

**`DOCKET_GATE_BASE` — the step's committed range** *(DKT-992)*. Executors
commit **before** `step record`, so at gate time a worktree-recorded step's
tree is clean: a working-tree-only scan measures zero lines however large the
change (RUN-66's secret-scan passed 8/8 write steps that way), and a gate
guessing `git diff HEAD~1` is wrong for every multi-commit step. The engine
already knows the step's base — the worktree's **fork point**, the same
resolution the diff stage's `runDiffBase` applies — so completion gates of a
`--worktree`-recorded step export it:

- **Worktree-recorded step**: `DOCKET_GATE_BASE` names the commit the worktree
  was created from. `git diff $DOCKET_GATE_BASE..HEAD` in the gate's own cwd
  (the worktree, per DKT-9) is exactly the step's committed change — the same
  range the recorded `issue.diff` describes.
- **Non-worktree step**: the variable is **unset** — that is the documented
  pick between the two admissible encodings (unset, or equal to `HEAD`). The
  shared checkout has no fork point, the run's pinned commit is not this
  step's base (sibling work lands between them, DKT-42's over-attribution),
  and a live `HEAD` read is a value docket cannot vouch for as a range
  endpoint. Absence — never an invented sha — is the encoding, the same
  convention as `DOCKET_SCOPE`.
- The variable is also unset when the fork point cannot be resolved, and on
  the pre-claim path (a pre-gate measures the tree under review, not a
  recorded completion; after integration sweeps a worktree, no honest base
  survives to export).
- **Fail closed on absence**: a range-shaped gate that finds the variable
  absent while the tree is clean has nothing it can honestly scan, and should
  fail rather than pass having measured nothing — "we couldn't check, so
  carry on" is what makes a control decorative (N3).

**`DOCKET_STEP` — the gate's own identity** *(DKT-1186)*. A gate frequently
needs an **artifact an earlier step of the same issue produced** — a threat
model feeding an abuse-case check, a synthesis feeding a verifier. Nothing in
the child environment named the step, so the only route was to rebuild the
gate's identity from outside the engine: `docket step list --issue
$DOCKET_ISSUE`, pick the row by a **hardcoded instance-name convention**, parse
that listing's JSON shape, then `docket step artifacts` on the result. That is
three couplings to things the engine is free to change — instance naming,
listing order, wire shape — and each one breaks silently when it moves. It was
observed in the wild (RUN-80's activation gate, `agentic-services` commit
`897c0a7`) and flagged as a precedent not to set.

- **Value**: the step's rendered reference, `STEP-N` — precisely the argument
  `docket step context STEP-N` and `docket step artifacts STEP-N` take. The
  bundle's `inputs` **are** the artifacts the step was handed, so
  `docket step context $DOCKET_STEP` answers "what were my inputs" in one verb
  against a stable identity, and `docket step artifact ARTIFACT-N` fetches a
  body from there.
- **Set on both paths**: completion gates (the saga) and pre-gates (the
  pre-claim path) alike. The pre-claim path is where it matters most — a
  pre-gate runs *before* the claim hands the bundle over, so asking the engine
  by reference is its only route to the chain's artifacts.
- **No new authority.** Both verbs are read-only and take no token, and the
  reference is an identifier the child could already reconstruct by hand. This
  makes the lookup cheap and correct rather than conventional and fragile; it
  does not widen what a gate may do. `DOCKET_PATH` and `DOCKET_TOKEN` remain
  excluded, unchanged.
- **Unset, never `STEP-0`**: a gate spawned with no step in hand (a bare runner,
  a future caller) sees the variable **absent**. A well-formed id that resolves
  to nothing fails inside the gate's own tooling with a misleading message,
  where absence is a condition the gate can test — the same encoding
  `DOCKET_SCOPE` and `DOCKET_GATE_BASE` use.

**Excluded, by name, in addition to being absent from the allowlist:**

| Variable | Why the belt-and-braces |
|---|---|
| **`DOCKET_TOKEN`** | **T5's core case.** A capability token in a gate child converts code execution into engine authority: the child could `docket step complete` with a forged artifact under the live lease. It is absent from the allowlist, and the constructor **additionally asserts it is not in the constructed set** before spawning, failing hard if it is. Two mechanisms because one of them is a table that a future editor might extend carelessly |
| `DOCKET_PATH` | a gate that reaches back into the database is doing something the trust model does not cover; if a check genuinely needs the repo, `DOCKET_REPO` gives it the path without pointing at the store |
| every `*_TOKEN`, `*_KEY`, `*_SECRET`, `*_PASSWORD` in the parent | not needed by the allowlist mechanism (they are simply not listed), but §9.1 asserts a parent environment seeded with such variables produces a child that has none of them — the test that proves the allowlist is an allowlist |

**There is no way to extend the allowlist at this stage.** No flag, no config
key, no trust-entry field. A gate needing a credential is a use case this stage
does not serve, and adding the escape hatch before there is a real requirement
means shipping the mechanism that undoes T5. §10 records it as a deliberate
non-goal with the shape a future amendment would take.

`TestGateChildEnvIsExactlyTheAllowlist` asserts **set equality** — not that the
expected variables are present, but that *no others are*. Membership assertions
pass for an environment that also leaked forty other variables.

## 5.4 Timeout with process-group kill (T7)

| # | Clause |
|---|---|
| X1 | The child starts with `SysProcAttr{Setpgid: true}`, so it leads a new process group. |
| X2 | On timeout, the runner signals **the group** (`syscall.Kill(-pgid, SIGTERM)`), waits a **grace period** (2s), then `syscall.Kill(-pgid, SIGKILL)`. Signaling the group is what reaches a build's compilers and a test runner's servers. |
| X3 | Capture pipes are closed after the kill, and the runner **does not block on the pipes** waiting for grandchildren that inherited them to exit — the read loop is bounded by the same deadline. A wait-on-EOF here is the classic hang: a killed child's surviving grandchild holds the write end open forever. |
| X4 | A timed-out gate records `verdict = "fail"`, its partial captured output, `truncated` per §5.5, `duration_ms` = the elapsed time, and `reason` naming the timeout and the limit. **Not `unmatched`** — the command was trusted and did run; it failed by exceeding its bound. |
| X5 | The timeout is the trust entry's `timeout` when set, else a default of **5m**, a package constant at this stage rather than a config key (§10 records the config-key deferral). |

`TestTimeoutKillsTheWholeProcessGroup` spawns a gate whose child spawns a
`sleep`-alike grandchild that outlives its parent, and asserts by pid that the
grandchild is gone. Testing only the direct child would pass against an
implementation that kills the pid rather than the group — the exact bug X2
exists to prevent.

## 5.5 Capture with explicit truncation (T6)

| # | Clause |
|---|---|
| C1 | stdout and stderr are captured into **one interleaved stream**, in write order, which is what an operator reading a failed check wants. §11.4 has one `output` field, so this is the spec's shape, not a choice. |
| C2 | Capture is bounded at **256 KiB**. Reaching the cap sets `truncated = true` and the reader **stops consuming** — it does not keep reading and discarding, so a gate that emits forever cannot spin the reader. |
| C3 | A truncated capture keeps the **first** cap bytes, not the last. The first bytes of a failing build contain the invocation and the first error; the last contain a summary that is usually reproducible from the first. Stated because the opposite is a defensible choice, and a future editor should have to argue against a recorded reason. |
| C4 | Truncation is byte-exact and never splits a UTF-8 sequence in a way that produces invalid JSON: the recorded output is truncated to the cap and then **backed off to the last valid rune boundary**. |
| C5 | `truncated` reaches §11.4's wire shape verbatim, and QA asserts a truncated gate's result renders `"truncated": true`. |

C2's cap is a package constant. It is not `context.error_bytes` — that governs
the context bundle, a different thing with a different consumer — and reusing it
would couple a security bound to a workflow-authoring knob.

## 5.6 Flaky-declared re-runs, recorded individually

§4, verbatim: *"flaky-declared re-runs recorded individually."*

| # | Clause |
|---|---|
| F1 | A trust entry may declare `flaky = true`. **Only then** does a failing gate re-run. A gate not declared flaky runs exactly once per saga gate stage. |
| F2 | A flaky gate that **fails** re-runs up to **2 additional times** (3 attempts total, a package constant). It stops at the first pass. |
| F3 | **Every attempt is its own `gate_results` row**, with `ordinal` 0, 1, 2 — argv, exit, output, and duration recorded per attempt. Nothing is overwritten and nothing is aggregated. This is what "recorded individually" means, and it is the point: a check that passes on the third try is a fact the operator should be able to see. |
| F4 | The gate's **verdict for routing** is the verdict of the **last** attempt. A gate that passes on attempt 2 passes; one that fails all three fails. |
| F5 | `flaky` interacts with `re_runnable` (§7.5) but is **not** the same flag, and conflating them is the likely implementation error: `flaky` governs re-running **within one gate stage** after a failure; `re_runnable` governs re-running **after a crash**, when the engine does not know whether the previous attempt ran at all. A gate may be either, both, or neither, and §7.5's table enumerates all four combinations. |

## 5.7 Rendering untrusted bytes to a terminal (T18)

Three value classes in this stage are **attacker-influenced and printed to the
operator**: an `argv` (from a fence line an issue author wrote, or echoed back by
`trust add`), a **fence command** (§7.7 prints every harvested one verbatim), and
a **`reason`** string (which quotes fence content when a line is unparseable or
unmatched). All three reach a TTY, and a TTY interprets control bytes.

**The attack this closes** is not exotic. A fence line of

```
make test\x1b[2K\r  make lint
```

prints as `  make lint` — the escape clears the line and the carriage return
resets the cursor, so the operator approving what they see approves something
else. Variants: `\x1b[1A` to overwrite the line above, a bare `\r` for the same
effect without CSI, `\x1b]0;…\x07` to rewrite the window title, and `\x08`
runs. D14's entire human backstop is the premise that **what is displayed is what
is approved**, so any divergence between rendered text and stored bytes is a
direct attack on the ratified control, not a cosmetic bug.

**The rule, stated so an implementation cannot pick differently:**

| # | Clause |
|---|---|
| E1 | **The mechanism is Go's `%q`** — `strconv.Quote` semantics — not C0/C1 stripping. Chosen deliberately: `%q` is **lossless and reversible**, so the operator sees that something odd is present (`"make test\x1b[2K\r  make lint"`) rather than seeing sanitized-but-plausible text with the hostile bytes silently removed. Stripping would render the attack line as `make test  make lint`, which is *still* misleading; escaping renders it as what it is. It is also one stdlib call with no hand-rolled character table to get wrong. |
| E2 | **One renderer, used everywhere.** A single exported helper in `internal/exec` (or a small shared `render` package — a group-1 decision) is the only way these three value classes reach human-mode output. An argv renders as its elements individually quoted and space-joined; a fence command and a `reason` render as one quoted string. |
| E3 | **Every human-mode print site** uses it: `trust add`'s disclosure (the argv and the repo binding), the `--prefix` over-authorization warning's argv block (§3.3), `trust list`'s argv column, §7.7's activation report (S1) including the per-command reason, §6.3's `unmatched` reasons wherever rendered, and the timeout reason (X4). |
| E4 | **JSON mode is untouched and is inherently safe.** `encoding/json` escapes control bytes as `` etc. by contract, and the consumer is a program that does not interpret them. Quoting on top of JSON encoding would double-escape and corrupt the value a machine consumer reads, so `--json` output carries the **raw** command bytes exactly as stored (§7.7 S2's `fences` array). The asymmetry is the point: the escaping exists for terminals, and only terminals get it. |
| E5 | **The stored bytes are never modified.** `run_fences.command`, `gate_results.argv`, and the trust entry's `argv` keep exactly the bytes that were harvested or approved — escaping is a *rendering* transform, applied at the print boundary. Mutating stored bytes would break §7.3's hash re-verification, which is the one thing that must compare unmodified content. |

**`TestOperatorFacingRenderingEscapesControlBytes`** (§9.1) drives a fence command
containing `\x1b[2K\r`, a bare `\n`, and a `\x07` through §7.7's report and
through `trust add`'s disclosure, and asserts the escape sequences appear as
visible text and that no raw `\x1b` or `\r` byte reaches the writer. A second
assertion covers E4: the same command under `--json` round-trips to the original
bytes. The **surface guard** half — that no human-mode print of an argv, a fence
command, or a reason bypasses the renderer — is a source-level check in the
family of §5.1's no-shell guard and §3.1's no-trust-path guard, and for the same
reason: the rule is one line to state and one careless `fmt.Printf` to violate.

---

# 6. The gate result, end to end

## 6.1 What a real result carries

| §11.4 field | Source |
|---|---|
| `step` | the step id, `STEP-N`, plus `instance` alongside (DKT-15's established pattern) |
| `gate` | the gate name from the definition, verbatim |
| `argv` | **the resolved argv that was executed** — the matched trust entry's argv for a named gate, the tokenized fence line for a fence gate. NULL when `unmatched` |
| `exit` | the process exit code; NULL when `unmatched` |
| `duration_ms` | wall clock around the spawn |
| `output` | the interleaved capture (§5.5) |
| `truncated` | §5.5 C5 |
| `verdict` | `pass` (exit 0) \| `fail` (non-zero, or timeout) \| **`unmatched`** |

## 6.2 `verdict = "unmatched"` is a first-class outcome

§4, verbatim: *"each must match a trust entry … or it is **not executed** and
reported as unmatched."*

| # | Clause |
|---|---|
| N1 | An unmatched gate **does not spawn**. The process table is never touched. |
| N2 | It records a `gate_results` row with `verdict='unmatched'`, `argv` NULL, `exit` NULL, `output` empty, and `reason` naming why (§3.4's diagnostic: no entry / bound to another repo / prefix not opted in / fence line unparseable). |
| N3 | **An unmatched gate is a gate failure for routing purposes** — the step routes per its `on_fail`. It is not a pass, not a skip, and not an error that aborts the run. A workflow whose check cannot run has not passed its check. |
| N4 | `stub` is **absent** from every result this stage records (T11), including unmatched ones. `stub` means "produced by S3's pass-through", and nothing at S4 is. |

N3 deserves its sentence because the alternative reading — "we couldn't check, so
carry on" — is precisely the failure mode that makes a security control
decorative.

## 6.3 `reason`, and the amendment it carries

§11.4's `gate result` does not list a `reason` field. This stage adds one and
files the deviation (§11 A6). The argument: an `unmatched` verdict has at least
four distinct causes (§3.4), and an operator who cannot tell them apart cannot
act — "no trust entry" needs `trust add`, "bound to another repo" needs
`trust add` *here* or a moved repo restored, "prefix not opted in" needs a
different flag, and "unparseable fence line" needs an issue-body edit. Without
`reason` each of those renders identically. The field is additive, `NULL` on
ordinary pass/fail results, and changes no existing key's meaning.

## 6.4 Event kinds added by this stage

engine-spine §7.6 fixed the S3 event kinds as a **closed set**, which is what
makes §9 item 2 checkable. This stage extends it by exactly six, listed here so
the closed-set guard test is updated in the same commit:

`gate-unmatched`, `gate-rerun`, `trust-added`, `trust-removed`, `vote-opened`, `vote-tallied` — the last two required by name by the §8.1 lifecycle table (reconciled 2026-08-03, DKT-21).

`gate-started` and `gate-recorded` already exist and keep their meanings.
`gate-unmatched` is separate from `gate-recorded` so an operator following the
feed sees a refusal to execute as its own event rather than a result they must
inspect. `gate-rerun` precedes each flaky re-run (§5.6), for the same
at-least-once observability reason `gate-started` exists.

---

# 7. Saga wiring — the real `GateRunner`

Commit group 2.

## 7.1 The S3 seam's promise, and exactly where it holds

engine-spine §5.6 promised: *"The saga is written against the interface, so S4
changes one constructor call and nothing else."* Holding the design to that, and
stating precisely where more moves — the work order requires the honest answer,
not the flattering one.

**The promise holds for the saga.** `internal/engine/saga.go`'s gate stage
(`runGateStage`) calls `e.Gates.Run(...)` with a fully-populated `GateSpec`
(name, source, `pre`, and the harvested `Commands` from `run_fences`), outside
any transaction, between a committed `gate-started` event and a committed result.
Every transaction boundary S4 needs already exists. The saga's diff for this
stage is:

```go
// internal/engine/engine.go, NewEngine
-   Gates: PassThroughRunner{},
+   Gates: NewExecRunner(trustStore, repoRoot),
```

**Four things move beyond that call, and each is a real requirement rather than
drift.** They are listed with their justification because a reviewer should be
able to check each against the spec rather than take the count on faith:

| # | What moves | Why it is not avoidable | Spec line |
|---|---|---|---|
| M-a | **Result persistence changes target**: `appendGateResult` + `SetStepGateTrailTx` become an insert into `gate_results` | v8 creates the table this stage is *for*; DKT-16's closure condition is exactly this migration | reliability-delta §2 (v8 row); DKT-16 |
| M-b | **`gateVerdict` reads `gate_results`** instead of the trail JSON, and learns the `unmatched` verdict | follows M-a mechanically; `unmatched` is a new verdict §4 requires | §4 "reported as unmatched" |
| M-c | **`ClaimStep` gains a pre-gate phase** (§7.6) | pre-gates run *at claim*, and `ClaimStep` is currently **one transaction** — a subprocess cannot run inside it (§6: "No subprocess ever executes inside a transaction"). This requires restructuring claim into pre-transaction / spawn / transaction, and `ClaimStep` is a package-level function with no access to `e.Gates`, so it also becomes a method or takes the runner | §11.1 `gates`: "`pre = true` gates run at claim with results included in the context bundle" |
| M-d | **Vote steps gain a lifecycle** (§8) | vote *execution* was deferred to this stage by engine-spine §1's own scope table; `human.go`'s vote park is replaced | §10 stage 4; engine-spine §1 scope table |

**M-c is the one that breaks the letter of the promise, and it is worth saying
plainly**: S3's seam anticipated the saga's gates, which run in their own stage
with the boundaries already carved, and it did not carve equivalent boundaries in
`claim`. That is not a defect in the S3 design — pre-gate execution is S4's
subject and S3 correctly did not build machinery for it — but "one constructor
call and nothing else" was scoped to the saga, and the pre-gate path is a second
call site. Recording it here is the amendment discipline applied to a *TDD's*
promise rather than to the spec (§11 A7 files it as a note, not a spec change,
since no engine-spec line is deviated from).

**What does not move**, asserted by tests so the claim is checkable: the saga's
stage table, its `saga_stage` values, its CAS-guarded resume, the `gate-started`
event's position before the spawn, the transaction boundaries, and routing's
consumption of the verdict. `TestSagaStageBoundariesUnchanged` compares the stage
sequence produced for the committed fixture against the S3 golden.

## 7.2 Trust matching (T4, T10)

The matcher is `internal/trust`'s, called by the runner. Pure: `(store snapshot,
repo identity, gate name, candidate argv) → (matched entry | unmatched reason)`.

| # | Rule |
|---|---|
| M1 | The store is read **once** into an immutable snapshot at the start of the gate stage, and matching runs against that snapshot. **The matched entry's own `argv` is what executes** — matching yields the argv, not a permission applied to an argv from elsewhere. This is T4's closure: there is no second read to race |
| M2 | Candidate entries are those whose `name` equals the gate name **and** whose binding matches the current repo identity (§3.4 P2). A name that exists only for another repo is `unmatched` with a reason naming that repo |
| M3 | **A full-argv hash satisfies the match** (§4, verbatim). For a **named** gate (no `source`), the candidate argv *is* the entry's argv, so the match is by name+binding and the hash is verified against the stored `argv_sha256` to catch a corrupted or hand-edited file. For a **fence** gate, the candidate argv is the tokenized fence line, and it must hash-equal the entry's `argv_sha256` — **or**, when and only when `entry.prefix` is true, be element-wise prefixed by the entry's argv (§3.3) |
| M4 | **Prefix entries match only when the entry opted in.** A full-argv entry never matches by prefix, in either direction. `TestPrefixMatchingRequiresOptIn` asserts both directions |
| M5 | **No entry ⇒ unmatched.** Never a fallback, never a default-allow, never a "the command looks safe" heuristic. Core has no opinion about which commands are safe; that is the operator's decision and the entire point of the file |
| M6 | Matching is **case-sensitive and byte-exact** on every element. No normalization, no path canonicalization of arguments, no trimming |

M6 is stated because normalization is a match-widening operation, and every
match-widening operation is an authorization decision made by a helper function
nobody reviewed as one.

## 7.3 Fence gates: what is executed and what is verified (T3)

The chain, end to end, showing exactly how S4 relies on S3's snapshot+hash rather
than re-solving it:

1. **At activation (S3, unchanged):** the issue body is snapshotted; fenced blocks
   whose tag a bound workflow declares are harvested **from the snapshot**,
   literally, one `run_fences` row per line, each with its SHA-256
   (engine-spine §5.3 stage 5). The operator sees them (§7.7).
2. **At the gate (S4):** `gateCommands` reads the `run_fences` rows for the gate's
   tag — **it does not read the issue body, the snapshot, or the working tree**.
3. **S4 adds one verification step**: for each row, the stored `sha256` is
   recomputed over the stored `command` and compared. A mismatch means the
   database row was altered after activation; the command is **not executed**,
   records `unmatched`, and `reason` names the tamper. This is cheap and closes
   the gap between "the body cannot inject" (S3's property) and "the row cannot
   be swapped" (a database-write attack, which §2's trust boundary places out of
   scope but which costs one hash to detect anyway).
4. Each row's command is tokenized (§5.2 K2) and matched (§7.2). **Each command
   is matched independently**; a fence block with three lines where two match
   produces three `gate_results` rows, two executed and one `unmatched`, and the
   gate's verdict is `fail` because N3 makes an unmatched command a failure.
5. Matched commands execute in **row order** (`run_fences.ordinal`), which is
   body order, which is what the operator read.

**Step 4's independence is the anti-pattern's opposite**: an implementation that
matched the block as a unit, or that ran the matched lines and silently skipped
the rest, would let an attacker append a line to a body that a prefix entry
happens to cover. Per-line matching with per-line results makes each line its own
decision with its own record.

## 7.4 The tree mutex (T13) — the mechanism, pinned

§4, verbatim: *"Gates that touch the working tree declare `tree = true` and
serialize on an engine-held per-repo mutex — parallel read-step completions never
race a build."*

**It cannot be the database transaction, and the spec's own rules say why**:
gates run **outside** transactions (engine-spec §6: "No subprocess ever executes
inside a transaction"; the saga's gate stage closes its transaction before
spawning — engine-spine §6.8). A transaction held across a subprocess would also
serialize every unrelated engine operation for the duration of a build, and would
be released by a crash *before* the subprocess it was protecting had exited.

**The mechanism: an OS-level advisory file lock** on `.docket/tree.lock` in the
repo, acquired for the duration of a `tree = true` gate's execution.

| # | Clause |
|---|---|
| L1 | The lock is `flock(2)` (`syscall.Flock`, `LOCK_EX`) on a lockfile created `0600` at `<repo>/.docket/tree.lock`. It is **advisory and process-scoped** — held by the docket process running the gate, released when that process exits **by any means, including SIGKILL**, because the kernel releases flocks on fd close. This is the property a database row or a lockfile-with-a-pid cannot match: a crashed engine leaves no stale lock to clear |
| L2 | It is **per repo**, keyed by the repo root — the same identity as §3.4's, so a second checkout of the same project does not serialize against the first |
| L3 | It is acquired **immediately before the spawn** and released **immediately after the process exits**, outside every transaction, and it is never held across a database write |
| L4 | Acquisition **blocks**, with the gate's own timeout as its bound: a gate waiting on the mutex longer than its timeout records `verdict='fail'` with `reason` naming the wait. Blocking rather than failing fast is correct — the whole purpose is to make the second gate wait for the first |
| L5 | An in-process `sync.Mutex` is held **in addition**, because `flock` semantics between two fds in the *same* process are not exclusion; two goroutines in one engine must serialize on the Go mutex, and two engine processes on the flock. Both, or the single-process case silently races |
| L6 | Gates without `tree = true` take **no lock** and run in parallel freely — engine-core §5's "read-only fan-outs parallelize freely (the proven win)" |
| L7 | **The lockfile is opened `O_NOFOLLOW`, and an existing non-regular file is a refusal.** `.docket/tree.lock` sits inside the repository, so it is repo-shippable content: a hostile repo can commit it as a **symlink** (to `~/.ssh/config`, to a device node, to a FIFO), and an ordinary open would follow it. The open is `O_RDWR\|O_CREAT\|O_NOFOLLOW` with mode `0600`; if the path exists and `Lstat` reports anything other than a **regular file**, docket refuses with a `VALIDATION_ERROR` naming the path and what it found — the **identical language and disposition as §3.2's I1**, which established this discipline for the trust file. The `tree = true` gate does not run; it records `verdict='fail'` with the refusal as its `reason`, because the serialization it requires cannot be provided. |

L7's blast radius is small on its own — the worst case is an `flock` taken on an
arbitrary file the operator can open, which is DoS-shaped rather than an
execution or disclosure path, and §2's trust boundary already concedes that a
gate can read what the operator can. It is closed anyway because the cost is one
`Lstat` and one open flag, and because leaving one repo-resident file opened
without the discipline §3.2 established for another repo-adjacent file is exactly
the inconsistency a later refactor generalizes in the wrong direction.
`TestTreeLockRefusesNonRegularFile` covers the table: a symlink, a FIFO, and a
directory at `.docket/tree.lock`, each refused with the path named; a normal
regular file and a missing file both succeed.

L1's crash property is why `flock` and not a lockfile-with-a-pid: the latter
requires stale-lock detection, which requires deciding whether a pid is alive,
which is the "probe-once death evidence" doctrine engine-core §5 says this design
retires.

**`tree` is a trust-entry flag, not a workflow field.** §4 places it on the
entry — the operator declaring a property of their own command — and that is
right: the workflow author may not know whether `make test` writes to the tree,
while the person who trusted it on their own machine does.

## 7.5 At-least-once and the resume decision (§9 item 10, T12)

§2, verbatim:

> Gates are **at-least-once**: a `gate-started` event precedes each spawn; on
> resume, a started-but-unrecorded gate re-runs only if its trust entry is
> flagged re-runnable, else the step parks `waiting-human`.

The mechanism, exactly:

| # | Clause |
|---|---|
| A1 | The saga's gate stage commits `gate-started` **before** the spawn (S3, unchanged). A crash between that commit and the result commit leaves a **started-but-unrecorded** gate — detectable as `saga_stage = "gate:<name>"` with no `gate_results` row for that step+gate at the current ordinal. |
| A2 | On resume, the engine detects that state and consults the **live trust store** for the entry that would match this gate. |
| A3 | `re_runnable = true` ⇒ the gate **re-runs**, recording a new result. A `gate-rerun` event precedes it, so the trail shows both the interrupted attempt's start and the re-run. |
| A4 | `re_runnable = false` (**the default**) ⇒ the step **parks `waiting-human`**, with a reason naming the gate, the crash-resume situation, and the two operator resolutions (`step resolve --as retry` to re-run from the top, `--as override-pass` to accept). It does **not** re-run and does **not** silently pass. |
| A5 | **An unmatched-at-resume gate parks too**, with the unmatched reason — not "re-run because we can't tell". Fail-closed. |
| A6 | The default is `false` because the safe assumption about an unknown command is that it is not idempotent. `trust add --re-runnable` is the operator asserting otherwise about their own command. |

**`re_runnable` × `flaky`, all four combinations** (§5.6 F5's obligation):

| `re_runnable` | `flaky` | Within one stage, on failure | After a crash mid-gate |
|---|---|---|---|
| false | false | runs once; verdict is that run's | parks `waiting-human` |
| false | true | re-runs up to 3 attempts, each recorded | parks `waiting-human` — a flaky command is not thereby a safe-to-re-run one |
| true | false | runs once | re-runs |
| true | true | re-runs up to 3 attempts | re-runs |

Row 2 is the one an implementation gets wrong by treating the flags as one. A
command that is *flaky* (non-deterministic exit) may still be *not re-runnable*
(it deploys); those are orthogonal properties and the table says so.

## 7.6 Pre-gates (`pre = true`) — claim-time execution (T14)

§11.1, verbatim: *"`pre = true` gates run at claim with results included in the
context bundle (measure-then-judge steps); the rest run in order inside
`complete`."*

### AMENDMENT (DKT-254) — a pre-gate binds the tree under review, or measures nothing

§7.6's original design settled WHEN a pre-gate runs and WHERE its result goes,
and left WHAT IT MEASURES to fall out of the claim. It fell out wrong, in two
modes, both ledger-verified across RUN-1..RUN-29 of the current store epoch.

**Mode 1 — a pre-claim gate measured the shared checkout.** `gate_exec.go` set
`dir := r.RepoRoot` and overrode it only when a worktree was known. DKT-9's
worktree-aware cwd covers gates that run after a claim; on the pre-claim path no
worktree exists yet, so every pre-gate fell back. RUN-2's `ac-commands` recorded
PASS with commit 76f5d0c — the shared checkout's HEAD — while the sha under
review was 2b9d9c8. A recorded verdict with zero evidence value, and nothing
said so.

**Mode 2 — the verify step's pre-gate bound an already-swept worktree.** VERIFY
resolves the IMPLEMENT wave's worktree, which integration sweeps before verify
runs in a later wave. Deterministic 2/2 (RUN-22 STEP-380, RUN-27 STEP-467) plus
RUN-29 STEP-746.

**The rule.** A pre-gate binds the tree under review, or it measures nothing.
Three cases, decided once per step because the condition is a property of the
step's inputs rather than of any gate:

| Resolved target | Behavior |
|---|---|
| no sha, no worktree | Nothing is under review yet — the shared checkout **is** the subject. Run. A first implement step asking "is the tree clean before we start" is doing its job |
| worktree exists | Measure it. Checked BEFORE reconstruction even though reconstruction is exact, because a live worktree may hold uncommitted work the sha does not |
| sha reachable, tree gone | **Reconstruct** it — `git worktree add --detach` into a throwaway checkout, measure, release. Sweeping a checkout does not delete the object |
| neither | `skipped`, spawning nothing, with a reason naming the sha or the swept path |

Reconstruction is what makes mode 2 a fixed bug rather than a documented park:
parking every swept-worktree verify is honest and useless. It is NOT a
best-effort fallback — if the sha cannot be checked out the gate skips, exactly
as it would without it. **Measuring a different tree is the defect; measuring no
tree is a gap**, and the two must not be traded for each other.

**The third defect — `skipped` was invisible.** `internal/db/rollups.go`'s
`verdictRollup` counted pass, fail, and unmatched only, so a skipped row landed
in no column and disappeared from the run report's gates section. A verdict
meaning "no evidence was collected" rendering as an absence reads as green. It
is now a column, in both `gate_results` and `action_results` — always zero for
actions today, which is an honest zero and the right shape, since omitting it
for one seam is how two definitions of "what outcomes exist" start to drift.

**The routing consequence is now STATED.** `skipped` still counts as not-pass
(the verdict is unchanged — "we couldn't check, so carry on" is what makes a
control decorative), but a step with an unmeasured gate parks at
`waiting-human` rather than routing per `on_fail`. It is decided before the fail
case, or it would be swallowed by it — which is how a swept worktree came to
route into a 3-seat verify-tribunal that deliberated over an infrastructure
artifact instead of the change. `on_fail` is wrong for the same reason it is
wrong for a gap-only completion: it routes as though a judgment was reached
about the work, and none was. Stated here, once, rather than inherited by each
caller from whatever the execution verdict happened to be — which is what let
RUN-29 STEP-746 record `skipped` and silently pass-route.

PG4 is unchanged and still applies: a PRE-gate that could not bind its tree is
data for the step's worker, not a park. Parking on it would be the engine
judging a step by an input it handed the step itself.

### AMENDMENT (DKT-1166) — a throwaway tree gets throwaway linter caches

DKT-254 gave a pre-gate the right tree. It did not give it a cache that dies
with that tree, and a class of tool needs exactly that.

**The mechanism.** golangci-lint and staticcheck cache each reported issue
keyed by **package content**, storing the **absolute path** the issue was found
at, and re-open that path afterwards to look for the `//nolint` (or
`//lint:ignore`) comment that would suppress it. A reconstruction is deleted
within the minute; the content hash is not. So one reconstruction's entries are
replayed in the next, the suppression lookup re-opens a file that is gone, and
an already-suppressed issue is re-emitted as live.

**Observed**: harness RUN-64/STEP-2939 recorded `ac-commands: fail, exit 2` over
a clean tree. Build and tests exited 0; `make lint` reported one forbidigo issue
at `../docket-pregate-4091742512/…/timelinecompare_test.go` — a directory an
earlier reconstruction had already removed — with golangci-lint warning it could
not read that file, while the source carried `//nolint:forbidigo` on the line
above and the same sha linted in place reported `0 issues.`

**The cwd was never the problem.** DKT-254 already binds the reconstruction and
`gate_exec.go` already spawns in it, which is why the reported path was
*relative to* the current reconstruction. The carrier is the cache, and it
poisons in both directions: the operator's own persistent cache also receives
entries naming a `docket-pregate-*` path docket is about to delete.

**The rule.** A gate that measures a tree docket will delete gets its
path-carrying result caches inside a scratch root docket deletes with that tree
(`GOLANGCI_LINT_CACHE`, `STATICCHECK_CACHE` — §5.3). A gate over a tree that
stays on disk keeps its shared caches: re-analysis is a real cost, and it is
only worth paying where the tree is genuinely throwaway. The Go **build** cache
is deliberately untouched — what makes an entry dangerous here is a stored
source path the tool re-opens to decide suppression, and relocating `GOCACHE`
would rebuild the standard library on every reconstruction for no such benefit.

A cache root that cannot be created **fails the reconstruction**, which records
`skipped`, on the same reasoning as the rest of this section: a measurement that
can report a suppressed issue as live is measuring the wrong thing, and that is
the defect, while measuring nothing is a gap.

### 7.6.1 Ordering and the claim restructure

`ClaimStep` today is **one transaction** (internal/engine/claim.go:73–202): reap,
readiness, CAS claim, status, event, context assembly, input materialization,
commit. A subprocess cannot run inside it. The restructure, in order:

| Phase | Contents | Transaction |
|---|---|---|
| 1 | reap check, readiness (R8), CAS claim, `started_ms`, status → `claimed`, `step-claimed` event | **transaction A**, committed |
| 2 | **pre-gates run, in declared order, one at a time**, each with its own `gate-started`/result commit exactly as the saga's do | **outside any transaction**; each result commits in its own small transaction |
| 3 | context assembly (now including the pre-gate results), input materialization, row-version read, **and the lease refresh (§7.6.1.1)** | **transaction B**, committed |

**The claim's atomicity guarantee is preserved where it matters and weakened
where it does not**, and the distinction is stated because "claim is atomic" is a
ratified property (engine-core §5):

- **The mutual-exclusion guarantee is unchanged**: exactly one claimant wins,
  and it wins in transaction A. Losers still get `CONFLICT` from the same CAS.
- **The "token and context in one response" guarantee is unchanged**: the caller
  still receives both in one response; §8 of engine-core's "one atomic mediation
  — an unclaimed executor has nothing, a claimed one has everything" is about
  what the *caller* observes, and the caller observes exactly that.
- **What changes**: a crash between phase 1 and phase 3 leaves a claimed step
  whose caller never got a response. That is **already** the pre-existing
  behavior for a crash between commit and the response reaching the caller, and
  it is handled by the same mechanism: the lease expires and the step is
  re-offered with `attempt++` (§9 item 4). Pre-gate results committed by the
  abandoned attempt remain as recorded facts; the re-claim runs the pre-gates
  again at a new ordinal. This is at-least-once, applied to pre-gates, and it is
  the same discipline §7.5 applies in the saga.

`TestClaimRemainsSingleWinnerAcrossThePreGatePhase` is the N-goroutine race
against a step with pre-gates: exactly one winner, `attempt` incremented once.

#### 7.6.1.1 Transaction B refreshes `expires_ms` — pre-gate time is not the worker's

Transaction A sets `expires_ms` when it wins the CAS. Phase 2 then runs
**subprocesses** — each bounded by the gate's own timeout, which defaults to
**5m** (§5.4 X5) and can be a per-entry override, and a step may declare several
pre-gates that run one at a time. The wall time between the CAS and the response
is therefore unbounded in principle and minutes in practice, and **all of it is
deducted from a lease the caller has not yet received**. A pre-gate-heavy step
hands its worker a mostly-spent lease; with a short configured TTL it can hand
over an **already-expired** one, so the worker's first `step complete` fails on a
lease it never had a chance to use, the step is reaped, and the pre-gates run
again on the re-offer — a livelock shaped exactly like a too-short TTL, but
caused by docket's own pre-gate phase rather than by the operator's setting.

The fix is one clause:

| # | Clause |
|---|---|
| LR1 | **Transaction B recomputes `expires_ms` from its own commit time** — `now + ttl(class)`, the same computation transaction A performs, using the same lease-TTL resolution (`lease.ttl.<class>`, engine-spine §6). The caller receives a **full** lease regardless of how long phase 2 took. |
| LR2 | **It is guarded by the same claim identity**, not an unconditional `UPDATE`: the refresh is conditional on the row still holding the claim token transaction A wrote. The claimant still holds the CAS, so this is **a heartbeat by another name** — the mechanism the lease model already sanctions for a live claimant — and not a second authorization decision. If the claim is gone (the lease already expired and another attempt won during phase 2), the refresh matches zero rows and the claim fails in the ordinary way: the caller gets `CONFLICT`, the abandoned pre-gate results remain recorded facts, and the winner runs its own. |
| LR3 | **No new event.** The refresh is part of the claim, and the claim already emits `step-claimed` in transaction A. Emitting a heartbeat event here would put a second lifecycle event on a single claim and break the event-sequence goldens for no observational gain. |
| LR4 | **The single-winner property is untouched.** Exclusion is decided in transaction A and only there (§7.6.1's first bullet); LR2's guard can only *fail*, never award a claim. `TestClaimRemainsSingleWinnerAcrossThePreGatePhase` continues to cover this. |

**`TestPreGateWallTimeDoesNotConsumeTheLease`**: a step with a pre-gate wired to a
deliberately slow fake command, run under a **short** configured lease TTL such
that the pre-gate's duration exceeds it; assert the claim response's
`lease.expires_ms` is a **full** TTL ahead of the response time (not of the CAS
time), and that the worker's subsequent `step complete` succeeds. Without LR1 the
assertion fails on both halves, which is the point of choosing a TTL shorter than
the pre-gate.

This is a **refinement of the phase split, not a new guarantee**: it restores the
property the single-transaction claim had for free — that a caller's lease starts
when the caller gets it.

### 7.6.2 Trust requirements — the same model, no exceptions

| # | Clause |
|---|---|
| PG1 | A pre-gate resolves through the **identical** matcher (§7.2), the identical env allowlist (§5.3), the identical timeout and process-group kill (§5.4), the identical capture (§5.5), and the identical tree mutex when `tree = true` (§7.4). There is no pre-gate-specific path anywhere in `internal/exec` or `internal/trust`. |
| PG2 | An **unmatched pre-gate does not execute**, records `verdict='unmatched'` with `pre = 1`, and **the claim still succeeds** — the result rides in the bundle as `unmatched`, and the step's worker sees that its measurement did not run. A pre-gate is a measurement whose *result* the step consumes; refusing the claim would make an untrusted command able to block work, which is a denial-of-service an issue author should not have. |
| PG3 | A **failing** pre-gate (non-zero exit) likewise does not refuse the claim: the result is `fail` and rides in the bundle. §11.1 calls these "measure-then-judge steps" — the judging is the step's job, which is exactly why the failure is data rather than a refusal. |
| PG4 | Pre-gate results are **excluded from the saga's gate verdict** (`gateVerdict` filters `pre = 1`). They are inputs to the step, not judgments of it. S3 already excludes `pre` gates from `completionGates` (saga.go:311–320); this is the read-side counterpart. |

PG2 and PG3 together mean a pre-gate never blocks — a deliberate asymmetry with
completion gates, and the reason is in §11.1's own parenthetical.

### 7.6.3 Where the results land in the bundle

§11.4's `context` gains one member:

```
context   { step, issue, inputs, pins, loop_entry, metadata,
            pre_gates: [<gate result>…] }        # NEW — §11 A5
```

`pre_gates` is a JSON array of §11.4-shaped gate results, in declared gate order,
present **only when the step declares pre-gates** (absent, not empty, otherwise —
the same rule the v6 `lease` object established). This is an additive wire change
and is filed as an amendment (§11 A5): §11.4's `context` line does not name a
member for them, while §11.1 requires the results be "included in the context
bundle". The amendment proposes the field name explicitly.

**The determinism consequence, faced directly.** engine-spine §8.3 golden-diffs
context bundles for byte-identity (§9 item 5). A pre-gate result contains a
duration and a command's output — neither reproducible. The rule:

| # | Clause |
|---|---|
| GD1 | The §9-item-5 goldens cover steps **without** pre-gates, byte-for-byte, unchanged. |
| GD2 | For a step **with** pre-gates, the golden covers the bundle with `pre_gates` **elided**, plus a separate structural assertion on `pre_gates` (gate names in declared order, verdicts, `argv`) that excludes `duration_ms` and `output`. |
| GD3 | This is **not a weakening of §9 item 5**, and the argument is the spec's own: item 5 requires "identical step topology and byte-identical context bundles, immune to **mid-run issue edits and working-tree changes**." A pre-gate exists precisely to **measure the working tree** at claim time (§11.1's "measure-then-judge"). Requiring its output to be immune to working-tree changes would require it to not measure anything. Determinism applies to what the engine assembles from pinned state; a pre-gate result is not assembled, it is observed, and it is stamped with the claim that observed it. |

GD3 is the reasoning a reviewer should check hardest, so it is stated as an
argument rather than asserted. The immunity property survives intact for
everything §9 item 5 was written about: the issue snapshot, the inputs, the pins,
the topology.

## 7.7 Activation surfaces every harvested command and its trust status (T16)

§2, verbatim: *"At plan approval the session surfaces what activation will bind —
including every harvested fenced command, verbatim — so what the human approves
is what was actually read, not a summary."*

S3 harvests and stores. This stage adds the **surfacing with trust status**,
because a verbatim list an operator cannot act on is only half the mechanism:

| # | Clause |
|---|---|
| S1 | `docket run activate` prints, after the fat transaction commits, every harvested fence command **verbatim**, grouped by issue and gate, each annotated `matched` (naming the trust entry) or `unmatched` (with §6.3's reason). **"Verbatim" means every byte is accounted for, not that raw bytes are written to the terminal**: the command and the reason render through §5.7's escaping renderer (T18), which is lossless — a command containing no control bytes renders identically to its stored form, and one that does renders its escapes visibly rather than executing them against the operator's cursor. |
| S2 | Under `--json`, the same data rides in the response as a `fences` array of `{issue, gate, tag, ordinal, command, matched, entry, reason}`. `command` carries the **raw stored bytes** — §5.7 E4: JSON escaping is `encoding/json`'s job and the consumer is a program. |
| S3 | **It is a report, not a gate**: activation succeeds with unmatched commands. They simply will not run, and their gates will route per `on_fail` when reached. Refusing activation would make an issue author able to block a run by adding an untrusted fence line — the same denial-of-service PG2 avoids. |
| S4 | `docket run activate --dry-run` prints the same report and **writes nothing**, so an operator can see what a run would bind and invoke before committing to it. This is the "at plan approval the session surfaces" half of §2, made available as a verb rather than left to the harness. |

---

# 8. Vote-step execution (deferred from S3)

engine-spine §1's scope table deferred vote execution here: *"votes are 'driven
as gates' (§2), so their execution lands with the gate machinery at §10 stage
4."* engine-core §6, verbatim:

> **Votes.** Docket's existing proposal/vote machinery is kept and demoted to a
> gate type: a `type="vote"` step creates a proposal, fans out the configured
> voters (engine-spec §11.1) to cast via CLI, and the engine computes the outcome
> from threshold config (it already does: weighted score, required voters).

**The whole of this section is wiring.** No vote semantics change. `CastVote`,
its weighted-score computation, its quorum rule, and its
approved/rejected outcome (internal/db/proposals.go) are **used unchanged** —
not copied, not re-implemented, not parameterized. That is the requirement and
§9.2 asserts it by proving the vote path reaches the same compiled function the
`docket vote cast` CLI does.

## 8.1 The step lifecycle around the existing machinery

| Phase | Trigger | Effect |
|---|---|---|
| 1 — **ready** | the ordinary §6.3 readiness predicate (R1/R2/R3/R6; R4/R5 do not apply — a vote step claims no scope and consumes no class headroom, exactly as a human step does) | the step is offered by `next --run` with `kind: "vote"`, and remains **unclaimable** (`CONFLICT` naming the class — engine-spine §6.15, unchanged) |
| 2 — **proposal open** | the first engine invocation that observes the step ready and without a proposal | creates a proposal via `db.CreateProposalIdempotent`, links it to the step's issue via `db.LinkProposalIssue`, records the proposal id on the step, writes a `vote-opened` event |
| 3 — **voters cast** | **the existing CLI**, unchanged: `docket vote cast DKT-V<n> -v approve --confidence … --domain-relevance …` | `db.CastVote` tallies exactly as it does today; quorum and weighted score are its computation, untouched |
| 4 — **tallied** | the first engine invocation after the proposal leaves `open` | reads the proposal's status; `approved` ⇒ the step's gate verdict is `pass`; `rejected` ⇒ `fail`; writes a `vote-tallied` event with the score |
| 5 — **routing** | the ordinary routing transaction | `pass` ⇒ the step is `done` and successors become ready; `fail` ⇒ routed per the step's **effective `on_fail`** — identically to a human gate's reject (engine-spine §4.3.2) |

**Phase 2's trigger is lazy and idempotent**, following the saga's own discipline:
the proposal is created by whichever engine invocation gets there first, under a
CAS guard on the step row, and `CreateProposalIdempotent` with a key derived from
`(run, step instance)` makes a double-invocation produce one proposal. No daemon
watches for ready vote steps; nothing is scheduled.

**Phase 3 requires no new verb.** Voters cast with the CLI Docket has shipped
since v2. That is the whole point of "kept and demoted to a gate type", and it is
also the stranger test holding: the vote feature was always usable by a
human-only team, and a workflow step now opens one for them.

## 8.2 `voters` and `vote_rule`, resolved

| Field | §11.1 says | Resolution at this stage |
|---|---|---|
| `voters` | "[executor hints]" | **opaque strings**, used for exactly one thing: `required_voters` = `len(voters)`, the count the existing quorum rule consumes. Core never interprets a voter hint, never validates it against anything, and never dispatches to one — a dispatcher reads the hints off the `next` row and arranges casting, exactly as it does for executor hints |
| `vote_rule` | "proposal-config name … which existing Docket threshold config tallies" | names a **threshold configuration**: the `criticality` + `threshold` pair the existing `vote create` surface takes. §8.3 pins where those live |

**`vote_rule` must name an existing threshold config, and a missing one is
caught at register time.** V14 (engine-spine §4.3, unchanged) already requires
`voters` and `vote_rule` on `type="vote"` steps; this stage adds **V26**: a
`vote_rule` naming an unregistered threshold config is a `VALIDATION_ERROR` at
`workflow register`, naming the rule and listing the registered ones. Catching it
at register rather than at run time is the same discipline every other V-rule
follows — a workflow that cannot possibly run should not register.

## 8.3 Where threshold configs live

The existing `vote create` takes `--criticality` (low|medium|high|critical) and
`--threshold` (a float in (0,1]) per invocation. A workflow step cannot pass
flags, so `vote_rule` needs a named, stored configuration.

**Threshold configs are `docket config` keys**, in the existing engine-config
registry (SKILL.md's engine-configuration table), not a new table:

| Key | Type | Meaning |
|---|---|---|
| `vote.rule.<name>.threshold` | float in (0,1] | the approval threshold this rule tallies at |
| `vote.rule.<name>.criticality` | `low\|medium\|high\|critical` | the proposal's criticality |

`<name>` is an opaque string, exactly as `lease.ttl.<class>`'s class is. A rule
"exists" iff `vote.rule.<name>.threshold` is set. This reuses the config
machinery, its `VALIDATION_ERROR`-on-unknown-key behavior, and its
`set`/`default` source reporting, and it adds no schema.

**`required_voters` comes from `len(voters)`, not from the config**, because
§11.1 puts the voter list on the step. A rule is about *how strictly to tally*;
the step is about *who casts*.

## 8.4 What is NOT in scope here

- **No new vote verb.** Casting, showing, listing, and committing use the
  existing surface unchanged.
- **No change to the tally.** Weighted score, quorum, and the approved/rejected
  decision are `db.CastVote`'s, byte-for-byte.
- **No auto-casting.** Nothing in core casts a vote; a voter is a person or a
  process running the CLI.
- **`vote commit`'s relationship to the step**: an operator committing a proposal
  manually (`docket vote commit`) sets its final outcome; the step's phase-4 read
  observes the resulting status like any other. No special case.

---

# 9. Test plan, per commit group

**§9.5's sandbox rule is binding on everything in this section** and is stated
first because a violation of it is a test that can damage an operator's machine.

## 9.1 Group 1 — `internal/trust` + `internal/exec` (pure, fully unit-tested)

No database, no engine, no CLI. Every test in this group runs against a temp
directory and a temp `XDG_CONFIG_HOME`.

**`internal/trust` (`store_test.go`, `match_test.go`)** — table-driven in
`internal/planner/plan_test.go`'s style, one case per documented rule:

- **The integrity table I1–I5** (§3.2): symlink, `0666`, `0640`, wrong owner,
  world-writable parent — each refused, each error naming the path and the mode.
  Plus: a **missing** file is an empty allowlist, not an error.
- **Store path resolution** (§3.1): `XDG_CONFIG_HOME` honored; unset falls back
  to `$HOME/.config`; and `TestNoTrustPathSurface` walks the Cobra tree and the
  config-key registry asserting **no flag and no config key names a trust-file
  path** (T8) — the same shape as `TestNoTokenFlag` (security.md §1.2).
- **Parse strictness**: unknown key errors; unknown `version` errors; an entry
  with neither `repo` nor `global` errors (P4) rather than matching everything.
- **The canonical-argv hash** (§3.3): `["a b"]` and `["a","b"]` produce
  **different** hashes — the collision test, which is the one that matters.
- **The matching table M1–M6** (§7.2): exact match; name matched but repo not;
  global entry matching any repo; prefix opted in; **prefix NOT opted in
  (`TestPrefixMatchingRequiresOptIn`, both directions)**; element-wise vs.
  string-prefix (`["make"]` does not match `["make-release"]`); case sensitivity;
  no entry ⇒ unmatched with a reason.
- **Repo identity P1–P4** (§3.4): symlinked repo root resolves to its real path;
  two checkouts of one project are distinct identities; a moved repo's entries no
  longer match **and the unmatched reason names the old path**.
- **`TestMatchReturnsTheExecutedArgv`** (T4): the match result carries the argv,
  and the matcher has no API that returns a bare boolean — the type makes the
  TOCTOU shape unrepresentable.
- **Add/rm semantics** (§3.5): idempotent identical add; `CONFLICT` on differing
  argv at the same name+repo, naming both; the `--prefix` warning asserted by
  substring; **the warning is emitted under `--yes` too**.
- **`TestConcurrentTrustAddsBothLand`** (§3.5.1, F5): N goroutines and — the case
  that matters — N **subprocesses** each adding a distinct name to one sandbox
  store; every entry present afterwards, the file parses. An in-process-mutex-only
  implementation fails the subprocess case. Plus W5's timeout: a held lock makes a
  second `trust add` exit `CONFLICT` naming the lock path, and W3's integrity
  table on the lockfile itself (symlink, non-regular, bad mode).

**`internal/exec` (`run_test.go`, `env_test.go`, `split_test.go`)**:

- **`TestNoShellEverInvoked`** (T2): an argv table over `;`, `&&`, `||`, `|`,
  `$(…)`, backticks, `>`, `<`, `*`, `?`, `~`, `\n`, `\0`-adjacent cases — each
  asserting the child received exactly one argument containing the literal text,
  via a helper binary that prints its argv. Plus the **source guard**: the
  package references no shell binary.
- **`TestFenceSplitterExpandsNothing`** (§5.2 K2): every metacharacter class
  survives as a literal; quoted whitespace stays one token; an unterminated quote
  refuses rather than guessing.
- **`TestArgv0IsNotResolvedAgainstTheWorkingDirectory`** (T15): a `./make`
  planted in `Dir` is not executed; `exec.ErrDot` is a refusal.
- **`TestArgv0NeverResolvesIntoTheRepoByName`** (T17, §5.2.1): the table the fix
  is worth, since each row is a distinct way in —
  (a) `PATH` contains `<repo>/bin` and the entry names `make` **by name** ⇒
  refused, `verdict='unmatched'`, the reason naming the **resolved absolute
  path** and the rule, and the witness binary's sentinel **absent**;
  (b) the same binary trusted by its **absolute path** ⇒ runs (R4), so the escape
  hatch is proven open and repo-owned scripts stay usable;
  (c) a `PATH` directory **outside** the repo holding `make → <repo>/bin/make`
  ⇒ refused (R1 — symlink resolution, the one-indirection variant);
  (d) a repo root that is itself reached through a symlink ⇒ still refused (R2,
  the same normalization on both sides);
  (e) `/Users/x/src/docket-evil/bin/make` with repo root `/Users/x/src/docket`
  ⇒ **runs** — the containment test is path-component-wise, and a string-prefix
  implementation fails exactly this row (R3);
  (f) an ordinary `/usr/bin/make` ⇒ runs, so the check is not refusing everything.
  Pre-gates and fence gates are driven through the same table (R5).
- **`TestGateChildEnvIsExactlyTheAllowlist`** (T5): **set equality** against
  §5.3's table, from a parent environment seeded with fifty extra variables
  including `AWS_SECRET_ACCESS_KEY`, `GITHUB_TOKEN`, and `DOCKET_PATH`.
- **`TestDocketTokenNeverReachesAGateChild`** (T5): `DOCKET_TOKEN` set in the
  parent to a known value; the child's environment is dumped and grepped; also
  asserts the pre-spawn guard fires if the constructed set is tampered with.
- **`TestTimeoutKillsTheWholeProcessGroup`** (T7): a child spawning a long-lived
  grandchild; the grandchild's pid is gone after the timeout. Asserted by pid, so
  an implementation that kills only the direct child fails.
- **`TestTimeoutDoesNotHangOnInheritedPipes`** (X3): a killed child whose
  grandchild holds the write end open; the runner returns within the deadline.
- **`TestCaptureIsCappedAndFlagged`** (T6): a generator emitting far past the cap;
  recorded output exactly cap-sized (backed to a rune boundary), `truncated`
  true, and the process not permitted to spin the reader.
- **`TestCaptureInterleavesStdoutAndStderrInWriteOrder`** (C1).
- **`TestOperatorFacingRenderingEscapesControlBytes`** (T18, §5.7): a value
  carrying `\x1b[2K\r`, a bare `\n`, a bare `\r`, `\x07`, and `\x1b]0;t\x07`
  driven through the renderer for all three classes — an argv, a fence command,
  and a `reason`; assert the escapes appear as **visible text** and that **no raw
  `\x1b`, `\r`, or other C0/C1 byte reaches the writer** (the assertion is on the
  written bytes, not on the string's appearance). Plus E4: the same value under
  `--json` round-trips to the **original** bytes, so machine consumers are
  unaffected; and E5: the stored bytes are unchanged, asserted by re-verifying
  §7.3's hash after a render. The **surface guard** — no human-mode print of these
  three classes bypasses the renderer — is a source-level check in the family of
  §5.1's no-shell guard.
- **Flaky re-runs F1–F5** (§5.6): a non-flaky failing gate runs **once**; a flaky
  one runs up to three times; each attempt is its own result with ascending
  `ordinal`; the verdict is the last attempt's; a flaky gate passing on attempt 2
  stops.

## 9.2 Group 2 — the acceptance criteria, proven

**§9 item 6 — the trust proof** (`internal/engine/gate_test.go` +
QA `test_zh_stranger.sh`'s hostile half):

> 6. Trust: a cloned malicious repo cannot cause execution without a prior local
>    `trust add` (proof includes fenced-command harvesting); unmatched commands
>    are reported, never run.

`TestMaliciousCloneExecutesNothing`, as a script:

1. Build a repo with a workflow whose gate is a **named** trusted gate, and an
   issue whose body carries a **fenced block** with a second command. Both
   commands are **witness commands**: each writes a sentinel file if it runs.
2. **No trust entries exist** (empty sandbox `XDG_CONFIG_HOME`).
3. Activate; assert the fences harvested (S3's behavior, still true) and the
   §7.7 report shows both as `unmatched`.
4. Run the full cycle to the gate stage.
5. Assert: **neither sentinel file exists** — nothing executed. Both gates
   recorded `verdict='unmatched'`, `argv` NULL, `exit` NULL, with reasons. The
   step routed per `on_fail`. `gate-unmatched` events written.
6. Now `trust add` **the named gate only**, in this repo. Re-run.
7. Assert: the named gate's sentinel exists; **the fenced command's does not**,
   and it is still `unmatched`. Trust is per-command, not per-repo-blanket.
8. **The clone half**: copy the repo to a second path, `trust add` in the
   *original* only, run in the *copy*. Assert nothing executes — §3.4's argument,
   mechanized.

Plus, as separate cases: `TestPostActivationBodyEditCannotInject` (T3),
`TestRunFenceHashTamperIsRefused` (T3 step 3),
`TestTrustFileRewriteMidGateDoesNotChangeWhatRuns` (T4),
`TestActivationReportsFenceTrustStatus` (T16),
`TestActivationReportEscapesFenceControlBytes` (T18 end to end — an issue body
whose fenced block carries `\x1b[2K\r`; the §7.7 report and `trust add`'s
disclosure both render it escaped, while the `--json` `fences` array carries the
raw bytes and the stored `run_fences.command` still hashes correctly),
`TestPreGatesUseTheSameTrustPath` (T14),
`TestReadVerbsNeverExecute` (§2.2 — a call-graph assertion that no read verb
reaches `internal/exec`).

**§9 item 10 — completed** (`internal/engine/saga_resume_test.go`):

> 10. Saga safety: crash at every stage boundary of `complete` — resume never
>     double-runs a non-re-runnable gate (parks `waiting-human` instead) …

engine-spine §6.16 proved the crash-at-every-boundary half with stubbed gates.
This stage completes it with real ones:

- **`TestCrashAtGateBoundaryNeverDoubleRunsNonRerunnable`**: for **each** saga
  boundary, with a witness gate that appends to a counter file, kill after the
  commit and resume. With `re_runnable = false`: the counter is **exactly 1** and
  the step is `waiting-human` with a reason naming the gate. With
  `re_runnable = true`: the counter is 2 and the step advanced, with a
  `gate-rerun` event.
- **`TestUnmatchedAtResumeParks`** (A5).
- **`TestSagaStageBoundariesUnchanged`** (§7.1): the stage sequence for the
  committed fixture matches the S3 golden.
- The `re_runnable` × `flaky` four-way table (§7.5), one case per row.

**T13 — the tree mutex**: `TestTreeGatesSerialize` — two concurrent `tree = true`
gates, each recording entry/exit timestamps into a file; asserted non-overlapping.
`TestTreeLockReleasedOnProcessDeath` — a subprocess holding the lock is `kill -9`'d
and a second acquisition succeeds immediately (L1's flock property, which a
pid-file scheme would fail). `TestNonTreeGatesRunConcurrently` (L6) — the
converse, so the mutex is not silently serializing everything.
`TestTreeLockRefusesNonRegularFile` (L7, F4) — `.docket/tree.lock` planted as a
symlink (pointing outside the repo, so a follow is observable), as a FIFO, and as
a directory; each refused with `VALIDATION_ERROR` naming the path and what it
found, the gate recording `verdict='fail'` with that reason and **not** spawning;
a regular file and a missing file both succeed.

**Migration (`internal/db/migrate_v8_test.go`)**: the U1–U7 table of §4.4, one
case each, plus `TestRewindGuardProbesEverySentinel` extended to v8 and
`TestGateTrailIsNotWrittenAtV8` (G3).

**Vote wiring (`internal/engine/vote_test.go`)**: the §8.1 lifecycle
phase-by-phase; `TestVoteTallyReachesTheExistingFunction` (§8's "unchanged"
requirement, asserted by identity as engine-spine §6.16 did for lease helpers);
proposal creation idempotent under concurrent engine invocations; V26 at register;
`approved` ⇒ pass, `rejected` ⇒ routed per effective `on_fail`, never
`waiting-human`.

**Pre-gates (`internal/engine/pregate_test.go`)**: PG1–PG4;
`TestClaimRemainsSingleWinnerAcrossThePreGatePhase` (§7.6.1); GD1–GD3's golden
split (§7.6.3); an unmatched pre-gate leaves the claim successful with the result
in the bundle; **`TestPreGateWallTimeDoesNotConsumeTheLease`** (§7.6.1.1, F3) — a
slow fake pre-gate under a lease TTL **shorter** than the pre-gate's duration;
the claim response's `lease.expires_ms` is a full TTL ahead of the **response**
time and the worker's `step complete` succeeds, both of which fail without LR1.
Plus LR2's negative: a claim whose lease is lost during phase 2 gets `CONFLICT`
from the guarded refresh, the winner's own pre-gates run, and the abandoned
attempt's pre-gate results remain recorded.

**CLI (`internal/cli/trust_test.go`)**: each verb's exit code and `.code`;
`trust list` as a `Collection` under `--json=v2`; the `--` argv boundary
(`docket trust add x -- foo --prefix` trusts `["foo","--prefix"]`).

## 9.3 Dormancy proof, per commit group

engine-spec §3 ("Dormant unless workflows are used") and §9 item 8. The claim for
this stage, precisely: **a repo with no trusted commands and no workflows behaves
byte-identically to v7 on every pre-existing verb, at every `--json` version, in
human mode, and in exit code.**

| Group | Proof |
|---|---|
| 1 | **Trivial and complete**: the group adds two new packages and wires nothing. No DDL, no migration, no verb, no change to any existing call site. The proof is the byte-compat sweep (§8.5 of engine-spine, re-run) plus a diff review showing zero edits outside `internal/trust/` and `internal/exec/` |
| 2 | v7→v8 migration applied; `gate_results` and `trust_cache` empty; **no trust file exists** (the sandbox `XDG_CONFIG_HOME` is empty); the full byte-compat sweep passes; `docket next`, `docket issue *`, `docket vote *`, `docket plan` output byte-identical to the pre-group baseline. Additionally: **a run of a gate-free workflow spawns no process**, asserted by a runner that counts spawns |

The "no trusted commands" half is what makes this stage's dormancy claim distinct
from S3's: the mechanism is inert **by default** because the allowlist is empty
by default. A stranger who installs docket and never runs `trust add` has a tool
that cannot execute anything, and §9.2's step 2 is exactly that state.

## 9.4 QA sections

`SECTIONS` gains **`ZH:test_zh_stranger`** after `ZG:test_zg_workflow`, and
`ZG` is extended.

**`test_zh_stranger.sh` — §9 item 1 AS A QA SCRIPT**, which the work order
requires. §9 item 1, verbatim:

> 1. **Stranger test:** a human-only demo — a docs-review workflow with two
>    steps, one fenced-command gate, no agents anywhere — is definable,
>    runnable, and comprehensible from public docs alone, with zero references to
>    AI concepts.

The script, start to finish, using only what a stranger has:

1. `docket workflow init --template standard-dev` — the shipped two-step
   check-then-approve workflow (S3's, unchanged), whose `check` step carries
   `gates = [{ name = "checks", source = "fence:checks" }]`.
2. `docket workflow register .docket/config/workflows/standard-dev.toml`.
3. `docket issue create` with a body containing a ```` ```checks ```` fenced block
   holding a **real, ordinary command** a documentation team would write — a
   spell-check-shaped `grep` over the docs it changes. No agents, no models,
   nothing AI-shaped anywhere in the fixture.
4. `docket run start` → `docket run activate` — assert the §7.7 report prints the
   fenced command **verbatim** and marks it `unmatched`.
5. `docket trust add checks -- <the argv>` — assert the argv and repo binding are
   printed, and that this is the **only** approval step in the whole script.
6. `docket next --run` → `docket step claim` → `docket step complete` — assert the
   gate ran (`verdict='pass'`, real `argv`, real `exit`, `stub` **absent** from
   the JSON), and the sentinel proving execution exists.
7. `docket step approve` on the `approve` human gate.
8. **Assert the run reaches `done`.**
9. **The comprehensibility half, mechanized**: assert the script's own commands
   and every string the run printed contain **none** of `scripts/qa/genericity.sh`'s
   `BANNED` list, sourced **from the gate script itself** rather than restated —
   restating the list in a test is how the gate erodes, and the gate scans
   `internal/**` including tests.
10. **The negative half**: re-run steps 3–6 in a **copy** of the repo at a
    different path with no `trust add` there, and assert the gate is `unmatched`
    and nothing executed — §9 item 6 folded into the stranger demo, since it is
    the same script with one step removed.

**`test_zg_workflow.sh` extensions**: the S3 loop run's gates are now **real**;
the comment saying "gates are stubbed" is removed in the same commit (a stale
comment asserting the opposite of the truth is worse than none). Assert real
`argv`/`exit` on the fixture's gate results and `stub` absent. Add the §6.9-style
refusal matrix for `trust add|rm` by exit code **and** `.code`, each followed by
a version-unchanged assertion.

## 9.5 THE SANDBOX RULE — no test may touch the operator's trust store

**Binding on every Go test, every QA section, and every helper in this stage.**

| # | Clause |
|---|---|
| SB1 | Every test that constructs, reads, or writes a trust store sets `XDG_CONFIG_HOME` to a `t.TempDir()` (Go) or a `mktemp -d` (QA), via `t.Setenv` / an exported assignment scoped to the section. |
| SB2 | `scripts/qa/helpers.sh`'s `setup()` exports `XDG_CONFIG_HOME="$QA_DIR/xdg"` and creates it, so **every** QA section inherits the sandbox whether or not it touches trust — a section added later cannot forget. `cleanup()` removes it with `$QA_DIR`. |
| SB3 | `internal/trust` exposes **no** way to open a store at an arbitrary path from outside the package: the store path is computed from the environment, and the path-taking constructor is unexported and used only by the package's own tests. This is why there is no `--trust-file` flag (§3.1) — the same decision serves T8 and SB3. |
| SB4 | **A guard test enforces the rule mechanically**: `TestNoTestReadsTheRealTrustStore` runs the trust package's tests with `HOME` and `XDG_CONFIG_HOME` pointed at a directory containing a **poisoned** trust file, and asserts no test's behavior changes — a test that read it would match an entry it should not see. |
| SB5 | The QA suite additionally asserts, before running section ZH, that `XDG_CONFIG_HOME` is set and is under `$QA_DIR` — **a hard abort, not a skip**, if it is not. A QA run that silently fell back to the real store would write entries into the operator's config. |

SB5 is an abort rather than a skip because the failure mode is not a missing test
result — it is a QA run that modifies the machine it is auditing.

---

# 10. Deliberate non-goals

Recorded so a reviewer can see they were considered, and so a later amendment has
a starting point rather than a blank page:

| Non-goal | Why not now | What a future amendment would look like |
|---|---|---|
| **Sandboxing gate execution** (seccomp, namespaces, a container) | v1 runs on the operator's machine as the operator, per §2's trust boundary. A half-sandbox is worse than none because it invites trust in a boundary that leaks | a per-entry `sandbox = "…"` field, with the profile as opaque config |
| **Extending the env allowlist** (§5.3) | shipping the escape hatch before there is a requirement is shipping the thing that undoes T5 | a trust-entry `env_passthrough = ["FOO"]`, per entry, never global, with the values named in `trust list` |
| **Timeout and capture cap as config keys** | both are security bounds; a config key makes them adjustable by anything that can write the database. Per-entry `timeout` already covers the legitimate case | `gate.timeout.default` / `gate.output.cap_bytes`, if a real need appears |
| **Network policy for gates** | docket has no network (§7) but a gate may; controlling that requires sandboxing, above | with the sandbox |
| **Trust entry expiry / rotation** | no evidence of need; adds a clock to an authorization decision | `expires_at_ms` on an entry, with `trust list` showing it |

---

# 11. Amendment issues filed by this stage

Per docs/design/amendments.md and CLAUDE.md: deviations become DKT issues citing
the exact line, never silent changes.

| # | Issue | Deviation | Spec line cited | Disposition |
|---|---|---|---|---|
| A5 | **DKT-19** | §11.4's `context` shape names no member for pre-gate results, while §11.1 requires `pre = true` gate results be "included in the context bundle". The implementation adds `context.pre_gates`, an array of §11.4-shaped gate results, present only when the step declares pre-gates. | §11.4 line `context { step: <next row>, issue: {…}, inputs: […], pins: […], loop_entry, metadata }` and §11.1 `gates` row: "`pre = true` gates run at claim with results included in the context bundle" | Additive field, no semantic change; the amendment proposes §11.4 name `pre_gates` explicitly. Filed **before** this TDD is committed. Argued in §7.6.3 |
| A6 | **DKT-20** | §11.4's `gate result` shape has no field carrying **why** a verdict is `unmatched`. The implementation adds `reason`, NULL on ordinary pass/fail results. | §11.4 line `gate result { step, gate, argv, exit, duration_ms, output, truncated, verdict }` | Additive field; four distinct unmatched causes (§3.4) are otherwise indistinguishable to an operator who must choose a different remedy for each. Argued in §6.3. Filed before this TDD is committed |
| A7 | **note on DKT-3** | engine-spine §5.6's promise — "S4 changes one constructor call and nothing else" — holds for the **saga** but not for **`claim`**: pre-gate execution is a second call site, and `ClaimStep` must be restructured from one transaction into three phases (§7.6.1) because a subprocess cannot run inside a transaction. | docs/tdd/engine-spine.md §5.6, and engine-spec §6 "No subprocess ever executes inside a transaction" | **No engine-spec line is deviated from** — this is a TDD's promise, recorded so the S3→S4 boundary claim is not quietly overstated. Filed as a comment on DKT-3 rather than a new amendment issue. Argued in §7.1 |
| — | **DKT-16** | **Closed by this stage**, not amended: v8 creates `gate_results` and migrates `steps.gate_trail` into it (§4.3), which is the exit condition DKT-16 itself records. | §11.4 line `gate result {…}`; docs/tdd/reliability-delta.md §2 (v8 row) | Closure. The group-2 commit that lands `migrateV7ToV8` closes it, citing §4.3 |

A5 (**DKT-19**) and A6 (**DKT-20**) are filed **before** this TDD is committed, so
the amendment trail exists ahead of the code that embodies it — the same discipline
engine-spine §10 followed for A1/A2. A7 is recorded as a comment on **DKT-3**,
the unit whose TDD made the promise it qualifies.

---

# 12. Implementation phases — TWO commit groups

Direct commits on `feature/graph-engine` per the operator decision recorded in
reliability-delta §11: no branches, no PRs, **never** tags, linear history. Draft
PR #33 provides CI on every push. **Every commit leaves the branch green.**

## Group 1 — `internal/trust` + `internal/exec`: pure, fully unit-tested, nothing wired

| | |
|---|---|
| **Contents** | `internal/trust` (§3: store, integrity, parse, canonical hash, matcher, repo identity, **the locked read-modify-write §3.5.1**) and `internal/exec` (§5: spec type, no-shell execution, the fence splitter, **the repo-containment check on resolved `argv[0]` §5.2.1**, env allowlist, timeout + process-group kill, capture + truncation, flaky re-runs, **the operator-facing renderer §5.7**). The full §9.1 unit tables. |
| **Not in it** | No DDL. No migration. No CLI verb. No engine call site. No change to any existing file outside these two new packages. |
| **Green** | The two packages build and their tests pass; every pre-existing test and QA section passes untouched. |
| **Independently stoppable** | Yes, and unusually cleanly: the packages are **unreachable from any command**. Stopping here leaves two well-tested libraries and a repo whose behavior is bit-for-bit S3's. |
| **Dormancy proof** (§9.3) | Trivially complete: a repo with no trusted commands and no workflows is unaffected because *no repo at all* is affected — nothing calls this code. Proven by the byte-compat sweep re-run plus a diff review showing zero edits outside the two new package directories. |
| **Docs in the same commit** | None required — no surface changes. (The §13 docs land with the surface they document, per CLAUDE.md's same-PR rule.) |

## Group 2 — v8 + saga wiring + vote execution + trust CLI + pre-gates

| | |
|---|---|
| **Contents** | v8 migration (§4: `gate_results`, `trust_cache`, the `gate_trail` backfill, the rewind guard, the U1–U7 upgrade tests); the real `GateRunner` and its constructor swap (§7.1); trust matching (§7.2); fence-gate execution with hash re-verification (§7.3); the tree mutex (§7.4) **incl. the `O_NOFOLLOW` lockfile open (L7)**; at-least-once resume with `re_runnable` (§7.5); pre-gates and the claim restructure (§7.6) **incl. transaction B's lease refresh (§7.6.1.1)**; the activation trust report incl. `--dry-run` (§7.7), **rendering through §5.7's renderer**; vote-step execution (§8); `docket trust add\|list\|rm` (§3.5); V26 (§8.2); the four new event kinds (§6.4); the §9.2 test set; QA section ZH + the ZG extensions; **closes DKT-16**. |
| **Green** | Full `go test ./...` plus `scripts/qa.sh` including the new ZH section and the genericity gate. |
| **Independently stoppable** | It is the stage's terminus, so "stoppable" here means *complete*: stopping after group 2 leaves stage 4 shipped in full. |
| **Dormancy proof** (§9.3) | v7→v8 applied; both new tables empty; **no trust file in the sandbox `XDG_CONFIG_HOME`**; the byte-compat sweep passes; a gate-free workflow run spawns zero processes, asserted by a spawn-counting runner. |
| **Docs in the same commit** | §13's full row for group 2. |

**Why two groups and not four.** S3 used four because its DDL genuinely sliced
into four independently-useful surfaces. Here the split is at the natural seam
the work order names: **pure mechanism** (which can be reviewed for security
without any engine context — and which is what the security lens will spend its
time on) and **wiring** (which is reviewable only against the engine). Slicing
group 2 further would produce a commit with a migration and no reader, or a
runner and no results table — neither of which leaves the branch green in any
meaningful sense.

---

# 13. Documentation, scheduled per group

CLAUDE.md's same-PR rule: *"skills/docket/SKILL.md documents the CLI — update its
flag/verb tables in the same PR as any surface change; a stale table is drift and
blocks review."*

| Group | Docs in the same commit group |
|---|---|
| 1 | **None.** No surface changes; nothing to document. Package doc comments carry §3's and §5's reasoning, in the style `internal/engine/gate.go` established |
| 2 | **docs/spec/security.md — the major update** (§13.1); **SKILL.md**: the `docket trust` verb/flag tables, the gate/fence documentation in the workflow-grammar section, `vote_rule` + the `vote.rule.*` config keys in the engine-configuration table, `run activate --dry-run`, the `gate result` shape, and the error codes; **docs/spec/architecture.md**: gate execution in the saga, the tree mutex, and the vote-step lifecycle |

## 13.1 docs/spec/security.md is the major update

The threat model of §2 **lands there in user-facing form** — that is the work
order's requirement and it is the right home: security.md is the living document
that records "the security contract **as implemented**, section by section, as
each stage lands" (its own status line). §2 here is the *design* argument for a
reviewer; security.md gets the *operator's* version.

New sections, in security.md's existing voice:

| § | Contents |
|---|---|
| **§7 The execution trust model** | What docket executes and when (the one place); why trust is user-level and repo-scoped; the trust file's location, format, and permissions; **repo identity and the malicious-clone argument** (§3.4, in prose an operator can act on, including the moved-repo cost); TOFU and `--yes`, with D14's residual stated plainly |
| **§8 What a gate child gets** | The env allowlist as a table; **`DOCKET_TOKEN` is never passed, and why** (a token in a gate child is engine authority); no shell, ever, with the argv-injection examples; cwd; the timeout and the process-group kill; the capture cap |
| **§8.1 Where `argv[0]` may come from** | T17/§5.2.1 in operator terms: docket resolves a trusted command name against **your** `PATH` and never modifies it — **but it refuses to run a binary that resolves to a path inside the repository**. The `.envrc`/direnv case named plainly, because it is the one an operator will actually hit: a repo-managed `PATH` entry is normal, useful dev tooling, and this rule is not a claim that direnv is unsafe — it is docket declining to let a *name* you trusted be answered by *code the repo ships*. The remedy is one line and is in the refusal message: trust the absolute path (`docket trust add build -- /abs/path/script.sh`) when running a repo-owned script is what you meant. The moved-repo-style cost is stated too: a toolchain that lives under the repo root must be trusted by path, not by name |
| **§8.2 What docket prints, and why it escapes** | T18/§5.7 in operator terms: fenced commands, argvs, and reasons are **content other people can write** — an issue author, a branch author, a clone. Docket renders them to your terminal with control characters escaped, so a command carrying terminal escapes shows up as `"make test\x1b[2K\r  make lint"` rather than silently repainting the line you are approving. **If you see `\x1b` or `\r` in a command docket shows you, that is the command, not a display bug — treat it as hostile.** `--json` output carries the raw bytes for programs to consume; the escaping is for humans reading a terminal, which is where the attack lands |
| **§9 Unmatched is a refusal, not a pass** | §6.2's N1–N4, for an operator reading a run report: what `unmatched` means, the four causes and their four different remedies, and why an unmatched gate fails the step |
| **§10 At-least-once gates** | `re_runnable` and what the operator is asserting by setting it; why the default parks; the `flaky` distinction (§7.5's four-way table) |
| **§11 Threat notes, extended** | The §2.1 residuals in operator terms: a trusted command is trusted; self-trust bounded by the permission layer and made auditable by `trust list` + the `trust-added` event; gates are not sandboxed in v1 |

Existing sections that gain a sentence rather than a rewrite: §1.2 (token
transport) gains the gate-child exclusion, cross-referencing §8; §2 (what a lease
is not) gains a note that the trust boundary argument now also carries the trust
file; §6 (threat notes) cross-references §11.

**SKILL.md's `docket trust` section** documents the verb tables, the `--` argv
boundary, the `--prefix` warning, the repo binding and its moved-repo
consequence, and the fact that a missing trust file means "nothing runs" rather
than an error — the stranger's first question, answered where they will look.
