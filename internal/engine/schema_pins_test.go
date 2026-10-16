package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// payloadWorkflow declares `payload` on its one executor step, which is what
// brings a USER-registered schema into the pin set. The fixture brings the
// builtin in by declaring `action = "aggregate"`; this brings in the other half.
const payloadWorkflow = `
[pipeline]
name = "pinned-payloads"
version = 1
[match]
kind = ["task"]
[[step]]
name = "assess"
after = []
executor = "someone"
emits = "report"
payload = "findings@1"
`

const pinFindingsSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "severity": {
        "type": "string",
        "enum": ["low", "medium", "high"],
        "ordered_enum": true
      }
    }
  }
}
`

// registerSchemaFixture puts a document in the registry the way `docket schema
// register` does — compile, derive, insert — so a test cannot register a
// document the CLI would have refused.
func registerSchemaFixture(t *testing.T, conn *sql.DB, name string, version int, body string) *model.Schema {
	t.Helper()

	compiled, err := schema.Compile(name, version, []byte(body))
	testsupport.Must(t, err, "compiling %s@%d: %v", name, version, err)
	ordered, err := json.Marshal(compiled.Ordered)
	testsupport.Must(t, err, "encoding the ordered index: %v", err)
	stored, _, err := db.InsertSchema(conn, &model.Schema{
		Name: name, Version: version,
		SourcePath:   name + ".json",
		SourceSHA256: workflow.SHA256([]byte(body)),
		Body:         body,
		Ordered:      string(ordered),
	}, nowMS)
	testsupport.Must(t, err, "registering %s@%d: %v", name, version, err)
	return stored
}

// pinsByKind indexes a run's pins for assertion.
func pinsByKind(t *testing.T, conn *sql.DB, runID int, kind string) []db.Pin {
	t.Helper()
	all, err := db.ListPins(conn, runID)
	testsupport.Must(t, err, "listing pins: %v", err)
	var out []db.Pin
	for _, p := range all {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

// TestActivatePinsReferencedSchemas pins one `pins` row per schema referenced
// by a bound workflow's steps, at the registry's own hash.
func TestActivatePinsReferencedSchemas(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(payloadWorkflow), "pinned-payloads.toml")
	registered := registerSchemaFixture(t, conn, "findings", 1, pinFindingsSchema)

	issue := createIssue(t, conn, "pins a schema", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	pins := pinsByKind(t, conn, run.ID, db.PinKindSchema)
	if len(pins) != 1 {
		t.Fatalf("recorded %d schema pins, want 1: %+v", len(pins), pins)
	}
	if pins[0].Ref != "findings@1" {
		t.Errorf("schema pin ref = %q, want findings@1", pins[0].Ref)
	}
	if pins[0].SHA256 != registered.SourceSHA256 {
		t.Errorf("schema pin hash = %q, want schemas.source_sha256 %q",
			pins[0].SHA256, registered.SourceSHA256)
	}
}

// TestActivateRefusesAnUnregisteredSchema covers a case unreachable through
// `workflow register` (V25a refuses it there) and reachable through a database
// restored from elsewhere — simulated here by deleting the row after
// registration, which is the only way to reach the state the check exists for.
func TestActivateRefusesAnUnregisteredSchema(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(payloadWorkflow), "pinned-payloads.toml")
	registerSchemaFixture(t, conn, "findings", 1, pinFindingsSchema)
	_, err := conn.Exec(`DELETE FROM schemas WHERE name = 'findings'`)
	testsupport.Must(t, err, "removing the schema: %v", err)

	issue := createIssue(t, conn, "pins a missing schema", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("expected activation to refuse an unregistered schema")
	}
	for _, want := range []string{"pinned-payloads@1", "assess", "findings@1", "docket schema register"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}

	// The fat transaction is all-or-nothing: a refusal at the pin stage leaves
	// no steps and no pins.
	for _, table := range []string{"steps", "pins"} {
		var n int
		if qerr := conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); qerr != nil {
			t.Fatalf("counting %s: %v", table, qerr)
		}
		if n != 0 {
			t.Errorf("%s holds %d rows after a refused activation, want 0", table, n)
		}
	}
}

// TestActivateRefusesANonArrayRootSchema covers a workflow registered before
// V25b existed: registerSource skips ValidateSchemas, so the bound workflow
// names a schema whose root `type` excludes "array" and activation is the
// only remaining check before every record of that step fails.
func TestActivateRefusesANonArrayRootSchema(t *testing.T) {
	for name, rootType := range map[string]string{
		"string": `"object"`,
		"list":   `["object", "null"]`,
	} {
		t.Run(name, func(t *testing.T) {
			conn := mustDB(t)
			registerSource(t, conn, []byte(payloadWorkflow), "pinned-payloads.toml")
			registerSchemaFixture(t, conn, "findings", 1, strings.Replace(
				pinFindingsSchema, `"type": "array"`, `"type": `+rootType, 1))

			issue := createIssue(t, conn, "pins an object-root schema", "a body", "task", nil)
			run := startRun(t, conn, issue)

			_, err := activate(conn, run.ID)
			if err == nil {
				t.Fatal("expected activation to refuse a schema whose root excludes arrays")
			}
			for _, want := range []string{"pinned-payloads@1", "assess", "findings@1", "array"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not name %q: %v", want, err)
				}
			}
			if n := countRows(t, conn, "pins"); n != 0 {
				t.Errorf("pins holds %d rows after a refused activation, want 0", n)
			}
		})
	}
}

// TestActivateRefusesAStoredSchemaThatDoesNotCompile: the root check needs a
// compiled document, so a stored body that no longer compiles (reachable only
// through a database edited or restored from elsewhere) is refused with the
// same coordinates as an unregistered one.
func TestActivateRefusesAStoredSchemaThatDoesNotCompile(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(payloadWorkflow), "pinned-payloads.toml")
	registerSchemaFixture(t, conn, "findings", 1, pinFindingsSchema)
	_, err := conn.Exec(`UPDATE schemas SET body = 'not json' WHERE name = 'findings'`)
	testsupport.Must(t, err, "corrupting the schema body: %v", err)

	issue := createIssue(t, conn, "pins a corrupt schema", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("expected activation to refuse a stored schema that does not compile")
	}
	for _, want := range []string{"pinned-payloads@1", "assess", "findings@1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if n := countRows(t, conn, "pins"); n != 0 {
		t.Errorf("pins holds %d rows after a refused activation, want 0", n)
	}
}

// TestReActivationInheritsSchemaPins: a schema registered mid-run does not
// enter a live run. Re-activation inherits the pin set, never recomputes it,
// and a schema is subject to that rule for the reason a workflow is — the
// acceptance criteria a run validates against must not change under it.
func TestReActivationInheritsSchemaPins(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(payloadWorkflow), "pinned-payloads.toml")
	first := registerSchemaFixture(t, conn, "findings", 1, pinFindingsSchema)

	issue := createIssue(t, conn, "inherits", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// A NEW version registers mid-run. The workflow names @1, so nothing about
	// this run changes — but the assertion is worth making, because "the schema
	// was updated" is the first thing an operator will assume when a threshold
	// behaves as it did yesterday.
	registerSchemaFixture(t, conn, "findings", 2, strings.Replace(
		pinFindingsSchema, `"low", "medium", "high"`, `"low", "high"`, 1))

	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "re-activate: %v", err)

	pins := pinsByKind(t, conn, run.ID, db.PinKindSchema)
	if len(pins) != 1 {
		t.Fatalf("recorded %d schema pins after re-activation, want 1: %+v", len(pins), pins)
	}
	if pins[0].Ref != "findings@1" || pins[0].SHA256 != first.SourceSHA256 {
		t.Errorf("the pin moved to %s/%s; RA2 inherits, never recomputes",
			pins[0].Ref, pins[0].SHA256)
	}
}

// TestTheBuiltinPinsWhenAnAggregateStepIsBound: `aggregate@1` pins like
// any other referenced schema. A run that cannot say which `aggregate@1` it
// computed against is a run that cannot explain its own output.
func TestTheBuiltinPinsWhenAnAggregateStepIsBound(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn) // its `reconcile` step is action = "aggregate"

	issue := createIssue(t, conn, "binds an aggregate", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Two, and the second is what proves the first is not an accident of the
	// fixture declaring nothing: `aggregate@1` arrives because the step's
	// ACTION is a builtin, and `findings@1` arrives because the same step
	// DECLARES it (V29). A run that recorded only one of them could not say
	// which document decided its output's shape.
	pins := pinsByKind(t, conn, run.ID, db.PinKindSchema)
	if len(pins) != 2 {
		t.Fatalf("recorded %d schema pins, want the builtin and the declared "+
			"schema: %+v", len(pins), pins)
	}

	byRef := make(map[string]db.Pin, len(pins))
	for _, p := range pins {
		byRef[p.Ref] = p
	}
	builtin, ok := byRef[schema.AggregateRef()]
	if !ok {
		t.Fatalf("%s is not pinned: %+v", schema.AggregateRef(), pins)
	}
	if builtin.SHA256 != schema.AggregateSHA256() {
		t.Errorf("builtin pin hash = %q, want the embedded document's %q",
			builtin.SHA256, schema.AggregateSHA256())
	}
	if _, ok := byRef["findings@1"]; !ok {
		t.Errorf("the schema the aggregate step declares is not pinned: %+v", pins)
	}
}

// TestAWorkflowWithNoPayloadDeclarationsPinsNoUserSchema is the dormancy half:
// a definition that names no schema and no aggregate brings nothing into the
// pin set, so the pin table of a schema-less run is what it was before schemas
// existed.
func TestAWorkflowWithNoPayloadDeclarationsPinsNoUserSchema(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(`
[pipeline]
name = "schema-free"
version = 1
[match]
kind = ["task"]
[[step]]
name = "work"
after = []
executor = "someone"
emits = "result"
`), "schema-free.toml")

	issue := createIssue(t, conn, "no schemas", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if pins := pinsByKind(t, conn, run.ID, db.PinKindSchema); len(pins) != 0 {
		t.Errorf("a schema-less workflow recorded %d schema pins: %+v", len(pins), pins)
	}
}

// TestSchemaPinsAreDeterministic is what the goldens rest on: the same
// inputs produce the same pin rows, in the same order, every time. An order
// that varied with map iteration would make the golden flap on nothing.
func TestSchemaPinsAreDeterministic(t *testing.T) {
	var first []string
	for range 5 {
		conn := mustDB(t)
		registerSource(t, conn, []byte(payloadWorkflow), "pinned-payloads.toml")
		registerSchemaFixture(t, conn, "findings", 1, pinFindingsSchema)
		registerSchemaFixture(t, conn, "other", 1, pinFindingsSchema)

		issue := createIssue(t, conn, "deterministic", "a body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)

		var got []string
		for _, p := range pinsByKind(t, conn, run.ID, db.PinKindSchema) {
			got = append(got, p.Ref+"|"+p.SHA256)
		}
		if first == nil {
			first = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("schema pins differ between activations:\n  %v\n  %v", first, got)
		}
	}
}

// TestSchemasRegisterBeforeWorkflows is the registration ordering contract,
// asserted over the real sort.
//
// THE BEHAVIORAL HALF LIVES IN autoregister_test.go, not here, because it needs
// an activated run: TestAutoRegistrationOrdersSchemasFirst asserts the order,
// and TestAutoRegistrationWrongOrderFails is its negative twin — the one that
// makes the first mean something by proving the refusal it avoids actually
// exists. This test keeps its original subject: the SORT, over paths, in
// isolation.
func TestSchemasRegisterBeforeWorkflows(t *testing.T) {
	got := RegistrationOrder([]string{
		".docket/config/workflows/standard-dev.toml",
		".docket/config/schemas/risk.json",
		".docket/config/workflows/a-first-alphabetically.toml",
		".docket/config/schemas/aaa.json",
		".docket/config/policy.toml",
	})
	want := []string{
		// Schemas in full, lexically, BEFORE anything else. A workflow whose
		// schema is in the same tree therefore always registers second, and the
		// zero-touch path never reaches the unregistered-schema refusal.
		".docket/config/schemas/aaa.json",
		".docket/config/schemas/risk.json",
		".docket/config/policy.toml",
		".docket/config/workflows/a-first-alphabetically.toml",
		".docket/config/workflows/standard-dev.toml",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("RegistrationOrder:\n got %v\nwant %v", got, want)
	}
}
