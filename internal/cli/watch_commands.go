package cli

import "github.com/spf13/cobra"

// watchEligible is the set of command paths that support --watch mode.
// Keys are Cobra CommandPath() values for unambiguous matching.
var watchEligible = map[string]bool{
	"docket board":                true,
	"docket issue list":           true,
	"docket issue show":           true,
	"docket issue log":            true,
	"docket issue graph":          true,
	"docket issue comment list":   true,
	"docket next":                 true,
	"docket plan":                 true,
	"docket stats":                true,
	"docket config":               true,
	"docket vote list":            true,
	"docket vote show":            true,
	"docket vote result":          true,
}

func isWatchEligible(cmd *cobra.Command) bool {
	return watchEligible[cmd.CommandPath()]
}

// hideWatchFlags hides --watch and --interval on commands that are not
// watch-eligible. Called during init after all subcommands are registered.
func hideWatchFlags(cmd *cobra.Command) {
	for _, child := range cmd.Commands() {
		hideWatchFlags(child)
	}
	if cmd != rootCmd && !isWatchEligible(cmd) {
		cmd.Flags().MarkHidden("watch")
		cmd.Flags().MarkHidden("interval")
	}
}
