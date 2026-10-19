package config

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

const (
	// BearerTokenEnvVar carries the client's bearer token for a remote daemon.
	// It is distinct from DOCKET_TOKEN, the lease capability, and the resolver
	// never reads that variable.
	BearerTokenEnvVar = "DOCKET_BEARER_TOKEN"
	// BearerTokenFileEnvVar names a file holding the bearer token, read only
	// when BearerTokenEnvVar is unset.
	BearerTokenFileEnvVar = "DOCKET_BEARER_TOKEN_FILE"

	maxBearerTokenBytes = 4096
)

// BearerToken returns the client's bearer token: DOCKET_BEARER_TOKEN when it
// is set, otherwise the contents of the file DOCKET_BEARER_TOKEN_FILE names.
// A whitespace-only variable counts as unset. Neither set yields "" and no
// error. A configured source that cannot supply a valid token is an error,
// never a silent fall back to no token.
//
// Errors name the variable and path but never the token content: a refused
// value may still be a live credential.
func BearerToken() (string, error) {
	if tok := strings.TrimSpace(os.Getenv(BearerTokenEnvVar)); tok != "" {
		if err := checkBearerToken(tok); err != nil {
			return "", fmt.Errorf("%s: %w", BearerTokenEnvVar, err)
		}
		return tok, nil
	}
	path := os.Getenv(BearerTokenFileEnvVar)
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	tok, err := readBearerTokenFile(path)
	if err != nil {
		return "", fmt.Errorf("%s=%s: %w", BearerTokenFileEnvVar, path, err)
	}
	return tok, nil
}

// readBearerTokenFile checks the OPENED descriptor, so the file inspected is
// the file read. O_NONBLOCK keeps the open of a FIFO from waiting for a writer
// before the regular-file check can refuse it. Symlinks are followed and
// root-owned files accepted, so read-only secret mounts work; what is refused
// is a source another user could have written.
func readBearerTokenFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file (mode %s)", info.Mode())
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return "", fmt.Errorf("mode %04o is group- or world-writable; fix it with: chmod go-w %s", perm, path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() && st.Uid != 0 {
		return "", fmt.Errorf("owned by uid %d, but docket is running as uid %d; the file must be owned by the calling user or root", st.Uid, os.Getuid())
	}

	data, err := io.ReadAll(io.LimitReader(f, maxBearerTokenBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBearerTokenBytes {
		return "", fmt.Errorf("larger than %d bytes", maxBearerTokenBytes)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("the file is empty")
	}
	if err := checkBearerToken(tok); err != nil {
		return "", err
	}
	return tok, nil
}

// checkBearerToken refuses a value that cannot travel as a single HTTP header
// token: anything outside printable, non-space ASCII, which rules out CR/LF
// injection and multi-line secrets such as PEM keys. Its error never quotes
// the value.
func checkBearerToken(tok string) error {
	if len(tok) > maxBearerTokenBytes {
		return fmt.Errorf("the token is longer than %d bytes", maxBearerTokenBytes)
	}
	for i := 0; i < len(tok); i++ {
		if c := tok[i]; c < 0x21 || c > 0x7e {
			return fmt.Errorf("the token contains a byte outside printable ASCII (whitespace, a control character, or non-ASCII)")
		}
	}
	return nil
}
