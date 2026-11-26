package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// clientConfigSandbox gives init and config a temp HOME with XDG_CONFIG_HOME
// unset, so the client config resolves to the default user config directory
// under that HOME, and a temp store and working directory that share nothing
// with it. It returns where the client config must land.
func clientConfigSandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	t.Chdir(t.TempDir())
	return filepath.Join(home, ".config", "docket", "client.toml")
}

// runRootVerb drives `docket <args>` through the real root command, captures
// stdout, and resets sub's flags and the root's afterwards so no later
// invocation inherits this one's.
func runRootVerb(t *testing.T, sub *cobra.Command, args ...string) (string, error) {
	t.Helper()
	rootCmd.SetArgs(args)
	defer func() {
		rootCmd.SetArgs(nil)
		reset := func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
		sub.Flags().Visit(reset)
		rootCmd.PersistentFlags().Visit(reset)
	}()
	stdout := captureStdout(t)
	err := rootCmd.Execute()
	return stdout(), err
}

// clientConfigDoc decodes the client config independently of the code under
// test.
type clientConfigDoc struct {
	Version        int    `toml:"version"`
	DefaultProfile string `toml:"default_profile"`
	Profile        map[string]struct {
		URL string `toml:"url"`
	} `toml:"profile"`
}

func readClientConfigDoc(t *testing.T, path string) clientConfigDoc {
	t.Helper()
	data, err := os.ReadFile(path)
	testsupport.Must(t, err, "reading the client config: %v", err)
	var doc clientConfigDoc
	_, err = toml.Decode(string(data), &doc)
	testsupport.Must(t, err, "decoding the client config: %v\n%s", err, data)
	return doc
}

func requireMode0600(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	testsupport.Must(t, err, "the client config is missing at %s: %v", path, err)
	if !info.Mode().IsRegular() {
		t.Fatalf("%s is %v, want a regular file", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("client config mode = %04o, want 0600", perm)
	}
}

func TestClientConfigInitCreatesFile(t *testing.T) {
	path := clientConfigSandbox(t)

	_, err := runRootVerb(t, initCmd, "init")
	testsupport.Must(t, err, "docket init: %v", err)

	requireMode0600(t, path)
	if doc := readClientConfigDoc(t, path); doc.Version != 1 {
		t.Errorf("client config version = %d, want 1", doc.Version)
	}
	loaded, err := config.LoadClientConfig()
	testsupport.Must(t, err, "the client config init wrote does not load: %v", err)
	if loaded.Path != path {
		t.Errorf("client config loads from %s, want %s", loaded.Path, path)
	}
}

func TestClientConfigSetUpdatesFile(t *testing.T) {
	path := clientConfigSandbox(t)
	_, err := runRootVerb(t, initCmd, "init")
	testsupport.Must(t, err, "docket init: %v", err)
	requireMode0600(t, path)

	// A profile for default_profile to name, written into the file init
	// created; WriteFile on an existing file keeps its mode.
	client := "version = 1\n\n[profile.prod]\nurl = \"https://docket.example.test\"\n"
	testsupport.Must(t, os.WriteFile(path, []byte(client), 0o600), "writing client.toml")

	_, err = runRootVerb(t, configSetCmd, "config", "set", "default_profile", "prod")
	testsupport.Must(t, err, "docket config set default_profile prod: %v", err)

	requireMode0600(t, path)
	doc := readClientConfigDoc(t, path)
	if doc.DefaultProfile != "prod" {
		t.Errorf("default_profile = %q in %s, want prod", doc.DefaultProfile, path)
	}
	if doc.Version != 1 || doc.Profile["prod"].URL != "https://docket.example.test" {
		t.Errorf("config set lost the rest of the file: %+v", doc)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	testsupport.Must(t, err, "listing the config dir: %v", err)
	if len(entries) != 1 || entries[0].Name() != "client.toml" {
		t.Errorf("config dir holds %v, want only client.toml", entries)
	}

	out, err := runRootVerb(t, configGetCmd, "config", "get", "default_profile", "--json")
	testsupport.Must(t, err, "docket config get default_profile: %v", err)
	var env struct {
		Data struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, out)
	}
	if env.Data.Key != "default_profile" || env.Data.Value != "prod" {
		t.Errorf("config get default_profile = %+v, want default_profile=prod:\n%s", env.Data, out)
	}
}
