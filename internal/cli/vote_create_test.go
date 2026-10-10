package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/pflag"
)

// `vote create --sealed` is the conversational counterpart of
// `vote.rule.<name>.sealed` (DKT-2447): a proposal a person or a harness opens
// by hand can be sealed the same way a workflow vote step's is. Driven through
// the real root command so the flag, the store hook and the create path are
// all exercised.
func TestVoteCreateSealedFlagStoresTheFlag(t *testing.T) {
	docketDir := filepath.Join(t.TempDir(), ".docket")
	err := os.MkdirAll(docketDir, 0o755)
	testsupport.Must(t, err, "creating the store directory: %v", err)
	t.Setenv("DOCKET_PATH", docketDir)

	conn, err := db.Open(filepath.Join(docketDir, "issues.db"))
	testsupport.Must(t, err, "opening: %v", err)
	t.Cleanup(func() { conn.Close() })
	testsupport.Must(t, db.Initialize(conn), "Initialize: %v", err)
	testsupport.Must(t, db.Migrate(conn), "Migrate: %v", err)

	rootCmd.SetArgs([]string{"vote", "create",
		"--description", "Sealed by hand", "--voters", "2", "--sealed", "--json"})
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		// Cobra keeps flag values across Execute calls; hand them back to
		// their defaults so no later test inherits this invocation's flags.
		voteCreateCmd.Flags().Visit(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	})
	err = rootCmd.Execute()
	testsupport.Must(t, err, "vote create --sealed: %v", err)

	created, err := db.GetProposal(conn, 1)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if !created.Sealed || created.Description != "Sealed by hand" {
		t.Errorf("created proposal = sealed:%v %q, want sealed \"Sealed by hand\"",
			created.Sealed, created.Description)
	}
}
