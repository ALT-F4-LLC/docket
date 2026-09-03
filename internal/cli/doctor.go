package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket doctor` — DKT-1285.

// doctorCheckJSON is one check's wire row.
type doctorCheckJSON struct {
	Check   string `json:"check"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail"`
}

// doctorResultJSON is doctor's whole envelope.
type doctorResultJSON struct {
	Clean   bool              `json:"clean"`
	Skipped bool              `json:"skipped"`
	Checks  []doctorCheckJSON `json:"checks"`
}

var doctorCmd = &cobra.Command{
	Use:   "doctor [--run RUN-N] [--source PATH]",
	Short: "Six read-only environment checks in one call",
	Long: `Answer the six checks a conductor clears by hand before the first
dispatch of an attach, in one call instead of six read-only agents: (1) cwd is
the git toplevel; (2) the store opens read-write from this seat; (3) the
shared config root and bin (` + "`~/.docket/config`" + `, ` + "`~/.docket/bin`" + `) match
--source's ` + "`src/user/docket/{config,bin}`" + `; (4) ` + "`run verify-pins`" + ` for --run, or
SKIP without it; (5) symlinks under ` + "`<cwd>/.docket/config`" + ` — debris from the
retired link-farm model, whether or not they still resolve; (6) a report of
detached worktrees homed under a scratch-shaped path.

READ-ONLY, and it WRITES NOTHING — no lease reap, no re-pin, no migration
beyond what any read verb performs.

Every check ALWAYS RUNS — none short-circuits another — and the return carries
one row per check: {check, verdict, detail}, verdict one of OK, FAIL, DRIFT,
SKIP, or WARN.

--run omitted makes the pins check SKIP, and ` + "`clean`" + ` reads false with
` + "`skipped`" + ` true: a conductor who forgot --run is told so rather than shown a
clean report that quietly checked five things instead of six. --source
omitted does the same to the install-drift check alone.

The stragglers check (6) never moves ` + "`clean`" + ` — it is a report, not a
verdict; reclaiming a straggler worktree is ` + "`git worktree prune`" + `'s job, not
this verb's.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDoctor(cmd, getWriter(cmd))
	},
}

func runDoctor(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)
	cfg := getCfg(cmd)

	runID, err := optionalRunFlag(cmd)
	if err != nil {
		return err
	}
	source, _ := cmd.Flags().GetString("source")

	cwd, err := os.Getwd()
	if err != nil {
		return cmdErr(fmt.Errorf("resolving the working directory: %w", err), output.ErrGeneral)
	}
	// Canonicalize before it reaches the seat check: git reports resolved
	// paths (macOS: /var vs /private/var) and Getwd does not.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	report := engine.Doctor(conn, engine.DoctorOptions{
		Cwd: cwd, DBPath: cfg.DBPath, RunID: runID, SourceRoot: source, NowMS: model.NowMS(),
	})

	result := doctorResultJSON{Clean: report.Clean, Skipped: report.Skipped}
	for _, c := range report.Checks {
		result.Checks = append(result.Checks, doctorCheckJSON{
			Check: c.Check, Verdict: string(c.Verdict), Detail: c.Detail,
		})
	}

	var message string
	if !w.JSONMode {
		message = renderDoctor(result)
	}
	w.Success(result, message)
	return nil
}

func renderDoctor(r doctorResultJSON) string {
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-14s %-5s %s\n", c.Check, c.Verdict, c.Detail)
	}
	fmt.Fprintf(&b, "clean: %t, skipped: %t", r.Clean, r.Skipped)
	return b.String()
}

func init() {
	doctorCmd.Flags().String("run", "", "Run to verify pins for (default: pins SKIPs)")
	doctorCmd.Flags().String("source", "",
		"Dotfiles-shaped checkout root — src/user/docket/{config,bin} compared "+
			"against ~/.docket/{config,bin} (default: install-drift SKIPs)")
	rootCmd.AddCommand(doctorCmd)
}
