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
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/model"
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
	writeDaemonConfigWindow(t, xdg, certFile, keyFile, secret, 1, 1)
}

// writeDaemonConfigWindow is writeDaemonConfig declaring the client API major
// window [minMajor, maxMajor].
func writeDaemonConfigWindow(t *testing.T, xdg, certFile, keyFile, secret string, minMajor, maxMajor uint32) {
	t.Helper()
	dir := filepath.Join(xdg, "docket")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("version = 1\nlisten = \"0.0.0.0:0\"\ntls_cert_file = %q\ntls_key_file = %q\nbearer_secret = %q\n"+
		"min_client_api_major = %d\nmax_client_api_major = %d\n",
		certFile, keyFile, secret, minMajor, maxMajor)
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

// fakeResolver answers lookups from a fixed table and errors on any other
// host, standing in for DNS and the hosts file.
type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs, nil
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

// startLoopbackDaemon serves handler on a plaintext listener for listen and
// returns its base URL.
func startLoopbackDaemon(t *testing.T, listen string, resolver Resolver, handler http.Handler) (*Server, string) {
	t.Helper()
	srv, err := ListenLoopbackHTTP(context.Background(), listen, resolver, handler)
	if err != nil {
		t.Fatalf("ListenLoopbackHTTP(%q): %v", listen, err)
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
	return srv, "http://" + srv.Addr().String()
}

// TestLoopbackHTTPRefusesNonLoopbackResolution: a plaintext listener starts
// only when every address its host resolves to is loopback. The decision is
// made on the resolved addresses, so a name that reads as local but resolves
// off-host is refused.
func TestLoopbackHTTPRefusesNonLoopbackResolution(t *testing.T) {
	resolver := fakeResolver{
		"localhost":  addrs("10.0.0.5"),
		"daemon.lan": addrs("192.168.1.20"),
		"mixed.test": addrs("127.0.0.1", "10.0.0.5"),
		"empty.test": nil,
		"0.0.0.0":    addrs("0.0.0.0"),
		"::":         addrs("::"),
	}
	for _, listen := range []string{
		"localhost:0",
		"daemon.lan:0",
		"mixed.test:0",
		"empty.test:0",
		"unresolvable.test:0",
		"0.0.0.0:0",
		"[::]:0",
		":0",
	} {
		t.Run(listen, func(t *testing.T) {
			srv, err := ListenLoopbackHTTP(context.Background(), listen, resolver, countingHandler(new(atomic.Int32)))
			if err == nil {
				srv.Shutdown(context.Background())
				t.Fatalf("started a plaintext listener on %s, bound %s", listen, srv.Addr())
			}
		})
	}
}

// TestLoopbackHTTPStartsOnLoopbackResolution: a host that resolves only to
// loopback starts, bound to the checked address rather than the name.
func TestLoopbackHTTPStartsOnLoopbackResolution(t *testing.T) {
	resolver := fakeResolver{
		"local.test":  addrs("127.0.0.1"),
		"mapped.test": addrs("::ffff:127.0.0.1"),
	}
	for _, listen := range []string{"local.test:0", "mapped.test:0"} {
		t.Run(listen, func(t *testing.T) {
			srv, _ := startLoopbackDaemon(t, listen, resolver, countingHandler(new(atomic.Int32)))
			host, _, err := net.SplitHostPort(srv.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if host != "127.0.0.1" {
				t.Fatalf("bound %s, want 127.0.0.1", host)
			}
		})
	}
}

// TestLoopbackHTTPExemptsBearerButNotConductor: on a loopback listener a
// request needs no bearer, and a valid bearer confers no conductor authority;
// only the conductor credential admits a conductor-gated operation.
func TestLoopbackHTTPExemptsBearerButNotConductor(t *testing.T) {
	bearer := testSecret(t)
	conductorToken := testSecret(t)
	var open, gated atomic.Int32
	mux := http.NewServeMux()
	mux.Handle("/", countingHandler(&open))
	mux.Handle("/gated", requireConductor(model.HashToken(conductorToken), countingHandler(&gated)))
	_, base := startLoopbackDaemon(t, "local.test:0", fakeResolver{"local.test": addrs("127.0.0.1")}, mux)
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	if resp := get(t, client, base+"/", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("no bearer on loopback: status = %d, want 200", resp.StatusCode)
	}
	if n := open.Load(); n != 1 {
		t.Fatalf("the open handler ran %d times, want 1", n)
	}

	denied := map[string]http.Header{
		"valid bearer only":           {"Authorization": {"Bearer " + bearer}},
		"conductor token as a bearer": {"Authorization": {"Bearer " + conductorToken}},
		"wrong conductor token":       {"Authorization": {"Bearer " + bearer}, ConductorTokenHeader: {testSecret(t)}},
		"no credential":               {},
	}
	for name, header := range denied {
		t.Run(name, func(t *testing.T) {
			resp := do(t, client, base+"/gated", header)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
	if n := gated.Load(); n != 0 {
		t.Fatalf("the gated operation ran %d times without the conductor credential, want 0", n)
	}

	resp := do(t, client, base+"/gated", http.Header{
		"Authorization":      {"Bearer " + bearer},
		ConductorTokenHeader: {conductorToken},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with the conductor credential: status = %d, want 200", resp.StatusCode)
	}
	if n := gated.Load(); n != 1 {
		t.Fatalf("the gated operation ran %d times, want 1", n)
	}
}

func do(t *testing.T, client *http.Client, url string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp
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
