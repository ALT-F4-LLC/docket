package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// SQLiteImage is a SQLite database built once per test binary and copied for
// each test that asks. Migrating a fresh store replays the whole migration
// chain, which costs far more than anything most tests then do with the
// store; a test that needs a migrated store, and is not about migrating one,
// takes a copy instead.
//
// The zero value is ready to use. File and Memory keep separate images, since
// a file image carries its journal mode and an in-memory database cannot
// adopt one built in write-ahead-log mode.
type SQLiteImage struct {
	file, memory sqliteImageBytes
}

type sqliteImageBytes struct {
	once  sync.Once
	bytes []byte
	err   error
}

// File writes a copy of the image to path and fails the test when path
// already exists, so a copy never replaces a store a test made. build creates
// the database at the path it is given, and must checkpoint and close it
// before returning; it runs once per test binary, on the first call.
func (s *SQLiteImage) File(t *testing.T, path string, build func(path string) error) {
	t.Helper()
	s.file.once.Do(func() { s.file.bytes, s.file.err = buildSQLiteFileImage(build) })
	Must(t, s.file.err, "building the SQLite file image: %v", s.file.err)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	Must(t, err, "creating %s for the SQLite image: %v", path, err)
	_, err = f.Write(s.file.bytes)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	Must(t, err, "writing the SQLite image to %s: %v", path, err)
}

func buildSQLiteFileImage(build func(path string) error) ([]byte, error) {
	dir, err := os.MkdirTemp("", "docket-sqlite-image-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "image.db")
	if err := build(path); err != nil {
		return nil, err
	}
	// A write-ahead log left beside the file holds pages the file lacks, so
	// the file alone would be an older database than the one build made.
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() > 0 {
		return nil, fmt.Errorf("%s-wal is not empty: build must checkpoint and close the database", path)
	}
	return os.ReadFile(path)
}

// Memory replaces the contents of conn, an in-memory database held on a
// single connection, with a copy of the image. build returns an open
// in-memory database holding the image's contents; it runs once per test
// binary, on the first call, and Memory closes what it returns.
func (s *SQLiteImage) Memory(t *testing.T, conn *sql.DB, build func() (*sql.DB, error)) {
	t.Helper()
	s.memory.once.Do(func() { s.memory.bytes, s.memory.err = buildSQLiteMemoryImage(build) })
	Must(t, s.memory.err, "building the SQLite memory image: %v", s.memory.err)

	err := onSQLiteConn(conn, func(c sqliteSerializer) error { return c.Deserialize(s.memory.bytes) })
	Must(t, err, "restoring the SQLite memory image: %v", err)
}

func buildSQLiteMemoryImage(build func() (*sql.DB, error)) (image []byte, err error) {
	source, err := build()
	if err != nil {
		return nil, err
	}
	defer source.Close()

	err = onSQLiteConn(source, func(c sqliteSerializer) error {
		image, err = c.Serialize()
		return err
	})
	return image, err
}

// sqliteSerializer is the part of the SQLite driver's connection that copies
// a whole database to and from bytes.
type sqliteSerializer interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

func onSQLiteConn(conn *sql.DB, f func(sqliteSerializer) error) error {
	c, err := conn.Conn(context.Background())
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Raw(func(driverConn any) error {
		s, ok := driverConn.(sqliteSerializer)
		if !ok {
			return fmt.Errorf("driver connection %T cannot serialize a database", driverConn)
		}
		return f(s)
	})
}
