package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4271: a blank comment body never launches $EDITOR without a terminal.

// markerEditor installs an EDITOR that records that it ran, so a spawned editor
// is observable as a file, and returns the marker's path.
func markerEditor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "editor-ran")
	script := filepath.Join(dir, "editor.sh")
	testsupport.Must(t, os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755), "writing the editor script")
	t.Setenv("EDITOR", script)
	return marker
}

// devNullStdin points os.Stdin at /dev/null for the test: not a terminal, and a
// character device, the case the pipe check alone does not catch.
func devNullStdin(t *testing.T) {
	t.Helper()
	null, err := os.Open(os.DevNull)
	testsupport.Must(t, err, "opening %s: %v", os.DevNull, err)
	saved := os.Stdin
	os.Stdin = null
	t.Cleanup(func() { os.Stdin = saved; null.Close() })
}

// TestBlankMessageIsAValidationErrorAndSpawnsNoEditor is the acceptance check:
// `issue comment add X -m ""` with stdin from /dev/null returns a validation
// error and starts no editor.
func TestBlankMessageIsAValidationErrorAndSpawnsNoEditor(t *testing.T) {
	for _, message := range []string{"", "   ", "\n\t \n"} {
		t.Run(fmt.Sprintf("%q", message), func(t *testing.T) {
			marker := markerEditor(t)
			devNullStdin(t)

			conn := newTestDB(t)
			issue := createIssue(t, conn, "commented", model.StatusBacklog, model.PriorityNone)
			cmd := cmdWithDB(conn)
			cmd.Flags().StringP("message", "m", "", "")
			cmd.Flags().String("idempotency-key", "", "")
			testsupport.Must(t, cmd.ParseFlags([]string{"-m", message}), "parsing flags")

			err := commentAddCmd.RunE(cmd, []string{model.FormatID(issue)})
			if err == nil || !strings.Contains(err.Error(), "message is empty") {
				t.Fatalf("issue comment add -m %q = %v; want a message-is-empty validation error", message, err)
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Error("the editor was spawned for a blank -m")
			}
			comments, err := db.ListComments(conn, issue)
			testsupport.Must(t, err, "ListComments: %v", err)
			if len(comments) != 0 {
				t.Errorf("%d comments recorded after a refusal", len(comments))
			}
		})
	}
}

// TestNoMessageAndNoTerminalSpawnsNoEditor: with no -m at all, stdin that is
// not a terminal, and no piped body, the editor still does not open.
func TestNoMessageAndNoTerminalSpawnsNoEditor(t *testing.T) {
	marker := markerEditor(t)
	devNullStdin(t)

	body, err := commentBodyOrEditor("", false, false)
	if err == nil || !strings.Contains(err.Error(), "editor opens only on a terminal") || body != "" {
		t.Fatalf("commentBodyOrEditor with no terminal = (%q, %v); want a terminal refusal", body, err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("the editor was spawned without a terminal")
	}
}

// TestCommentBodyPassesThroughUnchanged: a real body is never touched.
func TestCommentBodyPassesThroughUnchanged(t *testing.T) {
	body, err := commentBodyOrEditor("hello", true, false)
	if err != nil || body != "hello" {
		t.Errorf("commentBodyOrEditor(\"hello\") = (%q, %v)", body, err)
	}
}
