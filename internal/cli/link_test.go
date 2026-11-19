package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/spf13/pflag"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// linkSandbox gives `docket link` a user config directory and a project
// directory that share nothing: a temp HOME and XDG_CONFIG_HOME holding a
// client.toml with one profile, and a separate temp project directory that
// becomes the working directory. DOCKET_PATH is cleared so the project path
// resolves the way it does for an operator, from the working directory.
func linkSandbox(t *testing.T) (project, configDir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("DOCKET_PATH", "")

	configDir = filepath.Join(xdg, "docket")
	testsupport.Must(t, os.Mkdir(configDir, 0o700), "creating the config dir")
	client := "version = 1\n\n[profile.prod]\nurl = \"https://docket.example.test\"\n\n[profile.staging]\nurl = \"https://staging.example.test\"\n"
	testsupport.Must(t, os.WriteFile(filepath.Join(configDir, "client.toml"), []byte(client), 0o600),
		"writing client.toml")

	project, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving the project dir: %v", err)
	t.Chdir(project)
	return project, configDir
}

// runLink drives `docket link <args>` through the real root command and
// resets every flag afterwards so no later invocation inherits this one's.
func runLink(t *testing.T, args ...string) error {
	t.Helper()
	rootCmd.SetArgs(append([]string{"link"}, args...))
	defer func() {
		rootCmd.SetArgs(nil)
		reset := func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		projectLinkCmd.Flags().Visit(reset)
		rootCmd.PersistentFlags().Visit(reset)
	}()
	return rootCmd.Execute()
}

// treeListing names every file and directory under root, so a test can show
// a command left the tree exactly as it found it.
func treeListing(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		names = append(names, path)
		return nil
	})
	testsupport.Must(t, err, "walking %s: %v", root, err)
	return names
}

// readLinkEntries decodes the link file independently of the code under test.
func readLinkEntries(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	testsupport.Must(t, err, "reading the link file: %v", err)
	var doc struct {
		Version int                       `toml:"version"`
		Link    map[string]map[string]any `toml:"link"`
	}
	_, err = toml.Decode(string(data), &doc)
	testsupport.Must(t, err, "decoding the link file: %v\n%s", err, data)
	if doc.Version != 1 {
		t.Fatalf("link file version = %d, want 1", doc.Version)
	}
	return doc.Link
}

func TestLinkWritesUserLevelEntryAndNoRepoFile(t *testing.T) {
	project, configDir := linkSandbox(t)
	before := treeListing(t, project)

	testsupport.Must(t, runLink(t, "--profile", "prod", "--project-id", "42"), "docket link")

	if after := treeListing(t, project); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("docket link changed the working directory tree:\nbefore %v\nafter  %v", before, after)
	}

	linkPath := filepath.Join(configDir, "links.toml")
	info, err := os.Stat(linkPath)
	testsupport.Must(t, err, "the link file was not written: %v", err)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("link file mode = %04o, want 0600", perm)
	}

	entries := readLinkEntries(t, linkPath)
	entry, ok := entries[project]
	if !ok {
		t.Fatalf("no entry for %s in %v", project, entries)
	}
	if entry["profile"] != "prod" {
		t.Errorf("profile = %v, want prod", entry["profile"])
	}
	if entry["project_id"] != int64(42) {
		t.Errorf("project_id = %v (%T), want 42", entry["project_id"], entry["project_id"])
	}
}

func TestLinkAgainReplacesTheEntryForThePath(t *testing.T) {
	project, configDir := linkSandbox(t)

	testsupport.Must(t, runLink(t, "--profile", "prod", "--project-id", "42"), "first docket link")
	testsupport.Must(t, runLink(t, "--profile", "staging", "--project-id", "7"), "second docket link")

	linkPath := filepath.Join(configDir, "links.toml")
	data, err := os.ReadFile(linkPath)
	testsupport.Must(t, err, "reading the link file: %v", err)
	// Counted in the raw text as well as decoded, so a duplicate entry fails
	// even if a decoder were to tolerate it.
	if n := strings.Count(string(data), project); n != 1 {
		t.Fatalf("the link file names %s %d times, want exactly one entry:\n%s", project, n, data)
	}

	entries := readLinkEntries(t, linkPath)
	if len(entries) != 1 {
		t.Fatalf("link file holds %d entries, want 1: %v", len(entries), entries)
	}
	entry := entries[project]
	if entry["profile"] != "staging" || entry["project_id"] != int64(7) {
		t.Errorf("entry = %v, want profile staging and project_id 7", entry)
	}
}

func TestLinkRefusesAProfileClientConfigDoesNotDefine(t *testing.T) {
	_, configDir := linkSandbox(t)

	err := runLink(t, "--profile", "nowhere", "--project-id", "42")
	var ce *CmdError
	if !asCmdError(err, &ce) || ce.Code != "VALIDATION_ERROR" {
		t.Fatalf("err = %v, want a VALIDATION_ERROR naming the unknown profile", err)
	}
	if _, statErr := os.Stat(filepath.Join(configDir, "links.toml")); !os.IsNotExist(statErr) {
		t.Errorf("a refused link wrote the link file (stat err %v)", statErr)
	}
}
