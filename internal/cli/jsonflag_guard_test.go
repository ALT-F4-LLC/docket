package cli

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestNoGetBoolJSON guards the --json Bool→String conversion.
//
// --json is a string flag (with NoOptDefVal "v1"). pflag's GetBool on a string
// flag returns (false, error), and every call site in this package discards the
// error with `_`. So a reintroduced GetBool("json") does not fail loudly — it
// silently returns false and drops that command out of JSON mode entirely.
// There were 24 such call sites before the conversion; this test keeps them
// from creeping back.
//
// Read the --json flag via jsonModeOf / jsonVersionOf instead.
func TestNoGetBoolJSON(t *testing.T) {
	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading package dir: %v", err)

	const forbidden = `GetBool("json")`
	var offenders []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// This test names the forbidden pattern in its own documentation.
		if name == "jsonflag_guard_test.go" {
			continue
		}

		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}

		for i, line := range strings.Split(string(src), "\n") {
			// Skip comments — root.go documents why GetBool must not be used.
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, forbidden) {
				offenders = append(offenders, name+":"+strconv.Itoa(i+1))
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("found %s at %v; use jsonModeOf(cmd) instead — GetBool on a "+
			"string flag silently yields false and drops JSON mode",
			forbidden, offenders)
	}
}
