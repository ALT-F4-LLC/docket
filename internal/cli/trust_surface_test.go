package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestNoTrustPathSurface is T8's surface guard, EXTENDED to the CLI as
// internal/trust's own copy said group 2 would (gates-trust §3.1, §9.1).
//
// THE RULE: no flag and no config key may name a trust-file path. §3.1's
// resolution table has exactly two rows — $XDG_CONFIG_HOME/docket/trust.toml,
// else $HOME/.config/docket/trust.toml — and states "There is no third source."
//
// The reason is direct and is why this is a mechanical guard rather than a
// convention: every additional way to point docket at a trust file is another
// way for REPO CONTENT — a checked-in .envrc, a direnv hook, a Makefile — to
// point it at a file the repo controls. That is the whole of the malicious-clone
// threat (T1) reopening through a flag somebody added for convenience.
//
// Group 1 could only check the trust package's own surface, because the package
// was unreachable from any command. Now the verbs exist, so this walks the
// COBRA TREE — every command, every flag, including the ones this stage added —
// and the config-key registry.
func TestNoTrustPathSurface(t *testing.T) {
	// A path-shaped name for the trust store, in any of the spellings someone
	// would reach for. `--trust-file` is the obvious one; the rest are the
	// near misses a reviewer would have to catch by eye otherwise.
	banned := []string{
		"trust-file", "trust_file", "trustfile",
		"trust-path", "trust_path", "trustpath",
		"trust-store", "trust_store", "truststore",
		"trust-config", "trust_config",
	}

	var offenses []string
	walkCommands(rootCmd, func(cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			for _, bad := range banned {
				if strings.Contains(strings.ToLower(f.Name), bad) {
					offenses = append(offenses,
						"flag --"+f.Name+" on `"+cmd.CommandPath()+"`")
				}
			}
		})
		cmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
			for _, bad := range banned {
				if strings.Contains(strings.ToLower(f.Name), bad) {
					offenses = append(offenses,
						"persistent flag --"+f.Name+" on `"+cmd.CommandPath()+"`")
				}
			}
		})
	})

	// The config-key registry, the other half of §3.1's "no config key".
	for _, key := range db.KnownConfigKeys() {
		for _, bad := range banned {
			if strings.Contains(strings.ToLower(key), bad) {
				offenses = append(offenses, "config key "+key)
			}
		}
	}

	for _, offense := range offenses {
		t.Errorf("%s names a trust-file path; §3.1 admits no source for the "+
			"store path other than XDG_CONFIG_HOME, because every additional "+
			"one lets repo content choose the trust file", offense)
	}

	// POSITIVE HALF, so the guard cannot pass vacuously against a tree that
	// has no trust verbs at all: the verbs this stage added must be present.
	// A guard that passed because it found nothing to check is not a guard.
	found := map[string]bool{}
	walkCommands(rootCmd, func(cmd *cobra.Command) {
		found[cmd.CommandPath()] = true
	})
	for _, want := range []string{
		"docket trust", "docket trust add", "docket trust list", "docket trust rm",
	} {
		if !found[want] {
			t.Errorf("`%s` is not in the command tree; this guard would pass "+
				"vacuously without the verbs it exists to check", want)
		}
	}
}

// TestTrustVerbsSkipTheDatabase pins §3.5's other structural property: the
// trust store is USER-LEVEL, so managing it does not require a repository.
//
// `trust list` and `trust rm` outside a repo work and write no event (§3.6). If
// these verbs demanded a database, a stranger who installed docket could not
// inspect their own allowlist until they created a tracker.
func TestTrustVerbsSkipTheDatabase(t *testing.T) {
	for _, path := range []string{
		"docket trust", "docket trust add", "docket trust list", "docket trust rm",
	} {
		var cmd *cobra.Command
		walkCommands(rootCmd, func(c *cobra.Command) {
			if c.CommandPath() == path {
				cmd = c
			}
		})
		if cmd == nil {
			t.Errorf("`%s` is not in the command tree", path)
			continue
		}
		if _, ok := cmd.Annotations["skipDB"]; !ok {
			t.Errorf("`%s` does not carry skipDB; the trust store is user-level "+
				"and managing it must not require a repository", path)
		}
	}
}

// TestTrustAddDeclaredFilesPinTheExecRoot drives `trust add --declared-file`
// from a linked worktree, where the checkout a gate runs in (ExecRoot) is not
// the directory the entry binds to (Identity). Both checkouts hold the
// declared paths with DIFFERENT bytes, so only hashes of the ExecRoot copies
// pass. One path carries a comma, which a CSV-splitting flag would break in
// two. The refusals that follow must leave the store byte-for-byte unchanged.
func TestTrustAddDeclaredFilesPinTheExecRoot(t *testing.T) {
	main, cfg := trustNoRepo(t)
	repinWrite(t, main, "Makefile", "main makefile\n")
	repinWrite(t, main, "scripts/env,local.sh", "main env\n")
	repinGit(t, main, "init", "-q", ".")
	repinGit(t, main, "add", "-A")
	repinGit(t, main, "commit", "-q", "-m", "base")

	linked := filepath.Join(t.TempDir(), "linked")
	repinGit(t, main, "worktree", "add", "-q", "-b", "linked", linked)
	repinWrite(t, linked, "Makefile", "linked makefile\n")
	repinWrite(t, linked, "scripts/env,local.sh", "linked env\n")
	cfg.ExecRoot = linked

	sha := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		return hex.EncodeToString(sum[:])
	}
	want := []trust.DeclaredFile{
		{Path: "Makefile", SHA256: sha("linked makefile\n")},
		{Path: "scripts/env,local.sh", SHA256: sha("linked env\n")},
	}

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "build",
		"--declared-file", "Makefile", "--declared-file", "scripts/env,local.sh",
		"--", "make", "build")
	testsupport.Must(t, err, "trust add with declared files: %v", err)

	entries := trustEntries(t)
	if len(entries) != 1 || !slices.Equal(entries[0].DeclaredFiles, want) {
		t.Fatalf("stored entries = %+v, want one entry declaring %+v (the ExecRoot copies)", entries, want)
	}

	listCmd := newTrustListCmd()
	jsonCmdWithVersion(t, listCmd, "v1")
	stdout := captureStdout(t)
	err = runTrustVerb(t, cfg, listCmd)
	out := stdout()
	testsupport.Must(t, err, "trust list: %v", err)
	var envelope struct {
		Data struct {
			Entries []struct {
				DeclaredFiles []struct {
					Path   string `json:"path"`
					SHA256 string `json:"sha256"`
				} `json:"declared_files"`
			} `json:"entries"`
		} `json:"data"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &envelope); jsonErr != nil {
		t.Fatalf("trust list JSON did not parse: %v\n%s", jsonErr, out)
	}
	if len(envelope.Data.Entries) != 1 {
		t.Fatalf("trust list shows %d entries, want 1:\n%s", len(envelope.Data.Entries), out)
	}
	listed := envelope.Data.Entries[0].DeclaredFiles
	if len(listed) != len(want) {
		t.Fatalf("trust list shows declared files %+v, want %+v", listed, want)
	}
	for i, d := range listed {
		if d.Path != want[i].Path || d.SHA256 != want[i].SHA256 {
			t.Errorf("trust list declared file %d = %+v, want %+v", i, d, want[i])
		}
	}

	storePath, err := trust.StorePath()
	testsupport.Must(t, err, "StorePath: %v", err)
	before, err := os.ReadFile(storePath)
	testsupport.Must(t, err, "reading %s: %v", storePath, err)

	refusals := []struct {
		name    string
		args    []string
		mention string
	}{
		{"missing file", []string{"check",
			"--declared-file", "Makefile", "--declared-file", "absent.mk",
			"--", "make", "check"}, "absent.mk"},
		{"global", []string{"everywhere", "--global",
			"--declared-file", "Makefile", "--", "make", "everywhere"}, "global"},
	}
	for _, tc := range refusals {
		err := runTrustVerb(t, cfg, newTrustAddCmd(), tc.args...)
		if err == nil {
			t.Errorf("%s: trust add succeeded, want a refusal", tc.name)
		} else if !strings.Contains(err.Error(), tc.mention) {
			t.Errorf("%s: refusal %q does not mention %q", tc.name, err, tc.mention)
		}
		after, readErr := os.ReadFile(storePath)
		testsupport.Must(t, readErr, "reading %s: %v", storePath, readErr)
		if !bytes.Equal(before, after) {
			t.Errorf("%s: the refused add changed the store:\n%s", tc.name, after)
		}
	}
}

// walkCommands visits every command in the tree, root included.
func walkCommands(root *cobra.Command, visit func(*cobra.Command)) {
	visit(root)
	for _, child := range root.Commands() {
		walkCommands(child, visit)
	}
}
