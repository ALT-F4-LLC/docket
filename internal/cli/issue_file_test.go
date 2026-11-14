package cli

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestIssueFileCommandsOwnTheirProject: the file commands resolve their
// project through internal/verb, so the root hook must skip its eager
// resolution.
func TestIssueFileCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{fileAddCmd, fileRemoveCmd, fileListCmd} {
		if !verbOwnsProject(cmd) {
			t.Errorf("%s lacks the verbOwnsProject annotation", cmd.CommandPath())
		}
	}
}

// TestIssueFileRemoveEchoesTheRemainingFiles: file remove answers with the
// files still attached.
func TestIssueFileRemoveEchoesTheRemainingFiles(t *testing.T) {
	conn, id := seedIssue(t)
	runJSON(t, conn, fileAddCmd, id, "a/one.go", "b/two.go")

	data := runJSON(t, conn, fileRemoveCmd, id, "a/one.go")

	var files []string
	if err := json.Unmarshal(data, &files); err != nil {
		t.Fatalf("file remove data is not a path list: %v\n%s", err, data)
	}
	if want := []string{"b/two.go"}; !slices.Equal(files, want) {
		t.Errorf("file remove echoed %v, want %v", files, want)
	}
}

// TestIssueFileListRendersTheAttachedFiles pins the JSON list, the plain
// listing and the empty state.
func TestIssueFileListRendersTheAttachedFiles(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	conn, id := seedIssue(t)

	fileListCmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	restore := captureStdout(t)
	err := fileListCmd.RunE(fileListCmd, []string{id})
	empty := restore()
	testsupport.Must(t, err, "file list: %v", err)
	wantEmpty := "No files attached to " + id + "\nAdd one with: docket issue file add " + id + " <path>\n"
	if empty != wantEmpty {
		t.Errorf("empty file list =\n%q\nwant\n%q", empty, wantEmpty)
	}

	testsupport.Must(t, db.AttachFiles(conn, issueIDOf(t, id), []string{"a/one.go", "b/two.go"}, "tester"), "AttachFiles")

	restore = captureStdout(t)
	err = fileListCmd.RunE(fileListCmd, []string{id})
	plain := restore()
	testsupport.Must(t, err, "file list: %v", err)
	if want := "Files for " + id + ":\n  a/one.go\n  b/two.go\n\n"; plain != want {
		t.Errorf("file list =\n%q\nwant\n%q", plain, want)
	}

	var files []string
	data := runJSON(t, conn, fileListCmd, id)
	testsupport.Must(t, json.Unmarshal(data, &files), "decoding %s", data)
	if want := []string{"a/one.go", "b/two.go"}; !slices.Equal(files, want) {
		t.Errorf("file list --json = %v, want %v", files, want)
	}
}

// TestIssueFileCommandsOfAMissingIssueAreNotFound names the missing issue.
func TestIssueFileCommandsOfAMissingIssueAreNotFound(t *testing.T) {
	conn := newTestDB(t)
	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
	}{
		{fileAddCmd, []string{"DKT-9999", "a.go"}},
		{fileRemoveCmd, []string{"DKT-9999", "a.go"}},
		{fileListCmd, []string{"DKT-9999"}},
	} {
		_, err := runLeaseCommandOutput(t, conn, nil, tc.cmd, tc.args...)
		var cmdError *CmdError
		if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound ||
			cmdError.Error() != "issue DKT-9999 not found" {
			t.Errorf("issue file %s %v returned %v, want NOT_FOUND naming the issue", tc.cmd.Name(), tc.args, err)
		}
	}
}

func issueIDOf(t *testing.T, ref string) int {
	t.Helper()
	id, err := model.ParseID(ref)
	testsupport.Must(t, err, "ParseID(%q): %v", ref, err)
	return id
}
