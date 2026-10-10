package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
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

// resetCreateFlags returns createCmd's flags and the root --json flag to their
// defaults with Changed cleared. Set(name, "") is not enough: it leaves
// Changed true, and applyScope keys on Changed("scope"), so a leaked scope
// would be written by every later create in the package.
func resetCreateFlags() {
	createCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	})
	_ = rootCmd.PersistentFlags().Set("json", "")
	rootCmd.PersistentFlags().Lookup("json").Changed = false
}

// TestIssueCreateReplayWritesNothing: a create replayed under an idempotency
// key returns the earlier issue and leaves it exactly as it was. The replay
// used to overwrite the earlier issue's scope with its own --scope while
// version, updated_at, and the activity log stayed put, so nothing recorded
// the change.
func TestIssueCreateReplayWritesNothing(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)
	cmd.SetContext(context.WithValue(cmd.Context(), dbKey, conn))
	createCmd.SetContext(cmd.Context())
	resetCreateFlags()
	t.Cleanup(resetCreateFlags)

	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v1"), "set --json")
	testsupport.Must(t, createCmd.Flags().Set("idempotency-key", "K"), "set --idempotency-key")

	create := func(title, scope string) string {
		t.Helper()
		testsupport.Must(t, createCmd.Flags().Set("title", title), "set --title")
		testsupport.Must(t, createCmd.Flags().Set("scope", scope), "set --scope")
		restore := captureStdout(t)
		runErr := createCmd.RunE(createCmd, nil)
		out := restore()
		testsupport.Must(t, runErr, "issue create --scope %s: %v", scope, runErr)
		var env struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("create payload is not JSON: %v\n%s", err, out)
		}
		// Replace, not a second Set: a changed slice flag appends on Set.
		testsupport.Must(t, createCmd.Flags().Lookup("scope").Value.(pflag.SliceValue).Replace(nil), "clear --scope")
		return env.Data.ID
	}

	firstID := create("original", "a/**")
	id, err := model.ParseID(firstID)
	testsupport.Must(t, err, "ParseID(%q): %v", firstID, err)

	scopeBefore, err := db.IssueScopeGlobs(conn, id)
	testsupport.Must(t, err, "IssueScopeGlobs: %v", err)
	if scopeBefore != `["a/**"]` {
		t.Fatalf("first create under a fresh key stored scope %q, want %q", scopeBefore, `["a/**"]`)
	}

	// Pin updated_at to a past instant so a replay that rewrites it within
	// the same wall-clock second still shows up as a change.
	_, err = conn.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, "2020-01-01T00:00:00Z", id)
	testsupport.Must(t, err, "pin updated_at: %v", err)
	before, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	activityBefore, err := db.CountActivity(conn, id)
	testsupport.Must(t, err, "CountActivity: %v", err)

	replayID := create("replayed", "b/**")
	if replayID != firstID {
		t.Fatalf("replay under the same key returned %s, want the earlier issue %s", replayID, firstID)
	}

	scopeAfter, err := db.IssueScopeGlobs(conn, id)
	testsupport.Must(t, err, "IssueScopeGlobs: %v", err)
	if scopeAfter != scopeBefore {
		t.Errorf("replay changed the earlier issue's scope from %q to %q", scopeBefore, scopeAfter)
	}
	after, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if after.Version != before.Version {
		t.Errorf("replay changed version from %d to %d", before.Version, after.Version)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("replay changed updated_at from %s to %s", before.UpdatedAt, after.UpdatedAt)
	}
	activityAfter, err := db.CountActivity(conn, id)
	testsupport.Must(t, err, "CountActivity: %v", err)
	if activityAfter != activityBefore {
		t.Errorf("replay changed the activity row count from %d to %d", activityBefore, activityAfter)
	}
}

// TestIssueCreateReplayOfKeyRecordedElsewhereLeavesIssue: the key is recorded
// straight through the store, as another process would commit it, so the only
// thing that can tell the CLI the create is a replay is CreateIssueIdempotent's
// own answer. A replay with --scope b/** must leave the earlier issue's scope,
// version, updated_at, and activity untouched, while a fresh key still stores
// its --scope.
func TestIssueCreateReplayOfKeyRecordedElsewhereLeavesIssue(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)
	cmd.SetContext(context.WithValue(cmd.Context(), dbKey, conn))
	createCmd.SetContext(cmd.Context())
	resetCreateFlags()
	t.Cleanup(resetCreateFlags)

	id, inserted, err := db.CreateIssueIdempotent(conn, &model.Issue{
		Title:    "recorded elsewhere",
		Status:   model.StatusBacklog,
		Priority: model.PriorityNone,
		Kind:     model.IssueKindTask,
	}, nil, nil, "K")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	if !inserted {
		t.Fatal("seeding create under a fresh key reported no insert")
	}
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, id, `["a/**"]`), "SetIssueScopeGlobs")
	_, err = conn.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, "2020-01-01T00:00:00Z", id)
	testsupport.Must(t, err, "pin updated_at: %v", err)

	before, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	scopeBefore, err := db.IssueScopeGlobs(conn, id)
	testsupport.Must(t, err, "IssueScopeGlobs: %v", err)
	activityBefore, err := db.CountActivity(conn, id)
	testsupport.Must(t, err, "CountActivity: %v", err)

	create := func(key, scope string) int {
		t.Helper()
		testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v1"), "set --json")
		testsupport.Must(t, createCmd.Flags().Set("idempotency-key", key), "set --idempotency-key")
		testsupport.Must(t, createCmd.Flags().Set("title", "via cli"), "set --title")
		testsupport.Must(t, createCmd.Flags().Set("scope", scope), "set --scope")
		restore := captureStdout(t)
		runErr := createCmd.RunE(createCmd, nil)
		out := restore()
		testsupport.Must(t, runErr, "issue create --idempotency-key %s --scope %s: %v", key, scope, runErr)
		testsupport.Must(t, createCmd.Flags().Lookup("scope").Value.(pflag.SliceValue).Replace(nil), "clear --scope")
		var env struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("create payload is not JSON: %v\n%s", err, out)
		}
		got, err := model.ParseID(env.Data.ID)
		testsupport.Must(t, err, "ParseID(%q): %v", env.Data.ID, err)
		return got
	}

	if replayID := create("K", "b/**"); replayID != id {
		t.Fatalf("replay under K returned issue %d, want the recorded %d", replayID, id)
	}

	scopeAfter, err := db.IssueScopeGlobs(conn, id)
	testsupport.Must(t, err, "IssueScopeGlobs: %v", err)
	if scopeAfter != scopeBefore {
		t.Errorf("replay changed the earlier issue's scope from %q to %q", scopeBefore, scopeAfter)
	}
	after, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if after.Version != before.Version {
		t.Errorf("replay changed version from %d to %d", before.Version, after.Version)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("replay changed updated_at from %s to %s", before.UpdatedAt, after.UpdatedAt)
	}
	activityAfter, err := db.CountActivity(conn, id)
	testsupport.Must(t, err, "CountActivity: %v", err)
	if activityAfter != activityBefore {
		t.Errorf("replay changed the activity row count from %d to %d", activityBefore, activityAfter)
	}

	freshID := create("K2", "a/**")
	if freshID == id {
		t.Fatalf("create under fresh key K2 returned the earlier issue %d", id)
	}
	freshScope, err := db.IssueScopeGlobs(conn, freshID)
	testsupport.Must(t, err, "IssueScopeGlobs: %v", err)
	if freshScope != `["a/**"]` {
		t.Errorf("fresh create under K2 stored scope %q, want %q", freshScope, `["a/**"]`)
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
