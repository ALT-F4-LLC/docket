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
		otherSHA = run(other, "rev-parse", "HEAD")
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	if _, got := GitHead(want); got != wantSHA {
		t.Errorf("GitHead followed GIT_DIR: got %s, want %s", got, wantSHA)
	}
}

// GIT_CONFIG names a config file git is asked to read; this process inherits the
// executor's environment, so the head lookup drops it.
func TestGitHeadCmdDropsGitConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG", filepath.Join(t.TempDir(), "evil"))
	for _, kv := range gitHeadCmd(context.Background(), t.TempDir(), "status").Env {
		if strings.HasPrefix(kv, "GIT_CONFIG=") {
			t.Errorf("gitHeadCmd kept %q", kv)
		}
	}
}
