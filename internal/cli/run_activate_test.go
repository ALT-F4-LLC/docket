package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
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

// TestRunActivateScopeWarningsNameEachWarningsOwnRemedy: the human-mode scope
// warnings print the `--scope` remedy only for an issue that declares no scope,
// since the other scope lints' reasons already name their remedy, and a warning
// with no bound workflow leaves no gap where the workflow name would be.
func TestRunActivateScopeWarningsNameEachWarningsOwnRemedy(t *testing.T) {
	// The reasons are the engine lints' wording, spelled here rather than
	// borrowed so the test pins what an operator actually reads.
	warnings := []engine.ScopeWarning{
		{
			IssueID:  "DKT-1",
			Workflow: "standard-change@1",
			Reason: "no scope is declared, so its \"implement\" step holds the " +
				"tree without excluding, or being excluded by, any other issue",
		},
		{
			IssueID: "DKT-2",
			Reason: "none of its declared scope paths resolves under /repo — the " +
				"issue may belong to a different repository; re-home it with " +
				"`docket issue move --project` or fix the scope",
		},
		{
			IssueID: "DKT-3",
			Reason: "scope names a.go but not its existing test a_test.go — a fix " +
				"that must change the test cannot land; add the test to the issue's scope",
		},
	}
	stderr := &bytes.Buffer{}
	warnScopeWarnings(&output.Writer{Stdout: &bytes.Buffer{}, Stderr: stderr}, warnings)

	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != len(warnings) {
		t.Fatalf("want one line per warning (%d), got %d:\n%s", len(warnings), len(lines), stderr)
	}
	for i, w := range warnings {
		if !strings.Contains(lines[i], w.IssueID) || !strings.Contains(lines[i], w.Reason) {
			t.Fatalf("line %d does not carry %s and its reason: %q", i, w.IssueID, lines[i])
		}
	}

	if want := "declare one with `docket issue edit DKT-1 --scope GLOB`"; !strings.Contains(lines[0], want) {
		t.Errorf("no-scope warning lacks %q: %q", want, lines[0])
	}
	for _, line := range lines[1:] {
		if strings.Contains(line, "declare one with") {
			t.Errorf("warning appends the no-scope remedy to a reason naming its own: %q", line)
		}
		if strings.Contains(line, "  ") {
			t.Errorf("warning with no workflow leaves a doubled space: %q", line)
		}
	}
}

// The activating checkout's head rides on the run-activated event.
func TestRunActivateRecordsTheCheckoutHead(t *testing.T) {
	conn, runRef := reactivationFixture(t, true)
	checkout := t.TempDir()
	repinGit(t, checkout, "init", "-q", "-b", "trunk")
	repinGit(t, checkout, "commit", "-q", "--allow-empty", "-m", "base")
	commit := repinGit(t, checkout, "rev-parse", "HEAD")

	cmd := newRunActivateCmd()
	cmd.SetContext(context.WithValue(context.WithValue(context.Background(), dbKey, conn),
		cfgKey, &config.Config{ExecRoot: checkout}))
	w, _ := bufWriter(true)
	err := runRunActivate(cmd, []string{runRef}, w)
	testsupport.Must(t, err, "run activate: %v", err)

	var data string
	err = conn.QueryRow(`SELECT data FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		engine.EventRunActivated).Scan(&data)
	testsupport.Must(t, err, "reading the run-activated event: %v", err)
	var head struct {
		Branch string `json:"head_branch"`
		Commit string `json:"head_commit"`
	}
	if err := json.Unmarshal([]byte(data), &head); err != nil {
		t.Fatalf("decoding the event %s: %v", data, err)
	}
	if head.Branch != "trunk" || head.Commit != commit {
		t.Errorf("event head = %+v, want branch trunk at %s", head, commit)
	}
}

// lintIssue is one bound issue of a routing-lint fixture: its security label,
// its scope globs as JSON, and its attached files.
type lintIssue struct {
	security bool
	scope    string
	files    []string
}

// routingLintDryRun binds issues to a fresh run, configures the lexicon (empty
// leaves it off), and returns the human dry run's stderr.
func routingLintDryRun(t *testing.T, lexicon string, issues ...lintIssue) string {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, ".docket", "config")
	testsupport.Must(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755), "creating the workflows dir")
	testsupport.Must(t, os.MkdirAll(filepath.Join(configDir, "contracts"), 0o755), "creating the contracts dir")
	t.Setenv("DOCKET_PATH", filepath.Join(root, ".docket"))
	testsupport.Must(t, os.WriteFile(filepath.Join(configDir, "workflows", "pin-show.toml"),
		[]byte(pinShowWorkflow), 0o644), "writing the workflow")
	testsupport.Must(t, os.WriteFile(filepath.Join(configDir, "contracts", "judge.md"),
		[]byte("the judge contract\n"), 0o644), "writing the contract")

	conn := newTestDB(t)
	if lexicon != "" {
		testsupport.Must(t, db.SetConfig(conn, 1, db.KeyRoutingSecurityLexicon, lexicon), "setting the lexicon")
	}
	run, err := db.InsertRun(conn, 1, "test run", 0, model.NowMS())
	testsupport.Must(t, err, "InsertRun: %v", err)
	for i, issue := range issues {
		var labels []string
		if issue.security {
			labels = []string{"security"}
		}
		id, err := db.CreateIssue(conn, &model.Issue{
			Title: "issue " + string(rune('A'+i)), Status: model.StatusBacklog,
			Priority: model.PriorityNone, Kind: model.IssueKindFeature,
		}, labels, issue.files)
		testsupport.Must(t, err, "CreateIssue: %v", err)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, id, issue.scope), "declaring scope")
		testsupport.Must(t, db.AddRunIssue(conn, run.ID, id), "AddRunIssue")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	w := &output.Writer{Stdout: stdout, Stderr: stderr}
	testsupport.Must(t, runActivateWithWriter(t, conn, w, run.Ref(), "--dry-run"), "run activate --dry-run")
	return stderr.String()
}

// routingLintLine returns the security-routing lint's disclosure line, or "".
func routingLintLine(stderr string) string {
	for _, l := range strings.Split(stderr, "\n") {
		if strings.Contains(l, "security-routing lint") {
			return l
		}
	}
	return ""
}

// TestRunActivateDryRunDisclosesUncomparedIssues: an empty routing-warning
// list is read against what the overlap check compared. A linted issue that
// formed no comparable pair with any labelled issue is disclosed even when it
// is comparable in general and other pairs were compared, and the warnings
// for a lint that compared nothing describe comparability accurately.
func TestRunActivateDryRunDisclosesUncomparedIssues(t *testing.T) {
	const lexicon = "zzz-no-such-term"
	const uncompared = "linted issue(s) was compared against no labelled issue"
	goFile := []string{"internal/exec/resolve.go"}

	t.Run("1 a file-only issue meets a glob-only labelled issue", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, lexicon,
			lintIssue{security: true, scope: `["internal/exec/**"]`},
			lintIssue{scope: `["**"]`, files: goFile},
		))
		if !strings.Contains(line, "1 of 1 "+uncompared) {
			t.Errorf("want a warning that 1 of 1 %s; got %q", uncompared, line)
		}
	})

	t.Run("2 one of two linted issues is uncompared although a pair compared", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, lexicon,
			lintIssue{security: true, scope: `["internal/exec/**"]`},
			lintIssue{scope: `["internal/exec/sub/**"]`},
			lintIssue{scope: `["**"]`, files: goFile},
		))
		if !strings.Contains(line, "1 of 2 "+uncompared) {
			t.Errorf("want a warning that 1 of 2 %s; got %q", uncompared, line)
		}
	})

	t.Run("3 a glob-only issue meets a file-only labelled issue", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, lexicon,
			lintIssue{security: true, scope: `["**"]`, files: goFile},
			lintIssue{scope: `["internal/exec/**"]`},
		))
		if !strings.Contains(line, "1 of 1 "+uncompared) {
			t.Errorf("want a warning that 1 of 1 %s; got %q", uncompared, line)
		}
	})

	t.Run("4 checked nothing does not deny a comparable labelled issue", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, "",
			lintIssue{security: true, scope: `["internal/exec/**"]`},
			lintIssue{scope: `["**"]`},
		))
		if !strings.Contains(line, "checked nothing") {
			t.Fatalf("want the checked-nothing warning; got %q", line)
		}
		if strings.Contains(line, "no bound issue with a comparable scope carries") {
			t.Errorf("warning denies the comparable labelled issue that is bound: %q", line)
		}
	})

	t.Run("5 an unscoped issue is told a file makes it comparable", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, lexicon,
			lintIssue{security: true, scope: `["internal/exec/**"]`},
			lintIssue{scope: `["**"]`},
		))
		if !strings.Contains(line, "attaching a file") || !strings.Contains(line, "makes an issue comparable") {
			t.Errorf("want the warning to say attaching a file makes an issue comparable; got %q", line)
		}
	})
}

// TestRunActivateDryRunIsSilentWithNoLabelledIssueBound: with a lexicon
// configured, no labelled issue bound, and every linted issue comparable, the
// overlap check had no pair to compare, so the dry run prints no routing-lint
// line. A labelled issue that is bound but comparable with nothing still earns
// the uncompared warning.
func TestRunActivateDryRunIsSilentWithNoLabelledIssueBound(t *testing.T) {
	if line := routingLintLine(routingLintDryRun(t, "zzz-no-such-term",
		lintIssue{scope: `["internal/exec/**"]`},
		lintIssue{scope: `["**"]`, files: []string{"internal/exec/resolve.go"}},
	)); line != "" {
		t.Errorf("want no routing-lint line with no labelled issue bound; got %q", line)
	}
	if line := routingLintLine(routingLintDryRun(t, "zzz-no-such-term",
		lintIssue{security: true, scope: `["**"]`},
		lintIssue{scope: `["internal/exec/**"]`},
	)); !strings.Contains(line, "1 of 1 linted issue(s) was compared against no labelled issue") {
		t.Errorf("want the uncompared warning for a bound but uncomparable labelled issue; got %q", line)
	}
}

// TestRunActivateDryRunRoutingLintStatesWhatWasChecked: the uncompared warning
// claims a lexicon check only when a lexicon is configured, and the warning for
// a run with no labelled issue bound names the unscoped issues while a run
// with a bound but uncomparable labelled issue does not deny it is bound.
func TestRunActivateDryRunRoutingLintStatesWhatWasChecked(t *testing.T) {
	goFile := []string{"internal/exec/resolve.go"}

	t.Run("no lexicon: the uncompared issue was not lexicon-checked", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, "",
			lintIssue{security: true, scope: `["internal/exec/**"]`},
			lintIssue{scope: `["internal/exec/sub/**"]`},
			lintIssue{scope: `["**"]`, files: goFile},
		))
		if !strings.Contains(line, "1 of 2 linted issue(s) was compared against no labelled issue") ||
			!strings.Contains(line, "nothing was checked for them") || strings.Contains(line, "only the lexicon") {
			t.Errorf("want the uncompared warning saying nothing was checked; got %q", line)
		}
	})

	t.Run("no labelled issue bound and an unscoped issue", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, "zzz-no-such-term",
			lintIssue{scope: `["**"]`},
		))
		if !strings.Contains(line, "compared no scopes") || !strings.Contains(line, "attaching a file") {
			t.Errorf("want the compared-no-scopes warning naming the remedy; got %q", line)
		}
	})

	t.Run("a bound but uncomparable labelled issue is not denied", func(t *testing.T) {
		line := routingLintLine(routingLintDryRun(t, "zzz-no-such-term",
			lintIssue{security: true, scope: `["**"]`},
			lintIssue{scope: `["**"]`},
		))
		if !strings.Contains(line, "1 of 1 linted issue(s) was compared against no labelled issue") ||
			strings.Contains(line, "no bound issue carries") {
			t.Errorf("want the uncompared warning without denying the labelled issue; got %q", line)
		}
	})
}
