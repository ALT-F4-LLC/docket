package cli

import (
	"fmt"
	"io"
	"log"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/tui"
)

var openTUIDebugLog = func(path string) (io.Closer, error) {
	return tea.LogToFile(path, "docket-tui")
}

var hasInteractiveTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

var runTUIProgram = func(cmd *cobra.Command) error {
	render.ConfigureUIOutput()
	program := tea.NewProgram(
		tui.NewBrowser(getDB(cmd), getCfg(cmd).DocketDir),
		tea.WithAltScreen(),
	)
	_, err := program.Run()
	return err
}

func newTUICommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Browse issues in an interactive terminal UI",
		Long:  "Browse issues in a read-only interactive terminal UI.\n\nThis command requires an interactive terminal and does not support --json.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if debugPath := os.Getenv("DOCKET_TUI_DEBUG_LOG"); debugPath != "" {
				debugFile, err := openTUIDebugLog(debugPath)
				if err != nil {
					return cmdErr(fmt.Errorf("opening debug log: %w", err), output.ErrGeneral)
				}
				defer debugFile.Close()
				log.Printf("starting docket tui debug log")
			}

			jsonMode, _ := cmd.Flags().GetBool("json")
			if jsonMode {
				return cmdErr(fmt.Errorf("--json is not supported with 'docket tui'"), output.ErrValidation)
			}

			if !hasInteractiveTerminal() {
				return cmdErr(fmt.Errorf("'docket tui' requires an interactive terminal"), output.ErrValidation)
			}

			if err := runTUIProgram(cmd); err != nil {
				return cmdErr(fmt.Errorf("running docket tui: %w", err), output.ErrGeneral)
			}
			return nil
		},
	}
}

var tuiCmd = newTUICommand()

func init() {
	rootCmd.AddCommand(tuiCmd)
}
