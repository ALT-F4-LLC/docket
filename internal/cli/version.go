package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:         "version",
	Short:       "Print docket version information",
	Annotations: map[string]string{"skipDB": "true"},
	Run: func(cmd *cobra.Command, args []string) {
		w := getWriter(cmd)

		w.Success(struct {
			Version   string `json:"version"`
			Commit    string `json:"commit"`
			BuildDate string `json:"build_date"`
		}{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
		}, fmt.Sprintf("docket version %s (commit: %s, built: %s)", version, commit, buildDate))
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
