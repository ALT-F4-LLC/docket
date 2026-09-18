package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `dispatch open` reports the reaps IT performs (Manifest.Reaped) and the hold
// they leave (Manifest.ReapHold), the way `next` reports its own.
//
// The acks a caller passes are applied before the open's reap, so nothing the
// caller knew about can cover a lease that lapsed while the open ran — and the
// manifest's opened_seq is the boundary for reaps the relay has not yet seen,
// not for the ones this open minted after it. Without the two fields a relay
// learned of its own open's reap only when `guard spawn --active` denied the
// launch it had just composed: on RUN-95 two of four wave launches, each a
// nine-minute panel detour serial with the launch.

// TestOpenDispatchReportsItsOwnReaps: a writer whose lease lapsed before the
// open is reaped BY the open, named on the manifest, and the hold text names
// the seq and the flag that clears it.
func TestOpenDispatchReportsItsOwnReaps(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)

	claim := claimInstance(t, conn, "one@0", nowMS)
	at := claim.LeaseExpiresMS + 1

	manifest, err := NewEngine().OpenDispatch(conn, runID, 0, nil, at)
	testsupport.Must(t, err, "dispatch open past the lease: %v", err)

	if !contains(manifest.Reaped, "one@0") {
		t.Errorf("manifest.reaped = %v; the open reaped one@0 and must say so", manifest.Reaped)
	}
	acks := openReapsOf(t, conn, runID)
	if len(acks) != 1 {
		t.Fatalf("%d unacknowledged reaps after the open, want 1", len(acks))
	}
	if manifest.ReapHold == "" {
		t.Fatal("manifest.reap_hold is empty while the open's own reap holds headroom")
	}
	for _, want := range []string{"one@0", "--ack-reap", "seq"} {
		if !strings.Contains(manifest.ReapHold, want) {
			t.Errorf("reap_hold %q does not name %q", manifest.ReapHold, want)
		}
	}
	// The exact text `guard spawn` will deny the next launch with, so a relay
	// reading the manifest can act on the same words.
	if want := ReapHoldReason(acks); manifest.ReapHold != want {
		t.Errorf("reap_hold = %q, want the guard's own wording %q", manifest.ReapHold, want)
	}
}

// TestOpenDispatchWithNothingToReapReportsNothing pins the omission contract:
// a run holding no lapsed lease carries neither field, so every manifest that
// never reaped is byte-identical to what it always was.
func TestOpenDispatchWithNothingToReapReportsNothing(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)

	manifest, err := NewEngine().OpenDispatch(conn, runID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(manifest.Reaped) != 0 {
		t.Errorf("manifest.reaped = %v on a run with no lapsed lease", manifest.Reaped)
	}
	if manifest.ReapHold != "" {
		t.Errorf("manifest.reap_hold = %q on a run holding nothing", manifest.ReapHold)
	}
}
