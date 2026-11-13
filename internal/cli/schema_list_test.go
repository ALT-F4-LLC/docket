package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// inCallerProject registers a non-default project for identity and points
// cmd's config at it, with no resolved project in the context: the verb, not
// the root hook, decides which project the command reads.
func inCallerProject(t *testing.T, conn *sql.DB, cmd *cobra.Command, identity string) int {
	t.Helper()
	_, err := db.EnsureProject(conn, "/repo/default", "default", model.NowMS())
	testsupport.Must(t, err, "claiming the default project: %v", err)
	caller, err := db.EnsureProject(conn, identity, "caller", model.NowMS())
	testsupport.Must(t, err, "creating the caller's project: %v", err)
	if caller == db.DefaultProjectID {
		t.Fatalf("caller project = %d, want a non-default id", caller)
	}
	cmd.SetContext(context.WithValue(cmd.Context(), cfgKey,
		&config.Config{Identity: identity, Anchored: true}))
	return caller
}

func TestSchemaListOwnsItsProject(t *testing.T) {
	if _, ok := schemaListCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("schema list lacks the verbOwnsProject annotation")
	}
}

func TestSchemaListReadsTheConfiguredProject(t *testing.T) {
	conn := newTestDB(t)
	cmd := schemaListCmdWithDB(conn, 50)
	cmd.Flags().Set("name", "findings")
	caller := inCallerProject(t, conn, cmd, "/repo/caller")
	_, _, err := db.InsertSchema(conn,
		schemaRow("findings", 1, "findings.json", []byte(findingsSchema), "{}", caller),
		model.NowMS())
	testsupport.Must(t, err, "InsertSchema: %v", err)

	w, buf := bufWriter(true)
	err = runSchemaList(cmd, nil, w)
	testsupport.Must(t, err, "list: %v", err)

	var env struct {
		Data struct {
			Items []struct {
				Name    string `json:"name"`
				Version int    `json:"version"`
			} `json:"items"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "decoding the envelope: %v", err)
	if len(env.Data.Items) != 1 || env.Data.Items[0].Name != "findings" {
		t.Errorf("listed %+v, want the caller's findings@1", env.Data.Items)
	}
}
