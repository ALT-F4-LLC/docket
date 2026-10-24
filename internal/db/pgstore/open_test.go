package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
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
		db, err := Open(context.Background(), testsupport.PostgresDSN(t), "")
		if err != nil {
			t.Fatalf("Open against DOCKET_TEST_POSTGRES_DSN: %v", err)
		}
		db.Close()
	})
}

func TestPgstoreOpenRedactsCredentials(t *testing.T) {
	isolatePGEnv(t)
	const sentinel = "S3NTINEL-pw"
	const unreachable = "host=127.0.0.1 port=1 user=docket dbname=docket sslmode=disable connect_timeout=2"

	for name, c := range map[string]struct{ dsn, passwordFile string }{
		"password in url userinfo": {
			dsn: "postgres://docket:" + sentinel + "@127.0.0.1:1/docket?sslmode=disable&connect_timeout=2",
		},
		"password in key/value form": {
			dsn: unreachable + " password=" + sentinel,
		},
		"password pgx cannot redact": {
			dsn: "host=127.0.0.1 port=1 password=S3NTINEL " + sentinel + " connect_timeout=2",
		},
		"password file holding the sentinel": {
			dsn:          unreachable,
			passwordFile: writePasswordFile(t, sentinel+"\n", 0o600),
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			db, err := Open(ctx, c.dsn, c.passwordFile)
			if err == nil {
				db.Close()
				t.Fatal("Open connected to an unreachable address")
			}
			if strings.Contains(err.Error(), "S3NTINEL") {
				t.Fatalf("Open error carries the password: %q", err)
			}
			if c.passwordFile != "" && !strings.Contains(err.Error(), "reading postgres server version") {
				t.Fatalf("Open error %q is not a connection failure; the password file never reached the connect", err)
			}
			var connectErr *pgconn.ConnectError
			var parseErr *pgconn.ParseConfigError
			if errors.As(err, &connectErr) || errors.As(err, &parseErr) {
				t.Fatalf("Open error %q wraps a pgx error whose fields hold the password or DSN", err)
			}
		})
	}

	t.Run("driver error echoing the file's password", func(t *testing.T) {
		echoPassword := func(context.Context, *sql.DB) (int, error) {
			return 0, errors.New("server said: password " + sentinel + " rejected")
		}

		db, err := open(context.Background(), unreachable, writePasswordFile(t, sentinel+"\n", 0o600), echoPassword)
		if err == nil {
			db.Close()
			t.Fatal("open succeeded although reading the server version failed")
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("open error carries the password the driver echoed: %q", err)
		}
	})
}
