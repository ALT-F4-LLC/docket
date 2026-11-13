package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var runStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Create a run in planning",
	Long: `Create a run and attach the issues it covers.

The run is created in ` + "`planning`" + `: nothing is bound, nothing is pinned, and no
step exists until ` + "`docket run activate`" + `.

--budget is ENFORCED. The run pauses at ` + "`waiting-human`" + ` with a budget-breach
reason when the claim that would cross the cap arrives — and the quantity it
compares is max(reported usage, declared-cost floor), so a claimant that
reports nothing cannot spend past the cap. 0 means unlimited, and an explicit
` + "`--budget 0`" + ` overrides ` + "`budget.default`" + ` — omitting the flag is what
inherits that default.

The cap is read from the run row for the life of the run: a ` + "`budget.default`" + `
set after ` + "`run start`" + ` does not re-cap a run already started. ` + "`docket run report`" + `
prints the effective cap AND where it came from, so the question that follows
from that ("why didn't it stop?") is answered by a read verb.`,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunStart(cmd, getWriter(cmd))
	},
}

func runRunStart(cmd *cobra.Command, w *output.Writer) error {
	requestFile, _ := cmd.Flags().GetString("request-file")
	issueRefs, _ := cmd.Flags().GetStringSlice("issue")
	in := verb.RunStartInput{Issues: issueRefs}

	// OMITTED is `Changed`, not zero (DKT-1539): an explicit `--budget 0` is
	// unlimited, and only an omitted flag inherits the configured default.
	if cmd.Flags().Changed("budget") {
		budget, _ := cmd.Flags().GetFloat64("budget")
		in.Budget = &budget
	}
	if cmd.Flags().Changed("usage-budget") {
		usageBudget, _ := cmd.Flags().GetFloat64("usage-budget")
		in.UsageBudget = &usageBudget
	}

	if requestFile != "" {
		content, err := os.ReadFile(requestFile)
		if err != nil {
			if os.IsNotExist(err) {
				return cmdErr(
					fmt.Errorf("request file %s not found", requestFile), output.ErrNotFound)
			}
			return cmdErr(
				fmt.Errorf("reading request file %s: %w", requestFile, err), output.ErrNotFound)
		}
		in.Request = strings.TrimRight(string(content), "\n")
	}

	// The run records where and on what it started: the invoking
	// checkout's root, its branch and commit, and the machine. Best-effort —
	// outside a checkout the fields record empty rather than invented.
	if cfg := getCfg(cmd); cfg != nil {
		in.ExecRoot = cfg.ExecRoot
		in.Branch, in.CommitSHA = config.GitHead(cfg.ExecRoot)
	}
	if host, err := os.Hostname(); err == nil {
		in.Hostname = host
	}

	idempotencyKey, err := idempotencyKeyOf(cmd)
	if err != nil {
		return err
	}
	in.IdempotencyKey = idempotencyKey

	res, err := verb.RunStart(cmd.Context(), verbEnv(cmd), in)
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	run := res.Run
	message := fmt.Sprintf("Started %s in %s", run.Ref(), run.Status)
	if res.Inserted && len(issueRefs) > 0 {
		message = fmt.Sprintf("%s with %d issue(s)", message, len(issueRefs))
	}
	w.Success(withRunVersion(run), message)
	return nil
}

func init() {
	runStartCmd.Flags().String("request-file", "", "File holding the run's request text")
	runStartCmd.Flags().Float64("budget", 0, "Per-run budget cap, in the unit `budget.unit` names (see `docket run budget --help`); 0 means unlimited, and an explicit 0 overrides `budget.default`. Omit the flag to inherit that default")
	runStartCmd.Flags().Float64("usage-budget", 0,
		"Per-run cap over MEASURED usage of the unit `budget.usage.unit` names; "+
			"0 means unlimited. Separate from --budget, which counts declared "+
			"step costs")
	runStartCmd.Flags().StringSlice("issue", nil, "Issue IDs to attach (repeatable)")
	addIdempotencyKeyFlag(runStartCmd)
	runCmd.AddCommand(runStartCmd)
}
