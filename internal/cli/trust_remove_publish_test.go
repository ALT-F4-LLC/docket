package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// sealTrustStoreDir makes the publish — and only the publish — fail, by
// removing write permission from the store's directory AFTER the store and its
// lock file exist. Both are opened, not created, by the removal, so the lock is
// taken and the store is read as usual and writeStore's temp file is the first
// thing the directory refuses. Sealing an empty directory would fail at the
// lock instead and would prove nothing about ordering.
//
// Returns the resolved store path, which is what the failure must name.
func sealTrustStoreDir(t *testing.T) string {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions, so the publish cannot be made to fail this way")
	}
	path, err := trust.StorePath()
	testsupport.Must(t, err, "StorePath: %v", err)

	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	testsupport.Must(t, err, "pre-creating the lock file: %v", err)
	testsupport.Must(t, lock.Close(), "closing the lock file: %v", err)

	dir := filepath.Dir(path)
	err = os.Chmod(dir, 0o500)
	testsupport.Must(t, err, "sealing %s: %v", dir, err)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return path
}

// TestTrustRemoveRecordsNoEventOnFailedPublish is DKT-2198 through the real
// verb, with a real store and a real event log.
//
// A trust-removed event that outlives a failed publish is an audit record that
// did not revoke anything: every gate lookup keeps matching the entry while a
// reader of the log believes the command is no longer approved. The event log
// may over-report authority; it must never under-report it.
func TestTrustRemoveRecordsNoEventOnFailedPublish(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", "make", "test")
	testsupport.Must(t, err, "trust add: %v", err)

	storePath := sealTrustStoreDir(t)

	runErr := runTrustVerb(t, cfg, newTrustRmCmd(), "checks")
	if runErr == nil {
		t.Fatal("a removal whose store cannot be published must fail")
	}
	// AC2: non-zero exit naming the store it tried to publish.
	if !strings.Contains(runErr.Error(), storePath) {
		t.Errorf("the failure must name the store path %s; got: %v", storePath, runErr)
	}

	// The entry still authorizes execution...
	entries := trustEntries(t)
	if len(entries) != 1 || entries[0].Name != "checks" {
		t.Fatalf("a failed publish must leave the store untouched; got %+v", entries)
	}
	// ...so nothing may claim it was revoked.
	events := trustEvents(t, cfg)
	if len(events) != 1 {
		t.Fatalf("got %d events, want only the original add: %+v", len(events), events)
	}
	if events[0]["kind"] != engine.EventTrustAdded {
		t.Errorf("a failed removal recorded a %v event", events[0]["kind"])
	}
}

// TestTrustPublishFailureIsGeneralError: a store the filesystem refuses to
// publish is GENERAL_ERROR, not VALIDATION_ERROR, for both verbs. The argv was
// valid, so a caller must not retry with a "corrected" one.
func TestTrustPublishFailureIsGeneralError(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", "make", "test")
	testsupport.Must(t, err, "trust add: %v", err)

	sealTrustStoreDir(t)

	rmErr := runTrustVerb(t, cfg, newTrustRmCmd(), "checks")
	if rmErr == nil {
		t.Fatal("trust rm must fail when the store cannot be published")
	}
	if code := errorCodeOf(t, rmErr); code != output.ErrGeneral {
		t.Errorf("trust rm publish failure is %s, want GENERAL_ERROR: %v", code, rmErr)
	}

	// A new name, so the add is not the idempotent no-write branch.
	addErr := runTrustVerb(t, cfg, newTrustAddCmd(), "scan", "--", "make", "scan")
	if addErr == nil {
		t.Fatal("trust add must fail when the store cannot be published")
	}
	if code := errorCodeOf(t, addErr); code != output.ErrGeneral {
		t.Errorf("trust add publish failure is %s, want GENERAL_ERROR: %v", code, addErr)
	}
}

// TestTrustAddUnpinnableArgv0IsValidation pins trustCmdError's default branch:
// an absolute argv[0] that names no file is an argument fault, so it stays
// VALIDATION_ERROR even though the failure is a filesystem error. Only the
// publish sentinel moves to GENERAL_ERROR, not every I/O error.
func TestTrustAddUnpinnableArgv0IsValidation(t *testing.T) {
	_, cfg := trustRepo(t)

	missing := filepath.Join(t.TempDir(), "no-such-binary")
	err := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", missing)
	if err == nil {
		t.Fatal("trust add must refuse an absolute argv[0] that names no file")
	}
	if code := errorCodeOf(t, err); code != output.ErrValidation {
		t.Errorf("the refusal is %s, want VALIDATION_ERROR: %v", code, err)
	}
}
