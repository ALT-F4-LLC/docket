package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestUnknownRunVerbNamesTheIntendedVerb pins the second half of the fix.
//
// `docket run list` used to fall through to the help text and exit 0, which
// reads as "there are no runs" rather than "that verb does not exist". The
// error must both REFUSE and name where to go instead: the actual surface is
// `run status --active`, which nobody guesses from a help listing.
func TestUnknownRunVerbNamesTheIntendedVerb(t *testing.T) {
	parent := &cobra.Command{Use: "run"}
	for _, name := range []string{"status", "activate", "report", "start"} {
		parent.AddCommand(&cobra.Command{Use: name})
	}

	t.Run("suggests the replacement", func(t *testing.T) {
		err := unknownRunVerb(parent, "list")
		if err == nil {
			t.Fatal("unknownRunVerb(list) = nil, want an error")
		}
		got := err.Error()
		for _, want := range []string{`"list"`, "run status --active"} {
			if !strings.Contains(got, want) {
				t.Errorf("error %q missing %q", got, want)
			}
		}
	})

	t.Run("unmapped verb still lists what exists", func(t *testing.T) {
		got := unknownRunVerb(parent, "frobnicate").Error()
		if !strings.Contains(got, `"frobnicate"`) {
			t.Errorf("error %q does not name the verb typed", got)
		}
		// No suggestion is invented for a verb with no obvious target.
		if strings.Contains(got, "use \"") {
			t.Errorf("error %q invented a suggestion for an unmapped verb", got)
		}
		if !strings.Contains(got, "status") {
			t.Errorf("error %q does not list the available verbs", got)
		}
	})
}

// TestRunVerbListIsDerived guards against a hand-maintained verb list. The
// string is built from the registered subcommands, so a verb added later
// appears without anyone remembering to update an error message.
func TestRunVerbListIsDerived(t *testing.T) {
	parent := &cobra.Command{Use: "run"}
	parent.AddCommand(&cobra.Command{Use: "status"})
	parent.AddCommand(&cobra.Command{Use: "activate"})

	before := runVerbList(parent)
	if strings.Contains(before, "newverb") {
		t.Fatalf("runVerbList = %q, want no newverb before it is registered", before)
	}

	parent.AddCommand(&cobra.Command{Use: "newverb"})
	if after := runVerbList(parent); !strings.Contains(after, "newverb") {
		t.Errorf("runVerbList = %q, want it to pick up a newly registered verb", after)
	}
}

// TestRunVerbListSkipsHelpAndHidden keeps the suggestion list to verbs an
// operator can actually reach.
func TestRunVerbListSkipsHelpAndHidden(t *testing.T) {
	parent := &cobra.Command{Use: "run"}
	parent.AddCommand(&cobra.Command{Use: "status"})
	parent.AddCommand(&cobra.Command{Use: "help"})
	parent.AddCommand(&cobra.Command{Use: "secret", Hidden: true})

	got := runVerbList(parent)
	if !strings.Contains(got, "status") {
		t.Errorf("runVerbList = %q, want it to list status", got)
	}
	for _, unwanted := range []string{"help", "secret"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("runVerbList = %q, want it to omit %q", got, unwanted)
		}
	}
}

// TestNextModeHintNamesTheRunFlag pins the first half of the fix.
//
// Bare `docket next` is the ISSUE-mode verb and its output is frozen — QA
// section X exercises it in ten places, so making it an error would break a
// supported surface. The fix is a hint on the human channel naming the other
// mode, never on the JSON payload, which is a wire format.
func TestNextModeHintNamesTheRunFlag(t *testing.T) {
	for _, want := range []string{"--run", "ISSUES"} {
		if !strings.Contains(nextModeHint, want) {
			t.Errorf("nextModeHint = %q, want it to mention %q", nextModeHint, want)
		}
	}
}
