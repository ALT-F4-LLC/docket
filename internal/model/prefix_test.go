package model

import "testing"

// The per-project display prefix (v12): rendering follows the project's
// voice, while the NUMBER stays the identity — DKT always parses, a bare
// number always parses, and no reference goes stale when the display changes.
func TestDisplayPrefixIsDisplayOnly(t *testing.T) {
	t.Cleanup(func() { displayPrefix = IDPrefix })

	if got := FormatID(42); got != "DKT-42" {
		t.Fatalf("default FormatID = %q, want DKT-42", got)
	}

	SetDisplayPrefix("vor")
	if got := FormatID(42); got != "VOR-42" {
		t.Errorf("FormatID under a project prefix = %q, want VOR-42 (uppercased)", got)
	}

	for _, ref := range []string{"VOR-42", "vor-42", "DKT-42", "dkt-42", "42"} {
		id, err := ParseID(ref)
		if err != nil || id != 42 {
			t.Errorf("ParseID(%q) = (%d, %v), want (42, nil) — the number is the "+
				"identity, whatever the prefix", ref, id, err)
		}
	}

	// An unrelated prefix is still refused: RUN-3 handed to an issue verb must
	// not silently become issue 3.
	if _, err := ParseID("RUN-3"); err == nil {
		t.Error("ParseID accepted RUN-3; other entities' references must not parse as issues")
	}
}

func TestValidateProjectPrefix(t *testing.T) {
	for _, ok := range []string{"DKT", "vor", "A", "LONGPFX"} {
		if err := ValidateProjectPrefix(ok); err != nil {
			t.Errorf("ValidateProjectPrefix(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "TOOLONGPREFIX", "V0R", "A-B", "RUN", "doc", "STEP"} {
		if err := ValidateProjectPrefix(bad); err == nil {
			t.Errorf("ValidateProjectPrefix(%q) accepted, want refusal", bad)
		}
	}
}
