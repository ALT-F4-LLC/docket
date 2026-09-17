package db

import (
	"errors"
	"testing"
)

// TestVoteRuleIdentityKeys is DKT-2448 piece 0: `.roster` and `.weighting`
// register as vote-rule keys, each with its own default and its own closed
// value set.
//
// The two keys are the seam the roster-enforcement and equal-weighting pieces
// build on. Here they only have to EXIST, resolve to their defaults when
// unset, accept their declared values, and refuse anything else at `set` time.
func TestVoteRuleIdentityKeys(t *testing.T) {
	for _, tc := range []struct {
		key         string
		wantDefault string
		accepted    []string
		refused     []string
	}{
		{
			key:         VoteRuleRosterKey("majority"),
			wantDefault: VoteRosterOpen,
			accepted:    []string{VoteRosterOpen, VoteRosterStrict},
			refused:     []string{"loose", "Open", "", "closed"},
		},
		{
			key:         VoteRuleWeightingKey("majority"),
			wantDefault: VoteWeightingDeclared,
			accepted:    []string{VoteWeightingDeclared, VoteWeightingEqual},
			refused:     []string{"weighted", "Equal", "", "uniform"},
		},
	} {
		t.Run(tc.key, func(t *testing.T) {
			spec, err := LookupConfigSpec(tc.key)
			if err != nil {
				t.Fatalf("LookupConfigSpec(%q): %v", tc.key, err)
			}
			if spec.Default != tc.wantDefault {
				t.Errorf("default for %q is %q, want %q", tc.key, spec.Default, tc.wantDefault)
			}
			for _, value := range tc.accepted {
				if err := ValidateConfigValue(spec, value); err != nil {
					t.Errorf("%q rejected declared value %q: %v", tc.key, value, err)
				}
			}
			for _, value := range tc.refused {
				if err := ValidateConfigValue(spec, value); err == nil {
					t.Errorf("%q accepted %q, which is outside its value set", tc.key, value)
				}
			}
		})
	}
}

// TestVoteRuleIdentityKeysReadBack: a value set through the store's own writer
// reads back verbatim, and an unset key falls to the spec default — the two
// halves `resolveVoteRule` depends on.
func TestVoteRuleIdentityKeysReadBack(t *testing.T) {
	conn := mustOpen(t)
	if err := Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	projectID, err := EnsureProject(conn, "/repo/one", "one", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	for key, want := range map[string]string{
		VoteRuleRosterKey("majority"):    VoteRosterOpen,
		VoteRuleWeightingKey("majority"): VoteWeightingDeclared,
	} {
		entry, err := GetConfig(conn, projectID, key)
		if err != nil {
			t.Fatalf("GetConfig(%q): %v", key, err)
		}
		if entry.Value != want {
			t.Errorf("unset %q resolved %q, want the default %q", key, entry.Value, want)
		}
	}

	if err := SetConfig(conn, 0, VoteRuleRosterKey("majority"), VoteRosterStrict); err != nil {
		t.Fatalf("SetConfig roster: %v", err)
	}
	if err := SetConfig(conn, 0, VoteRuleWeightingKey("majority"), VoteWeightingEqual); err != nil {
		t.Fatalf("SetConfig weighting: %v", err)
	}
	for key, want := range map[string]string{
		VoteRuleRosterKey("majority"):    VoteRosterStrict,
		VoteRuleWeightingKey("majority"): VoteWeightingEqual,
	} {
		entry, err := GetConfig(conn, projectID, key)
		if err != nil {
			t.Fatalf("GetConfig(%q): %v", key, err)
		}
		if entry.Value != want {
			t.Errorf("%q read back %q, want %q", key, entry.Value, want)
		}
	}
}

// TestVoteRuleIdentityKeysRequireARuleName: the empty rule name is not a rule,
// exactly as it is not for `.threshold`.
func TestVoteRuleIdentityKeysRequireARuleName(t *testing.T) {
	for _, key := range []string{
		KeyVoteRulePrefix + KeyVoteRuleRosterSuffix,
		KeyVoteRulePrefix + KeyVoteRuleWeightingSuffix,
	} {
		if _, err := LookupConfigSpec(key); !errors.Is(err, ErrUnknownConfigKey) {
			t.Errorf("LookupConfigSpec(%q) = %v, want ErrUnknownConfigKey", key, err)
		}
	}
}
