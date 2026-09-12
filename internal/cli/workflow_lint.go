package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

// `docket workflow lint` — DKT-38. Before this verb the ONLY way to run the
// validator was to REGISTER, a persistent write: checking a draft inserted a
// row and minted a frozen name@version. An author iterating on a definition
// either accumulated registered versions they did not want or did not check
// at all.

var workflowLintCmd = &cobra.Command{
	Use:   "lint <file.toml>",
	Short: "Validate a workflow definition without registering it",
	Long: `Run the exact validation ` + "`docket workflow register`" + ` runs — grammar, step
rules, vote rules, and schema cross-checks — and WRITE NOTHING.

Pass - as the file to read from stdin.

The registry is consulted, never written: schema and vote-rule references
resolve against what is registered, and the verdict reports whether a real
register would be NEW, UNCHANGED (identical bytes already registered), or a
CONFLICT (different bytes at a frozen name@version — bump [pipeline].version
to adopt the edit). The conflict case fails the lint: a definition that can
never register as-is has not passed.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowLint(cmd, args, getWriter(cmd))
	},
}

// lintResult is the verb's wire shape.
type lintResult struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	// Registration is what a real register would do: `new`, `unchanged`.
	// (The conflict case refuses rather than reporting, so it never appears.)
	Registration string `json:"registration"`
}

func runWorkflowLint(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)
	path := args[0]

	src, err := readWorkflowSource(cmd, path)
	if err != nil {
		return err
	}

	// The SAME pipeline register runs, call for call, so the two cannot drift:
	// grammar and step rules (workflow.Load), vote rules against the config
	// registry (V26), threshold fields and literals against the registered
	// schemas (V21a-V21d, V25a).
	def, err := loadWorkflowSource(src, path)
	if err != nil {
		return workflowErr(err)
	}
	if err := workflow.ValidateVoteRules(def, voteRuleResolver{conn, getProjectID(cmd)}); err != nil {
		return workflowErr(err)
	}
	if err := workflow.ValidateSchemas(def, schemaResolver{conn, getProjectID(cmd)}); err != nil {
		return workflowErr(err)
	}

	// The registry probe (read-only): would this register, and as what? An
	// edited file at a frozen name@version is the retro-loop's observed trap —
	// the definition validates, then the next activation refuses the whole run
	// — so lint surfaces it while the author is still looking at the file.
	//
	// THIS IS ALREADY DKT-590's CHECK, FROM THE FILE'S SIDE, and it is why lint
	// gains nothing from the source-drift verdict `workflow show` and
	// `run activate` now report. Those two start from a REGISTERED ROW and read
	// the path the row recorded; lint starts from bytes the caller handed it
	// and looks up the row those bytes declare. When the file passed here IS
	// the file a row recorded, the comparison below is the same comparison on
	// the same two hashes, refusing where the others refuse. When it is not —
	// the DKT-590 population, where the file has moved on to version 8 while
	// version 4 stays registered — lint resolves @8, finds a free slot, and
	// correctly reports `new`: nothing about @8 is wrong, and @4's stale
	// provenance is a fact about a row this invocation was never asked about.
	// Reporting it here would fire on every ordinary version bump, since a
	// bumped file always leaves the superseded version's recorded path holding
	// different bytes.
	sum := workflow.SHA256(src)
	registration := "new"
	existing, err := db.GetWorkflow(conn, getProjectID(cmd), def.Pipeline.Name, def.Pipeline.Version)
	switch {
	case errors.Is(err, db.ErrWorkflowNotFound):
		// Free slot; a register would insert.
	case err != nil:
		return cmdErr(err, output.ErrGeneral)
	case existing.SourceSHA256 == sum:
		registration = "unchanged"
	default:
		return cmdErr(fmt.Errorf(
			"%s@%d is registered with different bytes\n\n"+
				"  registered  sha256:%s\n  this file   sha256:%s\n\n"+
				"A registered name@version is frozen so that a run which pinned it "+
				"can reproduce. To adopt these changes, bump [pipeline].version to "+
				"%d and lint again",
			def.Pipeline.Name, def.Pipeline.Version,
			existing.SourceSHA256, sum, def.Pipeline.Version+1),
			output.ErrConflict)
	}

	result := lintResult{
		Name:         def.Pipeline.Name,
		Version:      def.Pipeline.Version,
		SHA256:       sum,
		Registration: registration,
	}
	w.Success(result, fmt.Sprintf("%s@%d is valid (%s; nothing written)",
		result.Name, result.Version, registration))
	return nil
}

func init() {
	workflowCmd.AddCommand(workflowLintCmd)
}
