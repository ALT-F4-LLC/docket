package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestDocWriteCommandsRecordTheProjectsTheyRegister keeps the
// project-registered event the root hook wrote before the doc write commands
// resolved their own project, attributed to each command.
func TestDocWriteCommandsRecordTheProjectsTheyRegister(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	ref := model.FormatDocID(createDoc(t, conn, "subject", "note", "draft"))
	anchored := func(identity string) *config.Config {
		return &config.Config{Identity: identity, Anchored: true}
	}

	testsupport.Must(t, docCreateCmd.Flags().Set("title", "new doc"), "set --title")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/create"), docCreateCmd)
	testsupport.Must(t, err, "doc create: %v", err)
	testsupport.Must(t, docEditCmd.Flags().Set("title", "renamed"), "set --title")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/edit"), docEditCmd, ref)
	testsupport.Must(t, err, "doc edit: %v", err)
	testsupport.Must(t, docDeleteCmd.Flags().Set("force", "true"), "set --force")
	_, err = runLeaseCommandOutput(t, conn, anchored("/repos/delete"), docDeleteCmd, ref)
	testsupport.Must(t, err, "doc delete: %v", err)

	want := map[string]string{
		"/repos/create": "doc create",
		"/repos/edit":   "doc edit",
		"/repos/delete": "doc delete",
	}
	got := registrationEvents(t, conn)
	if len(got) != len(want) {
		t.Errorf("registration events = %v, want %v", got, want)
	}
	for identity, verb := range want {
		if got[identity] != verb {
			t.Errorf("registration of %s attributed to %q, want %q", identity, got[identity], verb)
		}
	}
}

func writeTempFile(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "body.txt")
	err := os.WriteFile(path, []byte(strings.Repeat("a", size)), 0o600)
	testsupport.Must(t, err, "WriteFile: %v", err)
	return path
}

func TestLoadDocBodyPathWithinCap(t *testing.T) {
	path := writeTempFile(t, maxDocBodySize)

	body, err := loadDocBody("@" + path)
	testsupport.Must(t, err, "loadDocBody: unexpected error: %v", err)
	if len(body) != maxDocBodySize {
		t.Fatalf("loadDocBody: got %d bytes, want %d", len(body), maxDocBodySize)
	}
}

func TestLoadDocBodyPathOverCap(t *testing.T) {
	path := writeTempFile(t, maxDocBodySize+1)

	_, err := loadDocBody("@" + path)
	if err == nil {
		t.Fatal("loadDocBody: expected error for over-cap file, got nil")
	}

	var cmdError *CmdError
	if !errors.As(err, &cmdError) {
		t.Fatalf("loadDocBody: error is not *CmdError: %v", err)
	}
	if cmdError.Code != output.ErrValidation {
		t.Fatalf("loadDocBody: got code %v, want %v", cmdError.Code, output.ErrValidation)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("loadDocBody: error %q does not mention exceeding the cap", err.Error())
	}
}

func TestLoadDocBodyPathFarOverCap(t *testing.T) {
	path := writeTempFile(t, maxDocBodySize*4)

	_, err := loadDocBody("@" + path)
	if err == nil {
		t.Fatal("loadDocBody: expected error for far-over-cap file, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("loadDocBody: error %q does not mention exceeding the cap", err.Error())
	}
}

func TestLoadDocBodyEmptyPath(t *testing.T) {
	_, err := loadDocBody("@")
	if err == nil {
		t.Fatal("loadDocBody: expected error for empty path, got nil")
	}
	var cmdError *CmdError
	if !errors.As(err, &cmdError) {
		t.Fatalf("loadDocBody: error is not *CmdError: %v", err)
	}
	if cmdError.Code != output.ErrValidation {
		t.Fatalf("loadDocBody: got code %v, want %v", cmdError.Code, output.ErrValidation)
	}
}
