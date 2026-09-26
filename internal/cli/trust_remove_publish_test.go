package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/engine"
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

// TestTrustListAgreesWithEventLog is the property the ordering exists to
// protect: what `trust list` shows and what the newest trust event per entry
// says must not contradict each other.
//
// Scope of "agree": this covers an add, a successful removal, and a removal
// whose PUBLISH failed. It deliberately does not cover a published removal
// whose event failed to record — there the log lags the store by design, which
// TestTrustRmFailsLoudlyWhenTheCwdCannotBeResolved pins instead.
func TestTrustListAgreesWithEventLog(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", "make", "test")
	testsupport.Must(t, err, "trust add checks: %v", err)
	err = runTrustVerb(t, cfg, newTrustAddCmd(), "scan", "--", "make", "scan")
	testsupport.Must(t, err, "trust add scan: %v", err)
	assertListAgreesWithEvents(t, cfg, "after two adds")

	err = runTrustVerb(t, cfg, newTrustRmCmd(), "scan")
	testsupport.Must(t, err, "trust rm scan: %v", err)
	assertListAgreesWithEvents(t, cfg, "after a successful removal")

	sealTrustStoreDir(t)
	if runErr := runTrustVerb(t, cfg, newTrustRmCmd(), "checks"); runErr == nil {
		t.Fatal("the removal must fail when the store cannot be published")
	}
	assertListAgreesWithEvents(t, cfg, "after a removal whose publish failed")
}

// assertListAgreesWithEvents derives the trusted set from the event log —
// newest event per entry wins — and compares it with what the store lists. The
// expectation comes from the log, an independent oracle, never from the same
// store read being checked.
func assertListAgreesWithEvents(t *testing.T, cfg *config.Config, when string) {
	t.Helper()

	trustedByLog := map[string]bool{}
	for _, e := range trustEvents(t, cfg) {
		name, _ := e["name"].(string)
		trustedByLog[name] = e["kind"] == engine.EventTrustAdded
	}

	inStore := map[string]bool{}
	for _, entry := range trustEntries(t) {
		inStore[entry.Name] = true
	}

	for name, trusted := range trustedByLog {
		if trusted && !inStore[name] {
			t.Errorf("%s: the log's newest event for %q is an add, but the store does not list it", when, name)
		}
		if !trusted && inStore[name] {
			t.Errorf("%s: the log says %q was removed, but the store still authorizes it", when, name)
		}
	}
	for name := range inStore {
		if _, seen := trustedByLog[name]; !seen {
			t.Errorf("%s: the store lists %q with no trust event for it at all", when, name)
		}
	}
}
