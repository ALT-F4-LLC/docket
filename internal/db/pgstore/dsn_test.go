package pgstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// isolatePGEnv keeps the developer's PG* variables and ~/.pgpass, service
// file, and ~/.postgresql certificates out of the resolver under test.
func isolatePGEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{
		"PGHOST", "PGPORT", "PGUSER", "PGDATABASE", "PGPASSWORD", "PGPASSFILE",
		"PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGSSLNEGOTIATION",
		"PGSERVICE", "PGSERVICEFILE",
	} {
		t.Setenv(name, "")
	}
}

func resolveWithRealStat(dsn, passwordFile string) (*pgx.ConnConfig, error) {
	return resolveDSN(dsn, passwordFile, (*os.File).Stat)
}

func TestResolveDSNRejectsEmbeddedPassword(t *testing.T) {
	isolatePGEnv(t)
	const secret = "SECRET"

	for name, dsn := range map[string]string{
		"url userinfo":   "postgres://docket:" + secret + "@db.example.com/docket?sslmode=verify-full",
		"key/value":      "host=db.example.com user=docket dbname=docket password=" + secret + " sslmode=verify-full",
		"url query":      "postgres://docket@db.example.com/docket?password=" + secret + "&sslmode=verify-full",
		"malformed url":  "postgres://docket:SEC:RET@db.example.com/docket?sslmode=verify-full",
		"key/value port": "host=db.example.com port=x user=docket password=" + secret + " sslmode=verify-full",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := resolveWithRealStat(dsn, "")
			if err == nil {
				t.Fatalf("resolveDSN accepted a DSN with an embedded password (resolved password %q)", cfg.Password)
			}
			if strings.Contains(err.Error(), "SEC") {
				t.Fatalf("rejection error carries the password: %q", err)
			}
		})
	}

	t.Run("password-free DSN resolves", func(t *testing.T) {
		if _, err := resolveWithRealStat("postgres://docket@db.example.com/docket?sslmode=verify-full", ""); err != nil {
			t.Fatalf("resolveDSN refused a password-free verify-full DSN: %v", err)
		}
	})
}

func writePasswordFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pgpassword")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

const socketDSN = "host=/var/run/postgresql user=docket dbname=docket"

func TestResolveDSNReadsOwned0600PasswordFile(t *testing.T) {
	isolatePGEnv(t)
	path := writePasswordFile(t, "file-pw\n", 0o600)

	cfg, err := resolveWithRealStat(socketDSN, path)
	if err != nil {
		t.Fatalf("resolveDSN refused an owned 0600 password file: %v", err)
	}
	if cfg.Password != "file-pw" {
		t.Fatalf("resolved password %q, want the file's contents %q", cfg.Password, "file-pw")
	}
}

func TestResolveDSNRefusesPasswordFileWithMode0644(t *testing.T) {
	isolatePGEnv(t)
	path := writePasswordFile(t, "file-pw\n", 0o644)

	cfg, err := resolveWithRealStat(socketDSN, path)
	if err == nil {
		t.Fatalf("resolveDSN loaded a 0644 password file (resolved password %q)", cfg.Password)
	}
	if strings.Contains(err.Error(), "file-pw") {
		t.Fatalf("refusal error carries the file's contents: %q", err)
	}
}

// foreignOwnerInfo reports a real file's stat result with its owner uid
// replaced, standing in for a file another user owns.
type foreignOwnerInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (f foreignOwnerInfo) Sys() any { return &f.stat }

func TestResolveDSNRefusesPasswordFileOwnedByAnotherUID(t *testing.T) {
	isolatePGEnv(t)
	path := writePasswordFile(t, "file-pw\n", 0o600)
	otherUID := uint32(os.Geteuid() + 1)
	statAsOtherOwner := func(f *os.File) (os.FileInfo, error) {
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		stat := *info.Sys().(*syscall.Stat_t)
		stat.Uid = otherUID
		return foreignOwnerInfo{FileInfo: info, stat: stat}, nil
	}

	cfg, err := resolveDSN(socketDSN, path, statAsOtherOwner)
	if err == nil {
		t.Fatalf("resolveDSN loaded a password file owned by uid %d (resolved password %q)", otherUID, cfg.Password)
	}
}

func TestResolveDSNAcceptsPeerAuthOnSocketHost(t *testing.T) {
	isolatePGEnv(t)

	cfg, err := resolveWithRealStat(socketDSN, "")
	if err != nil {
		t.Fatalf("resolveDSN refused peer auth on a unix-socket host: %v", err)
	}
	if cfg.Password != "" {
		t.Fatalf("resolved password %q for peer auth, want none", cfg.Password)
	}
}

func TestResolveDSNNeverTakesPasswordFromEnvironment(t *testing.T) {
	isolatePGEnv(t)
	t.Setenv("PGPASSWORD", "env-pw")

	cfg, err := resolveWithRealStat(socketDSN, "")
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	if cfg.Password != "" {
		t.Fatalf("resolved password %q from PGPASSWORD; only the password file may supply one", cfg.Password)
	}
}

func TestResolveDSNRefusesUnverifiedTLSOffHost(t *testing.T) {
	isolatePGEnv(t)
	modes := []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

	type row struct {
		host      string
		sslmode   string
		wantAllow bool
	}
	var rows []row
	for _, mode := range modes {
		rows = append(rows, row{"db.example.com", mode, mode == "verify-full"})
		rows = append(rows, row{"203.0.113.7", mode, mode == "verify-full"})
		for _, local := range []string{"localhost", "127.0.0.1", "127.0.0.2", "::1", "/var/run/postgresql"} {
			rows = append(rows, row{local, mode, true})
		}
	}
	rows = append(rows,
		row{"127.0.0.1.example.com", "disable", false},
		row{"localhost.example.com", "disable", false},
		row{"127.0.0.1,db.example.com", "prefer", false},
		row{"127.0.0.1,db.example.com", "verify-full", true},
	)

	for _, r := range rows {
		t.Run(r.host+"/"+r.sslmode, func(t *testing.T) {
			dsn := fmt.Sprintf("host=%s user=docket dbname=docket sslmode=%s", r.host, r.sslmode)
			_, err := resolveWithRealStat(dsn, "")
			if r.wantAllow && err != nil {
				t.Fatalf("resolveDSN refused %q: %v", dsn, err)
			}
			if !r.wantAllow && err == nil {
				t.Fatalf("resolveDSN accepted %q; an off-host server needs sslmode=verify-full", dsn)
			}
		})
	}

	t.Run("PGSSLMODE=disable under a DSN without sslmode", func(t *testing.T) {
		t.Setenv("PGSSLMODE", "disable")
		if _, err := resolveWithRealStat("host=db.example.com user=docket dbname=docket", ""); err == nil {
			t.Fatal("resolveDSN accepted an off-host DSN whose environment disables TLS")
		}
	})
}

type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemPath string
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(t.TempDir(), name+".crt")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return testCA{cert: cert, key: key, pemPath: pemPath}
}

// issueLoopbackLeaf returns a server certificate for 127.0.0.1 signed by ca.
func (ca testCA) issueLoopbackLeaf(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serveTLSOnce accepts one connection, answers the PostgreSQL SSLRequest with
// 'S', runs the server side of the TLS handshake with leaf, and closes. It
// returns the listening port and the server's handshake result.
func serveTLSOnce(t *testing.T, leaf tls.Certificate) (int, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	handshake := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			handshake <- err
			return
		}
		defer conn.Close()
		sslRequest := make([]byte, 8)
		if _, err := io.ReadFull(conn, sslRequest); err != nil {
			handshake <- err
			return
		}
		if _, err := conn.Write([]byte("S")); err != nil {
			handshake <- err
			return
		}
		handshake <- tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{leaf}}).Handshake()
	}()
	return ln.Addr().(*net.TCPAddr).Port, handshake
}

func connectVerifyFull(t *testing.T, port int, rootPEM string) error {
	t.Helper()
	dsn := fmt.Sprintf("host=127.0.0.1 port=%d user=docket dbname=docket sslmode=verify-full sslrootcert=%s", port, rootPEM)
	cfg, err := resolveWithRealStat(dsn, "")
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err == nil {
		conn.Close(ctx)
		t.Fatal("connect succeeded against a listener that closes after the handshake")
	}
	return err
}

func isCertificateVerificationError(err error) bool {
	var verifyErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	return errors.As(err, &verifyErr) || errors.As(err, &unknownAuthority)
}

func TestResolveDSNVerifyFullRefusesCertificateFromUntrustedRoot(t *testing.T) {
	isolatePGEnv(t)
	configuredRoot := newTestCA(t, "configured-root")
	otherRoot := newTestCA(t, "other-root")

	t.Run("leaf from another root fails verification", func(t *testing.T) {
		port, handshake := serveTLSOnce(t, otherRoot.issueLoopbackLeaf(t))
		err := connectVerifyFull(t, port, configuredRoot.pemPath)
		if !isCertificateVerificationError(err) {
			t.Fatalf("connect error %v is not a certificate-verification error", err)
		}
		if serverErr := <-handshake; serverErr == nil {
			t.Fatal("server completed the TLS handshake with an untrusted certificate")
		}
	})

	t.Run("control: leaf from the configured root completes the handshake", func(t *testing.T) {
		port, handshake := serveTLSOnce(t, configuredRoot.issueLoopbackLeaf(t))
		err := connectVerifyFull(t, port, configuredRoot.pemPath)
		if isCertificateVerificationError(err) {
			t.Fatalf("trusted certificate failed verification: %v", err)
		}
		if serverErr := <-handshake; serverErr != nil {
			t.Fatalf("server handshake with a trusted certificate: %v", serverErr)
		}
	})
}
