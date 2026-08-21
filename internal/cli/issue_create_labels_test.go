package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueCreateEchoesAppliedLabels is DKT-240.
//
// `issue create -l security-load-bearing --json` answered "labels":[] while the
// label was in fact linked — the create path refetched with db.GetIssue, which
// reads the issues row alone and never touches issue_labels. Read as a silent
// drop, it cost a repair `issue label add` on every create that used the flag.
// The label reaching the store was never the defect; the answer was.
func TestIssueCreateEchoesAppliedLabels(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)
	cmd.SetContext(context.WithValue(cmd.Context(), dbKey, conn))
	createCmd.SetContext(cmd.Context())

	testsupport.Must(t, createCmd.Flags().Set("title", "labelled"), "set --title")
	testsupport.Must(t, createCmd.Flags().Set("type", "bug"), "set --type")
	testsupport.Must(t, createCmd.Flags().Set("label", "security-load-bearing,routing"), "set --label")
	// --json is the root's persistent flag; jsonVersionOf finds it through
	// InheritedFlags, so it has to be set where it actually lives.
	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v1"), "set --json")
	t.Cleanup(func() {
		_ = createCmd.Flags().Set("label", "")
		_ = createCmd.Flags().Set("title", "")
		_ = rootCmd.PersistentFlags().Set("json", "")
	})

	restore := captureStdout(t)
	runErr := createCmd.RunE(createCmd, nil)
	out := restore()
	testsupport.Must(t, runErr, "issue create: %v", runErr)

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			ID     string   `json:"id"`
			Labels []string `json:"labels"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("create payload is not JSON: %v\n%s", err, out)
	}

	want := map[string]bool{"security-load-bearing": true, "routing": true}
	if len(env.Data.Labels) != len(want) {
		t.Fatalf("create echoed labels %v, want the two that were applied — an "+
			"empty or short list reads as a silent drop and provokes a repair "+
			"`issue label add` (DKT-240)", env.Data.Labels)
	}
	for _, got := range env.Data.Labels {
		if !want[got] {
			t.Errorf("create echoed unexpected label %q", got)
		}
	}

	// What was echoed is what is stored: the store is the reference, not a
	// second copy of the same in-memory slice.
	stored, err := db.GetIssueLabels(conn, 1)
	testsupport.Must(t, err, "GetIssueLabels: %v", err)
	if len(stored) != len(env.Data.Labels) {
		t.Errorf("stored labels %v disagree with the echoed %v", stored, env.Data.Labels)
	}
}

// TestHydrateIssueAssociationsFillsJoins pins the helper itself: a struct out
// of db.GetIssue carries no labels, files, or docs until it runs.
func TestHydrateIssueAssociationsFillsJoins(t *testing.T) {
	conn := newTestDB(t)
	id, err := db.CreateIssue(conn, &model.Issue{
		Title:  "joined",
		Status: model.StatusBacklog,
		Kind:   model.IssueKindBug,
	}, []string{"alpha"}, []string{"internal/engine/budget.go"})
	testsupport.Must(t, err, "CreateIssue: %v", err)

	bare, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if len(bare.Labels) != 0 || len(bare.Files) != 0 {
		t.Fatalf("db.GetIssue hydrated joins on its own (%v/%v) — this test's "+
			"premise, and the helper's reason to exist, is that it does not",
			bare.Labels, bare.Files)
	}

	testsupport.Must(t, hydrateIssueAssociations(conn, bare), "hydrate: %v", err)
	if len(bare.Labels) != 1 || bare.Labels[0] != "alpha" {
		t.Errorf("labels = %v, want [alpha]", bare.Labels)
	}
	if len(bare.Files) != 1 || bare.Files[0] != "internal/engine/budget.go" {
		t.Errorf("files = %v, want the one attached path", bare.Files)
	}
}
