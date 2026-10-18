# Connect API v1

Status: Accepted — 2026-10-18

The daemon serves the docket engine over Connect-Go, with Protobuf messages
defined in `proto/docket/v1/docket.proto`. This note records the v1 schema's
design: where it lives and how it is versioned, how RPCs and messages are
named, how the CLI's JSON envelopes relate to the wire, and how code is
generated.

## Package and version

- Proto package: `docket.v1`, in the file `proto/docket/v1/docket.proto`.
  The directory path mirrors the package, so `--proto_path=proto` resolves it.
- Major version: **1**. The major lives in the package name and nowhere else:
  no version header, no URL prefix beyond the one Connect derives from the
  package (`/docket.v1.DocketService/<Rpc>`).
- Compatible changes stay in `docket.v1`: new RPCs, new messages, new fields
  with new numbers. A field number is never reused or retyped; a removed
  field's number and name go into `reserved`.
- A breaking change creates `docket.v2` in `proto/docket/v2/`, served beside
  v1 until clients move. Nothing in v1 changes to make room for it.
- Go package: `github.com/ALT-F4-LLC/docket/internal/gen/docket/v1`, package
  name `docketv1`. protoc-gen-connect-go writes the handlers and clients to
  the sibling package `docketv1connect`.

### Codegen

Generation uses `protoc` with `protoc-gen-go` and `protoc-gen-connect-go`;
there is no buf configuration. From the repository root:

```sh
protoc --proto_path=proto \
  --go_out=. --go_opt=module=github.com/ALT-F4-LLC/docket \
  --connect-go_out=. --connect-go_opt=module=github.com/ALT-F4-LLC/docket \
  $(find proto -name '*.proto')
```

This writes `internal/gen/docket/v1/docket.pb.go` and
`internal/gen/docket/v1/docketv1connect/docket.connect.go`. The generated
files are committed, so `go build` needs neither protoc nor its plugins.
Wiring this command into a Makefile target, pinning the two plugin versions in
`go.mod` tools, and the drift check that regenerates and diffs belong to the
daemon implementation issue; this schema change touches only `proto/` and
`docs/design/`.

The schema compiles with protoc alone, no plugins:

```sh
protoc --proto_path=proto --descriptor_set_out="$TMPDIR/docket.desc" \
  $(find proto -name '*.proto')
```

The file imports nothing, including no well-known types, so that command
needs no include directory.

## Naming scheme

**One service.** Every RPC is a method of `docket.v1.DocketService`. Grouping
methods into per-noun services (`IssueService`, `RunService`) was weighed and
rejected: the CLI's groups nest up to three levels (`issue label add`,
`run note list`), so a service per top-level noun still needs path-derived
method names, and a client would hold a dozen service clients for one daemon.

**RPC names.** An RPC is named for its CLI verb's command path: each path
segment, and each hyphen-separated word inside a segment, capitalized and
concatenated.

| CLI command path | RPC |
|---|---|
| `issue comment add` | `IssueCommentAdd` |
| `step claim` | `StepClaim` |
| `dispatch backfill-usage` | `DispatchBackfillUsage` |
| `board` | `Board` |

To strip the service naming, drop the `docket.v1.DocketService.` prefix from
a method's full name. Comparisons between the descriptor and the CLI apply the
rule above to each CLI path and diff the results. The rule is collision-free
over the current command tree. Read in reverse, an RPC name splits at each
capital into the path's words. Eight verbs need a hyphen where that split
gives a space: `dispatch backfill-usage`, `dispatch waive-target`,
`project set-prefix`, `run refresh-body`, `run refresh-files`,
`run refresh-scope`, `run verify-pins`, and `vote backfill-usage`.

**Handshake.** `Handshake` is the one RPC with no CLI verb. A client calls it
before any verb. Its response carries `daemon_version`, the daemon's build
version string (the value `docket version` prints for the daemon binary), with
the commit, build date, API major, and store schema version beside it.

**Coverage.** The RPC set is every leaf command in `docket --help`, walked
recursively, minus `help`, `completion`, and the exclusions below, plus
`Handshake`. That is 119 verb RPCs and `Handshake`, 120 in all.

Excluded verbs: every leaf carrying the CLI's `skipDB` annotation. Those
verbs never open the store, so a daemon serving the store has nothing to do
for them:

- `init`: creates the store. A daemon serves a store that already exists.
- `version`: reports the client binary. The daemon's build comes from
  `Handshake`.
- `workflow init`: writes a starter definition into a directory on the
  client's filesystem.
- `trust add`, `trust list`, `trust probe`, `trust rm`: the trust roster is
  the operator's own `~/.config/docket/trust.toml`. It stays client-local:
  a daemon RPC would be a second write path into the allowlist that decides
  which commands docket executes.

Bare `docket config` runs, but it is not a leaf, since it has `get` and `set`
under it. It also carries `skipDB` and reports the client's own store
resolution, so it gets no RPC either. The hidden `step pregate` and
`step resume` are re-executions the engine launches itself. They are absent
from `--help` and get no RPC. Aliases (`step record` for `step complete`) map
to their command's RPC.

**Messages.** Each RPC `X` takes `XRequest` and returns `XResponse`. No two
RPCs share a request or response type, so either side of one verb can grow
without touching another verb. Shared entities are bare nouns: `Issue`, `Run`,
`Step`, `StepRow`, `Artifact`, `GateResult`, `Proposal`, `Vote`, and the rest.
`ErrorDetail` is the error payload. Fields are `snake_case` and follow the
JSON keys of today's `--json=v2` payloads. The daemon implementation pins
that parity per verb with golden envelope tests; a field the schema lacks is
added under a new number, which stays within v1.

**Field conventions.**

- Identifiers are display refs as strings (`DKT-12`, `RUN-3`, `STEP-9`,
  `ARTIFACT-4`). The server parses them as the CLI's argument parser does,
  including bare numbers.
- Engine vocabularies (statuses, priorities, kinds, verdicts) are strings.
  The server validates them at the boundary, as the CLI does today.
- A flag whose "not given" state differs from its zero value is a proto3
  `optional` field: `ttl_ms`, `cost_multiplier`, `budget`, edit fields.
- JSON documents travel as JSON text in `*_json` string fields: the opaque
  bags core never interprets (`--metadata`, `--usage`) and artifact
  payloads, which the server validates against their registered schema.
- A flag that names a file whose content is the input (`--artifact-file`,
  `--payload-file`, `--gap-file`, `--request-file`, `import <file>`,
  `workflow register <file>`) becomes a content field. The client reads the
  file; the daemon never opens a client path. A path the engine itself acts
  on (`--worktree`, `--source`) stays a path.
- Capability tokens travel in a `token` request field. The CLI's rule of no
  token in argv still holds on the client side: the client reads
  `DOCKET_TOKEN` or stdin and places the token in the request body, never in
  a URL or header that proxies log.
- The project a verb runs against travels in the `Docket-Project` request
  header. The CLI resolves it from the checkout, as it does today; verbs
  that take an explicit project (`issue move --project`,
  `registry audit --project`) keep it as a field.

**Recovery secret.** `StepClaimRequest.recovery_secret` is mandatory in v1.
The server rejects a claim whose recovery_secret is empty with
`VALIDATION_ERROR` before any lease is taken. It stores only the secret's
hash, and a re-claim of a held lease re-mints the token only when it presents
the same secret. The CLI's current rule (secret optional, read from a 0600
file on stdin) becomes the client's job: the client generates or reads the
secret and always sends one. `IssueClaimRequest` carries no recovery secret
in v1.

## Client-rendered envelopes

The wire carries no envelope. A response is the typed `XResponse` message,
and an error is a Connect error with an `ErrorDetail`. The client builds the
envelope the user asked for (`--json`, `--json=v2`, `--format json`, or human
output) from those messages. The envelope shapes stay byte-compatible with
today's output.

- **Success.** The client writes `{"ok": true, "data": ..., "message": ...}`.
  `data` is built from the response message. `message` is the human
  summary line, which the client composes, as the CLI's renderers do today.
- **v1 versus v2.** Every field v2 needs is on the wire, and the client drops
  what v1 omits. List responses carry `items`, `total`, and `truncated`;
  v2 renders all three, and v1 renders the verb's existing v1 payload from
  the same fields. Versioned entities
  carry `version` (and `lease` when held); v1 leaves both out. A field the v1
  payload shows only when set (`scope`, `resolution`, `size`) is rendered
  only when present: `Issue.scope` is a `ScopeGlobs` message, so "no scope"
  (absent) and "declared empty" (present, no globs) stay distinct.
- **Errors.** The daemon returns a Connect error whose first detail is
  `ErrorDetail{code}`, holding the CLI error code (`NOT_FOUND`,
  `VALIDATION_ERROR`, `CONFLICT`, `AUTH_ERROR`, `STALE_LEASE`, `TIMEOUT`,
  `UNTRUSTED`, `GONE`, `GENERAL_ERROR`). The client renders
  `{"ok": false, "error": <message>, "code": <code>}` and exits with the code's
  existing exit status. The Connect code is set for generic clients only:
  `not_found`, `invalid_argument`, `aborted` for `CONFLICT`,
  `permission_denied` for `AUTH_ERROR` and `UNTRUSTED`, `failed_precondition`
  for `STALE_LEASE`, `deadline_exceeded`, `out_of_range` for `GONE`, and
  `internal`. The docket client reads `ErrorDetail.code`, never the Connect
  code.
- **Partial failure.** A verb that fails but still has a report attaches its
  own `XResponse` as a second error detail. The client renders
  `{"ok": false, "data": ..., "error": ..., "code": ...}`, as the CLI's
  partial-failure envelope does.
- **Claim that committed but did not finish.** `step claim` still succeeds
  when the lease committed and a later stage failed: `StepClaimResponse`
  carries the token plus `claim_error`. The client renders the success
  envelope with that message.
- **Human output.** Rendering stays in the client's `internal/render` and
  `internal/output`. The daemon sends no terminal styling and no prose.
- **Watch and follow.** All v1 RPCs are unary. `--watch`, `--interval`, and
  `events list --follow` are client loops that call the RPC again at the
  interval.
