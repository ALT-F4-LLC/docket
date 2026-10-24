package cli

import (
	"context"
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
