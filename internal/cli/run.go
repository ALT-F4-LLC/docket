package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start, activate, and inspect workflow runs",
	Long: `Runs bind workflows to issues and schedule their steps.

A run is created in ` + "`planning`" + ` with the issues named at ` + "`run start`" + `; while it
is planning, ` + "`run issue add`" + ` and ` + "`run issue remove`" + ` reshape that set. The run
is then ACTIVATED: one transaction that binds each issue to exactly one registered
workflow, lints the work graph, pins what the run must reproduce, freezes the
issue state its steps will read, harvests the commands the issue bodies
declare, and expands the first phase's steps.

Activation runs no gate, no action, and no command. It reads files, to pin them
by content hash, and nothing else.`,
	// An unknown verb must FAIL, not print help and exit 0. `docket run list`
	// fell through to the help text with a success code, which reads as "there
	// are no runs" rather than "that verb does not exist".
	//
	// Cobra dispatches a known subcommand before this ever runs, so reaching
	// RunE means either no verb or an unrecognized one.
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmdErr(
				fmt.Errorf("run needs a subcommand: %s", runVerbList(cmd)),
				output.ErrValidation)
		}
		return cmdErr(unknownRunVerb(cmd, args[0]), output.ErrValidation)
	},
}

// unknownRunVerb builds the error for an unrecognized `docket run` verb. It
// names the intended verb where a common miss has an obvious target, because
// the whole cost of the defect was a caller reading help output and having to guess
// which of eight verbs replaced the one they typed.
func unknownRunVerb(cmd *cobra.Command, verb string) error {
	if intended, ok := runVerbSuggestions[verb]; ok {
		return fmt.Errorf("run has no %q subcommand; use %q. Available: %s",
			verb, intended, runVerbList(cmd))
	}
	return fmt.Errorf("run has no %q subcommand. Available: %s",
		verb, runVerbList(cmd))
}

// runVerbSuggestions maps a verb an operator plausibly reaches for onto the one
// that actually exists. `list` is the observed miss: runs are enumerated by
// `run status --active`, which is not a name anyone guesses.
var runVerbSuggestions = map[string]string{
	"list": "run status --active",
	"ls":   "run status --active",
	"show": "run status RUN-N",
	"log":  "run report RUN-N",
}

// runVerbList renders the registered subcommands, so the error is derived from
// what is actually wired rather than from a hand-maintained list that drifts.
func runVerbList(cmd *cobra.Command) string {
	names := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		if sub.Hidden || sub.Name() == "help" {
			continue
		}
		names = append(names, sub.Name())
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func init() {
	rootCmd.AddCommand(runCmd)
}

// runErr maps an activation failure to its error code, per the §5.5 taxonomy.
//
// engine.Error carries its own code, so the mapping is a lookup rather than a
// guess at the message — an error taxonomy inferred from strings is one that
// silently reclassifies itself when a message is reworded.
//
// S6 adds the ONE new code the taxonomy has gained since it was frozen: GONE,
// for a cursor below the retained minimum (docs/tdd/runs-dispatch.md §8.6).
// Every other situation still maps to a code that already existed.
func runErr(err error) error {
	if code, ok := engine.CodeOf(err); ok {
		switch code {
		case engine.CodeValidation:
			return cmdErr(err, output.ErrValidation)
		case engine.CodeNotFound:
			return cmdErr(err, output.ErrNotFound)
		case engine.CodeConflict:
			return cmdErr(err, output.ErrConflict)
		case engine.CodeGone:
			return cmdErr(err, output.ErrGone)
		}
	}
	switch {
	case errors.Is(err, db.ErrRunNotFound), errors.Is(err, db.ErrNotFound):
		return cmdErr(err, output.ErrNotFound)
	default:
		return cmdErr(err, output.ErrGeneral)
	}
}
