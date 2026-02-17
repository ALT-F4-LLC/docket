package cli

import "github.com/spf13/cobra"

var issueCmd = &cobra.Command{
	Use:     "issue",
	Short:   "Manage issues",
	Aliases: []string{"i"},
}

func init() {
	rootCmd.AddCommand(issueCmd)
}
