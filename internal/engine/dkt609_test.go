package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-609: a corpus commit renamed a workflow, the four registered versions of
// the old name stayed live in the store, and the first issue carrying the old
// label failed activation with "matches 2 workflows: security-change@17,
// security-load-bearing@12". The refusal named both candidates and said
// nothing about the fact that one of them had had no file on disk for weeks —
// which is what turned a `docket workflow deprecate` into a git archaeology
// session.
//
// These tests fix the three verdicts apart and pin the annotation onto the
// refusal that needed it.

// TestWorkflowOriginVerdicts separates present, orphaned, and unchecked at the
// index every caller shares. They are different facts: a name still declared
// somewhere is ordinary, a name declared nowhere is a deprecation candidate,
// and a name nobody looked for is neither.
func TestWorkflowOriginVerdicts(t *testing.T) {
	root := t.TempDir()
	err := os.MkdirAll(filepath.Join(root, "workflows"), 0o755)
	testsupport.Must(t, err, "creating the workflows directory: %v", err)
	live := filepath.Join(root, "workflows", "gone-renamed.toml")
	err = os.WriteFile(live, []byte(goneRenamedWorkflowSrc), 0o644)
	testsupport.Must(t, err, "writing the definition: %v", err)

	scan, err := scanConfigDirs([]string{root})
	testsupport.Must(t, err, "scanning the config root: %v", err)
	index := newWorkflowOriginIndex(scan)

	if !index.Scanned() {
		t.Fatal("premise: a root that exists must count as scanned")
	}

	present := index.Status("gone-renamed")
	if present.State != model.WorkflowOriginPresent {
		t.Errorf("a declared name = %q, want %q (%+v)",
			present.State, model.WorkflowOriginPresent, present)
	}
	// Compared against the CANONICALIZED path: the scan resolves its roots
	// (macOS puts /tmp behind a symlink), so the recorded path is the resolved
	// one and comparing against the unresolved literal would fail on the
	// platform this repo is developed on.
	wantLive, err := filepath.EvalSymlinks(live)
	testsupport.Must(t, err, "resolving %s: %v", live, err)
	if present.Path != wantLive {
		t.Errorf("the present verdict names %q, want the file that declares "+
			"the name, %q", present.Path, wantLive)
	}

	// The rename's other side: "gone" is registered nowhere on this disk.
	orphan := index.Status("gone")
	if orphan.State != model.WorkflowOriginOrphaned {
		t.Errorf("a name no file declares = %q, want %q (%+v)",
			orphan.State, model.WorkflowOriginOrphaned, orphan)
	}
	if len(orphan.Roots) == 0 {
		t.Error("the orphaned verdict names no roots, so a reader cannot tell " +
			"WHERE the name was looked for")
	}
	if !index.Orphaned("gone") || index.Orphaned("gone-renamed") {
		t.Error("Orphaned() disagrees with Status()")
	}

	// UNCHECKED is not "clean". A store with no instance-config root has had
	// nothing looked at, and reporting orphans from it would call every
	// registration on the machine an orphan on the strength of having looked
	// nowhere.
	var none *WorkflowOriginIndex
	if s := none.Status("gone-renamed"); s.State != model.WorkflowOriginUnchecked {
		t.Errorf("a nil index = %q, want %q", s.State, model.WorkflowOriginUnchecked)
	}
	if none.Orphaned("gone-renamed") {
		t.Error("a nil index reports an orphan; nothing was checked")
	}
	empty := newWorkflowOriginIndex(nil)
	if empty.Scanned() {
		t.Error("an index over no root reports itself scanned")
	}
	if s := empty.Status("gone"); s.State != model.WorkflowOriginUnchecked {
		t.Errorf("no root = %q, want %q", s.State, model.WorkflowOriginUnchecked)
	}
}

// TestWorkflowOriginIsPerNameNotPerVersion: a SUPERSEDED version is not an
// orphan while its name is still declared. Version bumps are the ordinary way
// the corpus evolves, and a check that flagged every superseded row would light
// up the whole registry — noise that would cost exactly the rename case it
// exists to find.
func TestWorkflowOriginIsPerNameNotPerVersion(t *testing.T) {
	root := t.TempDir()
	err := os.MkdirAll(filepath.Join(root, "workflows"), 0o755)
	testsupport.Must(t, err, "creating the workflows directory: %v", err)
	// Only version 2 of "gone" is on disk; version 1 was registered from the
	// file this one replaced.
	bumped := strings.Replace(goneWorkflowSrc, "version = 1", "version = 2", 1)
	err = os.WriteFile(filepath.Join(root, "workflows", "gone.toml"), []byte(bumped), 0o644)
	testsupport.Must(t, err, "writing the bumped definition: %v", err)

	scan, err := scanConfigDirs([]string{root})
	testsupport.Must(t, err, "scanning the config root: %v", err)
	if index := newWorkflowOriginIndex(scan); index.Orphaned("gone") {
		t.Error("a name whose file was BUMPED reads as orphaned; the verdict " +
			"is per name, and every superseded version of a live name would " +
			"otherwise be reported as a deprecation candidate")
	}
}

// TestDryRunRefusalNamesTheOrphan is DKT-609's second acceptance criterion, on
// the verb the RUN-45 operator actually ran: `run activate --dry-run` refuses
// the ambiguity and its candidate list now says WHICH side of it has no
// definition left.
//
// Both directions are asserted. The orphan carries the annotation AND the live
// candidate does not — an annotation on both would be no distinction at all,
// which is the state this issue is closing.
func TestDryRunRefusalNamesTheOrphan(t *testing.T) {
	conn, configDir := configRepo(t)
	path := writeConfigFile(t, configDir, "workflows/gone.toml", goneWorkflowSrc)

	first := createIssue(t, conn, "first", "body", "task", nil)
	firstRun := startRun(t, conn, first)
	_, err := activate(conn, firstRun.ID)
	testsupport.Must(t, err, "registering gone@1 through the first activation: %v", err)

	// The rename: the old file leaves the corpus, the new name arrives, and
	// the OLD REGISTRATION IS UNTOUCHED — a registration is a row, not a file.
	testsupport.Must(t, os.Remove(path), "deleting gone.toml: %v", err)
	writeConfigFile(t, configDir, "workflows/gone-renamed.toml", goneRenamedWorkflowSrc)

	issue := createIssue(t, conn, "the first issue after the rename", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	if err == nil {
		t.Fatal("the dry run bound an issue two names match; it must refuse " +
			"exactly as the real activation does")
	}
	assertRenamePlusBumpWedge(t, err, issue, wedgeCandidatesOrphaned)

	msg := err.Error()
	if strings.Contains(msg, "gone-renamed@2"+orphanAnnotation) {
		t.Errorf("the LIVE candidate is annotated as an orphan too, so the "+
			"refusal distinguishes nothing: %s", msg)
	}
	// The remedy, in the message, so the next operator does not have to go
	// looking for which verb clears a stranded registration.
	if !strings.Contains(msg, "docket workflow deprecate") ||
		!strings.Contains(msg, "docket workflow list --orphans") {
		t.Errorf("the refusal names no remedy for the orphan it found: %s", msg)
	}
}

// TestRefusalIsUnannotatedWithoutAConfigRoot is the other half of the honesty
// rule, asserted on the refusal itself rather than only on the index: with no
// root to scan, the message is EXACTLY the pre-DKT-609 one. Silence is what
// "nothing was checked" has to look like — a hint about deprecating a
// registration nobody verified is worse than no hint.
func TestRefusalIsUnannotatedWithoutAConfigRoot(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(goneWorkflowSrc), "gone.toml")
	registerSource(t, conn, []byte(goneRenamedWorkflowSrc), "gone-renamed.toml")

	issue := createIssue(t, conn, "wedged with no corpus on disk", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("premise: two names matching one issue must refuse")
	}
	assertRenamePlusBumpWedge(t, err, issue, wedgeCandidatesUnchecked)
	if strings.Contains(err.Error(), "ORPHANED") {
		t.Errorf("an unscanned store reported an orphan: %s", err.Error())
	}
}
