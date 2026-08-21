package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-453: the display PREFIX is a resolver key.
//
// `docket issue list --project FLX` answered NOT_FOUND. The prefix is the one
// identifier every issue id embeds — FLX-141 — and the second column of
// `project list`, so it is the first thing a caller reaches for; it was the
// only one of those columns the resolver did not accept. A RUN-37 conductor
// guessed it, got the refusal, and fell back to an unfiltered listing of every
// project in the store.
//
// The other half of the fix is that there is now ONE resolver. `issue move
// --project FLX` already worked, through a second copy of the ladder in
// issue_move.go — the divergence is what made the defect possible, so these
// tests exercise resolveProjectRef itself plus both surfaces that call it.

// registerProject seeds a project row and reads it back. The prefix is DERIVED
// at registration (db.availablePrefix), so a fixture that needs a particular
// one sets it explicitly rather than assuming what the derivation produced.
func registerProject(t *testing.T, conn *sql.DB, identity, name, prefix string, nowMS int64) *model.Project {
	t.Helper()
	id, err := db.EnsureProject(conn, identity, name, nowMS)
	testsupport.Must(t, err, "registering %s: %v", identity, err)
	if prefix != "" {
		if err := db.SetProjectPrefix(conn, id, prefix); err != nil {
			t.Fatalf("setting the prefix for %s: %v", identity, err)
		}
	}
	p, err := db.GetProject(conn, id)
	testsupport.Must(t, err, "reading back project %d: %v", id, err)
	return p
}

// twoProjects is the fixture every case here needs: the caller's own project,
// and another one holding a distinct prefix and one issue.
func twoProjects(t *testing.T, conn *sql.DB) (here, there *model.Project) {
	t.Helper()
	here = registerProject(t, conn, "/src/here.git", "here.git", "", 1)
	there = registerProject(t, conn, "/src/manifest-flux.git", "manifest-flux.git", "FLX", 2)
	if here.ID == there.ID {
		t.Fatalf("the fixture needs two distinct projects, got %d twice", here.ID)
	}
	return here, there
}

// TestProjectRefResolvesTheDisplayPrefix is the defect itself.
func TestProjectRefResolvesTheDisplayPrefix(t *testing.T) {
	conn := newTestDB(t)
	_, there := twoProjects(t, conn)

	// Typed as displayed, and typed as it falls out of a shell without the shift
	// key: a prefix is stored uppercase, so the lowercase spelling is the same
	// key and refusing it would reproduce the defect one keystroke lower.
	for _, ref := range []string{"FLX", "flx", "Flx"} {
		got, err := resolveProjectRef(conn, ref)
		if err != nil {
			t.Fatalf("resolveProjectRef(%q): %v — the prefix is the identifier "+
				"every issue id displays", ref, err)
		}
		if got.ID != there.ID {
			t.Errorf("resolveProjectRef(%q) = project %d, want %d",
				ref, got.ID, there.ID)
		}
	}
}

// TestProjectRefKeepsNameIdentityAndIDResolution is DKT-453's third acceptance
// criterion, verbatim: "name/path/row-id resolution unchanged". A fourth key
// that displaced any of the three would trade one broken guess for another.
func TestProjectRefKeepsNameIdentityAndIDResolution(t *testing.T) {
	conn := newTestDB(t)
	_, there := twoProjects(t, conn)

	for _, ref := range []string{
		"manifest-flux.git",      // name
		"/src/manifest-flux.git", // identity path
	} {
		got, err := resolveProjectRef(conn, ref)
		if err != nil {
			t.Fatalf("resolveProjectRef(%q): %v", ref, err)
		}
		if got.ID != there.ID {
			t.Errorf("resolveProjectRef(%q) = project %d, want %d",
				ref, got.ID, there.ID)
		}
	}

	byID, err := resolveProjectRef(conn, strconv.Itoa(there.ID))
	if err != nil {
		t.Fatalf("resolveProjectRef by row id: %v", err)
	}
	if byID.ID != there.ID {
		t.Errorf("row id resolved project %d, want %d", byID.ID, there.ID)
	}
}

// TestProjectRefMissesStayNotFound: a typo must read as a miss, with the
// refusal pointing at the listing that shows every key. A miss that resolved to
// something would be worse than the defect — it would list the wrong project's
// issues under the caller's own prefix.
func TestProjectRefMissesStayNotFound(t *testing.T) {
	conn := newTestDB(t)
	twoProjects(t, conn)

	for _, ref := range []string{"NOPE", "no-such-project.git", "4242"} {
		_, err := resolveProjectRef(conn, ref)
		if err == nil {
			t.Fatalf("resolveProjectRef(%q) resolved something", ref)
		}
		assertErrCode(t, err, output.ErrNotFound)
		if !strings.Contains(err.Error(), "project") {
			t.Errorf("the refusal for %q does not say what was not found: %v", ref, err)
		}
	}
}

// TestProjectRefRefusesAnAmbiguousPrefix is the second acceptance criterion.
//
// Prefixes are unique BY CONSTRUCTION — registration picks a free one and
// `project set-prefix` refuses one another project holds — so the collision is
// forced here the only ways it can really arise: a direct write, an import, a
// hand-edited store. Resolving it by picking the lowest id would answer a
// question nobody asked, since naming WHICH project is the whole point of the
// flag.
func TestProjectRefRefusesAnAmbiguousPrefix(t *testing.T) {
	conn := newTestDB(t)
	_, there := twoProjects(t, conn)
	other := registerProject(t, conn, "/src/flux-ui.git", "flux-ui.git", "", 3)

	_, err := conn.Exec(`UPDATE projects SET prefix = 'FLX' WHERE id = ?`, other.ID)
	testsupport.Must(t, err, "forcing the prefix collision: %v", err)

	_, err = resolveProjectRef(conn, "FLX")
	if err == nil {
		t.Fatal("an ambiguous prefix resolved to one of the candidates instead " +
			"of refusing")
	}
	assertErrCode(t, err, output.ErrValidation)
	for _, want := range []string{
		there.Identity, other.Identity, // which projects
		"2", "3", // and the ids to address them by
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestProjectRefRefusesAnAmbiguousName is the same rule on the key that was
// already ambiguous in principle: names are directory basenames, and two
// checkouts of the same repository have the same one.
func TestProjectRefRefusesAnAmbiguousName(t *testing.T) {
	conn := newTestDB(t)
	registerProject(t, conn, "/src/here.git", "here.git", "", 1)
	first := registerProject(t, conn, "/work/one/flux.git", "flux.git", "", 2)
	second := registerProject(t, conn, "/work/two/flux.git", "flux.git", "", 3)

	_, err := resolveProjectRef(conn, "flux.git")
	if err == nil {
		t.Fatal("an ambiguous name resolved to one of the candidates")
	}
	assertErrCode(t, err, output.ErrValidation)
	for _, want := range []string{first.Identity, second.Identity} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestIssueListProjectFlagAcceptsThePrefix is the first acceptance criterion,
// end to end through the verb: the listing is the named project's, and it
// renders in that project's voice.
//
// The caller's own project is a DIFFERENT one — that is what "from any cwd"
// means here, since cwd reaches this code only as the resolved ambient project.
func TestIssueListProjectFlagAcceptsThePrefix(t *testing.T) {
	conn := newTestDB(t)
	here, there := twoProjects(t, conn)

	mine := createIssue(t, conn, "mine", model.StatusBacklog, model.PriorityNone)
	theirs := createIssue(t, conn, "theirs", model.StatusBacklog, model.PriorityNone)
	setIssueProject(t, conn, mine, here.ID)
	setIssueProject(t, conn, theirs, there.ID)

	// The display prefix is process-global and this listing moves it on purpose.
	t.Cleanup(func() { model.SetDisplayPrefix("DKT") })

	cmd := listCmdWithDB(conn)
	cmd.Flags().String("project", "", "")
	if err := cmd.Flags().Set("project", there.Prefix); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	cmd.SetContext(context.WithValue(cmd.Context(), projectKey, here.ID))

	w, buf := bufWriter(true)
	if err := runIssueList(cmd, nil, w); err != nil {
		t.Fatalf("runIssueList --project %s: %v", there.Prefix, err)
	}

	var lj listJSON
	if err := json.Unmarshal(buf.Bytes(), &lj); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(lj.Data.Issues) != 1 {
		t.Fatalf("listed %d issues, want the named project's one: %s",
			len(lj.Data.Issues), buf.String())
	}
	// Both halves in one assertion: the row is the NAMED project's issue, and it
	// renders under the NAMED project's prefix rather than the caller's — the
	// prefix is the only thing on the row that says whose issue it is.
	if want := model.FormatIDWithPrefix(theirs, there.Prefix); lj.Data.Issues[0].ID != want {
		t.Errorf("listed %q, want %q", lj.Data.Issues[0].ID, want)
	}
}

// TestIssueMoveProjectSharesTheResolver is the sibling verb DKT-453 names.
// It resolved a prefix through its own copy of the ladder; consolidating onto
// one resolver must not cost it that.
func TestIssueMoveProjectSharesTheResolver(t *testing.T) {
	conn := newTestDB(t)
	here, there := twoProjects(t, conn)

	issue := createIssue(t, conn, "filed in the wrong place", model.StatusBacklog, model.PriorityNone)
	setIssueProject(t, conn, issue, here.ID)

	cmd := cmdWithDB(conn)
	if err := cmd.Flags().Set("quiet", "true"); err != nil {
		t.Fatalf("quieting the writer: %v", err)
	}
	if err := moveIssueToProject(cmd, issue, there.Prefix); err != nil {
		t.Fatalf("issue move --project %s: %v", there.Prefix, err)
	}

	got, err := db.IssueProjectID(conn, issue)
	testsupport.Must(t, err, "reading the issue's project: %v", err)
	if got != there.ID {
		t.Errorf("issue landed in project %d, want %d", got, there.ID)
	}
}
