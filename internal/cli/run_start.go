package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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
reports nothing cannot spend past the cap. 0 means unlimited.

The cap is read from the run row for the life of the run: a ` + "`budget.default`" + `
set after ` + "`run start`" + ` does not re-cap a run already started. ` + "`docket run report`" + `
prints the effective cap AND where it came from, so the question that follows
from that ("why didn't it stop?") is answered by a read verb.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunStart(cmd, getWriter(cmd))
	},
}

func runRunStart(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	requestFile, _ := cmd.Flags().GetString("request-file")
	budget, _ := cmd.Flags().GetFloat64("budget")
	issueRefs, _ := cmd.Flags().GetStringSlice("issue")

	if budget < 0 {
		return cmdErr(
			fmt.Errorf("--budget must be >= 0, got %g (0 means unlimited)", budget),
			output.ErrValidation)
	}

	var request string
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
		request = strings.TrimRight(string(content), "\n")
	}

	// Issues are resolved BEFORE the run is created, so a typo'd id refuses
	// without leaving an empty run behind for someone to wonder about.
	//
	// Homing is checked beside existence (DKT-21): the run will be created in
	// the INVOKING project, and a run's issues live in the run's own project —
	// activation binds them against that project's workflow registry and books
	// their snapshots, steps, and gaps there. Ids are store-wide, so without
	// this check any project's issue would attach from any cwd.
	runProject := getProjectID(cmd)
	issueIDs := make([]int, 0, len(issueRefs))
	for _, ref := range issueRefs {
		id, err := model.ParseID(ref)
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID %q: %w", ref, err), output.ErrValidation)
		}
		issueProject, err := db.IssueProjectID(conn, id)
		if errors.Is(err, db.ErrNotFound) {
			return cmdErr(
				fmt.Errorf("issue %s not found", model.FormatID(id)), output.ErrNotFound)
		}
		if err != nil {
			return cmdErr(fmt.Errorf("checking issue %s: %w", ref, err), output.ErrGeneral)
		}
		if issueProject != runProject {
			return cmdErr(fmt.Errorf(
				"issue %s belongs to %s, but this run would start in %s — a run's "+
					"issues live in the run's own project; migrate the issue first "+
					"with `docket issue move %s --project <target>`, or start the "+
					"run from the issue's own repository",
				model.FormatID(id), projectLabel(conn, issueProject),
				projectLabel(conn, runProject), model.FormatID(id)),
				output.ErrValidation)
		}
		issueIDs = append(issueIDs, id)
	}

	// B1's second branch, resolved HERE rather than at every claim: an omitted
	// `--budget` takes `docket config budget.default`, and the resolved number is
	// what the row stores. That is what makes B3 true — the cap a claim enforces
	// is read from the run row and from nowhere else, so a config change after
	// `run start` cannot silently re-cap a live run.
	if budget == 0 {
		entry, err := db.GetConfig(conn, getProjectID(cmd), db.KeyBudgetDefault)
		if err != nil {
			return cmdErr(fmt.Errorf("reading %s: %w", db.KeyBudgetDefault, err),
				output.ErrGeneral)
		}
		fallback, err := strconv.ParseFloat(entry.Value, 64)
		if err != nil {
			return cmdErr(
				fmt.Errorf("%s is %q, which is not a number", db.KeyBudgetDefault, entry.Value),
				output.ErrValidation)
		}
		budget = fallback
	}

	// The MEASURED cap resolves the same way and is pinned the same way
	// (DKT-238). Two flags because the two caps count different things: one
	// bounds how much work a run schedules, the other bounds what that work
	// actually consumed, and a run reasonably wants both.
	usageBudget, _ := cmd.Flags().GetFloat64("usage-budget")
	if usageBudget < 0 {
		return cmdErr(
			fmt.Errorf("--usage-budget must be >= 0, got %g (0 means unlimited)",
				usageBudget),
			output.ErrValidation)
	}
	if usageBudget == 0 {
		entry, err := db.GetConfig(conn, getProjectID(cmd), db.KeyUsageBudgetDefault)
		if err != nil {
			return cmdErr(fmt.Errorf("reading %s: %w", db.KeyUsageBudgetDefault, err),
				output.ErrGeneral)
		}
		fallback, err := strconv.ParseFloat(entry.Value, 64)
		if err != nil {
			return cmdErr(
				fmt.Errorf("%s is %q, which is not a number",
					db.KeyUsageBudgetDefault, entry.Value),
				output.ErrValidation)
		}
		usageBudget = fallback
	}

	// The run records where and on what it started (G8): the invoking
	// checkout's root, its branch and commit, and the machine. Best-effort —
	// outside a checkout the fields record empty rather than invented.
	runCtx := db.RunContext{UsageBudget: usageBudget}
	if cfg := getCfg(cmd); cfg != nil {
		runCtx.ExecRoot = cfg.ExecRoot
		runCtx.Branch, runCtx.CommitSHA = config.GitHead(cfg.ExecRoot)
	}
	if host, err := os.Hostname(); err == nil {
		runCtx.Hostname = host
	}

	idempotencyKey, err := idempotencyKeyOf(cmd)
	if err != nil {
		return err
	}

	run, err := db.InsertRunWithContextIdempotent(conn, getProjectID(cmd), request, budget, model.NowMS(), runCtx, idempotencyKey)
	if err != nil {
		return runErr(err)
	}

	for _, id := range issueIDs {
		if err := db.AddRunIssue(conn, run.ID, id); err != nil {
			return runErr(err)
		}
	}

	message := fmt.Sprintf("Started %s in %s", run.Ref(), run.Status)
	if len(issueIDs) > 0 {
		message = fmt.Sprintf("%s with %d issue(s)", message, len(issueIDs))
	}
	w.Success(withRunVersion(run), message)
	return nil
}

func init() {
	runStartCmd.Flags().String("request-file", "", "File holding the run's request text")
	runStartCmd.Flags().Float64("budget", 0, "Per-run budget cap, in the unit `budget.unit` names (see `docket run budget --help`); 0 means unlimited")
	runStartCmd.Flags().Float64("usage-budget", 0,
		"Per-run cap over MEASURED usage of the unit `budget.usage.unit` names; "+
			"0 means unlimited. Separate from --budget, which counts declared "+
			"step costs")
	runStartCmd.Flags().StringSlice("issue", nil, "Issue IDs to attach (repeatable)")
	addIdempotencyKeyFlag(runStartCmd)
	runCmd.AddCommand(runStartCmd)
}
