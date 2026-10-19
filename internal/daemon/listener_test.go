package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
)

// testKeyPair writes a self-signed certificate for 127.0.0.1 and its key into
// a temp dir, generated at runtime, and returns the two paths and a pool that
// trusts the certificate.
func testKeyPair(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "docket test daemon"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	return certFile, keyFile, roots
}

// testSecret is a synthetic 64-character bearer secret drawn at runtime.
func testSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// writeDaemonConfig writes the user-level daemon config under a sandboxed
// XDG_CONFIG_HOME, binding the wildcard address so the listener is not a
// loopback one.
func writeDaemonConfig(t *testing.T, xdg, certFile, keyFile, secret string) {
	t.Helper()
	dir := filepath.Join(xdg, "docket")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("version = 1\nlisten = \"0.0.0.0:0\"\ntls_cert_file = %q\ntls_key_file = %q\nbearer_secret = %q\n",
		certFile, keyFile, secret)
	if err := os.WriteFile(filepath.Join(dir, "daemon.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loadDaemonConfig reads the daemon config from disk, as a daemon start does.
func loadDaemonConfig(t *testing.T) config.DaemonConfig {
	t.Helper()
	cfg, err := config.LoadDaemonConfig()
	if err != nil {
		t.Fatalf("LoadDaemonConfig: %v", err)
	}
	return *cfg
}

// startDaemon serves handler with cfg and returns the base URL and a client
// that trusts the test certificate.
func startDaemon(t *testing.T, cfg config.DaemonConfig, handler http.Handler, roots *x509.CertPool) (string, *http.Client) {
	t.Helper()
	srv, err := ListenHTTPS(cfg, handler)
	if err != nil {
		t.Fatalf("ListenHTTPS: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v, want http.ErrServerClosed", err)
		}
	})

	host, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsLoopback() {
		t.Fatalf("the listener bound %s; the test needs a non-loopback bind", host)
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}},
	}
	t.Cleanup(client.CloseIdleConnections)
	return "https://127.0.0.1:" + port, client
}

// countingHandler answers 200 and counts the requests that reach it.
func countingHandler(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
}

func get(t *testing.T, client *http.Client, url, authorization string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp
}

// TestHTTPSListenerRequiresBearerSecret: with a cert and key configured and a
// non-loopback bind, the daemon serves HTTPS, refuses every request that does
// not carry the configured secret before the handler runs, and admits the one
// that does.
func TestHTTPSListenerRequiresBearerSecret(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, keyFile, roots := testKeyPair(t)
	secret := testSecret(t)
	writeDaemonConfig(t, xdg, certFile, keyFile, secret)

	var calls atomic.Int32
	base, client := startDaemon(t, loadDaemonConfig(t), countingHandler(&calls), roots)

	refused := map[string]struct{ path, authorization string }{
		"no header":             {"/", ""},
		"empty bearer":          {"/", "Bearer "},
		"wrong bearer":          {"/", "Bearer " + testSecret(t)},
		"one byte changed":      {"/", "Bearer " + secret[:len(secret)-1] + "x"},
		"secret as a prefix":    {"/", "Bearer " + secret[:32]},
		"basic scheme":          {"/", "Basic " + secret},
		"bare secret":           {"/", secret},
		"unknown path, no auth": {"/no/such/route", ""},
	}
	for name, tc := range refused {
		t.Run(name, func(t *testing.T) {
			resp := get(t, client, base+tc.path, tc.authorization)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the handler ran %d times for refused requests, want 0", n)
	}

	resp := get(t, client, base+"/", "Bearer "+secret)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with the secret: status = %d, want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Fatal("the admitted request was not served over TLS")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the handler ran %d times, want 1", n)
	}
}

// TestHTTPSListenerKeepsSecretReadAtStart: the secret is read once at start,
// so rewriting the config leaves the old secret valid and the new one
// refused until a restart.
func TestHTTPSListenerKeepsSecretReadAtStart(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, keyFile, roots := testKeyPair(t)
	oldSecret, newSecret := testSecret(t), testSecret(t)
	writeDaemonConfig(t, xdg, certFile, keyFile, oldSecret)

	var calls atomic.Int32
	base, client := startDaemon(t, loadDaemonConfig(t), countingHandler(&calls), roots)

	writeDaemonConfig(t, xdg, certFile, keyFile, newSecret)

	if resp := get(t, client, base+"/", "Bearer "+oldSecret); resp.StatusCode != http.StatusOK {
		t.Errorf("old secret after the config changed: status = %d, want 200", resp.StatusCode)
	}
	if resp := get(t, client, base+"/", "Bearer "+newSecret); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("new secret before a restart: status = %d, want 401", resp.StatusCode)
	}
}

// TestListenHTTPSRefusesMismatchedKeyPair: a key that does not belong to the
// certificate refuses start rather than serving without TLS.
func TestListenHTTPSRefusesMismatchedKeyPair(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, _, _ := testKeyPair(t)
	_, otherKey, _ := testKeyPair(t)
	writeDaemonConfig(t, xdg, certFile, otherKey, testSecret(t))

	if srv, err := ListenHTTPS(loadDaemonConfig(t), countingHandler(new(atomic.Int32))); err == nil {
		srv.Shutdown(context.Background())
		t.Fatal("ListenHTTPS started with a key that does not match the certificate")
	}
}

// TestHTTPSListenerEmptySecretAdmitsNothing: a daemon config with an empty
// bearer secret either refuses start or serves a daemon that still refuses
// the empty bearer. Either way no request authenticates with nothing.
func TestHTTPSListenerEmptySecretAdmitsNothing(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, keyFile, roots := testKeyPair(t)
	writeDaemonConfig(t, xdg, certFile, keyFile, "")

	cfg, err := config.LoadDaemonConfig()
	if err != nil {
		if !errors.Is(err, config.ErrDaemonConfigParse) {
			t.Fatalf("LoadDaemonConfig err = %v, want ErrDaemonConfigParse", err)
		}
		return
	}
	var calls atomic.Int32
	base, client := startDaemon(t, *cfg, countingHandler(&calls), roots)
	if resp := get(t, client, base+"/", "Bearer "); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty bearer against an empty secret: status = %d, want 401", resp.StatusCode)
	}
}
