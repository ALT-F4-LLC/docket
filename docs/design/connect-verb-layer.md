# Shared verb layer for the Connect handlers

Status: Proposed — 2026-10-31

The daemon must serve every verb RPC in `proto/docket/v1/docket.proto`, and a
handler must run the same validation, ref parsing, project resolution and
authorization the CLI runs. That logic lives in `internal/cli` today, and
`internal/cli` imports `internal/api` (`internal/cli/root.go:18`), so a
handler in `internal/api` cannot call it without an import cycle. Copying it
into each handler gives two implementations of every trust check, which drift.
This note decides the package both transports call instead, its import rules,
the order in which verbs move into it, how handlers bind to it, and the nouns
it serves. It changes no code itself.

Measurements below were taken at b0bbcf5 with `/usr/bin/grep` over non-test
`.go` files.

## Package

The shared layer is **`internal/verb`** (package `verb`). It holds one exported
function per verb RPC, grouped in one file per noun (`internal/verb/issue.go`,
`internal/verb/run.go`, and so on), plus the helpers every verb uses: ref
parsing (`issueArg` and `parseRunFlag` today), project resolution, and the
lease and CAS error translation (`leaseError`, `casError`).

A verb function has this shape:

```go
func IssueClose(ctx context.Context, env verb.Env, in verb.IssueCloseInput) (verb.IssueCloseResult, error)
```

- `Env` carries what the transport supplies: the `store.Store`, the author,
  the project identity input (identity string and whether it is anchored), and
  the build information `Handshake` reports. The CLI fills it from its config
  and connection; the daemon fills it from its own store and the request's
  `Docket-Project` header.
- `In` holds values, never flags or files. A token, a file's content and a
  JSON document arrive as fields, following the field conventions in
  `docs/design/connect-api-v1.md`. Reading stdin, opening a client path,
  `os.Getwd`, `--watch` loops and rendering stay in `internal/cli`.
- `Out` is built from `internal/model` types, so the CLI keeps its existing
  renderers and byte-compatible envelopes.
- A failure is a `*verb.Error` holding an `output.ErrorCode` and the cause.
  The CLI maps it to `CmdError`; the daemon maps it with
  `output.NewConnectError`. A verb that fails with a report returns both a
  result and an error, and each transport renders that pair as its
  partial-failure form.
- Project policy is declared by the verb, at the operation. Today
  `commandMayRegisterProject` and `commandUsesAmbientProject`
  (`internal/cli/project_resolve.go:94,127`) decide it from the cobra command
  path, which the daemon does not have. A moved verb calls
  `env.ResolveProject(policy)` with one of three policies: read-only,
  may-register, or run-addressed.

## Import direction

```
internal/cli ──► internal/verb ◄── internal/api
                     │
                     ▼
     store, engine, model, config, output, ...
```

- `internal/verb` imports neither `internal/cli` nor `internal/api`, nor any
  package under `internal/api/` (the generated `docketv1` and
  `docketv1connect` included), nor cobra, connect or `database/sql`. It depends
  on `store.Store`, `store.Tx` and `store.ReadTx`, never on `*sql.DB`,
  `store.LegacyDB` or `getDB`.
- `internal/api` imports `internal/verb` and never `internal/cli`.
- `internal/cli` imports `internal/verb` and never the `internal/api` package.
  Today's one edge, `internal/cli/root.go:18`, serves the client side:
  `api.NewClient`, `api.Connect` and the `api.DaemonVersion` global
  (`root.go:171,566,574`). The first move step splits that side out:
  `NewClient` and `Connect` go to `internal/api/client`, the strict
  protojson interceptor (`internal/api/codec.go`) goes to `internal/api/codec`
  so server and client share it, and the build version becomes a
  `NewService` argument instead of a global the CLI writes. The CLI then
  imports `internal/api/client` and the generated `docketv1connect`, and not
  `internal/api`.
- No package under `internal/` imports `internal/cli` today (only
  `cmd/docket` does), and that stays true. The cycle-freedom invariant is that the transitive imports of
  `internal/api` and `internal/verb` never include `internal/cli`.

The last move step adds a check over `go list -f '{{.ImportPath}} {{.Imports}}'`
and `go list -deps` that fails on any edge this section forbids.

## Handler binding

`internal/api` defines one `Service` that implements
`docketv1connect.DocketServiceHandler`:

```go
type Service struct {
	docketv1connect.UnimplementedDocketServiceHandler
	store store.Store
	build verb.BuildInfo
}
```

Each noun gets one file, `internal/api/<noun>.go`, that declares that noun's
methods on `*Service` together with its request decoders and response
encoders. Each method is one call to a generic adapter in
`internal/api/bind.go`:

```go
func (s *Service) IssueClose(ctx context.Context, req *connect.Request[docketv1.IssueCloseRequest]) (*connect.Response[docketv1.IssueCloseResponse], error) {
	return unary(ctx, s, req, decodeIssueClose, verb.IssueClose, encodeIssueClose)
}
```

`unary` is the one place that builds `Env` from the request (store, build,
`Docket-Project` header), turns a decode failure into `VALIDATION_ERROR`,
calls the verb, encodes the result, and maps a `*verb.Error` through
`output.NewConnectError`, attaching the encoded response as the second detail
on partial failure. A handler therefore cannot skip error mapping or project
input, and it has no logic of its own beyond field translation. Options that
apply to every RPC stay in `NewHandler`'s handler options, so a noun file never
sets one.

Methods are declared on `*Service` directly, not on embedded per-noun structs,
so adding a noun touches only that noun's new file. An RPC whose noun has not
moved falls through to `UnimplementedDocketServiceHandler` and answers
`unimplemented`. The last move step adds a test that calls every method and
fails on any `unimplemented` answer.

The CLI binds the same way: each `RunE` parses flags and arguments into the
verb's input, does its client-side reads, calls the verb function with an
`Env` built from `store.StoreOn(getDB(cmd))` (or the `Store` itself once the
CLI holds one), and renders the result with the existing writer.

## Move order

A move step moves one noun: it adds `internal/verb/<noun>.go`, adds
`internal/api/<noun>.go`, and rewrites that noun's CLI files to call the verb
layer. Two rules keep it clear of the CLI's in-flight move onto `store.Store`:

1. **That work goes first, per file.** A move step does not dispatch while an
   unlanded step of that work names one of its files, and that work never
   waits on a move step. Its steps pin file lists and query-site line numbers
   in their acceptance criteria (see `docs/design/sql-handle-migration.md`), so
   a move landing first would invalidate them. A move landing second rebases
   onto finished files.
2. **The verb layer starts on the target type.** Because `internal/verb`
   takes `store.Store` and never a `database/sql` handle, the later retarget of
   the store aliases never touches it, and no verb is moved twice.

Shared CLI helper files (`clihelpers.go`, `token.go`, `ifversion.go`,
`project_resolve.go`) change in only two steps, the foundation and the
cleanup. In between, a helper whose body moved keeps its CLI name as a
one-line call into `internal/verb` until its last caller has moved, so noun
steps never edit a shared file and can run in parallel.

The order:

1. **Client split and meta noun.** The `internal/api` client side moves to
   `internal/api/client` and `internal/api/codec`; `Service`, `unary` and
   `Handshake` land, and `MetaHandler` and `DaemonVersion` go. Files: the
   `internal/api` package, `internal/cli/root.go`,
   `internal/cli/root_test.go`, `internal/daemon/window_test.go`. It waits on the store work's step for
   `internal/cli/root.go`.
2. **Foundation.** `Env`, `Error`, ref parsing, project resolution and
   lease/CAS translation land in `internal/verb`, and the shared CLI helpers
   become calls into it. The CLI root's eager project resolution
   (`internal/cli/root.go:285`) skips any command carrying a `verbOwnsProject`
   annotation, which each move step sets on its own commands, following the
   `skipDB` precedent at `root.go:229`. It waits on the store work's steps for
   the four helper files.
3. **Nouns clear of the store work at b0bbcf5**, one step each, in parallel
   after `doc` lands first to set the pattern: doc, vote, run, dispatch,
   guard, next, board, plan, policy, pin, gate, report, stats, doctor. None of
   their CLI files imports `database/sql` at b0bbcf5.
4. **Nouns with files the store work still holds at b0bbcf5**, each after the
   store work's step for those files lands: issue (`issue_claim.go`,
   `issue_close.go`, `issue_scope.go`, `issue_show.go`), step (`step.go`),
   config (`config_classes.go`), events (`events_follow.go`), export
   (`export_manifest.go`), import (`import.go`, `import_remap.go`), project
   (`project.go`), registry (`registry_fanout.go`), schema
   (`schema_deprecate.go`, `schema_register.go`), and workflow
   (`workflow_deprecate.go`, `workflow_lint.go`, `workflow_register.go`).
5. **Cleanup.** The forwarding helpers, the command-path project tables and
   the root's eager resolution go. The import-direction check and the
   every-method coverage test land.

The two lists in steps 3 and 4 are a snapshot. A move step re-measures its
own files with `/usr/bin/grep -lE '"database/sql"' <files>` and against the
store work's open steps before it dispatches. Large nouns (issue with 25 RPCs,
run with 19, step with 17) may split into several steps along CLI file
boundaries without changing these rules.

## Nouns

The layer serves 25 nouns. A noun is the first segment of the RPC's CLI
command path (`docs/design/connect-api-v1.md`, "RPC names"), and `meta` holds
`Handshake`, which has no CLI verb. Each of the 120 RPCs `DocketService`
declares belongs to exactly one noun.

| Noun | RPCs |
|---|---|
| meta | Handshake |
| board | Board |
| config | ConfigGet, ConfigSet |
| dispatch | DispatchAbandon, DispatchBackfillUsage, DispatchClose, DispatchExtend, DispatchOpen, DispatchVerify, DispatchWaiveTarget |
| doc | DocCommentAdd, DocCommentList, DocCreate, DocDelete, DocEdit, DocLinkAdd, DocLinkRemove, DocList, DocShow |
| doctor | Doctor |
| events | EventsList, EventsPrune |
| export | Export |
| gate | GateStatus |
| guard | GuardGate, GuardRecord, GuardSpawn, GuardStop |
| import | Import |
| issue | IssueClaim, IssueClose, IssueCommentAdd, IssueCommentList, IssueCreate, IssueDelete, IssueEdit, IssueFileAdd, IssueFileList, IssueFileRemove, IssueGraph, IssueHeartbeat, IssueLabelAdd, IssueLabelDelete, IssueLabelList, IssueLabelRm, IssueLinkAdd, IssueLinkList, IssueLinkRemove, IssueList, IssueLog, IssueMove, IssueRelease, IssueReopen, IssueShow |
| next | Next |
| pin | PinShow |
| plan | Plan |
| policy | PolicyResolve |
| project | ProjectDelete, ProjectList, ProjectSetPrefix |
| registry | RegistryAudit |
| report | ReportExecutors |
| run | RunAbandon, RunActivate, RunBudget, RunConduct, RunFactAdd, RunIssueAdd, RunIssueRemove, RunNoteAdd, RunNoteList, RunPause, RunRefreshBody, RunRefreshFiles, RunRefreshScope, RunRepin, RunReport, RunResume, RunStart, RunStatus, RunVerifyPins |
| schema | SchemaDeprecate, SchemaList, SchemaRegister, SchemaShow |
| stats | Stats |
| step | StepAnnotate, StepApprove, StepArtifact, StepArtifacts, StepClaim, StepComplete, StepContext, StepFail, StepGates, StepHeartbeat, StepHold, StepList, StepReap, StepReject, StepRender, StepResolve, StepShow |
| vote | VoteBackfillUsage, VoteCast, VoteClose, VoteCommit, VoteCreate, VoteLink, VoteList, VoteResult, VoteShow, VoteUnlink |
| workflow | WorkflowDeprecate, WorkflowLint, WorkflowList, WorkflowRegister, WorkflowShow |

The table was checked by extracting its RPC names, sorting them, and diffing
against `grep -oE '^  rpc [A-Za-z]+' proto/docket/v1/docket.proto`: no
difference, 120 names on each side, no duplicates.

## Alternatives weighed

- **Copy the logic into each handler.** Rejected: two implementations of each
  trust check, which drift apart.
- **Move the verb logic into `internal/engine`.** Rejected: engine holds the
  state machine. Ref parsing, project-policy selection and input validation
  are boundary concerns, and putting them there would grow a package already
  under its own store migration.
- **Handlers call `internal/cli` through an interface the CLI implements and
  the daemon injects.** Rejected: the logic stays inside cobra `RunE`
  closures that write to the CLI's output writer, so the daemon would still
  run CLI code, and `internal/cli` would keep importing `internal/api`.
- **Verb functions take and return the generated `docketv1` messages.**
  Rejected: it removes the decoders and encoders, but it ties the shared layer
  to the wire schema and moves CLI rendering onto protojson, which breaks the
  byte-compatible envelopes.
- **One generic dispatcher keyed by RPC name.** Rejected: it gives up the
  compile-time check that each method matches its generated signature.
