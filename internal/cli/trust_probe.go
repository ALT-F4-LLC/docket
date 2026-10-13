package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/spf13/cobra"
)

// `docket trust probe` (DKT-1283) — run every trust-roster gate ONCE against a
// throwaway worktree of clean HEAD, so a gate that fails on clean HEAD is
// surfaced once instead of parking a step per issue. See trust_probe.go in
// internal/engine for the mechanism and what it replaces
// (dotfiles' src/user/claude_code/workflows/gate-probe.js).
//
// It carries `skipDB`, like every other `trust` verb: the roster comes from
// the user-level trust store and the git repository, not from a docket
// database — a repo need not even be activated for its gates to be probed.

var trustProbeCmd = newTrustProbeCmd()

func newTrustProbeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Run every trusted gate once against a throwaway worktree of clean HEAD",
		Long: `Run every non-action entry of the trust roster once, in one throwaway
detached worktree of the repository's current HEAD, and report a pass/fail row
per gate.

A gate that fails here fails on CLEAN HEAD, so no step's changes caused it —
run this once before dispatching a run, rather than rediscovering the same
failure as a parked step per issue.

An entry a workflow declares via 'action = "<name>"' is an engine ACTION, fed
a JSON bundle on stdin at record time; this verb cannot honor that contract and
skips such entries by name instead of failing them.

The worktree is removed unconditionally — on success, on failure, and on
interrupt — and nothing here executes anything that is not already a trusted
command.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"skipDB": ""},
		RunE:        runTrustProbe,
	}
	cmd.Flags().String("run", "",
		"a run label to include in the report; does not narrow the roster")
	return cmd
}

// trustProbeResult is `trust probe`'s wire shape: engine.TrustProbeResult plus
// the run label, when one was given.
type trustProbeResult struct {
	Run string `json:"run,omitempty"`
	engine.TrustProbeResult
}

func runTrustProbe(cmd *cobra.Command, args []string) error {
	w := getWriter(cmd)

	cfg := getCfg(cmd)
	if cfg == nil || cfg.ExecRoot == "" {
		return cmdErr(errors.New("the repository could not be resolved; run this inside a git worktree"),
			output.ErrValidation)
	}

	store, err := trust.Load()
	if err != nil {
		return trustCmdError(err)
	}
	identity, err := trust.RepoIdentity(cfg.Identity)
	if err != nil {
		return cmdErr(fmt.Errorf("resolving the repo identity: %w", err), output.ErrValidation)
	}
	entries := store.List(identity)

	actionNames, err := engine.ActionNames()
	if err != nil {
		return cmdErr(fmt.Errorf("reading the workflow corpus: %w", err), output.ErrGeneral)
	}

	run, _ := cmd.Flags().GetString("run")

	// SIGINT/SIGTERM cancel the context rather than killing the process, so
	// ProbeTrust's own defer removes the throwaway worktree before this
	// process exits (AC4) — the same pattern watchable() uses for `--watch`.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	result, err := engine.ProbeTrust(ctx, cfg.ExecRoot, entries, func(name string) bool {
		return actionNames[name]
	})
	if err != nil {
		if errors.Is(err, engine.ErrEmptyTrustRoster) {
			return cmdErr(err, output.ErrValidation)
		}
		return cmdErr(err, output.ErrGeneral)
	}

	data := trustProbeResult{Run: run, TrustProbeResult: *result}

	if !w.JSONMode {
		fmt.Fprintf(os.Stderr, "trust probe: HEAD %s", result.Head)
		if run != "" {
			fmt.Fprintf(os.Stderr, ", run %s", run)
		}
		fmt.Fprintf(os.Stderr, ", %d gate(s)\n", len(result.Gates))
		for _, s := range result.Skipped {
			fmt.Fprintf(os.Stderr, "SKIP %-22s %s\n", s.Name, s.Reason)
		}
		for _, g := range result.Gates {
			verdict := "OK  "
			if g.Exit == nil || *g.Exit != 0 {
				verdict = "FAIL"
			}
			exit := "-"
			if g.Exit != nil {
				exit = fmt.Sprintf("%d", *g.Exit)
			}
			stub := ""
			if g.Stub {
				stub = " STUB"
			}
			fmt.Fprintf(os.Stderr, "%s %-22s exit=%s%s\n", verdict, g.Name, exit, stub)
			if (g.Exit == nil || *g.Exit != 0) && g.LogTail != "" {
				for line := range strings.SplitSeq(g.LogTail, "\n") {
					fmt.Fprintf(os.Stderr, "                 %s\n", line)
				}
			}
		}
	}

	msg := fmt.Sprintf("%d gate(s) probed, all passed on clean HEAD", len(result.Gates))
	if !result.Passed {
		msg = fmt.Sprintf("%d gate(s) probed, %d FAILED on clean HEAD", len(result.Gates), len(result.Failed))
	}
	if result.Passed {
		w.Success(data, msg)
	} else {
		w.Outcome(data, msg)
	}
	return nil
}

func init() {
	trustCmd.AddCommand(trustProbeCmd)
}
