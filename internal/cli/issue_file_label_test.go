package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// runJSON runs sub's RunE against conn with --json=v2 and returns the
// envelope's data field.
func runJSON(t *testing.T, conn *sql.DB, sub *cobra.Command, args ...string) json.RawMessage {
	t.Helper()
	sub.SetContext(context.WithValue(context.Background(), dbKey, conn))
	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v2"), "set --json")
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("json", "")
		rootCmd.PersistentFlags().Lookup("json").Changed = false
	})

	restore := captureStdout(t)
	runErr := sub.RunE(sub, args)
	out := restore()
	testsupport.Must(t, runErr, "%s %v: %v", sub.Name(), args, runErr)

	var env struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%s payload is not JSON: %v\n%s", sub.Name(), err, out)
	}
	if !env.OK {
		t.Fatalf("%s answered ok=false:\n%s", sub.Name(), out)
	}
	return env.Data
}

func seedIssue(t *testing.T) (*sql.DB, string) {
	t.Helper()
	conn := newTestDB(t)
	id, err := db.CreateIssue(conn, &model.Issue{
		Title:  "attach target",
		Status: model.StatusBacklog,
		Kind:   model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	return conn, model.FormatID(id)
}

// TestIssueFileAddEchoesEveryAttachedPath: file add answers with the issue's
// full file list as stored, so a path the store drops or the answer omits
// shows up here.
func TestIssueFileAddEchoesEveryAttachedPath(t *testing.T) {
	conn, id := seedIssue(t)

	data := runJSON(t, conn, fileAddCmd, id, "a/one.go", "b/two.go")

	var files []string
	if err := json.Unmarshal(data, &files); err != nil {
		t.Fatalf("file add data is not a path list: %v\n%s", err, data)
	}
	slices.Sort(files)
	if want := []string{"a/one.go", "b/two.go"}; !slices.Equal(files, want) {
		t.Errorf("file add echoed %v, want %v", files, want)
	}
}

// TestIssueLabelAddEchoesEveryAttachedLabel: label add answers with the
// issue's full label set as stored.
func TestIssueLabelAddEchoesEveryAttachedLabel(t *testing.T) {
	conn, id := seedIssue(t)

	data := runJSON(t, conn, labelAddCmd, id, "alpha", "beta")

	var labels []model.Label
	if err := json.Unmarshal(data, &labels); err != nil {
		t.Fatalf("label add data is not a label list: %v\n%s", err, data)
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	slices.Sort(names)
	if want := []string{"alpha", "beta"}; !slices.Equal(names, want) {
		t.Errorf("label add echoed %v, want %v", names, want)
	}
}
