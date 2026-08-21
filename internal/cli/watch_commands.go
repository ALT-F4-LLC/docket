package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// watchEligible is the set of command paths that support --watch mode.
// Keys are Cobra CommandPath() values for unambiguous matching.
var watchEligible = map[string]bool{
	"docket board":              true,
	"docket issue list":         true,
	"docket issue show":         true,
	"docket issue log":          true,
	"docket issue graph":        true,
	"docket issue comment list": true,
	"docket doc list":           true,
	"docket doc show":           true,
	"docket doc comment list":   true,
	"docket next":               true,
	"docket plan":               true,
	"docket stats":              true,
	"docket config":             true,
	"docket vote list":          true,
	"docket vote show":          true,
	"docket vote result":        true,
	// The run-scoped step inventory (DKT-54) is a conductor's polling verb —
	// the same live-progress question `board` answers for issues.
	"docket step list": true,
	// `events list` polls under `--follow` rather than `--watch`, but it uses
	// the SAME `--interval` (docs/tdd/events-follow.md §4.1 W2). Listing it here
	// is what keeps that flag visible on the command and validated by the same
	// floor — the alternative was a second `--interval` shadowing the
	// persistent one with its own default and no minimum.
	"docket events list": true,
}

func isWatchEligible(cmd *cobra.Command) bool {
	return watchEligible[cmd.CommandPath()]
}

// watchRejectionMessage names the actual rule an ineligible --watch is
// rejected under: an exact allowlist of tracker verbs, not a read/write
// split. Naming the allowlist itself (rather than asserting "write
// commands") keeps the message honest for a read-only command that simply
// never joined the list, such as `docket project list`.
func watchRejectionMessage() string {
	paths := make([]string, 0, len(watchEligible))
	for path := range watchEligible {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return fmt.Sprintf("--watch is limited to: %s", strings.Join(paths, ", "))
}

// hideWatchFlags hides --watch and --interval from the help of commands that
// are not watch-eligible. Called once after all subcommands are registered.
//
// The hiding must happen at RENDER time, and pflag's sharing rules are why.
// Before parsing, a child's flag set does not yet hold the root's persistent
// flags (mergePersistentFlags runs at parse time), so an init-time
// `cmd.Flags().MarkHidden` misses and returns an error — the previous
// implementation discarded it and never hid anything. And after merging, the
// child's set holds POINTERS to the root's own flag objects, so marking one
// ineligible command would hide the flags on every command at once, the
// eligible ones included. The two root flag objects are therefore marked
// hidden for exactly the one help rendering an ineligible command performs,
// and restored after. The root's own help keeps them visible: they are real
// global flags, and the eligibility table below the fold is their contract.
func hideWatchFlags(root *cobra.Command) {
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		watch := root.PersistentFlags().Lookup("watch")
		interval := root.PersistentFlags().Lookup("interval")
		if cmd != root && !isWatchEligible(cmd) && watch != nil && interval != nil {
			watch.Hidden, interval.Hidden = true, true
			defer func() { watch.Hidden, interval.Hidden = false, false }()
		}
		defaultHelp(cmd, args)
	})
}
