package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// `docket guard`.
//
// Guards are HOOK PREDICATES, not ordinary verbs, and their contract is
// different: EXIT 0 ALLOW / EXIT 2 DENY WITH REASON, independent of the
// error taxonomy.
//
// Exit 2 collides numerically with NOT_FOUND's exit 2, and that is INTENTIONAL
// AND SPECIFIED. A guard's caller tests a boolean — `docket guard stop ||
// exit` — rather than mapping a code, so the contract is two outcomes, not a
// taxonomy. This is stated here and in the verbs' help because a reader
// encountering exit 2 will otherwise read it as a taxonomy violation.
//
// THE ONE PLACE THE COLLISION HAD TO BE BROKEN is "no database up-tree".
// That is the taxonomy's NOT_FOUND, and routing it through exit 2
// made it indistinguishable from a denial — so every hook wired on a guard
// denied in every non-docket repo. A guard reached with no engine present now
// ALLOWS (exit 0) and says why; see root.go's PersistentPreRunE. The reason is
// that the guard's question is "does engine state forbid this?", and where
// there is no engine state, nothing forbids it.
//
// Everything below still denies through exit 2. The break is scoped to the
// absence of a database, not to any verdict a guard actually computed.
//
// ENGINE STATE THAT EXISTS BUT CANNOT BE CONSULTED IS A DENIAL: a store that
// does not open, or a project linked to a daemon. Execute renders it with code
// UNAVAILABLE and exit 2, because any other exit lets a hook proceed.

var guardCmd = &cobra.Command{
	Use:   "guard",
	Short: "Deterministic predicates over engine state, for hooks",
	Long: `Guards answer a yes/no question about engine state.

They are hook predicates, not ordinary verbs: EXIT 0 means allow, EXIT 2 means
deny and a reason is printed. That contract is independent of the error
taxonomy other verbs use, so exit 2 here means "denied", not "not found".

With no database and no daemon link for this project, a guard allows (exit 0)
as not applicable. When engine state exists but cannot be consulted (the
database does not open, or the project is linked to a daemon), a guard denies
(exit 2) with code UNAVAILABLE.

The reason goes to stderr in human mode and into the JSON envelope's error
field under --json.`,
}

var guardStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Allow when no work is pending outside waiting-human",
	Long: `Allow (exit 0) when no active run has a step in pending, ready,
claimed, running, or gated. Deny (exit 2) otherwise, naming what is still
working.

A run nothing has ever happened to does NOT block: one that was activated but
never dispatched, with no step ever out of pending. Its ready steps have not
been handed to anything, so there is nothing for a stop to interrupt. The
exemption ends at the first ` + "`dispatch open`" + `, or the first step to
leave pending by any other route.

A step parked in waiting-human does NOT block a stop, and neither does a
PENDING step of a run that is itself waiting-human (paused): the guard asks
whether the machine is done working, and a run waiting on a person is not
something a stop interferes with. A claimed or running step still blocks even
while its run is paused, since ` + "`run pause`" + ` honors in-flight
completes rather than killing them.

A vote step whose proposal is still OPEN does not block either, and neither
does work waiting behind it: its panel decides out-of-session, and yielding
the turn is how a session waits for a panel. Once the proposal is decided the
step is dispatchable again and blocks until the next engine invocation routes
it.

"Active run" means the CURRENT PROJECT's active runs: under the shared
per-user store every project's runs live in one database, and a stop in one
repository does not interfere with another repository's work. --all-projects
asks the store-wide question instead.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		allProjects, _ := cmd.Flags().GetBool("all-projects")
		decision, err := verb.GuardStop(cmd.Context(), verbEnv(cmd), verb.GuardStopInput{
			AllProjects: allProjects,
		})
		if err != nil {
			return verbErr(err)
		}
		return emitGuard(cmd, decision)
	},
}

var guardGateCmd = &cobra.Command{
	Use:   "gate --step NAME [--run RUN-N]",
	Short: "Allow when a named gate has passed",
	Long: `Allow (exit 0) when a PASSED ` + "`type=\"human\"`" + ` or ` + "`type=\"vote\"`" + ` step
of the given name exists for the named run, or with no --run for any active run
of the current project. Deny (exit 2) otherwise, saying whether the gate is
undecided or absent.

BOTH GATE KINDS ANSWER, so converting a gate from one person's approval to a
vote does not silently stop every hook that checks it. "Passed" means the step
reached done by a DECISION — an approval on a human gate, a tallied approval on
a vote gate. A step that reached done some other way did not receive one, and
accepting it would let an override stand in for a decision nobody made. A vote
still being cast reads as undecided and denies.

WHICH RUNS ANSWER is the caller's choice. With --run RUN-N only that run's gate
answers: an approval is a decision about one run's change, so another run's
approval says nothing about it. Use this form when the caller knows its run —
a claimed step's rendered packet carries its step id, and ` + "`step show STEP-N`" + ` names the
run. The named run is honored regardless of project; a run that does not exist
is a NOT_FOUND error rather than a verdict; a run that has ended denies.

Without --run the gate is looked up across EVERY active run of the CURRENT
PROJECT, and the first approved one answers. That reading is cross-run: one
run's approval opens the gate for every caller in the project until that run
finishes, including a caller working under a second run whose own gate is
still undecided. It exists for callers with no run context, such as an operator
session's hook; a hook that has a run should pass it. An approval is a decision
about one project's gate, so a same-named gate elsewhere in the store never
answers; --all-projects widens the search to the whole store.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		stepName, _ := cmd.Flags().GetString("step")
		runRef, _ := cmd.Flags().GetString("run")
		allProjects, _ := cmd.Flags().GetBool("all-projects")
		decision, err := verb.GuardGate(cmd.Context(), verbEnv(cmd), verb.GuardGateInput{
			Step: stepName, Run: runRef, AllProjects: allProjects,
		})
		if err != nil {
			return verbErr(err)
		}
		return emitGuard(cmd, decision)
	},
}

var guardRecordCmd = &cobra.Command{
	Use:   "record [--run RUN-N]",
	Short: "Allow when no unreconciled dispatch exists",
	Long: `Allow (exit 0) when the run has no open dispatch and no discrepancy.
Deny (exit 2) otherwise, naming which and how to resolve it.

Wire this before letting a worker record its result. An unreconciled batch means
the engine's picture of what is running is already wrong, and recording into that
picture is how drift becomes durable.

Without --run the guard answers over every non-terminal run OF THE CURRENT
PROJECT, denying if any is unreconciled — so a hook wired once keeps working
as runs come and go without answering for other repositories' runs.
--all-projects widens to the store; an explicit --run is honored regardless
of project, because naming a run is naming intent.

It writes nothing, and it neither reaps nor abandons anything: a guard that
mutated scheduling state would make a hook's mere presence change a run.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		runRef, _ := cmd.Flags().GetString("run")
		allProjects, _ := cmd.Flags().GetBool("all-projects")
		decision, err := verb.GuardRecord(cmd.Context(), verbEnv(cmd), verb.GuardRecordInput{
			Run: runRef, AllProjects: allProjects,
		})
		if err != nil {
			return verbErr(err)
		}
		return emitGuard(cmd, decision)
	},
}

var guardSpawnCmd = &cobra.Command{
	Use:   "spawn (--run RUN-N | --active) [--rows FILE] [--ack-reap SEQ] [--deciding-vote PROPOSAL-N | --usage-join]",
	Short: "Allow when the proposed batch matches and no reap is unacknowledged",
	Long: `Allow (exit 0) when BOTH hold: the proposed rows byte-match the open
dispatch, and no write-class reap is unacknowledged. Deny (exit 2) otherwise.

--rows FILE (or - for stdin) is the JSON array of rows about to be spawned. With
no open dispatch and no --rows, the row half is vacuously satisfied and the reap
half still answers — so a relay that batches its own way still gets the check.
With --rows and no open dispatch it is a denial: the relay believes it is
spawning a batch the engine never issued.

--active (DKT-1287) checks EVERY active run of the current project instead of
one: it answers the reap half alone, over each non-terminal run in turn, oldest
first, and denies on the first that would, naming it. It exists because a hook
resolving "the active run" as ` + "`runs[0]`" + ` from ` + "`run status`" + ` leaves a second
concurrent run's reap hold unasked; --rows and --ack-reap are each an act about
ONE run's manifest or ledger and stay on the --run path, while --deciding-vote
resolves its run from the proposal and rides along.

--ack-reap SEQ (repeatable) acknowledges a reaped write-class lease by the seq of
its lease-reaped event, and is processed BEFORE the predicate — so one command
both acknowledges and answers. Acknowledging confirms YOU have established the
old writer is gone; the engine cannot check a process it did not start.

--deciding-vote PROPOSAL-N admits a batch spawned to DECIDE an open reap hold
(DKT-236). The sanctioned way to decide whether to acknowledge a reap is a judge
panel, and the hold denied the panel's own spawn — the exact state the panel
exists to decide. Nothing could move, so what happened instead was a ~10h
operator round trip followed by two self-passed --ack-reap calls with no panel
at all.

The carve-out is narrow. The proposal must EXIST and be OPEN — a decided one
authorizes nothing, ever again — and it relaxes the REAP half ONLY: row drift
is a different fact about a different actor, and no vote makes it acceptable.
Every use is event-logged as spawn-admitted with the proposal and the hold it
was admitted over, because a spawn let past a hold must not read like a spawn
nothing was holding.

With --active the proposal names the run itself: the carve-out admits the hold
of every active run holding a step for one of the proposal's linked issues
(` + "`vote link`" + `), and a run the proposal does not serve denies as it would without
the flag, saying so.

--usage-join admits a launch that CLAIMS NO STEP, such as a read-only usage
join over finished transcripts, past a reap hold: it consumes no write-class
headroom, so the hold has nothing to protect from it. It relaxes the REAP half
ONLY, never the row comparison, and is refused with --rows: a launch proposing
rows is dispatch-bearing, and is denied under the hold as it would be without
the flag. It is mutually exclusive with --deciding-vote, applies on both --run
and --active, and every use is event-logged as spawn-admitted with carve_out
usage-join. The engine cannot see what the launch does; naming it a usage join
is the caller's claim, and the event makes every such claim findable.

It writes nothing except that acknowledgment and, when a carve-out is used,
that audit event.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		in := verb.GuardSpawnInput{}
		in.Run, _ = cmd.Flags().GetString("run")
		in.Active, _ = cmd.Flags().GetBool("active")
		in.AckReap, _ = cmd.Flags().GetInt64Slice("ack-reap")
		in.DecidingVote, _ = cmd.Flags().GetString("deciding-vote")
		in.UsageJoin, _ = cmd.Flags().GetBool("usage-join")
		in.AllProjects, _ = cmd.Flags().GetBool("all-projects")
		if path, _ := cmd.Flags().GetString("rows"); path != "" {
			in.Rows = func() ([]byte, error) { return readProposedRows(cmd) }
		}

		decision, err := verb.GuardSpawn(cmd.Context(), verbEnv(cmd), in)
		if err != nil {
			return verbErr(err)
		}
		return emitGuard(cmd, decision)
	},
}

// readProposedRows reads `--rows FILE`, or stdin for "-".
//
// It returns nil when the flag is absent, which the engine distinguishes from an
// empty array: the first asks only the reap half, the second proposes
// spawning nothing and is compared against the manifest like any other batch.
func readProposedRows(cmd *cobra.Command) ([]byte, error) {
	path, _ := cmd.Flags().GetString("rows")
	if path == "" {
		return nil, nil
	}
	if path == "-" {
		raw, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, cmdErr(
				fmt.Errorf("reading proposed rows from stdin: %w", err), output.ErrGeneral)
		}
		return raw, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, cmdErr(
			fmt.Errorf("reading proposed rows from %s: %w", path, err), output.ErrNotFound)
	}
	return raw, nil
}

// guardResult is the allow payload. A denial does not use it: it goes out
// through the error channel so the JSON envelope carries the reason in `error`,
// which is where a denial belongs.
type guardResult struct {
	Allowed bool `json:"allowed"`
}

func emitGuard(cmd *cobra.Command, verdict verb.GuardDecision) error {
	w := getWriter(cmd)

	if verdict.Allowed {
		w.Success(guardResult{Allowed: true}, "allowed")
		return nil
	}

	// The denial rides the error channel, which puts the reason on stderr in
	// human mode and into the envelope's `error` under --json, and exits 2 via
	// the code above.
	return cmdErr(fmt.Errorf("%s", verdict.Reason), output.ErrNotFound)
}

func init() {
	guardGateCmd.Flags().String("step", "", "Name of the gate step to check")

	// Every store-wide-read guard gets the same opt-out of project scoping,
	// spelled the same as `events list`'s flag so one vocabulary covers both.
	for _, c := range []*cobra.Command{guardStopCmd, guardGateCmd, guardRecordCmd, guardSpawnCmd} {
		c.Flags().Bool("all-projects", false,
			"Answer over every project's runs, not just the current project's")
	}

	// `gate`'s and `record`'s --run are OPTIONAL and `spawn`'s is
	// required unless --active is given (DKT-1287), so they are registered
	// separately rather than in a shared loop — a loop would hide exactly the
	// difference that matters.
	guardGateCmd.Flags().String(
		"run", "", "Answer for this run's gate alone (default: every active run of the project)")
	guardRecordCmd.Flags().String(
		"run", "", "The run to check (default: every non-terminal run)")
	guardSpawnCmd.Flags().String(
		"run", "", "The run whose batch is being spawned (required unless --active)")
	guardSpawnCmd.Flags().Bool("active", false,
		"Check the reap half over every active run of the project, denying on "+
			"the first (oldest) that would deny; mutually exclusive with --run")
	guardSpawnCmd.Flags().String(
		"rows", "", "File holding the JSON array of rows about to be spawned (- for stdin)")
	guardSpawnCmd.Flags().String("deciding-vote", "",
		"Admit this batch past a reap hold because it exists to decide the "+
			"named OPEN proposal (PROPOSAL-N); event-logged")
	guardSpawnCmd.Flags().Bool("usage-join", false,
		"Admit a launch that claims no step past a reap hold (reap half only; "+
			"refused with --rows); event-logged")
	guardSpawnCmd.Flags().Int64Slice("ack-reap", nil,
		"Acknowledge a write-class reap by its `lease-reaped` event seq (repeatable)")

	guardCmd.AddCommand(guardStopCmd)
	guardCmd.AddCommand(guardGateCmd)
	guardCmd.AddCommand(guardRecordCmd)
	guardCmd.AddCommand(guardSpawnCmd)
	rootCmd.AddCommand(guardCmd)
}
