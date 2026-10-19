package pgstore

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dsnAllowedKeys are the only keys a DSN may set. Every key that carries or
// locates a password (password, passfile, service, servicefile, sslpassword)
// is absent, so pgx rejects a DSN that sets one, including a URL userinfo
// password, which pgx parses into the password key.
var dsnAllowedKeys = []string{
	"host", "port", "dbname", "user", "sslmode", "sslrootcert", "sslcert", "sslkey",
	"connect_timeout", "application_name",
}

var errDSNRejected = errors.New("postgres DSN rejected: it does not parse, or it sets a key other than " +
	strings.Join(dsnAllowedKeys, ", ") + "; a password belongs in the password file")

// fileStat returns the stat result of an opened file; tests substitute it to
// present a different owner.
type fileStat func(*os.File) (os.FileInfo, error)

// resolveDSN applies the store's connection policy and returns the config to
// connect with. The DSN must not carry a password. The password comes only
// from passwordFile, which must be a regular file with mode 0600 owned by the
// current uid; with no file the password is empty, which is peer auth on a
// unix socket. Values pgx would take from PGPASSWORD, ~/.pgpass, or a service
// file are discarded. Every host that is neither loopback nor a unix socket
// must use sslmode=verify-full.
func resolveDSN(dsn, passwordFile string, stat fileStat) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfigWithOptions(dsn, pgx.ParseConfigOptions{
		ParseConfigOptions: pgconn.ParseConfigOptions{ConnStringAllowedKeys: dsnAllowedKeys},
	})
	if err != nil {
		// pgx's error quotes the DSN with best-effort redaction only, so
		// neither it nor the DSN reaches the caller.
		return nil, errDSNRejected
	}
	if err := requireVerifiedTLSOffHost(&cfg.Config); err != nil {
		return nil, err
	}

	cfg.Password = ""
	if passwordFile != "" {
		if cfg.Password, err = readPasswordFile(passwordFile, stat); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// requireVerifiedTLSOffHost checks the primary host and every fallback pgx
// will try. Only sslmode=verify-full yields a TLS config that verifies the
// server certificate; the other modes yield no TLS config or one that sets
// InsecureSkipVerify.
func requireVerifiedTLSOffHost(cfg *pgconn.Config) error {
	targets := append([]*pgconn.FallbackConfig{{Host: cfg.Host, Port: cfg.Port, TLSConfig: cfg.TLSConfig}}, cfg.Fallbacks...)
	for _, target := range targets {
		if isLocalHost(target.Host, target.Port) {
			continue
		}
		if target.TLSConfig == nil || target.TLSConfig.InsecureSkipVerify {
			return fmt.Errorf("postgres host %q is neither loopback nor a unix socket, so it requires sslmode=verify-full; refusing the DSN", target.Host)
		}
	}
	return nil
}

func isLocalHost(host string, port uint16) bool {
	if network, _ := pgconn.NetworkAddress(host, port); network == "unix" {
		return true
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readPasswordFile checks the opened descriptor, not the path, so the checked
// file is the one read. Errors name the path and the failed check, never the
// contents.
func readPasswordFile(path string, stat fileStat) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening postgres password file: %w", err)
	}
	defer f.Close()

	info, err := stat(f)
	if err != nil {
		return "", fmt.Errorf("checking postgres password file %s: %w", path, err)
	}
	if info.Mode() != 0o600 {
		return "", fmt.Errorf("postgres password file %s has mode %v, want a regular file with mode 0600; refusing it", path, info.Mode())
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("postgres password file %s: owner uid unavailable; refusing it", path)
	}
	if owner.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("postgres password file %s is owned by uid %d, not %d; refusing it", path, owner.Uid, os.Geteuid())
	}

	contents, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("reading postgres password file %s: %w", path, err)
	}
	return strings.TrimRight(string(contents), "\r\n"), nil
}
