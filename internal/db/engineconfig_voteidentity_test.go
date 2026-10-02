package db

import (
	"errors"
	"strings"
	"testing"
)

// TestVoteRuleIdentityKeysAreRetired is DKT-2764: `vote.rule.<name>.roster`
// and `.weighting` — the two keys DKT-2448 registered — no longer register.
// `config set` refuses each with a message naming the vote-step field that
// replaced it, and LookupConfigSpec knows neither suffix.
//
// They refuse BY NAME rather than as generic unknown keys, so an operator
// following the old documentation learns where the switch went.
func TestVoteRuleIdentityKeysAreRetired(t *testing.T) {
	conn := mustOpen(t)
	if err := Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(conn); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, tc := range []struct {
		key   string
		value string
		field string
	}{
		{KeyVoteRulePrefix + "majority" + retiredVoteRuleRosterSuffix, "strict", "roster"},
		{KeyVoteRulePrefix + "majority" + retiredVoteRuleWeightingSuffix, "equal", "weighting"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			err := SetConfig(conn, 0, tc.key, tc.value)
			if err == nil {
				t.Fatalf("SetConfig(%q) succeeded; the key is retired", tc.key)
			}
			// ErrUnknownConfigKey is what the CLI maps to VALIDATION_ERROR.
			if !errors.Is(err, ErrUnknownConfigKey) {
				t.Errorf("refusal %v is not ErrUnknownConfigKey", err)
			}
			if !strings.Contains(err.Error(), "`"+tc.field+" = ") ||
				!strings.Contains(err.Error(), "vote step") {
				t.Errorf("refusal %q does not name the vote-step field `%s`", err, tc.field)
			}

			if _, err := LookupConfigSpec(tc.key); !errors.Is(err, ErrUnknownConfigKey) {
				t.Errorf("LookupConfigSpec(%q) = %v, want ErrUnknownConfigKey", tc.key, err)
			}
			var n int
			if err := conn.QueryRow(`SELECT COUNT(*) FROM meta WHERE key = ?`,
				metaConfigPrefix+tc.key).Scan(&n); err != nil {
				t.Fatalf("counting meta rows: %v", err)
			}
			if n != 0 {
				t.Errorf("the refused write stored a row under %q", tc.key)
			}
		})
	}

	for _, key := range KnownConfigKeys() {
		if strings.HasSuffix(key, retiredVoteRuleRosterSuffix) ||
			strings.HasSuffix(key, retiredVoteRuleWeightingSuffix) {
			t.Errorf("KnownConfigKeys still lists the retired %q", key)
		}
	}
}
