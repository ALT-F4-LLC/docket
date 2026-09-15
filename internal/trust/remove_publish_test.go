package trust

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// errTestRecord stands in for a recording backend that refuses the write.
var errTestRecord = errors.New("the record could not be written")

// sealStoreDir makes the store's directory unwritable AFTER the store and the
// lock file already exist, so acquireLock and loadAt still succeed (both open
// existing files) and writeStore's CreateTemp is the first operation the
// directory refuses. Sealing an empty directory instead would fail at the lock
// and prove nothing about publish ordering.
func sealStoreDir(t *testing.T, path string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions, so the publish cannot be made to fail this way")
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, storeFileMode)
	testsupport.Must(t, err, "pre-creating the lock file: %v", err)
	testsupport.Must(t, lock.Close(), "closing the lock file: %v", err)

	dir := filepath.Dir(path)
	err = os.Chmod(dir, 0o500)
	testsupport.Must(t, err, "sealing %s: %v", dir, err)
	t.Cleanup(func() { _ = os.Chmod(dir, storeDirMode) })
}

// TestTrustRemoveRecordsNoEventOnFailedPublish is DKT-2198: a removal whose
// store publish fails must leave NO record of the removal behind, because the
// entry it names still authorizes execution. The event log may over-report
// authority; it must never under-report it.
func TestTrustRemoveRecordsNoEventOnFailedPublish(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	_, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo,
	})
	testsupport.Must(t, err, "addAt: %v", err)

	sealStoreDir(t, path)

	var recorded int
	removed, err := removeAt(path, RemoveRequest{
		Name: "checks", RepoRoot: repo,
		OnChange: func(Entry) error { recorded++; return nil },
	})
	if err == nil {
		t.Fatal("a removal whose publish cannot land must fail")
	}
	if removed {
		t.Error("a removal that failed to publish must not report itself removed")
	}
	if recorded != 0 {
		t.Errorf("the removal was recorded %d time(s) despite a failed publish; the log now claims less authority than the store grants", recorded)
	}

	// AC2: the failure names the store it tried to publish, so an operator can
	// see WHICH file is diverging from the record.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the publish failure must name the store path %s; got: %v", path, err)
	}

	// The entry is still there, which is exactly why no record may exist.
	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 1 || st.Entries[0].Name != "checks" {
		t.Errorf("a failed publish must leave the store untouched; got %+v", st.Entries)
	}
}

// TestTrustRemoveRecordsAfterASuccessfulPublish is the benign half: an ordinary
// removal still records exactly one event, with the entry as it stood.
func TestTrustRemoveRecordsAfterASuccessfulPublish(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	_, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo,
	})
	testsupport.Must(t, err, "addAt: %v", err)

	var seen []Entry
	removed, err := removeAt(path, RemoveRequest{
		Name: "checks", RepoRoot: repo,
		OnChange: func(e Entry) error { seen = append(seen, e); return nil },
	})
	testsupport.Must(t, err, "removeAt: %v", err)
	if !removed {
		t.Fatal("removeAt must report that it removed the entry")
	}
	if len(seen) != 1 || seen[0].ArgvSHA256 != ArgvSHA256([]string{"make", "test"}) {
		t.Errorf("the hook must run exactly once with the removed entry; got %+v", seen)
	}
	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 0 {
		t.Errorf("the entry survived a successful removal: %+v", st.Entries)
	}
}

// TestTrustRemoveReportsAnUnrecordedRemoval pins the surviving partial failure:
// the entry IS gone and the record is not. removeAt must say the removal
// happened, or the caller would describe a completed revocation as a store
// failure — telling the operator the opposite of what occurred.
func TestTrustRemoveReportsAnUnrecordedRemoval(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	_, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo,
	})
	testsupport.Must(t, err, "addAt: %v", err)

	removed, err := removeAt(path, RemoveRequest{
		Name: "checks", RepoRoot: repo,
		OnChange: func(Entry) error { return errTestRecord },
	})
	if !errors.Is(err, errTestRecord) {
		t.Fatalf("a failed record must fail the verb; got %v", err)
	}
	if !removed {
		t.Error("the entry was already published as removed, so removeAt must report it removed")
	}
	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 0 {
		t.Errorf("the publish already landed; the entry must be gone: %+v", st.Entries)
	}
}
