package engine

import (
	"testing"
	"time"
)

// broadcastDeadline bounds every wait, so a wake that never arrives or a
// publish that blocks fails the test instead of hanging the package.
const broadcastDeadline = 2 * time.Second

// publishWithin runs Publish and fails the test when it panics or does not
// return before the deadline.
func publishWithin(t *testing.T, b *EventBroadcast) {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		b.Publish()
	}()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("Publish panicked: %v", p)
		}
	case <-time.After(broadcastDeadline):
		t.Fatalf("Publish did not return within %s", broadcastDeadline)
	}
}

func TestBroadcastWakesSubscriberOnPublish(t *testing.T) {
	var b EventBroadcast
	wake, cancel := b.Subscribe()
	defer cancel()

	publishWithin(t, &b)

	select {
	case _, ok := <-wake:
		if !ok {
			t.Fatal("the wake channel was closed instead of rung")
		}
	case <-time.After(broadcastDeadline):
		t.Fatalf("the subscriber was not woken within %s of a publish", broadcastDeadline)
	}
}

func TestBroadcastPublishWithNoSubscriberReturns(t *testing.T) {
	var b EventBroadcast
	publishWithin(t, &b)
}

// A subscriber that has not drained its last wake must not stall the
// publisher: rings coalesce into the one pending wake.
func TestBroadcastPublishDoesNotBlockOnUndrainedSubscriber(t *testing.T) {
	var b EventBroadcast
	wake, cancel := b.Subscribe()
	defer cancel()

	for range 3 {
		publishWithin(t, &b)
	}
	select {
	case <-wake:
	case <-time.After(broadcastDeadline):
		t.Fatalf("the subscriber was not woken within %s of a publish", broadcastDeadline)
	}
	select {
	case <-wake:
		t.Fatal("three publishes left more than one pending wake")
	default:
	}
}

func TestBroadcastUnsubscribedReceivesNothing(t *testing.T) {
	var b EventBroadcast
	removed, cancelRemoved := b.Subscribe()
	kept, cancelKept := b.Subscribe()
	defer cancelKept()

	cancelRemoved()
	publishWithin(t, &b)

	select {
	case <-kept:
	case <-time.After(broadcastDeadline):
		t.Fatalf("the remaining subscriber was not woken within %s", broadcastDeadline)
	}
	select {
	case _, ok := <-removed:
		t.Fatalf("the unsubscribed channel received (open=%v) after cancel", ok)
	default:
	}
}
