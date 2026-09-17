package db

import (
	"errors"
	"strings"
	"testing"
)

// TestVoteRuleHoldOnDissentKey is DKT-2449's config half: `.hold_on_dissent`
// is the rule's fourth dimension, opt-in and boolean, resolved for ANY rule
// name because <name> is an opaque string.
func TestVoteRuleHoldOnDissentKey(t *testing.T) {
	conn := mustOpen(t)
	if err := Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	t.Run("unset resolves false", func(t *testing.T) {
		entry, err := GetConfig(conn, 0, VoteRuleHoldOnDissentKey("never-set"))
		if err != nil {
			t.Fatalf("GetConfig: %v", err)
		}
		if entry.Value != "false" {
			t.Errorf("unset .hold_on_dissent = %q, want the %q default", entry.Value, "false")
		}
		if entry.Source == "set" {
			t.Errorf("unset .hold_on_dissent reports source %q, want a default", entry.Source)
		}
	})

	t.Run("a set true is read back", func(t *testing.T) {
		if err := SetConfig(conn, 0, VoteRuleHoldOnDissentKey("majority"), "true"); err != nil {
			t.Fatalf("SetConfig(.hold_on_dissent, true): %v", err)
		}
		entry, err := GetConfig(conn, 0, VoteRuleHoldOnDissentKey("majority"))
		if err != nil {
			t.Fatalf("GetConfig: %v", err)
		}
		if entry.Value != "true" || entry.Source != "set" {
			t.Errorf("set .hold_on_dissent = %q (source %q), want %q set",
				entry.Value, entry.Source, "true")
		}
		// The key must appear in the refusal message's known-key list, or an
		// operator who typos it is told the correct spelling does not exist.
		known := strings.Join(KnownConfigKeys(), ", ")
		if !strings.Contains(known, KeyVoteRuleHoldOnDissentSuffix) {
			t.Errorf("KnownConfigKeys() omits the %q pattern: %s",
				KeyVoteRuleHoldOnDissentSuffix, known)
		}
	})

	t.Run("a malformed value is refused", func(t *testing.T) {
		for _, value := range []string{"maybe", "held", ""} {
			if err := SetConfig(conn, 0, VoteRuleHoldOnDissentKey("r"), value); err == nil {
				t.Errorf("SetConfig(.hold_on_dissent, %q) succeeded, want a refusal", value)
			}
		}
		// A near-miss spelling stays unknown: the dynamic match is exact.
		_, err := LookupConfigSpec(KeyVoteRulePrefix + "r.hold_on_dissents")
		if !errors.Is(err, ErrUnknownConfigKey) {
			t.Errorf("LookupConfigSpec(.hold_on_dissents) error = %v, want ErrUnknownConfigKey", err)
		}
	})
}
