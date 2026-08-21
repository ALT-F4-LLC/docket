package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// The `docket step` verb family — TDD §6.10.

var stepCmd = &cobra.Command{
	Use:   "step",
	Short: "Claim, complete, and inspect a run's steps",
	Long: `Steps are the units of work a run schedules.

A step is CLAIMED — which mints a capability token and returns the whole
context bundle in one response — then COMPLETED with an artifact. Completion is
a saga: the artifact records and the token retires in one commit, after which
the step is engine-owned and finishes under any later invocation, even if the
worker that claimed it died.

Gate steps are never claimed. A human gate is approved or rejected directly; a
vote gate is decided by its voters casting on the proposal the engine opened
for it, whose id rides on the step row.

Read verbs here compute effective status and WRITE NOTHING. Only ` + "`next`" + ` and
` + "`step claim`" + ` reap an expired lease.`,
}

// stepErr maps an engine or lease failure to its CLI error code.
//
// It composes the two existing mappings rather than restating either:
// leaseError owns the S2 refusal matrix (which §6.9 reuses verbatim at step
// level), and runErr owns the engine taxonomy. A third mapping here is exactly
// how the step matrix would drift from the issue one.
func stepErr(err error, label string) error {
	if e := leaseError(err, label); e != nil {
		return e
	}
	if errors.Is(err, db.ErrStepNotFound) {
		return cmdErr(fmt.Errorf("%s not found", label), output.ErrNotFound)
	}
	return runErr(err)
}

// stepLabel formats the "step STEP-N" label stepErr's step-id callers pass.
func stepLabel(id int) string {
	return fmt.Sprintf("step %s", model.FormatStepID(id))
}

// stepArg parses a `STEP-N` argument.
func stepArg(arg string) (int, error) {
	id, err := model.ParseStepID(arg)
	if err != nil {
		return 0, cmdErr(fmt.Errorf("invalid step ID: %w", err), output.ErrValidation)
	}
	return id, nil
}

// tokenedStep is the shared prologue of `step heartbeat`, `step complete`, and
// `step fail`: parse the STEP-N argument and read the capability token off
// stdin/env. Callers needing an error label for stepErr pass id, which
// stepErr formats itself.
//
// It returns the writer and connection too, since every caller's next line is
// one or the other of those anyway — leaving three separate `getWriter` /
// `getDB` calls at each site would be its own small duplication.
func tokenedStep(cmd *cobra.Command, args []string) (
	w *output.Writer, conn *sql.DB, id int, token string, err error,
) {
	w = getWriter(cmd)
	conn = getDB(cmd)

	id, err = stepArg(args[0])
	if err != nil {
		return nil, nil, 0, "", err
	}
	token, err = readToken(os.Stdin)
	if err != nil {
		return nil, nil, 0, "", err
	}
	return w, conn, id, token, nil
}

// ---------------------------------------------------------------------------
// step claim
// ---------------------------------------------------------------------------

var stepClaimCmd = &cobra.Command{
	Use:   "claim STEP-N",
	Short: "Claim a step, minting a token and returning its context bundle",
	Long: `Claim a step, taking a lease and minting a capability token.

The response carries the token AND the full context bundle in ONE atomic
call: an unclaimed executor has nothing, a claimed one has everything. The
token is returned exactly once — capture it or claim again.

Claim enforces readiness itself rather than trusting that you ran ` + "`next`" + `.
A step that is not ready is refused with CONFLICT naming the unmet condition,
so a stalled dispatcher can diagnose itself.

Human and vote steps are NOT claimable: they are gates, not work. A claim
against one is refused naming its class.

With --render, the assembled work packet is returned instead of the bundle, in
the same atomic call.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := stepArg(args[0])
		if err != nil {
			return err
		}

		owner, _ := cmd.Flags().GetString("owner")
		if owner == "" {
			return cmdErr(
				fmt.Errorf("--owner is required: it identifies the lease holder"),
				output.ErrValidation)
		}

		var ttlMS int64
		if cmd.Flags().Changed("ttl") {
			ttl, _ := cmd.Flags().GetDuration("ttl")
			if ttl <= 0 {
				return cmdErr(fmt.Errorf("--ttl must be positive, got %s", ttl),
					output.ErrValidation)
			}
			ttlMS = ttl.Milliseconds()
		}

		label := stepLabel(id)
		// The gate-bearing claim: a step's `pre = true` gates run HERE, at
		// claim, with their results in the returned bundle (§11.1, §7.6). A
		// step with no pre-gates takes the identical single-transaction path.
		result, err := engine.NewEngine().ClaimStepWithGates(conn, id, engine.ClaimOptions{
			Owner: owner, TTLOverride: ttlMS, NowMS: model.NowMS(),
		})
		if err != nil {
			return stepErr(err, label)
		}

		render, _ := cmd.Flags().GetBool("render")
		templatePath, _ := cmd.Flags().GetString("template")
		if render {
			// `claim --render` returns the packet instead of the bundle, in the
			// same call (§6.11). The claim has already committed, so this is a
			// pure formatting step over what it returned.
			executor, _ := cmd.Flags().GetString("executor")
			packet, err := engine.RenderStepAs(conn, id, templatePath, executor, model.NowMS())
			if err != nil {
				return stepErr(err, label)
			}
			return emitClaim(w, result, packet)
		}

		return emitClaim(w, result, nil)
	},
}

// claimStepResponse is §11.4's `claim response`, VERBATIM:
// `{ step, token, lease_expires_ms, context }`.
//
// This is the shape a deviation was recorded against at issue level, and it
// is why that deviation CLOSES here (§6.4.1): the subject key is `step` and the
// context bundle is present, exactly as the spec was written.
type claimStepResponse struct {
	Step           string          `json:"step"`
	Token          string          `json:"token"`
	LeaseExpiresMS int64           `json:"lease_expires_ms"`
	Context        *engine.Context `json:"context,omitempty"`
	// Packet replaces Context under --render.
	Packet string `json:"packet,omitempty"`

	attempt    int
	rowVersion int
}

// claimStepResponseV2 adds the CAS version and the attempt, per
// reliability-delta §6.3.
type claimStepResponseV2 struct {
	Step           string          `json:"step"`
	Token          string          `json:"token"`
	LeaseExpiresMS int64           `json:"lease_expires_ms"`
	Context        *engine.Context `json:"context,omitempty"`
	Packet         string          `json:"packet,omitempty"`
	Attempt        int             `json:"attempt"`
	Version        int             `json:"version"`
}

func (c claimStepResponse) VersionedPayload() any {
	return claimStepResponseV2{
		Step: c.Step, Token: c.Token, LeaseExpiresMS: c.LeaseExpiresMS,
		Context: c.Context, Packet: c.Packet,
		Attempt: c.attempt, Version: c.rowVersion,
	}
}

var _ output.Versioned = claimStepResponse{}

func emitClaim(w *output.Writer, result *engine.ClaimResult, packet *engine.RenderResult) error {
	resp := claimStepResponse{
		Step: result.Step, Token: result.Token,
		LeaseExpiresMS: result.LeaseExpiresMS,
		Context:        result.Context,
		attempt:        result.Attempt, rowVersion: result.RowVersion,
	}
	if packet != nil {
		resp.Packet = packet.Packet
		resp.Context = nil
	}

	if w.JSONMode {
		w.Success(resp, "")
		return nil
	}

	// Human mode never puts the token inside the message: a token echoed into a
	// terminal transcript or a CI log is a live capability. It goes on its own
	// line, usable interactively but not copied along with a status line — the
	// same discipline `issue claim` established.
	w.Success(nil, fmt.Sprintf("Claimed %s until %s (attempt %d)",
		result.Step,
		time.UnixMilli(result.LeaseExpiresMS).UTC().Format(time.RFC3339),
		result.Attempt))
	fmt.Fprintln(w.Stdout, result.Token)
	if packet != nil {
		fmt.Fprintln(w.Stdout)
		fmt.Fprint(w.Stdout, packet.Packet)
	}
	return nil
}

// ---------------------------------------------------------------------------
// step heartbeat / complete / fail
// ---------------------------------------------------------------------------

var stepReapCmd = &cobra.Command{
	Use:   "reap STEP-N --reason R",
	Short: "Clear a dead holder's claim without waiting out the lease",
	Long: `Reap a claimed step whose holder you have established is gone.

Liveness is otherwise TTL-only, and a lease sized for healthy long writers
makes a dead holder's claim block its row for the same hours. The engine cannot
probe a process it did not start — but the relay that spawned the executor
can, and this verb is the channel for what it observed.

TOKEN-FREE, like approve and resolve: the authority is repository access plus
the recorded assertion (--reason, required) that the holder is dead. Every
consequence is the expiry reap's own — the same lease-reaped event (with
data.forced and the reason), the same write-class headroom hold awaiting
--ack-reap, the same return of the step to the pool. Reaping a holder that is
in fact alive carries exactly the risks a lease expiry does; assert liveness,
do not assume it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)
		id, err := stepArg(args[0])
		if err != nil {
			return err
		}
		reason, _ := cmd.Flags().GetString("reason")
		if err := engine.ForceReapStep(conn, id, reason, model.NowMS()); err != nil {
			return stepErr(err, stepLabel(id))
		}
		return emitStepState(w, conn, id, "Reaped")
	},
}

var stepHeartbeatCmd = &cobra.Command{
	Use:   "heartbeat STEP-N",
	Short: "Extend the lease on a claimed step",
	Long: `Extend the lease on a step you hold.

Requires the capability token from ` + "`docket step claim`" + `, supplied via
DOCKET_TOKEN or on stdin. Tokens are never accepted in argv.

The attempt counter is untouched — a heartbeat is not a new claim. Note that
heartbeating does NOT extend a step past its class's max_step_duration, which
is measured from the claim: a runaway holder cannot renew forever.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w, conn, id, token, err := tokenedStep(cmd, args)
		if err != nil {
			return err
		}

		lease, err := engine.HeartbeatStep(conn, id, token, model.NowMS())
		if err != nil {
			return stepErr(err, stepLabel(id))
		}

		w.Success(
			heartbeatResponse{
				Issue:          model.FormatStepID(id),
				LeaseExpiresMS: lease.ExpiresMS,
				Attempt:        lease.Attempt,
			},
			fmt.Sprintf("Extended lease on %s until %s", model.FormatStepID(id),
				time.UnixMilli(lease.ExpiresMS).UTC().Format(time.RFC3339)))
		return nil
	},
}

var stepCompleteCmd = &cobra.Command{
	Use:     "complete STEP-N --artifact-file F",
	Aliases: []string{"record"},
	Short:   "Record a step's artifact and run the completion saga",
	Long: `Complete a step: record its artifact and run the saga.

Also available as ` + "`step record`" + `, an identical alias. Some sandboxed
shells (worktree-isolated agents in particular) misparse the bare word
"complete" as the shell ` + "`complete`" + ` builtin and refuse the whole command
line before it reaches docket. ` + "`record`" + ` means exactly the same thing and
avoids the collision — use whichever reads better; both run the identical
saga below.

The saga is artifact+payload validation, then gates one by one, then routing.
THE TOKEN RETIRES WHEN THE ARTIFACT RECORDS — from that commit the step is
engine-owned and resumes under any later invocation. A worker that dies
mid-saga does not strand the step, and completing twice is refused with
AUTH_ERROR rather than recording a duplicate.

--usage records into the run's usage ledger: a JSON object of unit names to
numbers, at most 32 units, each name at most 64 printable-ASCII bytes with no
whitespace, each number finite and >= 0. The units are OPAQUE — docket never
interprets, converts, or routes on them. They sum per unit in ` + "`run report`" + `, and
the one named by ` + "`docket config budget.unit`" + `, if any, is the one the run's cap
counts.

--gap-file (repeatable) records an out-of-scope problem the work surfaced: an
auxiliary artifact of kind ` + "`gap`" + ` beside the step's declared emit, plus a
backlog issue related to the step's own — same transaction, so the residue
cannot evaporate. No workflow declaration is needed; the channel is always
open.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w, conn, id, token, err := tokenedStep(cmd, args)
		if err != nil {
			return err
		}

		artifactPath, _ := cmd.Flags().GetString("artifact-file")
		if artifactPath == "" {
			return cmdErr(
				fmt.Errorf("--artifact-file is required: a step completes by recording what it produced"),
				output.ErrValidation)
		}
		artifact, err := os.ReadFile(artifactPath)
		if err != nil {
			return cmdErr(fmt.Errorf("reading --artifact-file: %w", err), output.ErrNotFound)
		}

		var payload []byte
		if path, _ := cmd.Flags().GetString("payload-file"); path != "" {
			payload, err = os.ReadFile(path)
			if err != nil {
				return cmdErr(fmt.Errorf("reading --payload-file: %w", err), output.ErrNotFound)
			}
		}

		usage, _ := cmd.Flags().GetString("usage")
		metadata, _ := cmd.Flags().GetString("metadata")

		// Gaps are read whole before anything runs, so a bad path refuses
		// without spending the completion.
		var gaps [][]byte
		gapPaths, _ := cmd.Flags().GetStringSlice("gap-file")
		for _, path := range gapPaths {
			gap, err := os.ReadFile(path)
			if err != nil {
				return cmdErr(fmt.Errorf("reading --gap-file: %w", err), output.ErrNotFound)
			}
			gaps = append(gaps, gap)
		}

		// --worktree names the checkout the WORK happened in, for a conductor
		// recording on behalf of an executor elsewhere (G7/G8). Normalized to
		// absolute so a resumed saga in another cwd still resolves it.
		workDir, _ := cmd.Flags().GetString("worktree")
		if workDir != "" {
			if workDir, err = filepath.Abs(workDir); err != nil {
				return cmdErr(fmt.Errorf("resolving --worktree: %w", err), output.ErrValidation)
			}
		}

		var gapIssues []string
		e := engine.NewEngine()
		err = e.CompleteStep(conn, id, engine.CompleteOptions{
			Token: token, WorkDir: workDir, Artifact: artifact, Payload: payload,
			Usage: usage, Metadata: metadata,
			Gaps: gaps, GapIssues: &gapIssues, NowMS: model.NowMS(),
		})
		if err != nil {
			return stepErr(err, stepLabel(id))
		}

		// The recording may have READIED engine-run steps — a vote step whose
		// last dependency this was (its proposal opens now, mid-wave), or an
		// action step (it runs now). `next` cannot drive them while the
		// dispatch is open, so the recording verb is the observing invocation
		// (engine/drive.go) — this is what lets one wave carry a staged
		// `judges -> gate -> reconcile -> report` chain end to end.
		if step, readErr := db.GetStep(conn, id); readErr == nil {
			if err := e.DriveRunLifecycles(conn, step.RunID, model.NowMS()); err != nil {
				return cmdErr(fmt.Errorf(
					"the step recorded, but driving the run's engine-side "+
						"lifecycles failed: %w", err), output.ErrGeneral)
			}
		}

		verb := "Completed"
		if len(gapIssues) > 0 {
			verb = fmt.Sprintf("Completed, gaps filed as %s:",
				strings.Join(gapIssues, ", "))
		}
		return emitStepState(w, conn, id, verb)
	},
}

var stepFailCmd = &cobra.Command{
	Use:   "fail STEP-N",
	Short: "Report a step as failed, consuming an attempt",
	Long: `Report a step as failed.

An attempt is consumed. Below the step's max_attempts it returns to the pool
and is re-offered; once attempts are exhausted it routes per on_fail. A step
that declares no max_attempts retries indefinitely — core ships no default
limit.

--note and --metadata are complementary, not alternatives: --note lands on the
FAILURE EVENT (prose, for a human reading the run's history) and --metadata
lands on the STEP ROW (a JSON object of opaque keys to values, for a query).
--metadata is merged onto the step's own with the same shallow,
last-write-wins rule and the same 16KiB cap ` + "`step complete --metadata`" + ` uses,
and it SURVIVES INTO A RETRY: a failed attempt's bag merges into the step's
row like any other, so the next attempt's completion or failure overlays on
top of it.

Requires the capability token.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w, conn, id, token, err := tokenedStep(cmd, args)
		if err != nil {
			return err
		}
		note, _ := cmd.Flags().GetString("note")
		metadata, _ := cmd.Flags().GetString("metadata")

		e := engine.NewEngine()
		if err := e.FailStep(conn, id, token, note, metadata, model.NowMS()); err != nil {
			return stepErr(err, stepLabel(id))
		}

		// A failure that exhausted attempts ROUTES, and routing can ready
		// engine-run steps mid-wave exactly as a completion can — same hook,
		// same reasoning as `step record`'s (engine/drive.go).
		if step, readErr := db.GetStep(conn, id); readErr == nil {
			if err := e.DriveRunLifecycles(conn, step.RunID, model.NowMS()); err != nil {
				return cmdErr(fmt.Errorf(
					"the failure recorded, but driving the run's engine-side "+
						"lifecycles failed: %w", err), output.ErrGeneral)
			}
		}

		return emitStepState(w, conn, id, "Failed")
	},
}

// ---------------------------------------------------------------------------
// step approve / reject / resolve
// ---------------------------------------------------------------------------

var stepApproveCmd = &cobra.Command{
	Use:   "approve STEP-N",
	Short: "Approve a gate step awaiting a person",
	Long: `Approve a ` + "`type=\"human\"`" + ` gate step, moving it to done.

Also approves a HELD CLUSTER that a vote parked: when an instance decides holds
by tally, a vote that does not pass parks the cluster for an operator, and this
is the verb that answers it. Until it parks, the vote owns the decision and
approve is refused.

No token: a gate is never claimed, so there is no lease to authorize against.
Approving a step that is neither is a VALIDATION_ERROR.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDecide(cmd, args, true, getWriter(cmd))
	},
}

var stepRejectCmd = &cobra.Command{
	Use:   "reject STEP-N",
	Short: "Reject a gate step awaiting a person",
	Long: `Reject a ` + "`type=\"human\"`" + ` gate step, or a held cluster a vote parked.

The step routes per its declared on_fail — which the grammar requires human
gates to declare explicitly, and forbids from being ` + "`waiting-human`" + `: a gate
that parked its own rejections would wait on the resolution of the thing that
just rejected.

Rejecting a held cluster is the ESCALATING answer: the aggregate it gates routes
per its own on_fail, and the cluster is recorded as unresolved.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDecide(cmd, args, false, getWriter(cmd))
	},
}

func runDecide(cmd *cobra.Command, args []string, approve bool, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := stepArg(args[0])
	if err != nil {
		return err
	}
	note, _ := cmd.Flags().GetString("note")
	value := ""
	if approve {
		value, _ = cmd.Flags().GetString("value")
	}

	label := stepLabel(id)
	e := engine.NewEngine()
	if err := e.DecideStepValue(conn, id, approve, note, value, model.NowMS()); err != nil {
		return stepErr(err, label)
	}

	verb := "Approved"
	if !approve {
		verb = "Rejected"
	}
	// DKT-414: deciding a materialized held cluster may have just un-blocked
	// downstream verify/review steps whose packets render from a recorded
	// target sha the shared checkout's HEAD no longer carries. Advisory only —
	// the decision above already committed — and nil for every declared step,
	// so ordinary gate approvals emit exactly what they always have.
	return emitStepStateAdvised(w, conn, id, verb,
		e.HeldResolutionStaleTargets(conn, id, model.NowMS()))
}

var stepResolveCmd = &cobra.Command{
	Use:   "resolve STEP-N --as retry|rerun-gates|skip|abandon-issue|override-pass|fix-round",
	Short: "Resolve a parked step",
	Long: `Resolve a step parked in waiting-human.

  retry          reset the step's attempts and return it to the pool
  rerun-gates    re-run this step's gates WITHOUT re-executing it. For the
                 common case where a gate failed on the environment rather
                 than on the work — a missing trust entry, a broken tool —
                 and you have fixed that out of band (DKT-259)
  skip           mark the step skipped and continue
  abandon-issue  stop this run's work on the issue
  fix-round      authorize ONE more fix loop and enter it, minting a fresh
                 fix+review round. Distinct from retry: retry re-runs the
                 check that reported the problem, this schedules another round
                 of work ON the problem (DKT-237)
  override-pass  pass the step as though its gates had allowed it

` + "`retry`" + ` resets the STEP's attempt budget. That is a different counter from
the issue-level attempt trail, which is monotonic and never reset. It also
RELEASES THE LEASE, so the re-execution goes through a fresh claim and lands on
its own attempt number — without that, both executions shared one number, the
usage ledger could record only the first, and the report counted one attempt for
work that happened twice.

` + "`rerun-gates`" + ` is the cheap, non-destructive alternative when the work
itself was never in question. It rewinds the step to the point just after its
artifact was recorded and re-runs every gate from there; the step never returns
to the pool, no worker re-executes it, and the recorded artifact is untouched.
Reach for it when the gate was wrong; reach for ` + "`retry`" + ` when the work
was. Re-executing to fix a gate is expensive AND destructive — the second run
diffs a tree that already contains the change, so the diff comes back empty and
replaces the real one.

A step parked because its held clusters were REJECTED cannot be retried, and
` + "`retry`" + ` refuses there rather than silently re-parking it. The rejection is
sticky: re-running the aggregate re-reads the same rejected decision and routes
to the same place, so the attempt counter was never what blocked it. The
resolutions that can move such a step are ` + "`override-pass`" + ` (accept it as
passing), ` + "`skip`" + `, and ` + "`abandon-issue`" + `. The rejected verdict stays
addressable — resolving does not rewrite it.

` + "`resolve`" + ` is also how an operator moves a run past a ` + "`type=\"vote\"`" + `
step whose voters have not cast — a run must not be hostage to a quorum that
never arrives.

A HELD CLUSTER parked by a vote that did not pass is answered with
` + "`step approve`" + ` / ` + "`step reject`" + ` instead; ` + "`retry`" + ` refuses there for the
same reason it refuses on a rejected hold, because the same tally would be read
again.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStepResolve(cmd, args, getWriter(cmd))
	},
}

func runStepResolve(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := stepArg(args[0])
	if err != nil {
		return err
	}
	as, _ := cmd.Flags().GetString("as")
	if as == "" {
		return cmdErr(
			fmt.Errorf("--as is required: one of retry, rerun-gates, skip, abandon-issue, override-pass, fix-round"),
			output.ErrValidation)
	}
	note, _ := cmd.Flags().GetString("note")

	label := stepLabel(id)
	e := engine.NewEngine()

	// DKT-470: override-pass records a generic "pass" and never evaluates the
	// step's threshold, so an interposed step it names is skipped no matter
	// what the (unevaluated) payload would have decided. Named BEFORE the
	// resolution commits, so the warning is the blast radius the operator is
	// approving rather than a report of damage already done.
	if as == engine.ResolveOverridePass {
		for _, warning := range engine.OverridePassSkipsInterposedTargets(conn, id) {
			w.Warn("%s", warning)
		}
	}

	if err := e.ResolveStep(conn, id, as, note, model.NowMS()); err != nil {
		return stepErr(err, label)
	}

	// DKT-414: resolving a materialized held cluster (override-pass, skip,
	// abandon-issue, fix-round) may have just un-blocked downstream steps
	// whose packets render from a recorded target sha the shared checkout's
	// HEAD no longer carries. Advisory only — the resolution above already
	// committed — and nil for every non-materialized step.
	return emitStepStateAdvised(w, conn, id, "Resolved",
		e.HeldResolutionStaleTargets(conn, id, model.NowMS()))
}

var stepAnnotateCmd = &cobra.Command{
	Use:   "annotate STEP-N --metadata JSON",
	Short: "Merge opaque metadata onto a finished step's record",
	Long: `Annotate a finished step with facts that became true after it recorded.

The bag is opaque KV, merged onto the step's own metadata with the same
shallow, last-write-wins rule and the same 16KiB cap
` + "`step complete --metadata`" + ` uses. The merge is event-logged with the
annotation verbatim, so what was added survives a later annotation
overwriting the same key.

The motivating case is integration: a relay that rebases or cherry-picks a
recorded commit mints a NEW commit id, and every record citing the original
is unreachable from any ref once the worktree is swept. Annotating the step
with the durable id keeps the run record re-checkable after the fact.

A step that has not finished refuses: a live step's metadata lands with its
record, under its holder's token.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := stepArg(args[0])
		if err != nil {
			return err
		}
		metadata, _ := cmd.Flags().GetString("metadata")

		if _, err := engine.AnnotateStep(conn, id, metadata, model.NowMS()); err != nil {
			return stepErr(err, stepLabel(id))
		}
		return emitStepState(w, conn, id, "Annotated")
	},
}

// heldClusterPayload is `step show`'s shape for a materialized held step: the
// row it always emitted, plus the cluster linkage.
//
// The embedded StepRow marshals inline, and `held_cluster` is `omitempty`, so
// EVERY STEP THAT IS NOT A HELD ROW EMITS EXACTLY THE BYTES IT ALWAYS DID —
// the verb's own promise that "a single id emits the same row object it always
// has, unchanged" holds for all of them. The only steps whose shape moves are
// the ones DKT-239 is about, where the previous shape named neither the
// cluster nor the artifact carrying it.
type heldClusterPayload struct {
	model.StepRow
	HeldCluster *engine.HeldClusterLink `json:"held_cluster,omitempty"`
}

// stepShowPayload wraps a view only when there is something to add, so the
// unchanged case does not even pay for a wrapper type on the wire.
func stepShowPayload(view *engine.StepView) any {
	if view.HeldCluster == nil {
		return view.Row
	}
	return heldClusterPayload{StepRow: view.Row, HeldCluster: view.HeldCluster}
}

// renderHeldCluster is the human half: where the question came from.
//
// `step artifacts` on a held row says "produced no artifacts", which is true
// and useless — a hold produces nothing, and its payload sits on the routing
// step's artifact. This names that artifact, and states the `#N` suffix for
// what it is: a POSITION in the payload, out of a total, not a cluster id
// (DKT-239).
func renderHeldCluster(link *engine.HeldClusterLink) string {
	if link == nil {
		return ""
	}
	return fmt.Sprintf(
		"\nHeld cluster: index %d of %d (position in the payload, not an id)\n"+
			"  payload:  %s\n"+
			"  produced: %s\n",
		link.Cluster, link.Clusters, link.Artifact, link.ProducerStep)
}

// ---------------------------------------------------------------------------
// step show / context / render — READ-ONLY
// ---------------------------------------------------------------------------

var stepShowCmd = &cobra.Command{
	Use:   "show STEP-N...",
	Short: "Show a step at its effective status",
	Long: `Show one or more steps.

READ-ONLY. The status shown is EFFECTIVE — computed at read — so a step whose
lease has lapsed reads as pending even though the row still carries the stale
owner. This verb WRITES NOTHING, including no reap.

A single id emits the same row object it always has, unchanged. Two or more
ids emit a JSON array of that same shape under data — batch reads are a
conductor's most common shape, and a single-id restriction taxes every loop
iteration.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		if len(args) == 1 {
			id, err := stepArg(args[0])
			if err != nil {
				return err
			}
			view, err := engine.LoadStepView(conn, id, model.NowMS())
			if err != nil {
				return stepErr(err, stepLabel(id))
			}

			var message string
			if !w.JSONMode {
				message = render.RenderStepDetail(
					view.Row, view.Routing, view.SagaStage, view.Owner, view.ExpiresMS) +
					render.RenderStepGateSummary(view.Row.Step, gateRows(view.Gates)) +
					renderHeldCluster(view.HeldCluster)
			}
			w.Success(stepShowPayload(view), message)
			return nil
		}

		rows := make([]any, 0, len(args))
		var messages []string
		for _, arg := range args {
			id, err := stepArg(arg)
			if err != nil {
				return err
			}
			view, err := engine.LoadStepView(conn, id, model.NowMS())
			if err != nil {
				return stepErr(err, stepLabel(id))
			}
			rows = append(rows, stepShowPayload(view))
			if !w.JSONMode {
				messages = append(messages, render.RenderStepDetail(
					view.Row, view.Routing, view.SagaStage, view.Owner, view.ExpiresMS)+
					render.RenderStepGateSummary(view.Row.Step, gateRows(view.Gates))+
					renderHeldCluster(view.HeldCluster))
			}
		}

		var message string
		if !w.JSONMode {
			message = strings.Join(messages, "\n\n")
		}
		w.Success(rows, message)
		return nil
	},
}

// stepContextResult is `step context`'s payload. `--meta` rides as a SIBLING
// object, never a mutation of `context`, so the golden bundles are unaffected
// by asking for it.
type stepContextResult struct {
	Context *engine.Context     `json:"context"`
	Meta    *engine.ContextMeta `json:"meta,omitempty"`
}

var stepContextCmd = &cobra.Command{
	Use:   "context STEP-N",
	Short: "Re-emit a step's context bundle, read-only",
	Long: `Re-emit a step's context bundle. No token required.

The bundle is assembled from the run's PINNED and SNAPSHOTTED state only: the
issue as it read at activation, the recorded input artifacts, and the pin list.
It never reads the live issue, never reads the working tree, and never opens a
pinned file. Two calls at the same run state are byte-identical, whatever has
been edited in between.

--meta reports per-section byte counts alongside the bundle, and says whether
the template a render would use is pinned.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := stepArg(args[0])
		if err != nil {
			return err
		}
		bundle, err := engine.ReadContext(conn, id, model.NowMS())
		if err != nil {
			return stepErr(err, stepLabel(id))
		}

		result := stepContextResult{Context: bundle}
		if withMeta, _ := cmd.Flags().GetBool("meta"); withMeta {
			meta := bundle.Meta()
			// The default template ships in the binary, so it is always pinned;
			// a --template path is reported by `step render`.
			meta.TemplatePinned = true
			result.Meta = &meta
		}

		var message string
		if !w.JSONMode {
			message = fmt.Sprintf("Context for %s: %d input(s), %d pin(s)",
				bundle.Step.Step, len(bundle.Inputs), len(bundle.Pins))
		}
		w.Success(result, message)
		return nil
	},
}

var stepRenderCmd = &cobra.Command{
	Use:   "render STEP-N",
	Short: "Render a step's context bundle into a work packet",
	Long: `Render a step's context bundle through a template.

Without --template the shipped default is used; it ships in the binary, so it
cannot drift. With --template F, if the run PINNED that path, the file's bytes
are verified against the pin and a mismatch is refused with CONFLICT naming
both hashes — never a warning and never a silent re-pin. An unpinned template
renders unverified, and that gap is reported rather than assumed.

--executor HINT substitutes {executor} packet entries with a RESOLVED hint
instead of the step's declared one, and names it on the packet's target line.
This is the channel for a dispatcher that resolves the declared hint further
(e.g. by issue label) after the engine offered the step: without it, a
resolved executor renders the declared hint's contract rather than its own.
The resolved contract still verifies against the run's pins; a hint whose
contract was never pinned refuses naming the exact path.

THIS VERB CHECKS ONLY THE PINS IT READS — the template, and the packet files
THIS step declares. It is not a whole-run pin check and a clean exit here does
not mean the run's pin state is sound: a contract another step depends on can
have drifted already, blocking that step, while this one renders and exits 0.
Use ` + "`docket run verify-pins RUN-N`" + ` for the whole-run question.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := stepArg(args[0])
		if err != nil {
			return err
		}
		templatePath, _ := cmd.Flags().GetString("template")
		executor, _ := cmd.Flags().GetString("executor")

		result, err := engine.RenderStepAs(conn, id, templatePath, executor, model.NowMS())
		if err != nil {
			return stepErr(err, stepLabel(id))
		}

		if w.JSONMode {
			w.Success(result, "")
			return nil
		}
		fmt.Fprint(w.Stdout, result.Packet)
		if !result.TemplatePinned {
			w.Warn("template %s is not pinned by this run; the packet is "+
				"reproducible only while the file is unchanged", result.Template)
		}
		return nil
	},
}

// ---------------------------------------------------------------------------
// step artifacts / step artifact
// ---------------------------------------------------------------------------

// stepArtifactsResult is the listing payload. The artifacts ride under a named
// key rather than as a bare array so the envelope can gain a sibling field
// later without changing the shape a caller already parses.
type stepArtifactsResult struct {
	Step      string                `json:"step"`
	Artifacts []engine.StepArtifact `json:"artifacts"`
	// HeldCluster points a held row at the artifact its payload actually
	// lives on (DKT-239). A materialized hold produces nothing itself, so the
	// honest empty listing was also a dead end: `omitempty`, so every other
	// step's payload is unchanged.
	HeldCluster *engine.HeldClusterLink `json:"held_cluster,omitempty"`
}

var stepArtifactsCmd = &cobra.Command{
	Use:   "artifacts STEP-N",
	Short: "List what a step produced, without the bodies",
	Long: `List the artifacts one step produced: reference, kind, size, and hash.

READ-ONLY, and it WRITES NOTHING — no reap, no lease touch.

BODIES ARE NOT INCLUDED. An artifact may be up to 1MiB, so this reports sizes
and lets you choose; ` + "`step artifact ARTIFACT-N`" + ` fetches one in full.

A step that produced nothing lists nothing and exits 0 — many steps
legitimately produce no artifact. A step that does not EXIST is NOT_FOUND,
because "no artifacts" would otherwise read as a fact about a step that is not
there.

A MATERIALIZED HELD STEP produces nothing by construction, and its empty
listing names where its payload does live: the cluster index it decides, out
of how many, and the routing step's artifact carrying it (DKT-239).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := stepArg(args[0])
		if err != nil {
			return err
		}

		artifacts, err := engine.ListStepArtifacts(conn, id)
		if err != nil {
			return stepErr(err, stepLabel(id))
		}

		result := stepArtifactsResult{
			Step: model.FormatStepID(id), Artifacts: artifacts,
		}

		// Only on the empty path, and only then: resolving the hold costs a
		// scheduler load, and a step that produced something has already
		// answered the question this is for.
		if len(artifacts) == 0 {
			if view, verr := engine.LoadStepView(conn, id, model.NowMS()); verr == nil {
				result.HeldCluster = view.HeldCluster
			}
		}

		var message string
		if !w.JSONMode {
			if result.HeldCluster != nil {
				message = render.EmptyState(
					fmt.Sprintf("%s produced no artifacts.", result.Step),
					fmt.Sprintf(
						"It is a held cluster: it decides cluster index %d of %d, "+
							"and that payload lives on %s, produced by %s. "+
							"To read it: docket step artifact %s",
						result.HeldCluster.Cluster, result.HeldCluster.Clusters,
						result.HeldCluster.Artifact, result.HeldCluster.ProducerStep,
						result.HeldCluster.Artifact),
					false)
			} else {
				message = render.RenderStepArtifacts(result.Step, artifactRows(artifacts))
			}
		}
		w.Success(result, message)
		return nil
	},
}

// artifactRows converts the engine's artifacts into the renderer's row shape.
// The conversion lives here so internal/render keeps its independence from
// internal/engine, matching every other renderer in that package.
func artifactRows(artifacts []engine.StepArtifact) []render.StepArtifactRow {
	rows := make([]render.StepArtifactRow, 0, len(artifacts))
	for _, a := range artifacts {
		rows = append(rows, render.StepArtifactRow{
			Artifact:     a.Artifact,
			Kind:         a.Kind,
			Bytes:        a.Bytes,
			PayloadBytes: a.PayloadBytes,
			Stub:         a.Stub,
			SHA256:       a.SHA256,
		})
	}
	return rows
}

var stepArtifactCmd = &cobra.Command{
	Use:   "artifact ARTIFACT-N",
	Short: "Read one artifact in full, body and payload",
	Long: `Read one artifact: its body, its structured payload, and its metadata.

READ-ONLY, and it WRITES NOTHING.

This is how an ACTION step's verdict and an aggregate's held-cluster payload
are read. Both live in the artifacts table, and before this verb existed
neither had a CLI surface at all — reading one meant opening
.docket/issues.db with sqlite by hand.

Take the reference from ` + "`step artifacts STEP-N`" + ` or from the run
report's artifact index; both print the same ` + "`ARTIFACT-N`" + ` form.

--payload prints ONLY the structured payload, so a verdict can be piped
straight into jq without slicing the body out of the envelope first.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := artifactArg(args[0])
		if err != nil {
			return err
		}

		artifact, err := engine.ReadArtifact(conn, id)
		if err != nil {
			return stepErr(err, fmt.Sprintf("artifact ARTIFACT-%d", id))
		}

		// --payload narrows to the structured half. In JSON mode the payload
		// is emitted as the data itself rather than a string field, so `jq`
		// reaches the verdict's keys directly instead of having to parse a
		// nested document out of a string.
		if payloadOnly, _ := cmd.Flags().GetBool("payload"); payloadOnly {
			return emitArtifactPayload(w, artifact)
		}

		var message string
		if !w.JSONMode {
			message = render.RenderArtifact(
				artifact.Artifact, artifact.Kind, artifact.Producer,
				artifact.SHA256, artifact.Body, artifact.Payload)
		}
		w.Success(artifact, message)
		return nil
	},
}

// emitArtifactPayload handles `step artifact --payload`.
//
// An artifact with no payload is an ERROR here rather than empty output: the
// flag asks for a specific thing, and printing nothing would be
// indistinguishable from an empty verdict. Without the flag the same artifact
// reads fine.
func emitArtifactPayload(w *output.Writer, artifact *engine.StepArtifact) error {
	if artifact.Payload == "" {
		return cmdErr(
			fmt.Errorf("%s has no structured payload; omit --payload to read its body",
				artifact.Artifact),
			output.ErrNotFound)
	}

	if w.JSONMode {
		// Emitted as parsed JSON, not as a string, so the payload's own keys
		// are reachable. A payload that does not parse is passed through as a
		// string rather than refused — the artifact is immutable evidence, and
		// a reader must be able to see whatever was actually recorded.
		var parsed any
		if err := json.Unmarshal([]byte(artifact.Payload), &parsed); err != nil {
			w.Success(artifact.Payload, "")
			return nil
		}
		w.Success(parsed, "")
		return nil
	}

	fmt.Fprintln(w.Stdout, artifact.Payload)
	return nil
}

// artifactArg parses an ARTIFACT-N reference, or a bare N.
//
// The bare form is accepted because the run report and this package both print
// the prefixed form, but an operator copying an id out of a JSON field has the
// number alone — and refusing that would be a papercut with no upside.
func artifactArg(arg string) (int, error) {
	raw := strings.TrimPrefix(strings.TrimPrefix(arg, "ARTIFACT-"), "artifact-")
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return 0, cmdErr(
			fmt.Errorf("invalid artifact ID %q: want ARTIFACT-N", arg),
			output.ErrValidation)
	}
	return id, nil
}

// emitStepState re-reads a step after a mutation and reports its new effective
// status, so every mutating verb answers in the same shape.
func emitStepState(w *output.Writer, conn *sql.DB, id int, verb string) error {
	return emitStepStateAdvised(w, conn, id, verb, nil)
}

// resolvedStepPayload is the resolution verbs' JSON shape when the resolution
// un-blocked steps whose packets render from a diverged recorded target
// (DKT-414). The embedded StepRow marshals inline, so the shape is the row
// every resolution has always emitted plus `stale_targets` — the same field,
// carrying the same rows, `dispatch open` rides the advisory on (DKT-193).
type resolvedStepPayload struct {
	model.StepRow
	StaleTargets []engine.StaleTarget `json:"stale_targets"`
}

// emitStepStateAdvised is emitStepState carrying DKT-414's stale-target
// advisory over the two channels every advisory here uses (the DKT-193/DKT-408
// pattern): w.Warn lines on stderr for a human — suppressed in JSON mode by
// design — and a `stale_targets` field beside the row in the JSON envelope.
// A nil advisory emits exactly the bytes emitStepState always has.
func emitStepStateAdvised(
	w *output.Writer, conn *sql.DB, id int, verb string, stale []engine.StaleTarget,
) error {
	view, err := engine.LoadStepView(conn, id, model.NowMS())
	if err != nil {
		return stepErr(err, stepLabel(id))
	}
	message := fmt.Sprintf("%s %s (%s)", verb, view.Row.Step, view.Row.Status)
	if len(stale) == 0 {
		w.Success(view.Row, message)
		return nil
	}
	warnStaleTargets(w, stale)
	w.Success(resolvedStepPayload{StepRow: view.Row, StaleTargets: stale}, message)
	return nil
}

func init() {
	stepClaimCmd.Flags().String("owner", "", "Identity of the lease holder (required)")
	stepClaimCmd.Flags().Duration("ttl", 0, "Lease duration; defaults to the step's configured TTL")
	stepClaimCmd.Flags().Bool("render", false, "Return the rendered work packet instead of the bundle")
	stepClaimCmd.Flags().String("template", "", "Template file for --render")
	stepClaimCmd.Flags().String("executor", "",
		"Resolved executor hint for --render's {executor} substitution "+
			"(default: the step's declared hint)")

	stepCompleteCmd.Flags().String("artifact-file", "", "File holding the artifact body (required)")
	stepCompleteCmd.Flags().String("payload-file", "", "File holding the JSON payload")
	stepCompleteCmd.Flags().StringSlice("gap-file", nil,
		"File holding one out-of-scope problem (repeatable); each records a `gap` "+
			"artifact beside the declared emit and files a related backlog issue")
	stepCompleteCmd.Flags().String("usage", "", `Opaque usage record: {"unit": n, ...}, recorded in the run's ledger`)
	stepCompleteCmd.Flags().String("metadata", "", "Opaque metadata JSON")
	stepCompleteCmd.Flags().String("worktree", "",
		"Checkout the work happened in; the recorded diff is computed there "+
			"(default: the invoking checkout)")

	stepFailCmd.Flags().String("note", "", "Why the step failed")
	stepFailCmd.Flags().String("metadata", "", "Opaque metadata JSON, merged onto the step's own")
	stepApproveCmd.Flags().String("note", "", "Why the gate was approved")
	stepApproveCmd.Flags().String("value", "",
		"Corrected value for a held cluster's aggregated field; must be a member "+
			"of the pinned schema's declared enum, and is never parsed from --note")
	stepRejectCmd.Flags().String("note", "", "Why the gate was rejected")

	stepResolveCmd.Flags().String("as", "",
		"retry | skip | abandon-issue | override-pass | fix-round")
	stepResolveCmd.Flags().String("note", "", "Why the step was resolved this way")

	stepContextCmd.Flags().Bool("meta", false, "Report per-section byte counts alongside the bundle")
	stepRenderCmd.Flags().String("template", "", "Template file; defaults to the shipped one")
	stepRenderCmd.Flags().String("executor", "",
		"Resolved executor hint for {executor} substitution "+
			"(default: the step's declared hint)")

	stepArtifactCmd.Flags().Bool("payload", false,
		"Print only the structured payload, for piping into jq")

	stepReapCmd.Flags().String("reason", "",
		"Why the holder is being declared dead (required)")

	stepListCmd.Flags().String("run", "", "Run whose steps to list")
	stepListCmd.Flags().String("issue", "",
		"Issue whose steps to list, across every run that holds one "+
			"(combine with --run to stay inside one run)")

	stepAnnotateCmd.Flags().String("metadata", "",
		"Opaque metadata JSON, merged onto the finished step's own (required)")

	for _, sub := range []*cobra.Command{
		stepClaimCmd, stepHeartbeatCmd, stepCompleteCmd, stepFailCmd,
		stepApproveCmd, stepRejectCmd, stepResolveCmd, stepReapCmd,
		stepAnnotateCmd,
		stepShowCmd, stepListCmd, stepContextCmd, stepRenderCmd,
		stepArtifactsCmd, stepArtifactCmd,
	} {
		stepCmd.AddCommand(sub)
	}
	rootCmd.AddCommand(stepCmd)
}

// stepListResult implements output.Collection for the v2 envelope. The
// listing is never truncated — a run's step count is bounded by its own
// expansion — so total and item count always agree.
type stepListResult struct {
	Steps []engine.StepListEntry `json:"steps"`
	Total int                    `json:"total"`
}

func (r stepListResult) CollectionItems() any      { return r.Steps }
func (r stepListResult) CollectionTotal() int      { return r.Total }
func (r stepListResult) CollectionTruncated() bool { return false }

var stepListCmd = &cobra.Command{
	Use:   "list (--run RUN-N | --issue ISSUE-N)",
	Short: "List a run's or an issue's steps with effective status and cost",
	Long: `List steps with id, run, instance, issue, kind, effective status,
attempt, and expected cost, in (issue, creation) order.

Scope it by --run (every step of that run), by --issue (that issue's steps
across every run that holds one), or by both (that issue's steps inside that
run). At least one is required.

READ-ONLY, and the run-scoped enumeration a budget projection needs — done
spend plus pending expected costs against the cap. Step ids are one
store-wide sequence shared across projects, so id arithmetic cannot
enumerate a run; this verb is the supported way (DKT-54).

Status is EFFECTIVE (§6.2), computed at read exactly as ` + "`step show`" + `
computes it: a lapsed lease reads as pending, a pending step whose
predicate holds reads as ready. Nothing is written, including no reap.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runStepList)
	},
}

func runStepList(cmd *cobra.Command, _ []string, w *output.Writer) error {
	conn := getDB(cmd)

	runFlag, _ := cmd.Flags().GetString("run")
	issueFlag, _ := cmd.Flags().GetString("issue")
	if runFlag == "" && issueFlag == "" {
		return cmdErr(
			fmt.Errorf("--run RUN-N or --issue ISSUE-N is required"),
			output.ErrValidation)
	}

	var (
		rows  []engine.StepListEntry
		scope string
		err   error
	)
	switch {
	case runFlag == "":
		issueID, perr := model.ParseID(issueFlag)
		if perr != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", perr), output.ErrValidation)
		}
		if _, gerr := getIssueOrErr(conn, issueID, "issue"); gerr != nil {
			return gerr
		}
		rows, err = engine.IssueStepList(conn, issueID, model.NowMS())
		scope = model.FormatID(issueID)
	default:
		runID, perr := model.ParseRunID(runFlag)
		if perr != nil {
			return cmdErr(fmt.Errorf("invalid run ID: %w", perr), output.ErrValidation)
		}
		rows, err = engine.RunStepList(conn, runID, model.NowMS())
		scope = model.FormatRunID(runID)
		if err == nil && issueFlag != "" {
			issueID, perr := model.ParseID(issueFlag)
			if perr != nil {
				return cmdErr(fmt.Errorf("invalid issue ID: %w", perr), output.ErrValidation)
			}
			if _, gerr := getIssueOrErr(conn, issueID, "issue"); gerr != nil {
				return gerr
			}
			issue := model.FormatID(issueID)
			kept := make([]engine.StepListEntry, 0, len(rows))
			for _, row := range rows {
				if row.Issue == issue {
					kept = append(kept, row)
				}
			}
			rows = kept
			scope = issue + " in " + model.FormatRunID(runID)
		}
	}
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		var b strings.Builder
		fmt.Fprintf(&b, "%d step(s) in %s\n", len(rows), scope)
		for _, row := range rows {
			fmt.Fprintf(&b, "%-9s %-7s %-8s %-24s %-10s %-13s %2d  %6.2f\n",
				row.Step, row.Run, row.Issue, exec.Render(row.Instance), row.Kind,
				row.Status, row.Attempt, row.ExpectedCost)
		}
		message = strings.TrimRight(b.String(), "\n")
	}
	w.Success(stepListResult{Steps: rows, Total: len(rows)}, message)
	return nil
}
