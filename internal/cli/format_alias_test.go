package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// newFormatCmd builds a child command under a root carrying --json and
// --format as PERSISTENT flags, which is how they are registered on rootCmd.
//
// The parent/child split is load-bearing, not scaffolding. `--format` is
// inherited here, and a command that declares its OWN --format (as `export`
// does, with json|csv|markdown) deliberately opts out of the alias. A flat
// fixture registering --format locally would model `export`, not `issue show`,
// and would test the opposite of what it claims.
func newFormatCmd(jsonVal, formatVal string) *cobra.Command {
	root := &cobra.Command{Use: "root"}
	root.PersistentFlags().String("json", "", "")
	root.PersistentFlags().Lookup("json").NoOptDefVal = "v1"
	root.PersistentFlags().String("format", "", "")

	cmd := &cobra.Command{Use: "probe"}
	root.AddCommand(cmd)

	// Set on the ROOT's persistent flagset — that is the flag object the
	// child inherits and that jsonVersionOf reads through cmd.Flags().
	if jsonVal != "" {
		_ = root.PersistentFlags().Set("json", jsonVal)
	}
	if formatVal != "" {
		_ = root.PersistentFlags().Set("format", formatVal)
	}
	return cmd
}

// newOwnFormatCmd builds a command that declares its OWN --format, the
// `export` shape.
func newOwnFormatCmd(formatVal string) *cobra.Command {
	root := &cobra.Command{Use: "root"}
	root.PersistentFlags().String("json", "", "")
	root.PersistentFlags().Lookup("json").NoOptDefVal = "v1"
	root.PersistentFlags().String("format", "", "")

	cmd := &cobra.Command{Use: "export"}
	cmd.Flags().StringP("format", "o", "json", "")
	root.AddCommand(cmd)

	if formatVal != "" {
		_ = cmd.Flags().Set("format", formatVal)
	}
	return cmd
}

// TestCommandsOwningFormatOptOutOfTheAlias pins the collision the alias's first
// cut caused and QA caught: `export` has always owned
// --format json|csv|markdown, and the alias must not reinterpret those values
// or switch its envelope.
func TestCommandsOwningFormatOptOutOfTheAlias(t *testing.T) {
	for _, value := range []string{"json", "csv", "markdown"} {
		cmd := newOwnFormatCmd(value)
		if formatIsJSON(cmd) {
			t.Errorf("export --format %s was read as the v2 alias; a command "+
				"declaring its own --format owns its vocabulary", value)
		}
		if got := jsonVersionOf(cmd); got != output.JSONNone {
			t.Errorf("jsonVersionOf(export --format %s) = %v, want JSONNone",
				value, got)
		}
	}
}

// TestFormatAliasSelectsV2 pins the alias: --format json is an alias for
// --json=v2.
//
// The precedence case is the one that matters most. --format must not upgrade
// an envelope the caller pinned explicitly: a script that says --json=v1 is
// frozen on that shape, and having a second flag silently promote it to v2
// would be a compatibility break dressed up as an ergonomic win.
func TestFormatAliasSelectsV2(t *testing.T) {
	tests := []struct {
		name   string
		json   string
		format string
		want   output.JSONVersion
	}{
		{"format json selects v2", "", "json", output.JSONV2},
		{"neither flag is human mode", "", "", output.JSONNone},
		{"json alone is unchanged", "v1", "", output.JSONV1},
		{"explicit v1 beats the alias", "v1", "json", output.JSONV1},
		{"explicit v2 agrees with the alias", "v2", "json", output.JSONV2},
		{"invalid format does not select json", "", "yaml", output.JSONNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonVersionOf(newFormatCmd(tt.json, tt.format)); got != tt.want {
				t.Errorf("jsonVersionOf(json=%q, format=%q) = %v, want %v",
					tt.json, tt.format, got, tt.want)
			}
		})
	}
}

// TestFormatAliasAbsentFlagIsSafe covers commands built outside the root tree.
// Tests construct bare cobra.Commands with no --format registered, and looking
// one up must read as not-requested rather than panicking.
func TestFormatAliasAbsentFlagIsSafe(t *testing.T) {
	cmd := &cobra.Command{Use: "bare"}
	cmd.Flags().String("json", "", "")
	if got := jsonVersionOf(cmd); got != output.JSONNone {
		t.Errorf("jsonVersionOf on a command without --format = %v, want JSONNone", got)
	}
	if formatIsJSON(cmd) {
		t.Error("formatIsJSON on a command without --format = true, want false")
	}
}

// TestParseFormat pins the accepted spellings. Anything not listed here is
// rejected up front in PersistentPreRunE rather than degrading into human
// output the caller did not ask for.
func TestParseFormat(t *testing.T) {
	valid := []string{"json", "json=v2", "v2"}
	for _, raw := range valid {
		if got := parseFormat(raw); got != output.JSONV2 {
			t.Errorf("parseFormat(%q) = %v, want JSONV2", raw, got)
		}
	}

	invalid := []string{"", "yaml", "v1", "csv", "JSON", "table"}
	for _, raw := range invalid {
		if got := parseFormat(raw); got != output.JSONNone {
			t.Errorf("parseFormat(%q) = %v, want JSONNone", raw, got)
		}
	}
}
