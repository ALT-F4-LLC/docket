package cli

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/render"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:         "version",
	Short:       "Print docket version information",
	Annotations: map[string]string{"skipDB": "true"},
	Run: func(cmd *cobra.Command, args []string) {
		w := getWriter(cmd)

		bold := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
		dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		msg := fmt.Sprintf("docket version %s %s",
			render.StyledText(version, bold),
			render.StyledText(fmt.Sprintf("(commit: %s, built: %s)", commit, buildDate), dim),
		)

		w.Success(struct {
			Version   string `json:"version"`
			Commit    string `json:"commit"`
			BuildDate string `json:"build_date"`
		}{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
		}, msg)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
