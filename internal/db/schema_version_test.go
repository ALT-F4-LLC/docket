package db

import "testing"

func TestLatestSchemaVersion(t *testing.T) {
	highest := 0
	for v := range migrations {
		if v > highest {
			highest = v
		}
	}

	if got := LatestSchemaVersion(); got != highest {
		t.Errorf("LatestSchemaVersion() = %d, want highest migrations key %d", got, highest)
	}
	if currentSchemaVersion != highest {
		t.Errorf("currentSchemaVersion = %d, want highest migrations key %d", currentSchemaVersion, highest)
	}
}
