package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

func writeTempFile(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", size)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestLoadDocBodyPathWithinCap(t *testing.T) {
	path := writeTempFile(t, maxDocBodySize)

	body, err := loadDocBody("@" + path)
	if err != nil {
		t.Fatalf("loadDocBody: unexpected error: %v", err)
	}
	if len(body) != maxDocBodySize {
		t.Fatalf("loadDocBody: got %d bytes, want %d", len(body), maxDocBodySize)
	}
}

func TestLoadDocBodyPathOverCap(t *testing.T) {
	path := writeTempFile(t, maxDocBodySize+1)

	_, err := loadDocBody("@" + path)
	if err == nil {
		t.Fatal("loadDocBody: expected error for over-cap file, got nil")
	}

	var cmdError *CmdError
	if !errors.As(err, &cmdError) {
		t.Fatalf("loadDocBody: error is not *CmdError: %v", err)
	}
	if cmdError.Code != output.ErrValidation {
		t.Fatalf("loadDocBody: got code %v, want %v", cmdError.Code, output.ErrValidation)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("loadDocBody: error %q does not mention exceeding the cap", err.Error())
	}
}

func TestLoadDocBodyPathFarOverCap(t *testing.T) {
	path := writeTempFile(t, maxDocBodySize*4)

	_, err := loadDocBody("@" + path)
	if err == nil {
		t.Fatal("loadDocBody: expected error for far-over-cap file, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("loadDocBody: error %q does not mention exceeding the cap", err.Error())
	}
}

func TestLoadDocBodyEmptyPath(t *testing.T) {
	_, err := loadDocBody("@")
	if err == nil {
		t.Fatal("loadDocBody: expected error for empty path, got nil")
	}
	var cmdError *CmdError
	if !errors.As(err, &cmdError) {
		t.Fatalf("loadDocBody: error is not *CmdError: %v", err)
	}
	if cmdError.Code != output.ErrValidation {
		t.Fatalf("loadDocBody: got code %v, want %v", cmdError.Code, output.ErrValidation)
	}
}
