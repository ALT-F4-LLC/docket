package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestConfigSetRefusesAnInvalidClassMax: limits.max.<class> takes a positive
// integer or `auto`. Anything else is refused at set time with an error naming
// the key, and the store keeps the value it held before.
func TestConfigSetRefusesAnInvalidClassMax(t *testing.T) {
	conn := newTestDB(t)
	key := db.KeyClassMaxPrefix + "write"

	setClassMax := func(value string) error {
		cmd := cmdWithDB(conn)
		cmd.Flags().Bool("global", true, "")
		return configSetCmd.RunE(cmd, []string{key, value})
	}

	for _, valid := range []string{db.ClassMaxAuto, "4"} {
		if err := setClassMax(valid); err != nil {
			t.Fatalf("config set %s %q: %v", key, valid, err)
		}
	}

	for _, invalid := range []string{"0", "-2", "lots", "2.5", ""} {
		err := setClassMax(invalid)
		if err == nil {
			t.Errorf("config set %s %q succeeded; want a refusal", key, invalid)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("refusal of %q does not name %s: %v", invalid, key, err)
		}
	}

	entry, err := db.GetConfig(conn, 0, key)
	testsupport.Must(t, err, "reading %s: %v", key, err)
	if entry.Value != "4" || entry.Source != "set" {
		t.Errorf("%s = %q (%s) after the refusals, want the prior \"4\" (set)",
			key, entry.Value, entry.Source)
	}

	// `config get` with no key lists the set cap beside the fixed keys.
	entries, err := db.ListConfig(conn, 1)
	testsupport.Must(t, err, "ListConfig: %v", err)
	listed := false
	for _, e := range entries {
		listed = listed || (e.Key == key && e.Value == "4")
	}
	if !listed {
		t.Errorf("ListConfig omits %s=4: %v", key, entries)
	}
}
