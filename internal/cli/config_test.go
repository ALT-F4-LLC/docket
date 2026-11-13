package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// runConfigOutput runs `docket config` against cfg and returns what it wrote
// to stdout.
func runConfigOutput(t *testing.T, cfg *config.Config, jsonMode bool) string {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.WithValue(context.Background(), cfgKey, cfg))
	w, buf := bufWriter(jsonMode)
	if err := runConfig(cmd, nil, w); err != nil {
		t.Fatalf("runConfig: %v", err)
	}
	return buf.String()
}

// initializedStore creates an initialized database under a temporary store
// directory, so runConfig takes its database-found branch.
func initializedStore(t *testing.T) *config.Config {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".docket")
	testsupport.Must(t, os.MkdirAll(dir, 0o755), "mkdir store")
	cfg := &config.Config{DocketDir: dir, DBPath: filepath.Join(dir, "issues.db")}
	conn, err := db.Open(cfg.DBPath)
	testsupport.Must(t, err, "Open: %v", err)
	defer conn.Close()
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	return cfg
}

func TestConfigRedactsPostgresDSNAndNeverReadsPasswordFile(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "pgpass")
	testsupport.Must(t, os.WriteFile(passwordFile, []byte("FILESECRET\n"), 0o600), "write password file")
	t.Setenv(config.PostgresPasswordFileEnv, passwordFile)

	missing := t.TempDir()
	stores := map[string]func(*testing.T) *config.Config{
		"database missing": func(*testing.T) *config.Config {
			return &config.Config{DocketDir: missing, DBPath: filepath.Join(missing, "issues.db")}
		},
		"database found": initializedStore,
	}
	dsns := map[string]string{
		"userinfo password":        "postgres://u:USERSECRET@host/db",
		"password query parameter": "postgres://u@host/db?sslmode=disable&password=QUERYSECRET",
		"userinfo username":        "postgres://USERNAMESECRET:PWSECRET@host/db",
		"user query parameter":     "postgres://host/db?user=USERNAMESECRET",
		"secret query key":         "postgres://u@host/db?KEYSECRET=x",
	}
	for storeName, store := range stores {
		for dsnName, dsn := range dsns {
			for _, jsonMode := range []bool{false, true} {
				t.Run(storeName+"/"+dsnName+"/json="+map[bool]string{false: "off", true: "on"}[jsonMode], func(t *testing.T) {
					t.Setenv(config.PostgresDSNEnv, dsn)
					out := runConfigOutput(t, store(t), jsonMode)
					for _, secret := range []string{"USERSECRET", "QUERYSECRET", "FILESECRET", "USERNAMESECRET", "PWSECRET", "KEYSECRET"} {
						if strings.Contains(out, secret) {
							t.Errorf("docket config output contains %s:\n%s", secret, out)
						}
					}
					for _, want := range []string{config.RedactedMarker, "host/db", passwordFile} {
						if !strings.Contains(out, want) {
							t.Errorf("docket config output lacks %q:\n%s", want, out)
						}
					}
				})
			}
		}
	}
}

// TestConfigSetRecordsTheProjectItRegisters keeps the project-registered event
// the root hook wrote before `config set` resolved its own project.
func TestConfigSetRecordsTheProjectItRegisters(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)

	_, err = runLeaseCommandOutput(t, conn, &config.Config{Identity: "/repos/configured", Anchored: true},
		configSetCmd, "attempt.max", "5")
	testsupport.Must(t, err, "config set: %v", err)

	got := registrationEvents(t, conn)
	if len(got) != 1 || got["/repos/configured"] != "config set" {
		t.Errorf("registration events = %v, want /repos/configured attributed to \"config set\"", got)
	}
}

// TestConfigSetRecordsTheProjectItRegisteredWhenTheWriteIsRefused: the
// registration precedes the key and value checks, so a refused write from a
// new anchored checkout still records the project-registered event.
func TestConfigSetRecordsTheProjectItRegisteredWhenTheWriteIsRefused(t *testing.T) {
	conn := newTestDB(t)
	_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)

	_, err = runLeaseCommandOutput(t, conn, &config.Config{Identity: "/repos/configured", Anchored: true},
		configSetCmd, "no.such.key", "5")
	if err == nil || !strings.Contains(err.Error(), "no.such.key") {
		t.Fatalf("config set no.such.key = %v, want a refusal naming the key", err)
	}

	got := registrationEvents(t, conn)
	if len(got) != 1 || got["/repos/configured"] != "config set" {
		t.Errorf("registration events = %v, want /repos/configured attributed to \"config set\"", got)
	}
}

// TestConfigSetDeliversTheLeaseClassWarning: a lease.ttl.<class> no
// registered workflow declares is stored, and the warning reaches the user on
// stderr and in the success message.
func TestConfigSetDeliversTheLeaseClassWarning(t *testing.T) {
	conn := newTestDB(t)
	registerForRun(t, conn, runWorkflow)

	stderr := captureStderr(t)
	out, err := runLeaseCommandOutput(t, conn, nil, configSetCmd, "lease.ttl.read", "30m")
	printed := stderr()
	testsupport.Must(t, err, "config set lease.ttl.read: %v", err)

	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\n%s", err, out)
	}
	for name, text := range map[string]string{"stderr": printed, "message": env.Message} {
		for _, needle := range []string{"warning:", `"read"`, "someone"} {
			if !strings.Contains(text, needle) {
				t.Errorf("%s lacks %q:\n%s", name, needle, text)
			}
		}
	}
	if !strings.HasPrefix(env.Message, "Set lease.ttl.read = 30m\n") {
		t.Errorf("message = %q, want the set line before the warning", env.Message)
	}
}

// TestConfigSetDefaultProfileNeedsNoProject: default_profile lives in the
// per-user client config, so a caller with no project may set it, and doing
// so registers no project. The unanchored caller pins that nothing refuses
// it; the anchored one, which project resolution would register, pins that
// nothing registers it.
func TestConfigSetDefaultProfileNeedsNoProject(t *testing.T) {
	for _, anchored := range []bool{false, true} {
		t.Run(map[bool]string{false: "unanchored", true: "anchored"}[anchored], func(t *testing.T) {
			path := clientConfigSandbox(t)
			testsupport.Must(t, os.MkdirAll(filepath.Dir(path), 0o700), "mkdir client config dir")
			client := "version = 1\n\n[profile.prod]\nurl = \"https://docket.example.test\"\n"
			testsupport.Must(t, os.WriteFile(path, []byte(client), 0o600), "writing client.toml")
			conn := newTestDB(t)
			_, err := db.EnsureProject(conn, "/repos/first", "first", 1)
			testsupport.Must(t, err, "claiming the default project: %v", err)

			_, err = runLeaseCommandOutput(t, conn, &config.Config{Identity: "/scratch/unseen", Anchored: anchored},
				configSetCmd, clientDefaultProfileKey, "prod")
			testsupport.Must(t, err, "config set default_profile prod: %v", err)

			if doc := readClientConfigDoc(t, path); doc.DefaultProfile != "prod" {
				t.Errorf("default_profile = %q in %s, want prod", doc.DefaultProfile, path)
			}
			_, found, err := db.LookupProject(conn, "/scratch/unseen")
			testsupport.Must(t, err, "LookupProject: %v", err)
			if found {
				t.Error("config set default_profile registered /scratch/unseen")
			}
			if got := registrationEvents(t, conn); len(got) != 0 {
				t.Errorf("registration events = %v, want none", got)
			}
		})
	}
}

// TestConfigGetAndSetOwnTheirProject pins `config get` and `config set` to the
// verb layer: the root hook leaves project resolution to verb.ConfigGet and
// verb.ConfigSet, and get must still read the override set wrote for the
// same project.
func TestConfigGetAndSetOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{configGetCmd, configSetCmd} {
		if _, ok := cmd.Annotations["verbOwnsProject"]; !ok {
			t.Fatalf("config %s lacks the verbOwnsProject annotation", cmd.Name())
		}
	}

	clientConfigSandbox(t)
	_, err := runRootVerb(t, initCmd, "init")
	testsupport.Must(t, err, "docket init: %v", err)
	_, err = runRootVerb(t, configSetCmd, "config", "set", "attempt.max", "5")
	testsupport.Must(t, err, "docket config set attempt.max 5: %v", err)

	read := func(args ...string) db.ConfigEntry {
		t.Helper()
		out, err := runRootVerb(t, configGetCmd, append([]string{"config", "get"}, args...)...)
		testsupport.Must(t, err, "docket config get %v: %v", args, err)
		var env struct {
			Data db.ConfigEntry `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("unmarshal envelope: %v\n%s", err, out)
		}
		return env.Data
	}

	if got := read("attempt.max", "--json"); got != (db.ConfigEntry{Key: "attempt.max", Value: "5", Source: "set"}) {
		t.Errorf("config get attempt.max = %+v, want the project's 5 (set)", got)
	}
	if got := read("attempt.max", "--global", "--json"); got.Source != "default" {
		t.Errorf("config get attempt.max --global = %+v, want the store-wide default", got)
	}
}
