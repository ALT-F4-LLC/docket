package db

import "testing"

// TestAutoClassMaxHalvesCPUsWithFloorOfOne pins limits.max.<class>=auto:
// half the logical CPUs, rounded down, and never below one, so a one-CPU
// host still admits a step of every class.
func TestAutoClassMaxHalvesCPUsWithFloorOfOne(t *testing.T) {
	for _, tc := range []struct{ cpus, want int }{
		{0, 1},
		{1, 1},
		{2, 1},
		{3, 1},
		{4, 2},
		{18, 9},
	} {
		if got := autoClassMax(tc.cpus); got != tc.want {
			t.Errorf("autoClassMax(%d) = %d, want %d", tc.cpus, got, tc.want)
		}
	}
}
