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

// BeginTx runs a read-write transaction at READ COMMITTED, the level the
// write lock needs: each statement after the lock sees every earlier writer's
// commit. It refuses any other explicitly requested level rather than
// silently replacing it.
func (c *storeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.ReadOnly {
		return c.Conn.BeginTx(ctx, opts)
	}
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault, sql.LevelReadCommitted:
	default:
		return nil, fmt.Errorf("postgres read-write transactions run at READ COMMITTED, not %s", sql.IsolationLevel(opts.Isolation))
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
	query, args = fromSQLite(query, len(args)), integerBooleans(args)
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
	return c.Conn.QueryContext(ctx, fromSQLite(query, len(args)), integerBooleans(args))
}

// integerBooleans binds each bool argument as 1 or 0, as SQLite does. The
// PostgreSQL schema keeps SQLite's integer flag columns as BIGINT, and pgx
// will not encode a bool into one.
func integerBooleans(args []driver.NamedValue) []driver.NamedValue {
	for i, arg := range args {
		if b, ok := arg.Value.(bool); ok {
			args[i].Value = int64(0)
			if b {
				args[i].Value = int64(1)
			}
		}
	}
	return args
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
// the SQLite JSON functions become their jsonb forms, and the placeholders
// are rebound. The helpers keep OR IGNORE because on SQLite it also skips a
// row that fails NOT NULL or CHECK, which ON CONFLICT DO NOTHING would not;
// PostgreSQL rejects such a row instead.
func fromSQLite(query string, nargs int) string {
	return rebindPlaceholders(jsonCallsToJSONB(insertOrIgnoreToOnConflict(query)), nargs)
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
		if end := textEnd(query, i); end > i {
			b.WriteString(query[i:end])
			i = end - 1
			continue
		}
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// textEnd returns the index just past the string literal, quoted identifier,
// or comment that starts at i, or i when none does.
func textEnd(query string, i int) int {
	switch {
	case query[i] == '\'' || query[i] == '"':
		return closingQuote(query, i, query[i])
	case strings.HasPrefix(query[i:], "--"):
		if end := strings.IndexByte(query[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(query)
	case strings.HasPrefix(query[i:], "/*"):
		if end := strings.Index(query[i+2:], "*/"); end >= 0 {
			return i + 2 + end + 2
		}
		return len(query)
	}
	return i
}

// jsonCallsToJSONB rewrites the SQLite JSON functions the helpers call on
// their JSON columns, which PostgreSQL stores as jsonb:
//
//   - json_each(x) over an array becomes jsonb_array_elements_text(x), whose
//     single column is named value as json_each's value column is;
//   - json_valid(x) becomes (x IS JSON);
//   - json_extract(x, '$.key') becomes (x ->> 'key').
//
// Any other form, such as a deeper json_extract path, is left as written, so
// PostgreSQL rejects it rather than running a different query.
func jsonCallsToJSONB(query string) string {
	if !strings.Contains(strings.ToLower(query), "json_") {
		return query
	}
	var b strings.Builder
	for i := 0; i < len(query); {
		if end := textEnd(query, i); end > i {
			b.WriteString(query[i:end])
			i = end
			continue
		}
		if name, args, end, ok := sqliteJSONCall(query, i); ok {
			if rewritten, ok := jsonbCall(name, args); ok {
				b.WriteString(rewritten)
				i = end
				continue
			}
		}
		b.WriteByte(query[i])
		i++
	}
	return b.String()
}

var sqliteJSONFunctions = []string{"json_each", "json_valid", "json_extract"}

// sqliteJSONCall reports whether a call to one of sqliteJSONFunctions starts
// at i, returning the function's name, its top-level arguments as written,
// and the index just past its closing parenthesis.
func sqliteJSONCall(query string, i int) (name string, args []string, end int, ok bool) {
	if i > 0 && isIdentByte(query[i-1]) {
		return "", nil, 0, false
	}
	for _, fn := range sqliteJSONFunctions {
		if len(query)-i > len(fn) && strings.EqualFold(query[i:i+len(fn)], fn) && !isIdentByte(query[i+len(fn)]) {
			name = fn
			break
		}
	}
	if name == "" {
		return "", nil, 0, false
	}
	open := i + len(name)
	for open < len(query) && (query[open] == ' ' || query[open] == '\t' || query[open] == '\n' || query[open] == '\r') {
		open++
	}
	if open == len(query) || query[open] != '(' {
		return "", nil, 0, false
	}
	depth, argStart := 0, open+1
	for j := open; j < len(query); j++ {
		if e := textEnd(query, j); e > j {
			j = e - 1
			continue
		}
		switch query[j] {
		case '(':
			depth++
		case ',':
			if depth == 1 {
				args = append(args, strings.TrimSpace(query[argStart:j]))
				argStart = j + 1
			}
		case ')':
			depth--
			if depth == 0 {
				args = append(args, strings.TrimSpace(query[argStart:j]))
				return name, args, j + 1, true
			}
		}
	}
	return "", nil, 0, false
}

// jsonbCall is the PostgreSQL form of a SQLite JSON call, or false when the
// call has a form jsonCallsToJSONB does not translate.
func jsonbCall(name string, args []string) (string, bool) {
	for k := range args {
		args[k] = jsonCallsToJSONB(args[k])
	}
	switch {
	case name == "json_each" && len(args) == 1:
		return "jsonb_array_elements_text(" + args[0] + ")", true
	case name == "json_valid" && len(args) == 1:
		return "(" + args[0] + " IS JSON)", true
	case name == "json_extract" && len(args) == 2:
		key, ok := strings.CutPrefix(args[1], "'$.")
		if !ok || !strings.HasSuffix(key, "'") || !isIdent(key[:len(key)-1]) {
			return "", false
		}
		return "(" + args[0] + " ->> '" + key + ")", true
	}
	return "", false
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return true
}

func isIdentByte(ch byte) bool {
	return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
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
