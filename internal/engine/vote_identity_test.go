package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestVoteRuleIdentityKeys is DKT-2448 piece 0 at the engine seam: the resolved
// rule carries `.roster` and `.weighting`, defaulting to the permissive values
// a rule nobody configured has always had.
func TestVoteRuleIdentityKeys(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "")

	rule, err := resolveVoteRule(conn, 1, "majority")
	testsupport.Must(t, err, "resolveVoteRule: %v", err)
	if rule.Roster != db.VoteRosterOpen {
		t.Errorf("unconfigured rule resolved roster %q, want %q", rule.Roster, db.VoteRosterOpen)
	}
	if rule.Weighting != db.VoteWeightingDeclared {
		t.Errorf("unconfigured rule resolved weighting %q, want %q",
			rule.Weighting, db.VoteWeightingDeclared)
	}

	err = db.SetConfig(conn, 0, db.VoteRuleRosterKey("majority"), db.VoteRosterStrict)
	testsupport.Must(t, err, "setting the rule roster: %v", err)
	err = db.SetConfig(conn, 0, db.VoteRuleWeightingKey("majority"), db.VoteWeightingEqual)
	testsupport.Must(t, err, "setting the rule weighting: %v", err)

	rule, err = resolveVoteRule(conn, 1, "majority")
	testsupport.Must(t, err, "resolveVoteRule: %v", err)
	if rule.Roster != db.VoteRosterStrict {
		t.Errorf("configured rule resolved roster %q, want %q", rule.Roster, db.VoteRosterStrict)
	}
	if rule.Weighting != db.VoteWeightingEqual {
		t.Errorf("configured rule resolved weighting %q, want %q",
			rule.Weighting, db.VoteWeightingEqual)
	}
}

// TestVoteRuleIdentityKeysFailClosedOnAMalformedStoredValue: a stored value
// outside the key's set must fail the resolution loudly, not fall back to the
// permissive default.
//
// Set-time validation guards the CLI ingress only. A value that reached the
// store another way — a direct write, or a value stored before the set was
// narrowed — would otherwise silently downgrade a `strict` rule to `open`,
// which is the failure mode these keys exist to prevent. The threshold and the
// sealed flag already fail closed the same way.
func TestVoteRuleIdentityKeysFailClosedOnAMalformedStoredValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"roster", db.VoteRuleRosterKey("majority")},
		{"weighting", db.VoteRuleWeightingKey("majority")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			registerVoteRule(t, conn, "majority", "0.6", "")

			// Written straight into the store, bypassing SetConfig's validation,
			// because that validation is exactly the ingress this test assumes
			// was not used.
			_, err := conn.Exec(
				`INSERT INTO meta (key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				"config."+tc.key, "nonsense")
			testsupport.Must(t, err, "seeding a malformed stored value: %v", err)

			_, err = resolveVoteRule(conn, 1, "majority")
			if err == nil {
				t.Fatalf("resolveVoteRule accepted a malformed %s and fell back to the default",
					tc.name)
			}
			if !strings.Contains(err.Error(), "majority") ||
				!strings.Contains(err.Error(), "nonsense") {
				t.Errorf("error %q names neither the rule nor the stored value", err)
			}
		})
	}
}
