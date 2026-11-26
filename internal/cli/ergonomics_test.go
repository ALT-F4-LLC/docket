package cli

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The ergonomics group of the 2026-08-17 shadow post-mortem: DKT-69's silent
// unset key and DKT-72's two missing flags. Each is a surface an operator or an
// agent reached for and got a wrong answer or an `unknown flag`.

// TestUnsetConfigKeyIsVisiblyUnset is DKT-69.
//
// `config get <key>` printed the empty string for a key with no value and no
// shipped default, and exited 0 — indistinguishable from a key deliberately set
// to "". Read in bulk it was worse: several keys produced FEWER LINES THAN
// KEYS, so a reader matching lines to keys positionally read the wrong values.
// Measured misread: vote-rule keys read as `15m`, a value belonging to the
// neighboring lease keys.
func TestUnsetConfigKeyIsVisiblyUnset(t *testing.T) {
	unset := db.ConfigEntry{Key: "vote.rule.nonexistent.threshold", Source: "default"}
	if got := configValueForHuman(unset); got != unsetConfigValue {
		t.Errorf("an unset key renders %q, want %q — a line that prints nothing "+
			"cannot be told from a key set to the empty string, and in bulk it "+
			"silently shortens the output", got, unsetConfigValue)
	}

	// A key EXPLICITLY set to empty keeps printing empty. `source` is what
	// carries the distinction, and rendering both as `<unset>` would erase
	// exactly what it exists to say.
	setEmpty := db.ConfigEntry{Key: "budget.default", Source: "set"}
	if got := configValueForHuman(setEmpty); got != "" {
		t.Errorf("a key set to the empty string renders %q, want \"\" — "+
			"`source` distinguishes it from unset and must not be overridden", got)
	}

	// A real value is untouched.
	real := db.ConfigEntry{Key: "budget.default", Value: "0", Source: "default"}
	if got := configValueForHuman(real); got != "0" {
		t.Errorf("a defaulted value renders %q, want %q", got, "0")
	}
}

// TestConfigListKeepsOneLinePerKey is the bulk half of DKT-69: the listing has
// to keep the line count equal to the key count, since that is the property a
// positional reader was relying on and never had.
func TestConfigListKeepsOneLinePerKey(t *testing.T) {
	entries := []db.ConfigEntry{
		{Key: "a.key", Value: "15m", Source: "set"},
		{Key: "b.unset", Source: "default"},
		{Key: "c.key", Value: "3", Source: "default"},
	}

	lines := strings.Split(formatConfigEntries(entries), "\n")
	if len(lines) != len(entries) {
		t.Fatalf("rendered %d lines for %d keys: %q", len(lines), len(entries), lines)
	}
	if !strings.Contains(lines[1], unsetConfigValue) {
		t.Errorf("the unset key's line carries no value at all: %q", lines[1])
	}
	// The misread this reproduces: without a marker, line 1 would hold `c.key`'s
	// value and a reader counting down from the top would attribute it to
	// `b.unset`.
	if strings.Contains(lines[1], "3") {
		t.Errorf("the unset key's line carries the NEXT key's value: %q", lines[1])
	}
}

// setIssueProject re-homes a fixture issue. The create path takes the ambient
// project, and this test needs two.
func setIssueProject(t *testing.T, conn *sql.DB, issueID, projectID int) {
	t.Helper()
	_, err := conn.Exec(`UPDATE issues SET project_id = ? WHERE id = ?`, projectID, issueID)
	testsupport.Must(t, err, "re-homing issue %d: %v", issueID, err)
}
