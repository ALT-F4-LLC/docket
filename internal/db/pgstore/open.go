// Package pgstore is the PostgreSQL store. It connects through database/sql
// with the pgx stdlib driver as the one database role the DSN names.
package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// minServerVersionNum is PostgreSQL 16 in server_version_num form.
const minServerVersionNum = 160000

// versionReader returns the connected server's server_version_num.
type versionReader func(ctx context.Context, db *sql.DB) (int, error)

// Open connects to the PostgreSQL server the DSN names and refuses a server
// older than PostgreSQL 16. The DSN and passwordFile must satisfy resolveDSN's
// policy; an empty passwordFile means peer auth or no password.
func Open(ctx context.Context, dsn, passwordFile string) (*sql.DB, error) {
	return open(ctx, dsn, passwordFile, readServerVersionNum)
}

func open(ctx context.Context, dsn, passwordFile string, readVersion versionReader) (*sql.DB, error) {
	return openPool(ctx, dsn, passwordFile, readVersion, func(cfg *pgx.ConnConfig) *sql.DB {
		return stdlib.OpenDB(*cfg)
	})
}

// openPool resolves the DSN under the store's policy, builds the pool with
// newPool, and checks the server version on it. newPool receives the checked
// config and keeps it for every pooled connection; sql.Open with the DSN would
// re-parse it, and re-read the environment, per connection.
func openPool(ctx context.Context, dsn, passwordFile string, readVersion versionReader, newPool func(*pgx.ConnConfig) *sql.DB) (*sql.DB, error) {
	cfg, err := resolveDSN(dsn, passwordFile, (*os.File).Stat)
	if err != nil {
		return nil, err
	}
	db := newPool(cfg)

	versionNum, err := readVersion(ctx, db)
	if err != nil {
		db.Close()
		// The pgx connect error holds the config, password included, in
		// exported fields, so only its text, with the password scrubbed,
		// reaches the caller.
		msg := err.Error()
		if cfg.Password != "" {
			msg = strings.ReplaceAll(msg, cfg.Password, "[redacted]")
		}
		return nil, fmt.Errorf("reading postgres server version: %s", msg)
	}
	if versionNum < minServerVersionNum {
		db.Close()
		return nil, fmt.Errorf("postgres server version %d.%d (server_version_num %d) is older than the required 16",
			versionNum/10000, versionNum%10000, versionNum)
	}

	return db, nil
}

func readServerVersionNum(ctx context.Context, db *sql.DB) (int, error) {
	var versionNum int
	err := db.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::int").Scan(&versionNum)
	return versionNum, err
}
