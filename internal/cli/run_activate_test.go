package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

const reactivationPolicy = "opaque = \"policy added mid-run\"\n"

// reactivationFixture activates a run over a config tree, then adds a second
// issue so the next `run activate` is a re-activation. With policyAtStart the
// first activation already pins policy.toml; without it, policy.toml lands in
// the config only after the first activation.
func reactivationFixture(t *testing.T, policyAtStart bool) (*sql.DB, string) {
	t.Helper()
	// THE TEMP DIR COMES FIRST, BEFORE t.Setenv — `t.TempDir()` reads TMPDIR.
	root := t.TempDir()
	configDir := filepath.Join(root, ".docket", "config")
	for _, dir := range []string{"workflows", "contracts"} {
		testsupport.Must(t, os.MkdirAll(filepath.Join(configDir, dir), 0o755),
			"creating the %s dir", dir)
	}
	t.Setenv("DOCKET_PATH", filepath.Join(root, ".docket"))

	writeFile := func(rel, body string) {
		testsupport.Must(t, os.WriteFile(filepath.Join(configDir, rel), []byte(body), 0o644),
			"writing %s", rel)
	}
	writeFile("workflows/pin-show.toml", pinShowWorkflow)
	writeFile("contracts/judge.md", "the judge contract\n")
	if policyAtStart {
		writeFile("policy.toml", reactivationPolicy)
	}

	conn := newTestDB(t)
	first := createIssue(t, conn, "first", model.StatusBacklog, model.PriorityNone)
	run, err := db.InsertRun(conn, 1, "test run", 0, model.NowMS())
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, first), "AddRunIssue")
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "first Activate: %v", err)

	if !policyAtStart {
		writeFile("policy.toml", reactivationPolicy)
	}
	second := createIssue(t, conn, "late arrival", model.StatusBacklog, model.PriorityNone)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, second), "AddRunIssue")
	return conn, run.Ref()
}

// reactivate drives `run activate` over the fixture's run and returns stdout
// and stderr.
func reactivate(t *testing.T, conn *sql.DB, runRef string, jsonMode bool) (string, string) {
	t.Helper()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	w := &output.Writer{JSONMode: jsonMode, Stdout: stdout, Stderr: stderr}
	testsupport.Must(t, runActivateWithWriter(t, conn, w, runRef), "run activate")
	return stdout.String(), stderr.String()
}

// policyPinOf reads the run's pinned policy.toml hash from the store.
func policyPinOf(t *testing.T, conn *sql.DB, runRef string) string {
	t.Helper()
	runID, err := model.ParseRunID(runRef)
	testsupport.Must(t, err, "parsing %s: %v", runRef, err)
	pins, err := db.ListPins(conn, runID)
	testsupport.Must(t, err, "listing pins: %v", err)
	for _, p := range pins {
		if p.Kind == db.PinKindFile && p.Ref == "policy.toml" {
			return p.SHA256
		}
	}
	t.Fatalf("run %s pins no policy.toml", runRef)
	return ""
}

// TestRunActivateNamesANewlyPinnedPolicyOnStderr: a re-activation that pins a
// policy.toml the run did not start under says so in human mode, and says
// nothing when the policy pin was inherited or under --json.
func TestRunActivateNamesANewlyPinnedPolicyOnStderr(t *testing.T) {
	t.Run("a new policy pin prints its ref, hash, and the adoption", func(t *testing.T) {
		conn, runRef := reactivationFixture(t, false)

		_, stderr := reactivate(t, conn, runRef, false)
		sha := policyPinOf(t, conn, runRef)
		var line string
		for _, l := range strings.Split(stderr, "\n") {
			if strings.Contains(l, "policy.toml") && strings.Contains(l, sha) {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("stderr names no newly pinned policy.toml at %s:\n%s", sha, stderr)
		}
		if !strings.Contains(line, "waiting rows adopt it") {
			t.Errorf("policy line does not say waiting rows adopt it: %q", line)
		}
	})

	t.Run("an inherited policy pin prints nothing", func(t *testing.T) {
		conn, runRef := reactivationFixture(t, true)

		_, stderr := reactivate(t, conn, runRef, false)
		if strings.Contains(stderr, "waiting rows adopt") {
			t.Errorf("stderr reports a new policy pin for an inherited one:\n%s", stderr)
		}
	})

	t.Run("--json prints nothing on stderr", func(t *testing.T) {
		conn, runRef := reactivationFixture(t, false)

		_, stderr := reactivate(t, conn, runRef, true)
		if strings.Contains(stderr, "policy.toml") {
			t.Errorf("--json wrote a policy line to stderr:\n%s", stderr)
		}
	})
}

// TestRunActivateJSONCarriesANewPolicyPin: the envelope carries the newly
// pinned policy.toml under `new_policy_pin` and omits the key otherwise.
func TestRunActivateJSONCarriesANewPolicyPin(t *testing.T) {
	// THE KEYS ARE SPELLED HERE, not borrowed from activateResult: decoding
	// into the production type would follow a renamed tag and pass.
	type envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}

	t.Run("a new policy pin is carried with ref and sha256", func(t *testing.T) {
		conn, runRef := reactivationFixture(t, false)

		stdout, _ := reactivate(t, conn, runRef, true)
		var env envelope
		testsupport.Must(t, json.Unmarshal([]byte(stdout), &env), "decoding %s", stdout)
		raw, ok := env.Data["new_policy_pin"]
		if !ok {
			t.Fatalf("envelope carries no new_policy_pin key: %s", stdout)
		}
		var pin struct {
			Ref    string `json:"ref"`
			SHA256 string `json:"sha256"`
		}
		testsupport.Must(t, json.Unmarshal(raw, &pin), "decoding new_policy_pin %s", raw)
		if want := policyPinOf(t, conn, runRef); pin.Ref != "policy.toml" || pin.SHA256 != want {
			t.Errorf("new_policy_pin = %+v, want ref policy.toml and sha256 %s", pin, want)
		}
	})

	t.Run("an inherited policy pin omits the key", func(t *testing.T) {
		conn, runRef := reactivationFixture(t, true)

		stdout, _ := reactivate(t, conn, runRef, true)
		var env envelope
		testsupport.Must(t, json.Unmarshal([]byte(stdout), &env), "decoding %s", stdout)
		if raw, ok := env.Data["new_policy_pin"]; ok {
			t.Errorf("envelope carries new_policy_pin %s for an inherited policy pin", raw)
		}
	})
}
