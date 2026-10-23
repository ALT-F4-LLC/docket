package config

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The caller of a record is the executor, so GitHead must not follow a GIT_DIR
// it exports: the head it returns is the directory's own.
func TestGitHeadIgnoresARedirectingGitDir(t *testing.T) {
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mk := func() (string, string) {
		dir := t.TempDir()
		run(dir, "init", "-q")
		run(dir, "commit", "-q", "--allow-empty", "-m", "c")
		return dir, run(dir, "rev-parse", "HEAD")
	}
	want, wantSHA := mk()
	other, otherSHA := mk()
	if wantSHA == otherSHA {
		run(other, "commit", "-q", "--allow-empty", "-m", "d")
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	if _, got := GitHead(want); got != wantSHA {
		t.Errorf("GitHead followed GIT_DIR: got %s, want %s", got, wantSHA)
	}
}

// GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM name config files every git command
// reads (GIT_CONFIG only `git config` does), and GIT_EXEC_PATH names where git
// finds its own sub-commands; this process inherits the executor's environment,
// so the head lookup drops them.
func TestGitHeadCmdDropsGitConfig(t *testing.T) {
	evil := filepath.Join(t.TempDir(), "evil")
	for _, k := range []string{"GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_EXEC_PATH", "GIT_TEMPLATE_DIR"} {
		t.Setenv(k, evil)
	}
	env := strings.Join(gitHeadCmd(context.Background(), t.TempDir(), "status").Env, "\n")
	for _, bad := range []string{"GIT_CONFIG=", "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=", "GIT_EXEC_PATH=", "GIT_TEMPLATE_DIR="} {
		if strings.Contains(env, bad) {
			t.Errorf("gitHeadCmd kept %s", bad)
		}
	}
}
