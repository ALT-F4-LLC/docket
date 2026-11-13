package cli

import (
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// runThroughRoot drives `docket <args>` through the real root command and
// returns what it printed. cmd's flags and the root's persistent flags are
// reset before and after, so this invocation neither inherits another's
// values nor leaves its own behind.
func runThroughRoot(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	resetFlags := func() {
		reset := func(f *pflag.Flag) {
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
			} else {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		}
		cmd.Flags().VisitAll(reset)
		rootCmd.PersistentFlags().VisitAll(reset)
	}
	resetFlags()
	rootCmd.SetArgs(args)
	defer func() {
		rootCmd.SetArgs(nil)
		resetFlags()
	}()
	stdout := captureStdout(t)
	err := rootCmd.Execute()
	return stdout(), err
}

// TestIssueCreateOwnsItsProject: issue create resolves its project through
// internal/verb, so the root hook must skip its eager resolution.
func TestIssueCreateOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(createCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", createCmd.CommandPath())
	}
}

// TestIssueCreateThroughRootEchoesTheStoredIssue: a create run the way an
// operator runs it lands in the caller's project and echoes the stored row,
// its labels and its declared scope.
func TestIssueCreateThroughRootEchoesTheStoredIssue(t *testing.T) {
	conn := newVoteCreateStore(t)

	out, err := runThroughRoot(t, createCmd, "issue", "create", "--json",
		"-t", "through root", "-l", "routing", "--scope", "internal/verb/**")
	testsupport.Must(t, err, "issue create: %v", err)

	var env struct {
		Data struct {
			ID     string   `json:"id"`
			Title  string   `json:"title"`
			Labels []string `json:"labels"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("create payload is not JSON: %v\n%s", err, out)
	}
	id, err := model.ParseID(env.Data.ID)
	testsupport.Must(t, err, "parsing the echoed id %q: %v", env.Data.ID, err)

	stored, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if stored.Title != "through root" || env.Data.Title != stored.Title {
		t.Errorf("echoed title %q, stored %q; want %q", env.Data.Title, stored.Title, "through root")
	}
	if len(env.Data.Labels) != 1 || env.Data.Labels[0] != "routing" {
		t.Errorf("echoed labels %v, want [routing]", env.Data.Labels)
	}
	if got := scopeColumn(t, conn, id); !got.Valid || got.String != `["internal/verb/**"]` {
		t.Errorf("scope_globs = %v, want [\"internal/verb/**\"]", got)
	}
}
