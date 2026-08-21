package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
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

// walkCommands visits every command in the tree, root included.
func walkCommands(root *cobra.Command, visit func(*cobra.Command)) {
	visit(root)
	for _, child := range root.Commands() {
		walkCommands(child, visit)
	}
}
