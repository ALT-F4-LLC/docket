package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	Short: "Eight read-only environment checks in one call",
	Long: `Answer the checks a conductor clears by hand before the first
dispatch of an attach, in one call instead of seven read-only agents: (1) cwd
is the git toplevel; (2) the store opens read-write from this seat; (3) the cwd
resolves to a registered project — REPORTED, never registered by this verb, so
an unbound repository reads FAIL here instead of being bound by the asking;
(4) the shared config root and bin (` + "`~/.docket/config`" + `, ` + "`~/.docket/bin`" + `) match
--source's ` + "`src/user/docket/{config,bin}`" + `; (5) ` + "`run verify-pins`" + ` for --run, or
SKIP without it; (6) symlinks under ` + "`<cwd>/.docket/config`" + ` — debris from the
retired link-farm model, whether or not they still resolve; (7) a report of
detached worktrees homed under a scratch-shaped path; (8) whether
` + "`DOCKET_POSTGRES_DSN`" + ` is configured, without printing the DSN, its username, or
its password, and without connecting.

READ-ONLY, and it WRITES NOTHING — no lease reap, no re-pin, no migration, no
project registration — beyond what any read verb performs.

Every check ALWAYS RUNS — none short-circuits another — and the return carries
one row per check: {check, verdict, detail}, verdict one of OK, FAIL, DRIFT,
SKIP, or WARN.

--run omitted makes the pins check SKIP, and ` + "`clean`" + ` reads false with
` + "`skipped`" + ` true: a conductor who forgot --run is told so rather than shown a
clean report that quietly checked six things instead of seven. --source
omitted does the same to the install-drift check alone.

The stragglers (7) and postgres (8) checks never move ` + "`clean`" + ` — they are
reports, not verdicts; reclaiming a straggler worktree is ` + "`git worktree prune`" + `'s job, not
this verb's.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDoctor(cmd, getWriter(cmd))
	},
}

func runDoctor(cmd *cobra.Command, w *output.Writer) error {
	run, _ := cmd.Flags().GetString("run")
	source, _ := cmd.Flags().GetString("source")

	cwd, err := os.Getwd()
	if err != nil {
		return cmdErr(fmt.Errorf("resolving the working directory: %w", err), output.ErrGeneral)
	}
	env := verbEnv(cmd)
	env.Cwd = cwd

	report, err := verb.Doctor(cmd.Context(), env, verb.DoctorInput{
		Run: run, Source: source, DBPath: getCfg(cmd).DBPath,
	})
	if err != nil {
		return verbErr(err)
	}

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
