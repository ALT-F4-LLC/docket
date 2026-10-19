// Package pgstore is the PostgreSQL store. It connects through database/sql
// with the pgx stdlib driver as the one database role the DSN names.
package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// minServerVersionNum is PostgreSQL 16 in server_version_num form.
const minServerVersionNum = 160000

// versionReader returns the connected server's server_version_num.
type versionReader func(ctx context.Context, db *sql.DB) (int, error)

// Open connects to the PostgreSQL server the DSN names and refuses a server
// older than PostgreSQL 16.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	return open(ctx, dsn, readServerVersionNum)
}

func open(ctx context.Context, dsn string, readVersion versionReader) (*sql.DB, error) {
	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres database: %w", err)
	}

	versionNum, err := readVersion(ctx, db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("reading postgres server version: %w", err)
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
