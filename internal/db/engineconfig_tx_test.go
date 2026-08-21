package db

import (
	"testing"
)

// GetConfigTx must resolve a key exactly as GetConfig does.
//
// It exists for the reader that holds the single pooled connection and would
// deadlock against a pool read (the scheduler's snapshot). Two resolvers over
// one key are only safe while they agree, so the test is a COMPARISON rather
// than a restatement of the expected values: whatever GetConfig answers, this
// must answer too, across all three tiers of the ladder.
func TestGetConfigTxMatchesGetConfig(t *testing.T) {
	conn := mustOpen(t)
	if err := Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	one, _ := EnsureProject(conn, "/repo/one", "one", 1)
	two, _ := EnsureProject(conn, "/repo/two", "two", 2)

	// A store-wide value one project overrides, plus a key nobody set — so the
	// three cases are the override, the store-wide fallback, and the builtin
	// default.
	if err := SetConfig(conn, 0, KeyVoteHoldVoters, "alice,bob"); err != nil {
		t.Fatalf("SetConfig global: %v", err)
	}
	if err := SetConfig(conn, one, KeyVoteHoldVoters, "carol"); err != nil {
		t.Fatalf("SetConfig project: %v", err)
	}

	for _, tc := range []struct {
		name      string
		projectID int
		key       string
	}{
		{"project override", one, KeyVoteHoldVoters},
		{"store-wide fallback", two, KeyVoteHoldVoters},
		{"builtin default", one, KeyVoteHoldRule},
		{"store-wide read", 0, KeyVoteHoldVoters},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := GetConfig(conn, tc.projectID, tc.key)
			if err != nil {
				t.Fatalf("GetConfig: %v", err)
			}

			tx, err := conn.Begin()
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			defer tx.Rollback()

			got, err := GetConfigTx(tx, tc.projectID, tc.key)
			if err != nil {
				t.Fatalf("GetConfigTx: %v", err)
			}
			if got != want {
				t.Errorf("GetConfigTx = %+v, GetConfig = %+v; the two resolvers "+
					"must not disagree about one key", got, want)
			}
		})
	}

	// An unknown key is refused identically, so a typo cannot resolve inside a
	// transaction where it would refuse outside one.
	tx, err := conn.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := GetConfigTx(tx, one, "no.such.key"); err == nil {
		t.Error("GetConfigTx accepted an unknown key")
	}
}

// TestSplitNameListRoundTrips: the validator and the reader must agree about
// where one name ends and the next begins, or a roster that passed `config set`
// is counted differently by whatever reads it.
func TestSplitNameListRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", 0},
		{"alice", 1},
		{"alice,bob", 2},
		{"alice, bob, carol", 3},
	} {
		if err := ValidateNameList("name", tc.value); err != nil {
			t.Errorf("ValidateNameList(%q): %v", tc.value, err)
			continue
		}
		if got := len(SplitNameList(tc.value)); got != tc.want {
			t.Errorf("SplitNameList(%q) has %d names, want %d", tc.value, got, tc.want)
		}
	}

	// The spacing the validator tolerates is stripped by the reader, so a name
	// never carries the separator's whitespace into whatever counts it.
	names := SplitNameList("alice, bob")
	if names[1] != "bob" {
		t.Errorf("second name = %q, want %q", names[1], "bob")
	}
}
