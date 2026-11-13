package cli

import (
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestSchemaShowOwnsItsProject(t *testing.T) {
	if _, ok := schemaShowCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("schema show lacks the verbOwnsProject annotation")
	}
}

func TestSchemaShowReadsTheConfiguredProject(t *testing.T) {
	conn := newTestDB(t)
	cmd := schemaShowCmdWithDB(conn, false)
	caller := inCallerProject(t, conn, cmd, "/repo/caller")
	_, _, err := db.InsertSchema(conn,
		schemaRow("findings", 1, "findings.json", []byte(findingsSchema), "{}", caller),
		model.NowMS())
	testsupport.Must(t, err, "InsertSchema: %v", err)

	w, buf := bufWriter(true)
	err = runSchemaShow(cmd, []string{"findings@1"}, w)
	testsupport.Must(t, err, "show: %v", err)

	var env struct {
		Data struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "decoding the envelope: %v", err)
	if env.Data.Name != "findings" || env.Data.Version != 1 {
		t.Errorf("showed %s@%d, want the caller's findings@1", env.Data.Name, env.Data.Version)
	}
}
