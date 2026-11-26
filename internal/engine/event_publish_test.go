package engine

import (
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// subscribeAppends subscribes to the engine's append broadcast for the rest of
// the test.
func subscribeAppends(t *testing.T) <-chan struct{} {
	t.Helper()
	wake, cancel := AppendBroadcast().Subscribe()
	t.Cleanup(cancel)
	return wake
}

// pendingWakes drains wake without waiting and reports how many wakes were
// pending: 0 or 1, since publishes coalesce into one pending wake.
func pendingWakes(wake <-chan struct{}) int {
	n := 0
	for {
		select {
		case <-wake:
			n++
		default:
			return n
		}
	}
}

func TestPublishOnAppendCommitWakesOnceAfterCommit(t *testing.T) {
	conn := mustDB(t)
	wake := subscribeAppends(t)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	err = recordEvent(tx, eventRecord{Kind: EventConfigChanged, Data: `{"key":"k"}`})
	testsupport.Must(t, err, "recordEvent: %v", err)

	if n := pendingWakes(wake); n != 0 {
		t.Fatalf("subscriber woke %d time(s) before the commit; an uncommitted append must not publish", n)
	}
	err = commitWrite(tx)
	testsupport.Must(t, err, "commitWrite: %v", err)
	if n := pendingWakes(wake); n != 1 {
		t.Fatalf("subscriber woke %d time(s) after the commit, want 1", n)
	}
}

func TestPublishOnAppendRollbackPublishesNothing(t *testing.T) {
	conn := mustDB(t)
	wake := subscribeAppends(t)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = recordEvent(tx, eventRecord{Kind: EventConfigChanged, Data: `{"key":"k"}`})
	testsupport.Must(t, err, "recordEvent: %v", err)
	err = tx.Rollback()
	testsupport.Must(t, err, "Rollback: %v", err)

	if n := pendingWakes(wake); n != 0 {
		t.Fatalf("subscriber woke %d time(s) for a rolled-back append, want 0", n)
	}
	// A commit that fails, here because the transaction already ended, must
	// not publish either.
	if err := commitWrite(tx); err == nil {
		t.Fatal("commitWrite committed a rolled-back transaction")
	}
	if n := pendingWakes(wake); n != 0 {
		t.Fatalf("subscriber woke %d time(s) for a failed commit, want 0", n)
	}
}

func TestPublishOnAppendRefusedEventPublishesNothing(t *testing.T) {
	conn := mustDB(t)
	wake := subscribeAppends(t)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	if err := recordEvent(tx, eventRecord{Kind: "step-invented"}); err == nil {
		t.Fatal("recordEvent accepted a kind outside the closed set")
	}

	if n := pendingWakes(wake); n != 0 {
		t.Fatalf("subscriber woke %d time(s) for a refused append, want 0", n)
	}
}

// SetConfig on a vote-rule key appends a config-changed event through
// recordEvent and commits; the subscriber must wake without the test
// publishing anything itself.
func TestPublishOnAppendWakesSubscriberThroughWritePath(t *testing.T) {
	conn := mustDB(t)
	wake := subscribeAppends(t)

	err := SetConfig(conn, 0, store.VoteRuleHoldOnDissentKey("majority"), "true", "tester")
	testsupport.Must(t, err, "SetConfig: %v", err)

	select {
	case <-wake:
	case <-time.After(broadcastDeadline):
		t.Fatalf("the subscriber was not woken within %s of a committed append", broadcastDeadline)
	}
}
