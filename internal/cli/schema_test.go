package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

// findingsSchema is the §4.2 shape: an array of objects, one property declaring
// an ordered enum. `severity` is an INSTANCE TOKEN in test data, exactly as
// engine-spine §1.1 recorded the fixture's own — the registry never learns what
// it means, only that its values run in the order the author wrote them.
const findingsSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "severity": {
        "type": "string",
        "enum": ["info", "low", "medium", "high", "blocker"],
        "ordered_enum": true
      }
    },
    "required": ["severity"]
  }
}
`

func schemaListCmdWithDB(conn *sql.DB, limit int) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("name", "", "")
	cmd.Flags().Int("limit", limit, "")
	return cmd
}

func schemaShowCmdWithDB(conn *sql.DB, body bool) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().Bool("body", body, "")
	return cmd
}

// writeSchema drops a document in a temp file and returns its path.
func writeSchema(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.json")
	err := os.WriteFile(path, []byte(body), 0o644)
	testsupport.Must(t, err, "writing the schema: %v", err)
	return path
}

// registerSchema runs the register verb over a document.
func registerSchema(t *testing.T, conn *sql.DB, ref, body string) error {
	t.Helper()
	w, _ := bufWriter(true)
	return runSchemaRegister(schemaCmdWithDB(conn), []string{ref, writeSchema(t, body)}, w)
}

func schemaCmdWithDB(conn *sql.DB) *cobra.Command { return cmdWithDB(conn) }

func TestSchemaRegisterSucceeds(t *testing.T) {
	conn := newTestDB(t)
	err := registerSchema(t, conn, "findings@1", findingsSchema)
	testsupport.Must(t, err, "register: %v", err)

	w, buf := bufWriter(true)
	err = runSchemaShow(schemaShowCmdWithDB(conn, false), []string{"findings@1"}, w)
	testsupport.Must(t, err, "show: %v", err)

	var env struct {
		Data struct {
			Name          string   `json:"name"`
			Version       int      `json:"version"`
			OrderedFields []string `json:"ordered_fields"`
			Builtin       bool     `json:"builtin"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "decoding the envelope: %v", err)
	if env.Data.Name != "findings" || env.Data.Version != 1 {
		t.Errorf("registered %s@%d, want findings@1", env.Data.Name, env.Data.Version)
	}
	if len(env.Data.OrderedFields) != 1 || env.Data.OrderedFields[0] != "severity" {
		t.Errorf("ordered_fields = %v, want [severity]", env.Data.OrderedFields)
	}
	if env.Data.Builtin {
		t.Error("a user-registered schema is not builtin")
	}
}

// TestSchemaRegisterLandsInTheCallersProject pins DKT-20: the register verb
// resolves the INVOKING project the same way the read verbs do, rather than
// letting the insert fall through to the schemas column's DEFAULT 1. Before
// the fix a registration from any project but the default one reported
// success and then read back NOT_FOUND from the very cwd that registered it.
func TestSchemaRegisterLandsInTheCallersProject(t *testing.T) {
	conn := newTestDB(t)

	// EnsureProject claims the unclaimed default row in place before it
	// inserts, so two calls are what it takes to get a caller that is NOT the
	// default project.
	_, err := db.EnsureProject(conn, "/repo/default", "default", model.NowMS())
	testsupport.Must(t, err, "claiming the default project: %v", err)
	caller, err := db.EnsureProject(conn, "/repo/caller", "caller", model.NowMS())
	testsupport.Must(t, err, "creating the caller's project: %v", err)
	if caller == db.DefaultProjectID {
		t.Fatalf("caller project = %d, want a non-default id", caller)
	}

	cmd := schemaCmdWithDB(conn)
	cmd.SetContext(context.WithValue(cmd.Context(), projectKey, caller))
	w, _ := bufWriter(true)
	err = runSchemaRegister(cmd, []string{"findings@1", writeSchema(t, findingsSchema)}, w)
	testsupport.Must(t, err, "register: %v", err)

	if _, err := db.GetSchema(conn, caller, "findings", 1); err != nil {
		t.Errorf("GetSchema from the registering project: %v, want the row", err)
	}
	if _, err := db.GetSchema(conn, db.DefaultProjectID, "findings", 1); !errors.Is(err, db.ErrSchemaNotFound) {
		t.Errorf("GetSchema from the default project = %v, want ErrSchemaNotFound", err)
	}
}

// TestSchemaRegisterIsIdempotentOnIdenticalBytes is §4.4's second outcome: the
// same bytes again are a success that inserts nothing and does not bump
// row_version — or `--if-version` would fail for a caller that changed nothing.
func TestSchemaRegisterIsIdempotentOnIdenticalBytes(t *testing.T) {
	conn := newTestDB(t)
	for i := range 2 {
		if err := registerSchema(t, conn, "findings@1", findingsSchema); err != nil {
			t.Fatalf("register #%d: %v", i+1, err)
		}
	}

	var (
		rows       int
		rowVersion int
	)
	if err := conn.QueryRow(
		`SELECT COUNT(*), MAX(row_version) FROM schemas WHERE name = 'findings'`,
	).Scan(&rows, &rowVersion); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Errorf("schemas holds %d findings rows, want 1", rows)
	}
	if rowVersion != 1 {
		t.Errorf("row_version = %d after an idempotent re-register, want 1", rowVersion)
	}
}

// TestSchemaRefusalMatrix is §4.5's refusal table, by error code. The codes are
// what a harness branches on, so each is asserted rather than "an error was
// returned".
func TestSchemaRefusalMatrix(t *testing.T) {
	cases := []struct {
		name string
		// run performs the verb against a database that already holds
		// findings@1.
		run  func(t *testing.T, conn *sql.DB) error
		want output.ErrorCode
		// contains is a substring the refusal must carry, so an operator can act
		// on it.
		contains string
	}{
		{
			name: "an absent file",
			run: func(t *testing.T, conn *sql.DB) error {
				w, _ := bufWriter(true)
				return runSchemaRegister(schemaCmdWithDB(conn),
					[]string{"x@1", filepath.Join(t.TempDir(), "nope.json")}, w)
			},
			want: output.ErrNotFound, contains: "not found",
		},
		{
			name: "a malformed reference",
			run: func(t *testing.T, conn *sql.DB) error {
				w, _ := bufWriter(true)
				return runSchemaRegister(schemaCmdWithDB(conn),
					[]string{"findings", writeSchema(t, findingsSchema)}, w)
			},
			want: output.ErrValidation, contains: "name@version",
		},
		{
			name: "a version below 1",
			run: func(t *testing.T, conn *sql.DB) error {
				w, _ := bufWriter(true)
				return runSchemaRegister(schemaCmdWithDB(conn),
					[]string{"findings@0", writeSchema(t, findingsSchema)}, w)
			},
			want: output.ErrValidation, contains: "integer >= 1",
		},
		{
			name: "malformed JSON",
			run: func(t *testing.T, conn *sql.DB) error {
				return registerSchema(t, conn, "broken@1", `{"type": `)
			},
			want: output.ErrValidation, contains: "not valid JSON",
		},
		{
			name: "a document that does not compile",
			run: func(t *testing.T, conn *sql.DB) error {
				return registerSchema(t, conn, "broken@1",
					`{"type": "array", "items": {"required": "severity"}}`)
			},
			want: output.ErrValidation, contains: "does not compile",
		},
		{
			name: "an O2 violation names the property path",
			run: func(t *testing.T, conn *sql.DB) error {
				return registerSchema(t, conn, "broken@1", `{
  "type": "array",
  "items": {"type": "object", "properties": {"severity": {"ordered_enum": true}}}
}`)
			},
			want: output.ErrValidation, contains: "items.properties.severity",
		},
		{
			name: "different bytes at a registered name@version",
			run: func(t *testing.T, conn *sql.DB) error {
				return registerSchema(t, conn, "findings@1", `{"type": "array"}`)
			},
			want: output.ErrConflict, contains: "is registered as",
		},
		{
			name: "showing an unregistered schema",
			run: func(t *testing.T, conn *sql.DB) error {
				w, _ := bufWriter(true)
				return runSchemaShow(schemaShowCmdWithDB(conn, false), []string{"nope@1"}, w)
			},
			want: output.ErrNotFound, contains: "not registered",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := newTestDB(t)
			err := registerSchema(t, conn, "findings@1", findingsSchema)
			testsupport.Must(t, err, "seeding findings@1: %v", err)

			err = tc.run(t, conn)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if got := codeOf(t, err); got != tc.want {
				t.Errorf("code = %s, want %s (%v)", got, tc.want, err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("refusal %q does not carry %q", err.Error(), tc.contains)
			}

			// Every refusal writes nothing. The registry holds the one seeded
			// row plus the builtin, and neither has moved.
			var rows, maxVersion int
			if err := conn.QueryRow(
				`SELECT COUNT(*), MAX(row_version) FROM schemas`).Scan(&rows, &maxVersion); err != nil {
				t.Fatalf("counting: %v", err)
			}
			if rows != 2 {
				t.Errorf("schemas holds %d rows after a refusal, want 2 (the builtin and findings@1)", rows)
			}
			if maxVersion != 1 {
				t.Errorf("row_version moved to %d during a refusal", maxVersion)
			}
		})
	}
}

// TestSchemaConflictNamesBothHashes is §4.4's CONFLICT row: an operator needs to
// know WHICH bytes are registered and which they just handed over, or the only
// remedy is to guess.
func TestSchemaConflictNamesBothHashes(t *testing.T) {
	conn := newTestDB(t)
	err := registerSchema(t, conn, "findings@1", findingsSchema)
	testsupport.Must(t, err, "register: %v", err)

	const other = `{"type": "array"}`
	err = registerSchema(t, conn, "findings@1", other)
	if err == nil {
		t.Fatal("expected a CONFLICT")
	}

	var registered string
	if qerr := conn.QueryRow(
		`SELECT source_sha256 FROM schemas WHERE name = 'findings'`).Scan(&registered); qerr != nil {
		t.Fatalf("reading the stored hash: %v", qerr)
	}
	if !strings.Contains(err.Error(), registered) {
		t.Errorf("the refusal does not name the REGISTERED hash: %v", err)
	}
	if offered := workflow.SHA256([]byte(other)); !strings.Contains(err.Error(), offered) {
		t.Errorf("the refusal does not name the OFFERED hash: %v", err)
	}
}

// TestSchemaListIsACollection is §4.5's list row: a Collection envelope under
// v2, per the `workflow list` precedent, with a total a limit cannot distort.
func TestSchemaListIsACollection(t *testing.T) {
	conn := newTestDB(t)
	err := registerSchema(t, conn, "findings@1", findingsSchema)
	testsupport.Must(t, err, "register: %v", err)

	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	err = runSchemaList(schemaListCmdWithDB(conn, 1), nil, w)
	testsupport.Must(t, err, "list: %v", err)

	var env struct {
		Data struct {
			Items []struct {
				Name       string `json:"name"`
				Builtin    bool   `json:"builtin"`
				RowVersion int    `json:"row_version"`
			} `json:"items"`
			Total     int  `json:"total"`
			Truncated bool `json:"truncated"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "decoding the envelope: %v", err)
	// The builtin plus findings@1, limited to one: the total is the true count
	// before the limit, so truncation is computable rather than guessed.
	if env.Data.Total != 2 {
		t.Errorf("total = %d, want 2", env.Data.Total)
	}
	if !env.Data.Truncated {
		t.Error("truncated = false with a limit of 1 over 2 rows")
	}
	if len(env.Data.Items) != 1 {
		t.Fatalf("returned %d items under --limit 1", len(env.Data.Items))
	}
	if env.Data.Items[0].RowVersion != 1 {
		t.Errorf("v2 items carry no row_version: %+v", env.Data.Items[0])
	}
}

// TestSchemaListV1CarriesNoRowVersion is the dormancy rule at the envelope: the
// CAS column surfaces under --json=v2 only.
func TestSchemaListV1CarriesNoRowVersion(t *testing.T) {
	conn := newTestDB(t)
	w, buf := bufWriter(true)
	err := runSchemaList(schemaListCmdWithDB(conn, 50), nil, w)
	testsupport.Must(t, err, "list: %v", err)
	if strings.Contains(buf.String(), "row_version") {
		t.Errorf("v1 output carries row_version: %s", buf.String())
	}
}

// TestSchemaShowBodyIsTheRegisteredBytes is §4.5's `--body` row: what a run
// validates against, byte for byte, not a re-serialization.
func TestSchemaShowBodyIsTheRegisteredBytes(t *testing.T) {
	conn := newTestDB(t)
	err := registerSchema(t, conn, "findings@1", findingsSchema)
	testsupport.Must(t, err, "register: %v", err)

	w, buf := bufWriter(true)
	err = runSchemaShow(schemaShowCmdWithDB(conn, true), []string{"findings@1"}, w)
	testsupport.Must(t, err, "show --body: %v", err)

	var env struct {
		Data struct {
			Body string `json:"body"`
		} `json:"data"`
	}
	err = json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "decoding the envelope: %v", err)
	if env.Data.Body != findingsSchema {
		t.Errorf("--body is not the registered bytes\n  got:  %q\n  want: %q",
			env.Data.Body, findingsSchema)
	}
}

// TestTheBuiltinIsVisibleAndMarked is §3's honesty clause made observable: v9
// seeds exactly one row, and an operator can see both that it is there and that
// it came from the binary.
func TestTheBuiltinIsVisibleAndMarked(t *testing.T) {
	conn := newTestDB(t)

	w, buf := bufWriter(true)
	if err := runSchemaShow(schemaShowCmdWithDB(conn, false), []string{schema.AggregateRef()}, w); err != nil {
		t.Fatalf("show %s: %v", schema.AggregateRef(), err)
	}

	var env struct {
		Data struct {
			Builtin      bool   `json:"builtin"`
			SourcePath   string `json:"source_path"`
			SourceSHA256 string `json:"source_sha256"`
		} `json:"data"`
	}
	err := json.Unmarshal(buf.Bytes(), &env)
	testsupport.Must(t, err, "decoding the envelope: %v", err)
	if !env.Data.Builtin {
		t.Error("the shipped schema is not marked builtin")
	}
	if env.Data.SourcePath != "" {
		t.Errorf("the builtin reports source_path %q; its bytes came from the binary",
			env.Data.SourcePath)
	}
	if env.Data.SourceSHA256 != schema.AggregateSHA256() {
		t.Errorf("the seeded hash is not the embedded document's")
	}
}
