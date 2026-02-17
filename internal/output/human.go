package output

import (
	"fmt"
	"io"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/render"
)

// writeHumanSuccess writes a human-readable success message to w.
func writeHumanSuccess(w io.Writer, message string) {
	if message == "" {
		return
	}
	if render.ColorsEnabled() {
		icon := lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("\u2714")
		fmt.Fprintf(w, "%s %s\n", icon, message)
	} else {
		fmt.Fprintln(w, message)
	}
}

// writeHumanError writes a human-readable error message to w.
func writeHumanError(w io.Writer, err error) {
	if render.ColorsEnabled() {
		icon := lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render("\u2718")
		label := lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render("Error:")
		fmt.Fprintf(w, "%s %s %s\n", icon, label, err)
	} else {
		fmt.Fprintf(w, "Error: %s\n", err)
	}
}
