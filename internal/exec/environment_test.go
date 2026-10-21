package exec

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// DKT-4251: only a repo root that does not resolve is an environment refusal.
func TestOnlyAnUnresolvableRepoRootIsAnEnvironmentRefusal(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "removed")
	_, err := Resolve("git", gone, "git")
	if !errors.Is(err, ErrRefused) || !errors.Is(err, ErrEnvironment) {
		t.Fatalf("Resolve with a missing root = %v; want both ErrRefused and ErrEnvironment", err)
	}

	// A binary inside the repository is a trust-shaped refusal.
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(bin, root, "tool")
	if !errors.Is(err, ErrRefused) || errors.Is(err, ErrEnvironment) {
		t.Fatalf("Resolve of a repo-resident binary = %v; want ErrRefused only", err)
	}

	// A pin mismatch is an ErrRefused too, and must never read as environment.
	pinErr := errors.Join(ErrRefused, ErrPinMismatch)
	if errors.Is(pinErr, ErrEnvironment) {
		t.Fatal("a pin mismatch reads as an environment refusal")
	}
}
