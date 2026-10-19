package pgstore

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
)

func TestPgstoreOpenVersionCheck(t *testing.T) {
	t.Run("refuses a server older than 16", func(t *testing.T) {
		pg15 := func(context.Context, *sql.DB) (int, error) { return 150000, nil }

		db, err := open(context.Background(), "postgres://docket@localhost/docket", "", pg15)
		if err == nil {
			db.Close()
			t.Fatal("open accepted server_version_num 150000; want an error naming version 15")
		}
		if !strings.Contains(err.Error(), "15") {
			t.Fatalf("error %q does not name the version found (15)", err)
		}
	})

	t.Run("refuses a DSN the policy rejects before connecting", func(t *testing.T) {
		isolatePGEnv(t)
		neverRead := func(context.Context, *sql.DB) (int, error) {
			t.Fatal("open read the server version for a DSN the policy rejects")
			return 0, nil
		}

		db, err := open(context.Background(), "host=db.example.com user=docket dbname=docket sslmode=prefer", "", neverRead)
		if err == nil {
			db.Close()
			t.Fatal("open accepted an off-host DSN with sslmode=prefer")
		}
	})

	t.Run("opens a real 16 server", func(t *testing.T) {
		dsn := os.Getenv("DOCKET_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("DOCKET_TEST_POSTGRES_DSN is unset; skipping the real-server check")
		}

		db, err := Open(context.Background(), dsn, "")
		if err != nil {
			t.Fatalf("Open against DOCKET_TEST_POSTGRES_DSN: %v", err)
		}
		db.Close()
	})
}
