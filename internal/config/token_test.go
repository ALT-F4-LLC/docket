package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bearerEnv sets both bearer variables for one test; an empty value is the
// unset state the resolver recognizes.
func bearerEnv(t *testing.T, token, file string) {
	t.Helper()
	t.Setenv(BearerTokenEnvVar, token)
	t.Setenv(BearerTokenFileEnvVar, file)
}

// tokenFile writes content to a fresh file with exactly the given mode.
func tokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBearerTokenReadsFileWhenEnvUnset(t *testing.T) {
	bearerEnv(t, "", tokenFile(t, "file-token\n", 0o600))

	got, err := BearerToken()
	if err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if got != "file-token" {
		t.Fatalf("BearerToken = %q, want the file contents %q", got, "file-token")
	}
}

func TestBearerTokenEnvWinsOverFile(t *testing.T) {
	bearerEnv(t, "env-token", tokenFile(t, "file-token", 0o600))

	got, err := BearerToken()
	if err != nil {
		t.Fatalf("BearerToken: %v", err)
	}
	if got != "env-token" {
		t.Fatalf("BearerToken = %q, want the environment value %q", got, "env-token")
	}
}

func TestBearerTokenEnvDoesNotOpenFile(t *testing.T) {
	bearerEnv(t, "env-token", filepath.Join(t.TempDir(), "missing"))

	got, err := BearerToken()
	if err != nil || got != "env-token" {
		t.Fatalf("BearerToken = %q, %v; want %q with no error, since the file is never opened", got, err, "env-token")
	}
}

func TestBearerTokenUnconfiguredIgnoresLeaseToken(t *testing.T) {
	bearerEnv(t, "  ", "")
	t.Setenv("DOCKET_TOKEN", "lease-x")

	got, err := BearerToken()
	if err != nil || got != "" {
		t.Fatalf("BearerToken = %q, %v; want no token and no error", got, err)
	}
}

func TestBearerTokenAcceptsReadableSources(t *testing.T) {
	private := tokenFile(t, "tok-0600", 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ path, want string }{
		"owner-only file":           {private, "tok-0600"},
		"world-readable file":       {tokenFile(t, "tok-0644", 0o644), "tok-0644"},
		"symlink to a private file": {link, "tok-0600"},
		"b64token punctuation":      {tokenFile(t, "abc.DEF-123_~+/==\n", 0o600), "abc.DEF-123_~+/=="},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			bearerEnv(t, "", tc.path)
			got, err := BearerToken()
			if err != nil || got != tc.want {
				t.Fatalf("BearerToken = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// The marker never appears in an error: a refused token may still be a live
// credential, and errors reach logs and CI transcripts.
const leakMarker = "SENTINEL-abc"

func TestBearerTokenFileRefusals(t *testing.T) {
	writable := tokenFile(t, leakMarker, 0o666)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(writable, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing file":             filepath.Join(t.TempDir(), "missing"),
		"empty file":               tokenFile(t, "", 0o600),
		"whitespace-only file":     tokenFile(t, " \n\t", 0o600),
		"world-writable file":      writable,
		"group-writable file":      tokenFile(t, leakMarker, 0o620),
		"symlink to writable file": link,
		"directory":                t.TempDir(),
		"oversized file":           tokenFile(t, strings.Repeat("a", maxBearerTokenBytes+1), 0o600),
		"two-line PEM body":        tokenFile(t, "-----BEGIN KEY-----\n"+leakMarker+"\n-----END KEY-----\n", 0o600),
		"embedded space":           tokenFile(t, leakMarker+" more", 0o600),
		"embedded NUL":             tokenFile(t, leakMarker+"\x00", 0o600),
		"non-ASCII byte":           tokenFile(t, leakMarker+"\xc3\xa9", 0o600),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			bearerEnv(t, "", path)
			got, err := BearerToken()
			if err == nil {
				t.Fatalf("BearerToken = %q, nil; want an error", got)
			}
			if got != "" {
				t.Fatalf("BearerToken returned %q alongside its error", got)
			}
			msg := err.Error()
			if !strings.Contains(msg, BearerTokenFileEnvVar) || !strings.Contains(msg, path) {
				t.Fatalf("error %q does not name %s and the path %s", msg, BearerTokenFileEnvVar, path)
			}
			if strings.Contains(msg, leakMarker) {
				t.Fatalf("error %q echoes the token content", msg)
			}
		})
	}
}

func TestBearerTokenEnvRefusesInvalidValue(t *testing.T) {
	bearerEnv(t, leakMarker+"\r\nX-Injected: 1", "")

	_, err := BearerToken()
	if err == nil {
		t.Fatal("BearerToken accepted a value with an embedded CR/LF")
	}
	if !strings.Contains(err.Error(), BearerTokenEnvVar) || strings.Contains(err.Error(), leakMarker) {
		t.Fatalf("error %q must name %s and must not echo the value", err, BearerTokenEnvVar)
	}
}

func TestBearerTokenRefusesFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	bearerEnv(t, "", fifo)

	done := make(chan error, 1)
	go func() {
		_, err := BearerToken()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("BearerToken accepted a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BearerToken blocked opening a FIFO")
	}
}
