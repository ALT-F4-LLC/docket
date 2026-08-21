package cli

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-58: which invocations may bring a project row into being.
//
// EnsureProject ran in PersistentPreRunE on EVERY verb, so one command from a
// directory that was not a repository minted a permanent project row — a judge
// executor recording a step from the shared scratchpad root created one named
// `claude-501`, which then collided on the display prefix (DKT-60) and could
// not be removed by any supported verb (DKT-59).

// commandAt walks the real command tree to the named path, so these tests
// classify the ACTUAL registered commands rather than stand-ins that could
// drift from them.
func commandAt(t *testing.T, path string) *cobra.Command {
	t.Helper()
	cmd := rootCmd
	for _, name := range strings.Fields(path) {
		var next *cobra.Command
		for _, child := range cmd.Commands() {
			if child.Name() == name {
				next = child
				break
			}
		}
		if next == nil {
			t.Fatalf("no command %q under %q", name, cmd.CommandPath())
		}
		cmd = next
	}
	return cmd
}

// TestReadVerbsNeverRegisterAProject is the second half of the rule: reading is
// observation, and observation that creates rows is not observation.
//
// The listed paths are real commands, walked out of the tree, so a rename that
// silently changed a verb's classification fails here.
func TestReadVerbsNeverRegisterAProject(t *testing.T) {
	reads := []string{
		"issue list", "issue show", "issue graph", "issue log",
		"run report", "run status",
		"step list", "step show", "step gates", "step context", "step render",
		"step artifact", "step artifacts",
		"doc list", "doc show",
		"vote list", "vote show", "vote result",
		"workflow list", "workflow show", "workflow lint",
		"schema list", "schema show",
		"project list", "trust list", "events list", "config get",
		"dispatch verify",
		"guard stop", "guard gate", "guard record", "guard spawn",
		"board", "next", "plan", "stats", "export",
	}
	for _, path := range reads {
		if commandMayRegisterProject(commandAt(t, path)) {
			t.Errorf("`docket %s` may register a project; a read verb that "+
				"creates permanent state is how the store filled with rows "+
				"nobody asked for", path)
		}
	}

	// The counterpart: writes still register, or a fresh repository could never
	// get a project at all. `unknown leaves may register` is the deliberate
	// default, so this half is what stops the guard from becoming a blanket no.
	writes := []string{
		"issue create", "issue edit", "issue close", "issue delete",
		"run start", "run activate",
		"step claim", "step complete", "step approve",
		"doc create", "vote cast", "workflow register", "schema register",
		"project delete", "project set-prefix", "trust add", "config set",
		"dispatch open", "dispatch close", "init", "import",
	}
	for _, path := range writes {
		if !commandMayRegisterProject(commandAt(t, path)) {
			t.Errorf("`docket %s` may NOT register a project; a repository's "+
				"first write has to be able to create the project it writes to",
				path)
		}
	}
}

// TestRunAddressedVerbsCarryNoAmbientProject is DKT-58's first ask, verbatim:
// "a step-write verb already knows its run's project and must never derive one
// from cwd". Those verbs keep working from ANY directory — an executor's
// scratchpad included — because they read the project off the run.
func TestRunAddressedVerbsCarryNoAmbientProject(t *testing.T) {
	for _, path := range []string{
		"step complete", "step claim", "step fail",
		"dispatch open", "dispatch close", "trust add", "guard stop",
	} {
		if commandUsesAmbientProject(commandAt(t, path)) {
			t.Errorf("`docket %s` depends on the cwd's project; it is addressed "+
				"by a run, a step, or the store itself", path)
		}
	}
	for _, path := range []string{"issue create", "run start", "doc create"} {
		if !commandUsesAmbientProject(commandAt(t, path)) {
			t.Errorf("`docket %s` claims to need no ambient project, but it "+
				"writes rows into one", path)
		}
	}
}

// TestUnanchoredIdentityNeverRegisters is the measured defect itself: a verb run
// from a directory that is not a repository, against the global store.
func TestUnanchoredIdentityNeverRegisters(t *testing.T) {
	conn := newTestDB(t)
	before := projectCount(t, conn)

	cfg := &config.Config{
		Identity: "/private/tmp/claude-501",
		Source:   config.SourceGlobal,
		Anchored: false,
	}

	// A run-addressed write carries on with NO ambient project — the executor
	// that minted `claude-501` was running `step record`, and it must keep
	// working, just without creating anything.
	id, err := resolveInvocationProject(commandAt(t, "step complete"), conn, cfg)
	testsupport.Must(t, err, "step complete from a non-repo cwd: %v", err)
	if id != db.UnregisteredProjectID {
		t.Errorf("resolved project %d, want the unregistered sentinel %d",
			id, db.UnregisteredProjectID)
	}

	// A read gets an honest empty answer rather than somewhere to put one.
	id, err = resolveInvocationProject(commandAt(t, "issue list"), conn, cfg)
	testsupport.Must(t, err, "issue list from a non-repo cwd: %v", err)
	if id != db.UnregisteredProjectID {
		t.Errorf("read resolved project %d, want %d", id, db.UnregisteredProjectID)
	}

	// A write through the AMBIENT project is refused BY NAME. The alternatives
	// are both worse: an opaque foreign-key error at the INSERT, or the row
	// landing in a project the operator never named.
	_, err = resolveInvocationProject(commandAt(t, "issue create"), conn, cfg)
	if err == nil {
		t.Fatal("`issue create` from a non-repo cwd was allowed to resolve a project")
	}
	var cmdError *CmdError
	if !asCmdError(err, &cmdError) || cmdError.Code != output.ErrValidation {
		t.Errorf("refusal is %v, want a VALIDATION_ERROR", err)
	}
	if !strings.Contains(err.Error(), "not a git worktree") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}

	if after := projectCount(t, conn); after != before {
		t.Errorf("the store gained %d project row(s) from a directory that is "+
			"not a repository", after-before)
	}
}

// TestAnchoredWriteRegistersOnce is the other direction — the feature this
// guard must not break. A repository's first write creates its project, and the
// second write finds it rather than making another.
func TestAnchoredWriteRegistersOnce(t *testing.T) {
	conn := newTestDB(t)
	cfg := &config.Config{
		Identity: "/src/real.git",
		Source:   config.SourceGlobal,
		Anchored: true,
	}

	first, err := resolveInvocationProject(commandAt(t, "issue create"), conn, cfg)
	testsupport.Must(t, err, "first write: %v", err)
	second, err := resolveInvocationProject(commandAt(t, "issue create"), conn, cfg)
	testsupport.Must(t, err, "second write: %v", err)
	if first != second {
		t.Errorf("two writes from one repository resolved projects %d and %d",
			first, second)
	}

	// And a READ from the now-registered repository resolves it too: the lookup
	// runs first, unconditionally, so the gate never hides an existing row.
	read, err := resolveInvocationProject(commandAt(t, "issue list"), conn, cfg)
	testsupport.Must(t, err, "read after registration: %v", err)
	if read != first {
		t.Errorf("a read resolved project %d, want the registered %d — the "+
			"lookup precedes the gate", read, first)
	}
}

// TestExistingRowResolvesEvenWhenUnanchored: the junk rows this change stops
// creating must stay REACHABLE, or they could not be inspected and deleted.
func TestExistingRowResolvesEvenWhenUnanchored(t *testing.T) {
	conn := newTestDB(t)
	// A row already bound to a non-repository path — exactly what the defect
	// left behind in real stores.
	if _, err := db.EnsureProject(conn, "/src/first.git", "first.git", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	junk, err := db.EnsureProject(conn, "/private/tmp/claude-501", "claude-501", 2)
	testsupport.Must(t, err, "seeding the junk row: %v", err)

	cfg := &config.Config{
		Identity: "/private/tmp/claude-501",
		Source:   config.SourceGlobal,
		Anchored: false,
	}
	got, err := resolveInvocationProject(commandAt(t, "issue list"), conn, cfg)
	testsupport.Must(t, err, "listing from the junk row's directory: %v", err)
	if got != junk {
		t.Errorf("resolved %d, want the existing row %d; a row that cannot be "+
			"resolved cannot be inspected or deleted", got, junk)
	}
}

// projectCount is the assertion that matters most in this file: the store must
// not GROW from a directory that is not a repository.
func projectCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n)
	testsupport.Must(t, err, "counting projects: %v", err)
	return n
}
