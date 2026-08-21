package cli

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-260 — `lease.ttl.<class>` never bound for the classes the docs implied.
//
// Every unforced reap of the current epoch killed a HEALTHY step 15.4-30.3
// minutes into its claim against a 15m default, and 312 completed non-write
// attempts show a survivorship wall at 14.4 minutes — a population truncated by
// a timer, not by work finishing.
//
// Core CANNOT fix this by learning what `read` and `write` mean: engine-spec §2
// makes the class name instance policy, writeClassOf is keyed on the declared
// bound precisely because a mechanism keyed on a class named `write` would be
// unimplementable, and TestNoWriteClassLiteral greps core for the literal. So
// the fix makes the mismatch visible where it is made.

// TestDeclaredClassesIncludesTheExecutorDefault is the fact the whole issue
// turns on: a step with no `class` carries its EXECUTOR NAME in that column.
//
// That is why the only strings ever seen there were executor names, and why no
// operator reading "per-class lease TTL" would have thought to configure one.
func TestDeclaredClassesIncludesTheExecutorDefault(t *testing.T) {
	conn := newTestDB(t)
	registerClassFixture(t, conn)

	classes, err := declaredClasses(conn, 1)
	testsupport.Must(t, err, "declaredClasses: %v", err)

	joined := strings.Join(classes, ",")
	if !strings.Contains(joined, "impl") {
		t.Errorf("declaredClasses = %v; a step that declares no class carries "+
			"its executor name, so `impl` must appear", classes)
	}
	if !strings.Contains(joined, "serialize") {
		t.Errorf("declaredClasses = %v; a step's explicit `class` must appear",
			classes)
	}
}

// TestLeaseTTLWarningFiresOnAClassNothingDeclares is the discoverability fix.
//
// `lease.ttl.read` is the exact key an operator reading the old help text would
// set, and it bound nothing. The warning has to name the classes that ARE
// declared, or it tells the operator they are wrong without telling them what
// is right.
func TestLeaseTTLWarningFiresOnAClassNothingDeclares(t *testing.T) {
	conn := newTestDB(t)
	registerClassFixture(t, conn)

	warning := leaseTTLClassWarning(conn, 1, db.KeyLeaseTTLPrefix+"read")
	if warning == "" {
		t.Fatal("no warning for lease.ttl.read; this is the key an operator " +
			"reading `per-class lease TTL` sets, and it binds nothing")
	}
	for _, needle := range []string{"read", "impl", "EXECUTOR NAME"} {
		if !strings.Contains(warning, needle) {
			t.Errorf("the warning does not mention %q:\n%s", needle, warning)
		}
	}
}

// TestLeaseTTLWarningIsSilentOnAClassThatBinds: the warning must not cry wolf.
func TestLeaseTTLWarningIsSilentOnAClassThatBinds(t *testing.T) {
	conn := newTestDB(t)
	registerClassFixture(t, conn)

	for _, key := range []string{
		db.KeyLeaseTTLPrefix + "impl",      // the executor default
		db.KeyLeaseTTLPrefix + "serialize", // an explicit class
		db.KeyLeaseTTLDefault,              // never a class at all
		"attempt.max",                      // not a lease key
	} {
		if got := leaseTTLClassWarning(conn, 1, key); got != "" {
			t.Errorf("leaseTTLClassWarning(%q) warned when it should not:\n%s",
				key, got)
		}
	}
}

// TestLeaseTTLWarningIsSilentWithNoWorkflows: a fresh project has nothing to
// disagree with, and a warning naming an empty list would be noise on the very
// first configuration somebody writes.
func TestLeaseTTLWarningIsSilentWithNoWorkflows(t *testing.T) {
	conn := newTestDB(t)
	if got := leaseTTLClassWarning(conn, 1, db.KeyLeaseTTLPrefix+"read"); got != "" {
		t.Errorf("warned on a project with no registered workflows:\n%s", got)
	}
}

// registerClassFixture registers a definition with one class-declaring step and
// one that leans on the executor default, which is the pair the warning has to
// tell apart.
//
// It goes through workflow.Parse and workflow.Canonical, the same path
// `workflow register` takes, so the `Parsed` JSON this reads back is the pinned
// interpretation the engine itself acts on — a fixture that hand-wrote the JSON
// could encode a shape the parser never produces.
func registerClassFixture(t *testing.T, conn *sql.DB) {
	t.Helper()
	const src = `
[pipeline]
name = "classes"
version = 1

[match]
kind = ["task"]

[limits]
serialize = { max = 1 }

[[step]]
name = "build"
executor = "impl"
emits = "out"
after = []

[[step]]
name = "commit"
executor = "impl"
class = "serialize"
emits = "done"
after = ["build"]
`
	def, err := workflow.Parse([]byte(src))
	testsupport.Must(t, err, "parsing the fixture: %v", err)
	testsupport.Must(t, workflow.Validate(def), "validating: %v", err)
	parsed, err := workflow.Canonical(def)
	testsupport.Must(t, err, "canonicalizing: %v", err)

	_, _, err = db.InsertWorkflow(conn, &model.Workflow{
		ProjectID: 1, Name: def.Pipeline.Name, Version: def.Pipeline.Version,
		Body: src, Parsed: string(parsed), SourceSHA256: "fixture",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
}
