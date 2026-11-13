package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// mapResolver answers lookups from a fixed table and fails any host it does
// not hold, so a test cannot pass by reaching real DNS.
type mapResolver map[string][]netip.Addr

func (m mapResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := m[host]
	if !ok {
		return nil, fmt.Errorf("mapResolver: no entry for %q", host)
	}
	return addrs, nil
}

func addrs(ips ...string) []netip.Addr {
	out := make([]netip.Addr, len(ips))
	for i, ip := range ips {
		out[i] = netip.MustParseAddr(ip)
	}
	return out
}

var testDaemonResolver = mapResolver{
	"remote.test": addrs("203.0.113.5"),
	"local.test":  addrs("127.0.0.1"),
	"local6.test": addrs("::1"),
	"localhost":   addrs("192.0.2.7"),
	"mixed.test":  addrs("127.0.0.1", "203.0.113.5"),
	"empty.test":  {},
}

// TestCheckDaemonURLRefusesNonLoopbackPlaintext: plaintext is judged by the
// addresses the host resolves to, never by the host string, so a name that
// looks local but resolves elsewhere is refused along with a remote one.
func TestCheckDaemonURLRefusesNonLoopbackPlaintext(t *testing.T) {
	for _, raw := range []string{
		"http://remote.test:8080",
		"http://localhost:8080",
		"http://mixed.test:8080",
		"http://empty.test:8080",
		"http://unresolvable.test:8080",
		"http://0.0.0.0:8080",
		"http://203.0.113.5:8080",
		"http://",
		"ws://local.test:8080",
	} {
		if err := checkDaemonURL(context.Background(), raw, testDaemonResolver); err == nil {
			t.Errorf("checkDaemonURL(%q) = nil, want a refusal", raw)
		}
	}
}

// TestCheckDaemonURLRefusalRedactsPassword: a refusal names the URL, but never
// the password a URL's userinfo carries.
func TestCheckDaemonURLRefusalRedactsPassword(t *testing.T) {
	err := checkDaemonURL(context.Background(), "http://user:hunter2@remote.test:8080", testDaemonResolver)
	if err == nil {
		t.Fatal("checkDaemonURL = nil, want a refusal")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("refusal %q echoes the URL's password", err)
	}
}

// TestCheckDaemonURLAcceptsLoopbackPlaintextAndHTTPS: plaintext is fine when
// every resolved address is loopback, and https is fine wherever it points.
func TestCheckDaemonURLAcceptsLoopbackPlaintextAndHTTPS(t *testing.T) {
	for _, raw := range []string{
		"http://local.test:8080",
		"HTTP://local6.test:8080",
		"http://127.0.0.1:8080",
		"http://[::ffff:127.0.0.1]:8080",
		"https://remote.test:8443",
	} {
		if err := checkDaemonURL(context.Background(), raw, testDaemonResolver); err != nil {
			t.Errorf("checkDaemonURL(%q) = %v, want nil", raw, err)
		}
	}
}

var discardLogger = slog.New(slog.DiscardHandler)

// routedDial dials the address routes names for a host in place of resolving
// it, so a hostname reaches an httptest server without DNS. A host with no
// route fails, so a test cannot reach the network.
func routedDial(routes map[string]string) daemonDialFunc {
	var dialer net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		target, ok := routes[host]
		if !ok {
			return nil, fmt.Errorf("routedDial: no route for %q", addr)
		}
		return dialer.DialContext(ctx, network, target)
	}
}

// countingServer answers every request with an empty Connect unary JSON
// message and counts the requests it receives.
type countingServer struct {
	*httptest.Server
	hits atomic.Int32
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	s := &countingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(s.Close)
	return s
}

func handshake(client docketv1connect.DocketServiceClient) error {
	_, err := client.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{}))
	return err
}

// TestNewDaemonClientChecksURL: construction refuses a plaintext URL to a host
// that resolves off this host, with checkDaemonURL's error, and accepts https
// to the same host.
func TestNewDaemonClientChecksURL(t *testing.T) {
	ctx := context.Background()
	dial := routedDial(nil)

	const plaintext = "http://remote.test:8080"
	want := checkDaemonURL(ctx, plaintext, testDaemonResolver)
	if want == nil {
		t.Fatalf("checkDaemonURL(%q) = nil, want a refusal to compare against", plaintext)
	}
	client, err := newDaemonClient(ctx, plaintext, testDaemonResolver, dial, discardLogger)
	if err == nil || client != nil {
		t.Fatalf("newDaemonClient(%q) returned a client: %t, error: %v; want no client and the refusal", plaintext, client != nil, err)
	}
	if err.Error() != want.Error() {
		t.Errorf("newDaemonClient(%q) error = %q, want checkDaemonURL's %q", plaintext, err, want)
	}

	const secure = "https://remote.test:8443"
	if client, err := newDaemonClient(ctx, secure, testDaemonResolver, dial, discardLogger); err != nil || client == nil {
		t.Errorf("newDaemonClient(%q) returned a client: %t, error: %v; want a client", secure, client != nil, err)
	}
}

// TestDaemonClientRefusesRedirectToRefusedURL: a daemon response redirecting
// to a plaintext off-host URL fails the request with checkDaemonURL's refusal,
// and the redirect target is never dialed.
func TestDaemonClientRefusesRedirectToRefusedURL(t *testing.T) {
	recorder := newCountingServer(t)
	_, recorderPort, err := net.SplitHostPort(recorder.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	target := "http://remote.test:" + recorderPort + "/redirected"
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	dial := routedDial(map[string]string{
		"127.0.0.1":   redirector.Listener.Addr().String(),
		"remote.test": recorder.Listener.Addr().String(),
	})
	client, err := newDaemonClient(context.Background(), redirector.URL, testDaemonResolver, dial, discardLogger)
	if err != nil {
		t.Fatalf("newDaemonClient(%q) = %v, want a client", redirector.URL, err)
	}

	want := checkDaemonURL(context.Background(), target, testDaemonResolver)
	if want == nil {
		t.Fatalf("checkDaemonURL(%q) = nil, want a refusal to compare against", target)
	}
	err = handshake(client)
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Errorf("Handshake error = %v, want checkDaemonURL's refusal %q", err, want)
	}
	if got := recorder.hits.Load(); got != 0 {
		t.Errorf("redirect target received %d requests, want 0", got)
	}
}

// TestDaemonClientIgnoresEnvironmentProxy: a plaintext request to a loopback
// daemon goes to the daemon directly even when the environment names a proxy.
// The URL uses a hostname, not a loopback literal, because net/http never
// proxies a loopback literal and the test would pass without the control.
func TestDaemonClientIgnoresEnvironmentProxy(t *testing.T) {
	daemon := newCountingServer(t)
	proxy := newCountingServer(t)
	_, daemonPort, err := net.SplitHostPort(daemon.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, proxyPort, err := net.SplitHostPort(proxy.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://proxy.test:" + proxyPort
	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	dial := routedDial(map[string]string{
		"local.test": daemon.Listener.Addr().String(),
		"proxy.test": proxy.Listener.Addr().String(),
	})
	daemonURL := "http://local.test:" + daemonPort
	client, err := newDaemonClient(context.Background(), daemonURL, testDaemonResolver, dial, discardLogger)
	if err != nil {
		t.Fatalf("newDaemonClient(%q) = %v, want a client", daemonURL, err)
	}

	if err := handshake(client); err != nil {
		t.Errorf("Handshake = %v, want success", err)
	}
	if got := daemon.hits.Load(); got != 1 {
		t.Errorf("daemon received %d requests, want 1", got)
	}
	if got := proxy.hits.Load(); got != 0 {
		t.Errorf("proxy received %d requests, want 0", got)
	}
}

// setClientBuild stands in for the ldflags-stamped client build, restoring it
// when the test ends.
func setClientBuild(t *testing.T, client string) {
	t.Helper()
	previous := version
	version = client
	t.Cleanup(func() { version = previous })
}

// handshakeDaemon answers Handshake with its build, as the daemon does, and
// leaves every other procedure unimplemented.
type handshakeDaemon struct {
	docketv1connect.UnimplementedDocketServiceHandler
	build string
}

func (d handshakeDaemon) Handshake(context.Context, *connect.Request[docketv1.HandshakeRequest]) (*connect.Response[docketv1.HandshakeResponse], error) {
	return connect.NewResponse(&docketv1.HandshakeResponse{DaemonVersion: d.build}), nil
}

// runLocalDaemonCommand serves a daemon reporting daemonBuild as the local
// daemon, runs a command that connects to it, and returns the command's
// stderr and error.
func runLocalDaemonCommand(t *testing.T, daemonBuild string) (string, error) {
	t.Helper()
	path, handler := docketv1connect.NewDocketServiceHandler(handshakeDaemon{build: daemonBuild})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	daemon := httptest.NewServer(mux)
	t.Cleanup(daemon.Close)

	cmd := &cobra.Command{
		Use: "probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := connectLocalDaemon(cmd, daemon.Client(), daemon.URL, discardLogger)
			return err
		},
	}
	var stderr bytes.Buffer
	cmd.SetArgs(nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	return stderr.String(), err
}

// TestLocalDaemonBuildMismatchWarnsOnce: a local daemon on another build gets
// one stderr warning naming both builds, and the command still succeeds.
func TestLocalDaemonBuildMismatchWarnsOnce(t *testing.T) {
	const client, daemon = "v1.4.0-client+aa11", "v1.3.9-daemon+bb22"
	setClientBuild(t, client)

	stderr, err := runLocalDaemonCommand(t, daemon)

	if err != nil {
		t.Errorf("command error = %v, want nil (exit 0) on a build mismatch", err)
	}
	var warnings []string
	for line := range strings.Lines(stderr) {
		if strings.Contains(line, client) || strings.Contains(line, daemon) {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("stderr has %d lines naming a build, want exactly 1; stderr:\n%s", len(warnings), stderr)
	}
	if !strings.Contains(warnings[0], client) || !strings.Contains(warnings[0], daemon) {
		t.Errorf("warning %q names only one build, want both %q and %q", warnings[0], client, daemon)
	}
}

// TestLocalDaemonSameBuildIsSilent: a local daemon on the client's own build
// gets no warning.
func TestLocalDaemonSameBuildIsSilent(t *testing.T) {
	const build = "v1.4.0-same+cc33"
	setClientBuild(t, build)

	stderr, err := runLocalDaemonCommand(t, build)

	if err != nil {
		t.Errorf("command error = %v, want nil", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty for equal builds", stderr)
	}
}

// TestVersionReportsBuild: Version returns the build version the binary was
// built with, so a caller outside the cli package can hand it on.
func TestVersionReportsBuild(t *testing.T) {
	const build = "v2.0.1-setup+dd44"
	setClientBuild(t, build)

	if got := Version(); got != build {
		t.Errorf("Version() = %q, want the build version %q", got, build)
	}
}

// TestPersistentPostRunClosesHandles: the root hook's teardown closes both the
// command's database handle and the id-owner reader handle it opened.
func TestPersistentPostRunClosesHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docket.db")
	conn, err := store.OpenSQLiteDB(path)
	if err != nil {
		t.Fatalf("OpenSQLiteDB: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.Ping(); err != nil {
		t.Fatalf("root handle Ping before teardown: %v", err)
	}
	readerDB, err := store.OpenSQLiteReaderDB(path)
	if err != nil {
		t.Fatalf("OpenSQLiteReaderDB: %v", err)
	}
	t.Cleanup(func() { readerDB.Close() })

	ctx := context.WithValue(context.Background(), dbKey, conn)
	ctx = context.WithValue(ctx, readerDBKey, readerDB)
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)

	if err := rootCmd.PersistentPostRunE(cmd, nil); err != nil {
		t.Fatalf("PersistentPostRunE: %v", err)
	}

	const closed = "sql: database is closed"
	for name, handle := range map[string]store.LegacyDB{"root": conn, "reader": readerDB} {
		if err := handle.Ping(); err == nil || err.Error() != closed {
			t.Errorf("%s handle Ping after teardown = %v, want %q", name, err, closed)
		}
	}
}

// TestRootSkipsProjectResolutionForVerbOwnedProject: a command annotated
// verbOwnsProject resolves its own project, so the root hook opens the store
// but does not resolve one. From a directory that is not a repository, against
// the global store, the same command unannotated is refused by eager
// resolution; annotated, it runs.
func TestRootSkipsProjectResolutionForVerbOwnedProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("DOCKET_PATH", "")
	t.Chdir(t.TempDir())
	if err := os.Mkdir(filepath.Join(home, ".docket"), 0o700); err != nil {
		t.Fatalf("creating the global store directory: %v", err)
	}
	conn, err := store.OpenSQLiteDB(filepath.Join(home, ".docket", "issues.db"))
	if err != nil {
		t.Fatalf("OpenSQLiteDB: %v", err)
	}
	if err := store.StoreOn(conn).Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	conn.Close()

	run := func(t *testing.T, annotations map[string]string) (ran, hasProject bool, err error) {
		t.Helper()
		probe := &cobra.Command{
			Use:         "verb-owned-project-probe",
			Annotations: annotations,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ran = true
				getDB(cmd)
				_, hasProject = cmd.Context().Value(projectKey).(int)
				return nil
			},
		}
		rootCmd.AddCommand(probe)
		defer rootCmd.RemoveCommand(probe)
		rootCmd.SetArgs([]string{"verb-owned-project-probe"})
		defer rootCmd.SetArgs(nil)
		err = rootCmd.Execute()
		return ran, hasProject, err
	}

	ran, _, err := run(t, nil)
	if err == nil || !strings.Contains(err.Error(), "not a git worktree") {
		t.Fatalf("unannotated probe: err = %v, want the eager-resolution refusal", err)
	}
	if ran {
		t.Fatal("unannotated probe ran despite the eager-resolution refusal")
	}

	ran, hasProject, err := run(t, map[string]string{"verbOwnsProject": ""})
	if err != nil {
		t.Fatalf("annotated probe: %v, want no eager-resolution error", err)
	}
	if !ran {
		t.Fatal("annotated probe did not run")
	}
	if hasProject {
		t.Error("annotated probe carries an eagerly resolved project")
	}
}

// TestGetProjectIDPanicsForVerbOwnedProject: a command annotated
// verbOwnsProject has no eagerly resolved project, so reaching getProjectID
// from one panics with a typed error instead of falling back to the default
// project. An unannotated command still reads the project the root hook
// resolved.
func TestGetProjectIDPanicsForVerbOwnedProject(t *testing.T) {
	const configured = 7
	plain := &cobra.Command{Use: "plain-probe"}
	plain.SetContext(context.WithValue(context.Background(), projectKey, configured))
	if got := getProjectID(plain); got != configured {
		t.Errorf("getProjectID(unannotated) = %d, want the configured project %d", got, configured)
	}

	annotated := &cobra.Command{
		Use:         "verb-owned-probe",
		Annotations: map[string]string{"verbOwnsProject": ""},
	}
	annotated.SetContext(context.Background())
	got, recovered := func() (id int, recovered any) {
		defer func() { recovered = recover() }()
		return getProjectID(annotated), nil
	}()
	err, _ := recovered.(error)
	var typed *verbOwnedProjectError
	if !errors.As(err, &typed) {
		t.Fatalf("getProjectID(annotated) = %d, panic %v; want a panic with *verbOwnedProjectError", got, recovered)
	}
	if !strings.Contains(typed.Error(), "verb-owned-probe") {
		t.Errorf("panic error %q does not name the command", typed)
	}
}
