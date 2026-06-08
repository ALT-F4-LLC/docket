package cli

import "github.com/spf13/cobra"

var docCmd = &cobra.Command{
	Use:     "doc",
	Short:   "Manage documents",
	Aliases: []string{"d"},
}

func init() {
	rootCmd.AddCommand(docCmd)
}
