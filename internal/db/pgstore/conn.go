package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// storeConnector hands out the connections the store's pool runs the
// internal/db helpers on. Those helpers are written once for both stores, with
// SQLite's ? placeholders and with database/sql transactions whose write lock
// the SQLite handle takes at BEGIN. A storeConn keeps both contracts on
// PostgreSQL:
//
//   - fromSQLite translates each statement's SQLite-only syntax;
//   - a read-write transaction, and an Exec issued outside any transaction,
//     takes the write lock WithWriteTx takes, at READ COMMITTED, before its
//     first statement; a read-only transaction and a query outside a
//     transaction take no lock, so reads never wait on a writer.
type storeConnector struct {
	inner driver.Connector
}

func newStoreConnector(cfg *pgx.ConnConfig) storeConnector {
	return storeConnector{inner: stdlib.GetConnector(*cfg)}
}

func (c storeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &storeConn{Conn: conn.(*stdlib.Conn)}, nil
}

func (c storeConnector) Driver() driver.Driver { return c.inner.Driver() }

// storeConn embeds the pgx connection, so Ping, CheckNamedValue,
// ResetSession, IsValid and Close keep pgx's behavior.
type storeConn struct {
	*stdlib.Conn
}

func (c *storeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *storeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly {
		return c.Conn.BeginTx(ctx, opts)
	}
	opts.Isolation = driver.IsolationLevel(sql.LevelReadCommitted)
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if _, err := c.Conn.ExecContext(ctx, takeWriteLockSQL, []driver.NamedValue{{Ordinal: 1, Value: writeLockKey}}); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("taking the postgres write lock: %w", err)
	}
	return tx, nil
}

func (c *storeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	query = fromSQLite(query, len(args))
	if c.inTransaction() {
		return c.Conn.ExecContext(ctx, query, args)
	}
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		return nil, err
	}
	res, err := c.Conn.ExecContext(ctx, query, args)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *storeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.QueryContext(ctx, fromSQLite(query, len(args)), args)
}

func (c *storeConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *storeConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Conn.PrepareContext(ctx, fromSQLite(query, strings.Count(query, "?")))
}

// inTransaction reports whether the session has a transaction open, failed
// ones included.
func (c *storeConn) inTransaction() bool {
	return c.Conn.Conn().PgConn().TxStatus() != 'I'
}

// fromSQLite turns a helper's SQLite statement into the PostgreSQL one that
// does the same: INSERT OR IGNORE becomes INSERT ... ON CONFLICT DO NOTHING,
// and the placeholders are rebound. The helpers keep OR IGNORE because on
// SQLite it also skips a row that fails NOT NULL or CHECK, which ON CONFLICT
// DO NOTHING would not; PostgreSQL rejects such a row instead.
func fromSQLite(query string, nargs int) string {
	return rebindPlaceholders(insertOrIgnoreToOnConflict(query), nargs)
}

const insertOrIgnorePrefix = "INSERT OR IGNORE INTO"

func insertOrIgnoreToOnConflict(query string) string {
	body := strings.TrimLeft(query, " \t\r\n")
	if len(body) < len(insertOrIgnorePrefix) || !strings.EqualFold(body[:len(insertOrIgnorePrefix)], insertOrIgnorePrefix) {
		return query
	}
	body = strings.TrimRight(body[len(insertOrIgnorePrefix):], " \t\r\n;")
	return "INSERT INTO" + body + " ON CONFLICT DO NOTHING"
}

// rebindPlaceholders rewrites each ? that SQLite would bind as a parameter to
// $1, $2, ... in order. A ? inside a string literal, a quoted identifier, or a
// comment is text, not a parameter, and stays. A statement sent without
// arguments is returned unchanged, so a migration's DDL reaches the server as
// written.
func rebindPlaceholders(query string, nargs int) string {
	if nargs == 0 || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + nargs*2)
	n := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		switch {
		case ch == '\'' || ch == '"':
			end := closingQuote(query, i, ch)
			b.WriteString(query[i:end])
			i = end - 1
		case ch == '-' && strings.HasPrefix(query[i:], "--"):
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = len(query) - i
			}
			b.WriteString(query[i : i+end])
			i += end - 1
		case ch == '/' && strings.HasPrefix(query[i:], "/*"):
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				end = len(query) - i
			} else {
				end += 4
			}
			b.WriteString(query[i : i+end])
			i += end - 1
		case ch == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// closingQuote returns the index just past the quote that closes the one at
// start, treating a doubled quote as an escaped one, or len(query) when it is
// unterminated.
func closingQuote(query string, start int, quote byte) int {
	for i := start + 1; i < len(query); i++ {
		if query[i] != quote {
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(query)
}
