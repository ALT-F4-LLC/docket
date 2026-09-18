package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/spf13/cobra"
)

// probeGitRepo git-inits `repo` (the trustNoRepo/trustRepo fixture) with one
// commit, so `trust probe`'s `git rev-parse HEAD` / `git worktree add` have a
// real object database — the same hermetic identity gitdiff_test.go's gitEnv
// uses, so no test reads the operator's own git config.
func probeGitRepo(t *testing.T, repo string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=qa", "GIT_AUTHOR_EMAIL=qa@example.invalid",
			"GIT_COMMITTER_NAME=qa", "GIT_COMMITTER_EMAIL=qa@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		testsupport.Must(t, err, "git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	err := os.WriteFile(repo+"/README.md", []byte("hello\n"), 0o644)
	testsupport.Must(t, err, "writing README: %v", err)
	run("init", "-q", ".")
	run("add", "-A")
	run("commit", "-qm", "base")
}

// addTrustEntry writes one entry to the isolated store `cfg` points at,
// bound to this repository, bypassing the CLI so a probe test's fixture is
// one call rather than a shelled-out `trust add`.
func addTrustEntry(t *testing.T, cfg *config.Config, name string, argv ...string) {
	t.Helper()
	_, err := trust.Add(trust.AddRequest{
		Name: name, Argv: argv, RepoRoot: cfg.Identity, NowMS: time.Now().UnixMilli(),
	})
	testsupport.Must(t, err, "trust.Add(%s): %v", name, err)
}

// jsonCmdWithVersion is a bare command wired with the same string --json flag
// rootCmd's persistent one provides, set to the given version — the pattern
// trust_event_test.go's captureStdout callers use, since these commands are
// built standalone rather than through rootCmd.
func jsonCmdWithVersion(t *testing.T, cmd *cobra.Command, version string) {
	t.Helper()
	cmd.Flags().String("json", "", "")
	err := cmd.Flags().Set("json", version)
	testsupport.Must(t, err, "setting --json=%s: %v", version, err)
}

// TestTrustProbeRunsTheRosterAndCleansUp is DKT-1283's CLI-level proof: a
// trusted gate that fails is reported, a declared action is skipped, and the
// worktree the probe stood up is gone afterward — end to end, through cobra.
func TestTrustProbeRunsTheRosterAndCleansUp(t *testing.T) {
	repo, cfg := trustNoRepo(t)
	probeGitRepo(t, repo)

	addTrustEntry(t, cfg, "passes", "true")
	addTrustEntry(t, cfg, "fails", "false")

	cmd := newTrustProbeCmd()
	jsonCmdWithVersion(t, cmd, "v2")

	stdout := captureStdout(t)
	err := runTrustVerb(t, cfg, cmd, "--run", "RUN-9")
	out := stdout()
	testsupport.Must(t, err, "trust probe: %v", err)

	var envelope struct {
		Data struct {
			Run    string   `json:"run"`
			Head   string   `json:"head"`
			Passed bool     `json:"passed"`
			Failed []string `json:"failed"`
			Gates  []struct {
				Name string `json:"name"`
				Exit *int   `json:"exit"`
			} `json:"gates"`
		} `json:"data"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &envelope); jsonErr != nil {
		t.Fatalf("trust probe's JSON did not parse: %v\n%s", jsonErr, out)
	}

	if envelope.Data.Run != "RUN-9" {
		t.Errorf("run = %q, want RUN-9", envelope.Data.Run)
	}
	if envelope.Data.Head == "" {
		t.Error("want a resolved HEAD sha")
	}
	if envelope.Data.Passed {
		t.Error("the `fails` entry must make the probe not-passed")
	}
	if len(envelope.Data.Failed) != 1 || envelope.Data.Failed[0] != "fails" {
		t.Errorf("failed = %v, want [fails]", envelope.Data.Failed)
	}
	if len(envelope.Data.Gates) != 2 {
		t.Errorf("want both entries run, got %d gate rows", len(envelope.Data.Gates))
	}

	if n := worktreeCountCLI(t, repo); n != 0 {
		t.Errorf("want no leftover worktree after `trust probe`, got %d", n)
	}
}

// TestTrustProbeRefusesOutsideARepo: ProbeTrust needs a real checkout to
// stand a worktree up from, so an unresolved repo is a validation error, not
// a panic or a silent empty report.
func TestTrustProbeRefusesOutsideARepo(t *testing.T) {
	cmd := newTrustProbeCmd()
	err := runTrustVerb(t, nil, cmd)
	if err == nil {
		t.Fatal("want a refusal with no resolvable repository")
	}
	errorCodeOf(t, err) // fatals unless the failure carries a taxonomy code
}

func worktreeCountCLI(t *testing.T, repo string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	testsupport.Must(t, err, "git worktree list: %v", err)
	return strings.Count(string(out), "worktree ") - 1
}
