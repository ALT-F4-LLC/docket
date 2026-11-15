package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// commandPath renders the full verb path (`issue create`, `step complete`) for
// the registration event's `verb` field.
func commandPath(cmd *cobra.Command) string {
	var parts []string
	for c := cmd; c != nil && c.Parent() != nil; c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	return strings.Join(parts, " ")
}
