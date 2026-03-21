package cli

import "github.com/spf13/cobra"

var voteCmd = &cobra.Command{
	Use:     "vote",
	Short:   "Manage consensus proposals",
	Aliases: []string{"v"},
}

func init() {
	rootCmd.AddCommand(voteCmd)
}
