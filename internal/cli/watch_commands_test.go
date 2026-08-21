package cli

import (
	"bytes"
	"strings"
	"testing"
)

// helpOutput renders one command path's --help through the real rootCmd and
// returns what a user would read.
func helpOutput(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(append(args, "--help"))
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	err := rootCmd.Execute()
	// The parsed --help value persists on the command tree, and a later test
	// executing the same path would render help instead of its own behavior.
	if c, _, findErr := rootCmd.Find(args); findErr == nil {
		if f := c.Flags().Lookup("help"); f != nil {
			_ = f.Value.Set("false")
			f.Changed = false
		}
	}
	if err != nil {
		t.Fatalf("rendering help for %v: %v", args, err)
	}
	return buf.String()
}

// TestHelpHidesWatchFlagsOnIneligibleCommands pins DKT-31: an ineligible
// command's --help must not advertise --watch/--interval, and an eligible
// command's help — rendered AFTER an ineligible one — must still show them.
// The second half guards the pflag hazard that makes this fix delicate:
// merged flag sets share the root's flag POINTERS, so a hiding that sticks
// past one rendering hides the flags everywhere at once.
func TestHelpHidesWatchFlagsOnIneligibleCommands(t *testing.T) {
	initWatchFlags()

	ineligible := helpOutput(t, "project", "list")
	if strings.Contains(ineligible, "--watch") ||
		strings.Contains(ineligible, "--interval") {
		t.Errorf("docket project list --help still advertises watch flags:\n%s",
			ineligible)
	}

	eligible := helpOutput(t, "issue", "list")
	if !strings.Contains(eligible, "--watch") ||
		!strings.Contains(eligible, "--interval") {
		t.Errorf("docket issue list --help lost the watch flags after an "+
			"ineligible rendering — the hiding leaked across commands:\n%s",
			eligible)
	}
}

// TestWatchRejectionNamesTheRealRule pins DKT-14's fix: the message an
// ineligible --watch is rejected with must name the actual rule — an exact
// allowlist of tracker verbs — rather than asserting a "write commands"
// split that a read-only command like `docket project list` falsifies.
func TestWatchRejectionNamesTheRealRule(t *testing.T) {
	got := watchRejectionMessage()

	if strings.Contains(got, "write commands") {
		t.Errorf("watchRejectionMessage() = %q, still asserts the false "+
			"write-only rule", got)
	}
	if !strings.Contains(got, "docket board") {
		t.Errorf("watchRejectionMessage() = %q, want it to name the "+
			"allowlist (e.g. %q)", got, "docket board")
	}
}

// TestReadOnlyIneligibleCommandRejectsWatchWithTheRealRule is the repro from
// DKT-14: `docket project list --watch` is read-only yet was rejected with a
// message claiming it is a write command. `project list` never joined the
// watch-eligible allowlist, so it must still be rejected — but with the
// honest reason.
func TestReadOnlyIneligibleCommandRejectsWatchWithTheRealRule(t *testing.T) {
	if isWatchEligible(projectListCmd) {
		t.Fatal("test assumption broken: docket project list is now watch-eligible")
	}

	rootCmd.SetArgs([]string{"project", "list", "--watch", "--json"})
	defer rootCmd.SetArgs(nil)

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("docket project list --watch must still be rejected; it never joined the allowlist")
	}

	code := errorCodeOf(t, err)
	if code != "VALIDATION_ERROR" {
		t.Errorf("error code = %s, want VALIDATION_ERROR", code)
	}
	if strings.Contains(err.Error(), "write commands") {
		t.Errorf("error = %q, still asserts the false write-only rule", err.Error())
	}
}
