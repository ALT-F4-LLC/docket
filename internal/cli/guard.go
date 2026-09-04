package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket guard` — TDD §6.12.
//
// Guards are HOOK PREDICATES, not ordinary verbs, and their contract is
// different: EXIT 0 ALLOW / EXIT 2 DENY WITH REASON (§2), independent of the
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

var guardCmd = &cobra.Command{
	Use:   "guard",
	Short: "Deterministic predicates over engine state, for hooks",
	Long: `Guards answer a yes/no question about engine state.

They are hook predicates, not ordinary verbs: EXIT 0 means allow, EXIT 2 means
deny and a reason is printed. That contract is independent of the error
taxonomy other verbs use, so exit 2 here means "denied", not "not found".

The reason goes to stderr in human mode and into the JSON envelope's error
field under --json.`,
}

var guardStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Allow when no work is pending outside waiting-human",
	Long: `Allow (exit 0) when no active run has a step in pending, ready,
claimed, running, or gated. Deny (exit 2) otherwise, naming what is still
working.

A step parked in waiting-human does NOT block a stop: the guard asks whether
the machine is done working, and a run waiting on a person is not something a
stop interferes with.

A vote step whose proposal is still OPEN does not block either, and neither
does work waiting behind it: its panel decides out-of-session, and yielding
the turn is how a session waits for a panel. Once the proposal is decided the
step is dispatchable again and blocks until the next engine invocation routes
it.

"Active run" means the CURRENT PROJECT's active runs: under the shared
per-user store every project's runs live in one database, and a stop in one
repository does not interfere with another repository's work. --all-projects
asks the store-wide question instead.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		verdict, err := engine.GuardStop(getDB(cmd), guardProjectScope(cmd), model.NowMS())
		if err != nil {
			return runErr(err)
		}
		return emitGuard(cmd, verdict)
	},
}

var guardGateCmd = &cobra.Command{
	Use:   "gate --step NAME",
	Short: "Allow when a named gate has passed",
	Long: `Allow (exit 0) when a PASSED ` + "`type=\"human\"`" + ` or ` + "`type=\"vote\"`" + ` step
of the given name exists for an active run. Deny (exit 2) otherwise, saying
whether the gate is undecided or absent.

BOTH GATE KINDS ANSWER, so converting a gate from one person's approval to a
vote does not silently stop every hook that checks it. "Passed" means the step
reached done by a DECISION — an approval on a human gate, a tallied approval on
a vote gate. A step that reached done some other way did not receive one, and
accepting it would let an override stand in for a decision nobody made. A vote
still being cast reads as undecided and denies.

The gate is looked up in the CURRENT PROJECT's active runs — an approval is a
decision about one project's gate, and a same-named gate elsewhere in the
store must not answer for it. --all-projects widens the search.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		stepName, _ := cmd.Flags().GetString("step")
		verdict, err := engine.GuardGate(getDB(cmd), stepName, guardProjectScope(cmd))
		if err != nil {
			return runErr(err)
		}
		return emitGuard(cmd, verdict)
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
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		runID, err := optionalRunFlag(cmd)
		if err != nil {
			return err
		}
		verdict, err := engine.GuardRecord(
			getDB(cmd), runID, guardProjectScope(cmd), model.NowMS())
		if err != nil {
			return runErr(err)
		}
		return emitGuard(cmd, verdict)
	},
}

var guardSpawnCmd = &cobra.Command{
	Use:   "spawn (--run RUN-N | --active) [--rows FILE] [--ack-reap SEQ] [--deciding-vote PROPOSAL-N]",
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
resolving "the active run" as ` + "`runs[0]`" + ` from ` + "`run status --active`" + ` leaves a second
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

It writes nothing except that acknowledgment and, when the carve-out is used,
that audit event.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		active, _ := cmd.Flags().GetBool("active")
		if active {
			return runGuardSpawnActive(cmd)
		}

		runID, err := requiredRunFlag(cmd)
		if err != nil {
			return err
		}
		seqs, err := ackSeqs(cmd)
		if err != nil {
			return err
		}
		rows, err := readProposedRows(cmd)
		if err != nil {
			return err
		}

		decidingVote, err := decidingVoteFlag(cmd)
		if err != nil {
			return err
		}

		verdict, err := engine.NewEngine().GuardSpawn(
			getDB(cmd), runID, engine.SpawnOptions{
				Rows: rows, AckSeqs: seqs, DecidingVote: decidingVote,
				NowMS: model.NowMS(),
			})
		if err != nil {
			return runErr(err)
		}
		return emitGuard(cmd, verdict)
	},
}

// runGuardSpawnActive is `guard spawn --active` (DKT-1287). --run, --rows and
// --ack-reap are each an act about ONE run's manifest or ledger, and --active
// answers a different question — every active run's reap half at once — so it
// refuses to guess which run they meant instead of silently narrowing back to
// one. --deciding-vote needs no guess: the proposal resolves its own run.
func runGuardSpawnActive(cmd *cobra.Command) error {
	if ref, _ := cmd.Flags().GetString("run"); ref != "" {
		return cmdErr(fmt.Errorf(
			"--active and --run are mutually exclusive: --active already means "+
				"every active run"), output.ErrValidation)
	}
	if path, _ := cmd.Flags().GetString("rows"); path != "" {
		return cmdErr(fmt.Errorf(
			"--active does not take --rows: a proposed batch is one run's "+
				"manifest, so name that run with --run instead"), output.ErrValidation)
	}
	if seqs, _ := cmd.Flags().GetInt64Slice("ack-reap"); len(seqs) > 0 {
		return cmdErr(fmt.Errorf(
			"--active does not take --ack-reap: an acknowledgment is one run's "+
				"ledger entry, so name that run with --run instead"), output.ErrValidation)
	}
	decidingVote, err := decidingVoteFlag(cmd)
	if err != nil {
		return err
	}

	verdict, err := engine.GuardSpawnActive(
		getDB(cmd), guardProjectScope(cmd), decidingVote, model.NowMS())
	if err != nil {
		return runErr(err)
	}
	return emitGuard(cmd, verdict)
}

// decidingVoteFlag resolves `--deciding-vote` (DKT-236). Absent is 0, meaning
// no carve-out is claimed; a malformed reference is a VALIDATION_ERROR rather
// than a silent 0, because a hook that typed the id wrong would otherwise be
// denied with a message that never mentions its own typo.
func decidingVoteFlag(cmd *cobra.Command) (int, error) {
	ref, _ := cmd.Flags().GetString("deciding-vote")
	if ref == "" {
		return 0, nil
	}
	id, err := model.ParseProposalID(ref)
	if err != nil {
		return 0, cmdErr(
			fmt.Errorf("invalid --deciding-vote %q: %w", ref, err),
			output.ErrValidation)
	}
	return id, nil
}

// guardProjectScope resolves which project a guard answers for: the invoking
// cwd's project by default, or 0 (every project in the store) under
// --all-projects.
//
// Scoped-by-default is the fix for the shared store widening every guard from
// repo-wide to machine-wide (a Stop hook in one repository was denied over
// another repository's run, observed 2026-08-11): the guard's question is
// "does engine state forbid THIS session's action", and another project's
// engine state does not.
func guardProjectScope(cmd *cobra.Command) int {
	if all, _ := cmd.Flags().GetBool("all-projects"); all {
		return 0
	}
	return getProjectID(cmd)
}

// optionalRunFlag resolves `guard record`'s OPTIONAL `--run` (G4).
//
// An absent flag is run 0, meaning "every non-terminal run" — matching `guard
// stop`'s all-active-runs shape. A present but malformed one is a
// VALIDATION_ERROR, because a hook that typed the reference wrong should learn
// that rather than silently widening to every run.
func optionalRunFlag(cmd *cobra.Command) (int, error) {
	ref, _ := cmd.Flags().GetString("run")
	if ref == "" {
		return 0, nil
	}
	return parseRunFlag(ref)
}

// requiredRunFlag resolves `guard spawn`'s REQUIRED `--run`.
//
// It is required here and optional for `record` because the two questions have
// different scopes: "is anything unreconciled" is answerable over every run,
// while "does this batch match the manifest" is about ONE run's dispatch and has
// no all-runs reading.
func requiredRunFlag(cmd *cobra.Command) (int, error) {
	ref, _ := cmd.Flags().GetString("run")
	if ref == "" {
		return 0, cmdErr(
			fmt.Errorf("--run is required: name the run whose batch is being spawned"),
			output.ErrValidation)
	}
	return parseRunFlag(ref)
}

// readProposedRows reads `--rows FILE`, or stdin for "-" (G6).
//
// It returns nil when the flag is absent, which the engine distinguishes from an
// empty array: the first asks only the reap half (G7), the second proposes
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
// which is where §6.12 puts it.
type guardResult struct {
	Allowed bool `json:"allowed"`
}

func emitGuard(cmd *cobra.Command, verdict *engine.GuardVerdict) error {
	w := getWriter(cmd)

	if verdict.Allowed {
		w.Success(guardResult{Allowed: true}, "allowed")
		return nil
	}

	// The denial rides the error channel, which puts the reason on stderr in
	// human mode and into the envelope's `error` under --json — exactly where
	// §6.12 puts it — and exits 2 via the code above.
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

	// `record`'s --run is OPTIONAL (G4) and `spawn`'s is required unless
	// --active is given (DKT-1287), so they are registered separately rather
	// than in a shared loop — a loop would hide exactly the difference that
	// matters.
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
	guardSpawnCmd.Flags().Int64Slice("ack-reap", nil,
		"Acknowledge a write-class reap by its `lease-reaped` event seq (repeatable)")

	guardCmd.AddCommand(guardStopCmd)
	guardCmd.AddCommand(guardGateCmd)
	guardCmd.AddCommand(guardRecordCmd)
	guardCmd.AddCommand(guardSpawnCmd)
	rootCmd.AddCommand(guardCmd)
}
