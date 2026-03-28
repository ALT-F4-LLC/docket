package cli

import (
	"fmt"
	"log"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/tui"
)

var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Browse issues in an interactive terminal UI",
	RunE: func(cmd *cobra.Command, args []string) error {
		if debugPath := os.Getenv("DOCKET_UI_DEBUG_LOG"); debugPath != "" {
			debugFile, err := tea.LogToFile(debugPath, "docket-ui")
			if err != nil {
				return cmdErr(fmt.Errorf("opening debug log: %w", err), output.ErrGeneral)
			}
			defer debugFile.Close()
			log.Printf("starting docket ui debug log")
		}

		jsonMode, _ := cmd.Flags().GetBool("json")
		if jsonMode {
			return cmdErr(fmt.Errorf("--json is not supported with 'docket ui'"), output.ErrValidation)
		}

		if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
			return cmdErr(fmt.Errorf("'docket ui' requires an interactive terminal"), output.ErrValidation)
		}

		render.ConfigureUIOutput()

		program := tea.NewProgram(
			tui.NewBrowser(getDB(cmd), getCfg(cmd).DocketDir),
			tea.WithAltScreen(),
		)
		if _, err := program.Run(); err != nil {
			return cmdErr(fmt.Errorf("running docket ui: %w", err), output.ErrGeneral)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(uiCmd)
}
