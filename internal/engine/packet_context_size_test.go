package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A small contract can carry a large fragment. The expansion cap must refuse
// that packet before any activation state is committed.
func TestPacketContextSizeRefusesOversizedInclude(t *testing.T) {
	conn, dir := configRepo(t)
	writeConfigFile(t, dir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/a.md\"]\n")
	writeConfigFile(t, dir, "contracts/a.md",
		"---\npacket_includes:\n  - fragments/style.md\n---\nA\n")
	writeConfigFile(t, dir, "fragments/style.md", strings.Repeat("x", 4096))
	err := db.SetConfig(conn, 0, db.KeyContextErrorBytes, "1024")
	testsupport.Must(t, err, "setting context cap: %v", err)
	issue := createIssue(t, conn, "small subject", "small body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("activation accepted a fragment larger than the context cap")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	for _, want := range []string{"context.error_bytes", "1024", "implement@0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	assertNothingWritten(t, conn)
	if n := countRows(t, conn, "workflows"); n != 0 {
		t.Errorf("%d workflows survived the refused activation", n)
	}
}

// Accounting follows the renderer's first-occurrence, one-level traversal.
// In particular an include subsequently declared directly does not expand its
// own includes: that file was already visited at the shallower position.
func TestPacketContextSizeMatchesRenderedFiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packet  string
		counted []string
	}{
		{
			name:    "nested includes stop after one level",
			packet:  `["contracts/a.md"]`,
			counted: []string{"contracts/a.md", "fragments/shared.md"},
		},
		{
			name:    "shared and direct duplicates retain their first position",
			packet:  `["contracts/a.md", "contracts/b.md", "fragments/shared.md", "contracts/a.md"]`,
			counted: []string{"contracts/a.md", "fragments/shared.md", "contracts/b.md"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, dir := configRepo(t)
			writeConfigFile(t, dir, "workflows/auto-dev.toml",
				autoWorkflowSrc+"packet = "+tc.packet+"\n")
			files := map[string]string{
				"contracts/a.md":      "---\npacket_includes:\n  - fragments/shared.md\n---\nA\n",
				"contracts/b.md":      "---\npacket_includes:\n  - fragments/shared.md\n---\nB\n",
				"fragments/shared.md": "---\npacket_includes:\n  - fragments/deeper.md\n---\n" + strings.Repeat("s", 2048),
				"fragments/deeper.md": strings.Repeat("d", 32768),
			}
			for ref, body := range files {
				writeConfigFile(t, dir, ref, body)
			}
			err := db.SetConfig(conn, 0, db.KeyContextWarnBytes, "1024")
			testsupport.Must(t, err, "setting warning cap: %v", err)
			err = db.SetConfig(conn, 0, db.KeyContextErrorBytes, "8192")
			testsupport.Must(t, err, "setting error cap: %v", err)
			issue := createIssue(t, conn, "small subject", "small body", "task", nil)
			run := startRun(t, conn, issue)

			result, err := activate(conn, run.ID)
			testsupport.Must(t, err, "activation counted an unrendered grandchild: %v", err)
			steps, err := db.ListRunSteps(conn, run.ID)
			testsupport.Must(t, err, "listing steps: %v", err)
			runIssues, err := db.ListRunIssues(conn, run.ID)
			testsupport.Must(t, err, "listing run issues: %v", err)
			if len(steps) != 1 || len(runIssues) != 1 {
				t.Fatalf("got %d steps and %d issues, want one of each", len(steps), len(runIssues))
			}
			// The fixture has no metadata. File estimates retain the existing
			// conservative raw-byte convention, including frontmatter.
			want := len(runIssues[0].BodySnapshot) + len(runIssues[0].IssueSnapshot) + len(steps[0].Instance)
			for _, ref := range tc.counted {
				want += len(ref) + len(files[ref])
			}
			if steps[0].ContextBytes != want {
				t.Errorf("stored context_bytes = %d, want %d for unique rendered files", steps[0].ContextBytes, want)
			}
			if len(result.ContextWarnings) != 1 {
				t.Fatalf("got %d context warnings, want one for the included fragment", len(result.ContextWarnings))
			}
			if warning := result.ContextWarnings[0]; warning.Bytes != want || warning.Cap != 1024 {
				t.Errorf("warning = %+v, want %d bytes against cap 1024", warning, want)
			}
		})
	}
}

// Re-activation inherits old pins without their original in-memory sizes.
// Neither an edited contract's new bytes nor its new includes may turn that
// inheritance into an oversized-context refusal for a newly added issue.
func TestPacketContextSizePreservesInheritedPins(t *testing.T) {
	conn, dir := configRepo(t)
	writeConfigFile(t, dir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/a.md\"]\n")
	writeConfigFile(t, dir, "contracts/a.md",
		"---\npacket_includes:\n  - fragments/style.md\n---\nA\n")
	writeConfigFile(t, dir, "fragments/style.md", "original style\n")
	err := db.SetConfig(conn, 0, db.KeyContextErrorBytes, "1024")
	testsupport.Must(t, err, "setting context cap: %v", err)
	first := createIssue(t, conn, "first", "small body", "task", nil)
	run := startRun(t, conn, first)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "first activation: %v", err)
	original := pinsByKind(t, conn, run.ID, db.PinKindFile)

	rewriteConfigFile(t, dir, "contracts/a.md",
		"---\npacket_includes:\n  - fragments/new.md\n---\n"+strings.Repeat("a", 4096))
	writeConfigFile(t, dir, "fragments/new.md", strings.Repeat("n", 4096))
	second := createIssue(t, conn, "second", "small body", "task", nil)
	err = db.AddRunIssue(conn, run.ID, second)
	testsupport.Must(t, err, "adding second issue: %v", err)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "re-activation counted edited inherited bytes: %v", err)
	if !result.Reactivation || result.IssuesExpanded != 1 {
		t.Errorf("re-activation = %v, expanded = %d; want true and one", result.Reactivation, result.IssuesExpanded)
	}
	after := map[string]string{}
	for _, pin := range pinsByKind(t, conn, run.ID, db.PinKindFile) {
		after[pin.Ref] = pin.SHA256
	}
	for _, pin := range original {
		if after[pin.Ref] != pin.SHA256 {
			t.Errorf("inherited pin %q changed from %s to %s", pin.Ref, pin.SHA256, after[pin.Ref])
		}
	}
}
