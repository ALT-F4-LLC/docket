package cli

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestStatusVerbsEchoStoredAssociations pins the status verbs to what
// `issue show` reports. `issue close --json=v2` answered "labels":[] and
// "files":[] for an issue that kept both, because the refetch read the issues
// row alone; `issue move`, the already-in-state paths of close and reopen, and
// a no-op `issue edit` had the same gap. Each verb's payload must carry the
// stored labels and files.
func TestStatusVerbsEchoStoredAssociations(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "labelled and filed", model.StatusTodo, model.PriorityNone)
	testsupport.Must(t, db.AddLabelsToIssue(conn, id, []string{"route-tend", "small"}, "", "test"), "adding labels")
	testsupport.Must(t, db.AttachFiles(conn, id, []string{"internal/engine/context.go"}, "test"), "attaching files")

	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v2"), "set --json")
	t.Cleanup(func() { _ = rootCmd.PersistentFlags().Set("json", "") })

	ref := model.FormatID(id)
	steps := []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"move to in-progress", moveCmd, []string{ref, "in-progress"}},
		{"move already in-progress", moveCmd, []string{ref, "in-progress"}},
		{"close", closeCmd, []string{ref}},
		{"close already closed", closeCmd, []string{ref}},
		{"reopen", reopenCmd, []string{ref}},
		{"reopen not closed", reopenCmd, []string{ref}},
		{"edit with no changes", editCmd, []string{ref}},
	}
	for _, s := range steps {
		s.cmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
		restore := captureStdout(t)
		runErr := s.cmd.RunE(s.cmd, s.args)
		out := restore()
		testsupport.Must(t, runErr, "%s: %v", s.name, runErr)

		var env struct {
			Data struct {
				Labels []string `json:"labels"`
				Files  []string `json:"files"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%s: payload is not JSON: %v\n%s", s.name, err, out)
		}

		labels, err := db.GetIssueLabels(conn, id)
		testsupport.Must(t, err, "GetIssueLabels: %v", err)
		files, err := db.GetIssueFiles(conn, id)
		testsupport.Must(t, err, "GetIssueFiles: %v", err)
		if !slices.Equal(env.Data.Labels, labels) {
			t.Errorf("%s: echoed labels %v, stored %v", s.name, env.Data.Labels, labels)
		}
		if !slices.Equal(env.Data.Files, files) {
			t.Errorf("%s: echoed files %v, stored %v", s.name, env.Data.Files, files)
		}
	}
}
