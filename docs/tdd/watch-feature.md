---
project: "docket"
maturity: "approved"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "Add --watch flag to all read subcommands for polling-based real-time refresh"
owner: "@staff-engineer"
dependencies:
  - ../spec/architecture.md
---

# TDD: Watch Mode for Read Subcommands

## 1. Problem Statement

Docket is increasingly used in AI-driven workflows where agents create, move, and close issues
autonomously. Humans observing these workflows currently have to manually re-run commands
(`docket board`, `docket issue list`, etc.) to see changes. This creates a poor feedback loop
for human oversight of AI agents.

**Goal:** Add a `--watch` flag to all read subcommands that enables polling-based real-time
refresh. The terminal output is cleared and reprinted on a configurable interval, giving humans
a live dashboard view of AI-driven changes.

**Constraints:**
- No TUI framework (no bubbletea, no curses). Simple terminal clear + reprint.
- No websocket or event streaming. Polling only.
- Must work with `--json` mode (for piping to other tools).
- Must work in non-TTY environments (CI, piped output) with graceful degradation.
- Single DB connection reused across poll cycles (not reopened each time).

**Acceptance Criteria:**
1. `--watch` flag is available on all read subcommands (see Section 4 for the full list).
2. `--interval` flag controls polling frequency (default: 2s, minimum: 500ms).
3. Human mode: screen clears and reprints output each cycle. A subtle status line shows
   the last refresh timestamp and interval.
4. JSON mode with `--watch`: emits one complete JSON envelope per cycle, separated by newlines
   (NDJSON). Each object is a self-contained response identical to a non-watch invocation.
5. Ctrl+C cleanly exits the watch loop with no partial output or dangling goroutines.
6. `--quiet` mode suppresses the watch status line but still refreshes output.
7. `--watch` without a TTY (piped stdout) skips ANSI clear sequences and just prints
   successive outputs separated by a blank line.
8. The DB connection opened in `PersistentPreRunE` is reused for all poll cycles.
9. Existing non-watch behavior is completely unchanged.
10. `--watch` on a write command (e.g., `issue create`) produces a validation error.

## 2. Context & Prior Art

### Existing Patterns in Docket

- **Global flags** are registered on `rootCmd.PersistentFlags()` in `root.go:init()`:
  `--json` and `--quiet`. Both are read via `cmd.Flags().GetBool()` in command handlers and
  the `getWriter()` helper.
- **DB lifecycle** is managed in `PersistentPreRunE` (open + migrate) and
  `PersistentPostRunE` (close). The `*sql.DB` is stored in the command context. Commands
  retrieve it via `getDB(cmd)`.
- **Output dispatch** goes through `output.Writer.Success(data, message)`. JSON mode wraps
  data in `{"ok": true, "data": ..., "message": "..."}`. Human mode prints the message string.
- **All read commands** follow the same pattern: get writer, get DB, query, render, call
  `w.Success()`.

### How Other CLIs Do Watch

- **`kubectl get --watch`**: Uses server-side watch streams (event-based). Falls back to
  polling for resources that don't support watch. Outputs incremental updates.
- **`watch(1)` (coreutils)**: External wrapper. Runs any command on an interval. Uses
  ANSI clear (`\033[H\033[2J`). Shows header with interval and command.
- **`docker stats`**: Continuous streaming mode. ANSI clear for TTY, plain append for non-TTY.
- **`gh run watch`**: Polls GitHub API on an interval. Clears and reprints. Exits when the
  run completes.

Docket's approach most closely resembles `watch(1)` — a polling loop with terminal clear. The
key difference is that it lives inside the CLI rather than being an external wrapper, which
allows it to reuse the DB connection and integrate with `--json` mode.

## 3. Alternatives Considered

### Alternative A: External `watch` wrapper (Rejected)

Users could run `watch -n 2 docket board`. This works but:
- Reopens the DB on every invocation (migration check, pragma setup).
- Cannot emit NDJSON (each run is independent).
- No `--json` integration.
- Platform-dependent (`watch` is not available on macOS by default).
- Status: **Rejected** — too many limitations for the primary use case.

### Alternative B: Per-command `--watch` flag (Rejected)

Each command registers its own `--watch` flag and implements its own loop. This would work but:
- Duplicates loop logic across 12+ commands.
- Inconsistent behavior risk.
- Higher maintenance burden.
- Status: **Rejected** — violates DRY.

### Alternative C: Persistent flag on root + shared watch utility (Recommended)

Register `--watch` and `--interval` as persistent flags on `rootCmd`. Add a shared utility
package (`internal/watch`) that wraps the command's `RunE` in a polling loop. Each command
opts in by being listed in a watch-eligible registry. Write commands are excluded via
validation in `PersistentPreRunE`.

- Centralized loop logic.
- Consistent behavior across all read commands.
- Easy to add new commands to the watch-eligible list.
- Status: **Recommended**.

### Alternative D: Middleware/decorator pattern on RunE (Considered)

Instead of a shared utility, wrap each command's `RunE` function with a decorator at
registration time. The decorator checks `--watch` and either runs once or enters the loop.

This is functionally equivalent to Alternative C but couples the watch logic to command
registration rather than command execution. It's slightly more magical but eliminates the
need for commands to explicitly call a watch helper.

- Status: **Viable alternative**. The recommended approach (C) is preferred for explicitness,
  but this could be adopted if the team prefers less boilerplate in command handlers.

## 4. Architecture & System Design

### 4.1 Watch-Eligible Commands

All read/query commands that display data and do not mutate state:

| Command | Parent | Notes |
|---------|--------|-------|
| `board` | root | Kanban board view |
| `issue list` | issue | Issue listing |
| `issue show <id>` | issue | Single issue detail |
| `issue log <id>` | issue | Activity history |
| `issue graph <id>` | issue | Dependency graph |
| `issue comment list <id>` | issue comment | Comment listing |
| `next` | root | Work-ready issues |
| `plan` | root | Execution plan |
| `stats` | root | Summary statistics |
| `vote list` | vote | Proposal listing |
| `vote show <id>` | vote | Proposal detail |
| `vote result <id>` | vote | Consensus result |
| `config` | root | Configuration display (borderline — config rarely changes, but including it is harmless and consistent) |

**Excluded commands** (write/mutate operations):
`init`, `issue create`, `issue edit`, `issue close`, `issue reopen`, `issue delete`,
`issue move`, `issue comment add`, `issue label`, `issue link`, `issue file`,
`import`, `export`, `vote create`, `vote cast`, `vote commit`, `vote link`, `vote unlink`,
`version`.

### 4.2 Component Layout

```
internal/
  watch/
    watch.go          -- RunWatch loop, signal handling, terminal clear
  cli/
    root.go           -- Register --watch, --interval persistent flags
                      -- PersistentPreRunE: validate --watch not on write commands
    watch_commands.go  -- watchEligible set (command names -> bool)
```

### 4.3 Flag Registration

In `root.go:init()`, alongside existing persistent flags:

```go
rootCmd.PersistentFlags().BoolP("watch", "w", false, "Watch for changes and refresh output")
rootCmd.PersistentFlags().Duration("interval", 2*time.Second, "Refresh interval for --watch")
```

`-w` as the shorthand is available (no current flag uses it).

### 4.4 Watch Validation

In `PersistentPreRunE`, after config/DB setup, if `--watch` is set:

1. Check that the current command is in the watch-eligible set. If not, return a
   `CmdError` with `ErrValidation`: `"--watch is not supported on write commands"`.
2. Validate `--interval` is >= 500ms. Return `ErrValidation` if not.

**Flag visibility:** Since `--watch` and `--interval` are persistent flags on `rootCmd`,
they appear in `--help` output for all commands — including write commands where they are
invalid. To avoid confusion, use Cobra's `MarkHidden` to hide these flags on ineligible
commands. In `watch_commands.go`, during `init()`, iterate over all registered commands
and call `cmd.Flags().MarkHidden("watch")` and `cmd.Flags().MarkHidden("interval")` for
commands not in the watch-eligible set. This keeps the flags functional (for validation
error reporting) but removes them from help output where they don't apply.

### 4.5 Watch Loop Architecture

The `internal/watch` package exposes a single function:

```go
// RunWatch executes fn repeatedly on the given interval until ctx is cancelled.
// It handles terminal clearing, output buffering, and newline separation.
// On each cycle, RunWatch creates a fresh bytes.Buffer, wraps it in an
// output.Writer, and passes it to fn. After fn returns, RunWatch clears the
// screen (if TTY) and flushes the buffer to real stdout atomically.
func RunWatch(ctx context.Context, opts Options, fn func(ctx context.Context, w *output.Writer) error) error

type Options struct {
    Interval  time.Duration
    JSONMode  bool
    QuietMode bool
    IsTTY     bool       // whether stdout is a terminal
    Stdout    io.Writer  // real stdout destination (typically os.Stdout)
    Stderr    io.Writer  // real stderr destination (typically os.Stderr)
}
```

**Loop behavior:**

1. Create a `bytes.Buffer` for output capture.
2. Create an `output.Writer` with `Stdout` set to the buffer.
3. Call `fn(ctx, w)` — the command logic writes to the buffer via the Writer.
4. **First cycle**: Write buffer contents directly to real `Stdout`.
5. **Subsequent cycles**:
   - **Human mode + TTY**: Write ANSI escape `\033[H\033[2J` (cursor home + clear screen)
     to real stdout, then print a status line (`Watching every Xs... (last update: HH:MM:SS)`),
     then flush the buffer atomically.
   - **Human mode + non-TTY**: Print a blank line separator, then flush the buffer.
   - **JSON mode**: Flush the buffer directly. Each cycle emits one JSON envelope via
     `w.Success()`, which already writes a newline. No additional separator needed. The
     result is NDJSON.
   - **Quiet mode**: Suppress the status line but still clear and reprint.
6. Reset the buffer for the next cycle.
7. Wait for `interval` using `time.NewTicker`.
8. On `ctx.Done()` (from signal handler), return `nil`.

The buffer is allocated once and reused (`buf.Reset()`) across cycles to avoid allocation
churn.

**Signal handling:**

The caller (command handler) sets up signal handling:

```go
ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
defer stop()
```

This is passed to `RunWatch`. When the user presses Ctrl+C, the context is cancelled,
the ticker stops, and the function returns cleanly.

### 4.6 Command Integration Pattern

Each watch-eligible command modifies its `RunE` to check for watch mode. The extracted
command logic functions accept an `*output.Writer` parameter rather than calling
`getWriter(cmd)` internally. This allows `RunWatch` to inject a buffer-backed Writer on
each cycle, and lets the non-watch path use the standard Writer from `getWriter(cmd)`.

The pattern is:

```go
// runBoard contains the actual query + render + output logic.
// It accepts a Writer so the caller controls where output goes.
func runBoard(cmd *cobra.Command, args []string, w *output.Writer) error {
    conn := getDB(cmd)
    // ... query, render, w.Success(result, message)
    return nil
}

var boardCmd = &cobra.Command{
    RunE: func(cmd *cobra.Command, args []string) error {
        watchMode, _ := cmd.Flags().GetBool("watch")
        if watchMode {
            interval, _ := cmd.Flags().GetDuration("interval")
            jsonMode, _ := cmd.Flags().GetBool("json")
            quietMode, _ := cmd.Flags().GetBool("quiet")
            ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
            defer stop()
            return watch.RunWatch(ctx, watch.Options{
                Interval:  interval,
                JSONMode:  jsonMode,
                QuietMode: quietMode,
                IsTTY:     term.IsTerminal(int(os.Stdout.Fd())),
                Stdout:    os.Stdout,
                Stderr:    os.Stderr,
            }, func(ctx context.Context, w *output.Writer) error {
                return runBoard(cmd, args, w)
            })
        }
        return runBoard(cmd, args, getWriter(cmd))
    },
}
```

Key points:
- `runBoard` (and all extracted `runXxx` functions) accept `*output.Writer` as a parameter.
- In watch mode, `RunWatch` creates a buffer-backed Writer and passes it to `fn` each cycle.
- In non-watch mode, the standard `getWriter(cmd)` is passed directly.
- `getDB(cmd)` is still called inside `runXxx` — the DB connection is stable across cycles
  (see Section 4.7).

### 4.7 DB Connection Reuse

The existing `PersistentPreRunE` opens the DB once and stores it in the command context.
`PersistentPostRunE` closes it. Since `RunWatch` runs within `RunE` (between pre and post
hooks), the same `*sql.DB` connection is reused for all poll cycles. No changes to the
DB lifecycle are needed.

SQLite WAL mode with `busy_timeout=5000` ensures that the watching process can read even
while another process (e.g., an AI agent) is writing. This is the intended concurrency model.

**Single-connection constraint:** The DB is opened with `MaxOpenConns(1)`. This is correct
for the watch loop because polling is sequential — each cycle runs a query, waits for the
result, renders, then sleeps. There is no concurrent goroutine access within the watch
process. Implementers must not introduce concurrent DB access within the watch loop (e.g.,
parallel queries across goroutines) as this would deadlock on the single-connection pool.
If future work requires concurrent queries within a cycle, `MaxOpenConns` would need to
be increased, but that is out of scope for this feature.

### 4.8 Output Buffering

To avoid partial screen renders (flickering), `RunWatch` owns the output buffer and
performs atomic writes. The sequence on each cycle is:

1. `RunWatch` creates an `output.Writer` with `Stdout` pointing to a `bytes.Buffer`.
2. `fn(ctx, w)` runs the command logic, which writes to the buffer via the Writer.
3. After `fn` returns, `RunWatch` clears the screen (if TTY + human mode), writes the
   status line, then flushes the entire buffer to real stdout in a single `Write` call.

This ensures the user never sees a partially rendered frame. The clear + write happens
as a single atomic sequence from the terminal's perspective.

The `output.Writer` struct already accepts an `io.Writer` for `Stdout`, so no changes to
the `output` package are needed — `RunWatch` simply constructs a Writer with the buffer
as its `Stdout` field each cycle.

The `bytes.Buffer` is allocated once and reused via `buf.Reset()` across cycles to avoid
per-cycle allocation overhead.

### 4.9 Terminal Width Changes

The existing render layer (`internal/render`) already queries terminal width dynamically
(e.g., `render.TerminalWidth()` for board column layout). Since this is called on each
render cycle, terminal resize during watch mode is handled automatically.

## 5. Data Models & Storage

No data model or storage changes. Watch mode is purely a presentation-layer feature.

## 6. API Contracts

### CLI Interface

```
# Watch the board, refresh every 2s (default)
docket board --watch

# Watch with custom interval
docket board --watch --interval 5s

# Watch in JSON mode (produces NDJSON)
docket board --watch --json

# Watch issue list with filters
docket issue list --watch --status in-progress --label backend

# Watch a specific issue
docket issue show DKT-5 --watch

# Watch the execution plan
docket plan --watch

# Invalid: watch on a write command
docket issue create --watch  # Error: --watch is not supported on write commands
```

### NDJSON Output (--watch --json)

Each line is a complete, self-contained JSON envelope:

```json
{"ok":true,"data":{"columns":[...]},"message":""}
{"ok":true,"data":{"columns":[...]},"message":""}
{"ok":true,"data":{"columns":[...]},"message":""}
```

Consumers can process each line independently with `jq`, `jaq`, or any JSON streaming parser.

### Human Output

```
Watching every 2s... (last update: 14:32:05) [Ctrl+C to exit]

[normal board/list/detail output here]
```

The status line appears at the top. On non-TTY, the status line is omitted and outputs are
separated by blank lines.

## 7. Migration & Rollout

No database migration. No breaking changes. The feature is purely additive:

- `--watch` defaults to `false` — existing behavior is unchanged.
- `--interval` is only meaningful when `--watch` is set.
- No existing flags conflict with `-w` shorthand.

**Rollout:** Ship in a single release. No feature flag needed. The feature is opt-in
via `--watch`.

## 8. Risks & Open Questions

### Risks

| Risk | Severity | Mitigation |
|------|----------|------------|
| Flickering on slow renders | Low | Output buffering (Section 4.8) eliminates partial renders |
| DB lock contention with concurrent writer | Low | WAL mode + `busy_timeout=5000` handles this. Read-only queries don't block writers |
| Accidental watch on write command | Low | Explicit validation in `PersistentPreRunE` prevents this |
| Memory growth on long watch sessions | Low | Each cycle overwrites the buffer; no accumulation. `bytes.Buffer` is reused |
| ANSI clear doesn't work in all terminals | Low | Degrade to newline separation for non-TTY. The `\033[H\033[2J` sequence works in all modern terminals (iTerm2, Terminal.app, Windows Terminal, xterm, alacritty) |

### Open Questions

1. **Should `export` support `--watch`?** Export writes to a file or stdout in a specific
   format (JSON/CSV/Markdown). Watching an export seems unusual but not harmful. Current
   decision: **exclude** — export is conceptually a snapshot operation, not a live view.
   Revisit if users request it.

2. **Should `config` support `--watch`?** Config rarely changes during a session. Including
   it is harmless for consistency but of questionable utility. Current decision: **include**
   for consistency, since it is a read-only command.

3. **Should the status line include a cycle counter?** e.g., `Watching every 2s... (refresh #42,
   last update: 14:32:05)`. Useful for debugging but adds noise. Current decision: **omit**
   the counter. Add it later if debugging watch behavior becomes a need.

## 9. Testing Strategy

### Unit Tests

- `internal/watch/watch_test.go`:
  - Test that `RunWatch` calls `fn` immediately on first cycle.
  - Test that `RunWatch` calls `fn` again after interval.
  - Test that `RunWatch` returns cleanly when context is cancelled.
  - Test that TTY mode emits ANSI clear before each cycle (after first).
  - Test that non-TTY mode emits blank line separators.
  - Test that JSON mode does not emit ANSI sequences.
  - Test that quiet mode suppresses the status line.
  - Test that interval < 500ms is rejected at validation.

### Integration Tests (in `internal/cli/`)

- Test that `--watch` flag is accepted on all watch-eligible commands.
- Test that `--watch` on a write command returns `ErrValidation`.
- Test that `--watch` with `--json` produces valid NDJSON (multiple JSON objects parseable
  independently).

### Manual Testing

- Run `docket board --watch` in a TTY terminal. Verify smooth clear and reprint.
- In a second terminal, create/move/close issues. Verify the board updates.
- Run `docket board --watch --json | head -3` and verify 3 valid JSON lines.
- Pipe `docket board --watch` to `cat` (non-TTY). Verify no ANSI codes in output.
- Press Ctrl+C during watch. Verify clean exit (no stack trace, no partial output).
- Test with `--interval 500ms` and `--interval 100ms` (should reject).

## 10. Observability & Operational Readiness

Watch mode is a client-side presentation feature with no server component. Observability
is limited to the CLI itself:

- **Exit code**: 0 on clean Ctrl+C exit. Non-zero on errors (same as existing error handling).
- **Stderr diagnostics**: Errors during a watch cycle are printed to stderr (same as existing
  error handling). The watch loop continues unless the error is unrecoverable (DB connection
  lost).
- **Error recovery in watch loop**: If a single cycle's `fn` returns an error, print the error
  to stderr and continue watching (do not exit). This handles transient DB busy errors. If
  the error persists for 3 consecutive cycles, exit with the error.

## 11. Implementation Phases

### Phase 1: Watch utility package (Size: S)

- Create `internal/watch/watch.go` with `RunWatch` function.
- Implement terminal clear (ANSI), TTY detection, output buffering, signal handling.
- Unit tests for `RunWatch`.
- No command integration yet.

### Phase 2: Flag registration and validation (Size: S)

- Add `--watch` and `--interval` persistent flags to `rootCmd` in `root.go`.
- Create `watch_commands.go` with the watch-eligible command set.
- Add validation in `PersistentPreRunE`.
- Unit test for validation (watch on write command -> error).

### Phase 3: Integrate watch into read commands (Size: M)

- Extract command logic into named functions for each watch-eligible command.
- Add the `if watchMode { RunWatch(...) }` pattern to each command's `RunE`.
- Integration tests.

**Dependencies:** Phase 3 depends on Phase 1 and Phase 2. Phases 1 and 2 are independent
and can be done in parallel.
