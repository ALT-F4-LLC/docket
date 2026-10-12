package exec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrPinMismatch is returned when the file argv[0] resolves to does not hold
// the bytes its content pin names. It always arrives wrapped with ErrRefused:
// nothing ran, so the caller records unmatched.
var ErrPinMismatch = errors.New("content pin mismatch")

// RunPinned runs spec only if the executable it spawns holds exactly the bytes
// pin (a hex SHA-256) names, and spawns exactly the file it hashed.
//
// spec.Argv[0] is the UNRESOLVED argv[0]. RunPinned resolves it once, through
// Resolve (R1–R4 with repoRoot and entryArgv0), hashes the resolved path, and
// hands that same path string to Run as argv[0]. Nothing re-derives the path
// from spec.Argv[0] after the hash, so a symlink on the way to the executable
// that is retargeted mid-call can only change which file is hashed, never make
// the spawn land on a file other than the hashed one.
//
// The executed path is still walked again by execve, and a script's
// interpreter opens it again by name. A rename over that path, a write into the
// hashed file, or a directory component swapped for a symlink between the hash
// and those walks are outside what a path-based spawn can bind.
func RunPinned(spec Spec, pin, repoRoot, entryArgv0 string) (Result, error) {
	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("%w: the argv is empty", ErrRefused)
	}
	resolved, err := Resolve(spec.Argv[0], repoRoot, entryArgv0)
	if err != nil {
		return Result{}, err
	}
	sum, err := FileSHA256(resolved)
	if err != nil {
		return Result{}, fmt.Errorf("%w: cannot verify the content pin of %s: %v", ErrRefused, Render(resolved), err)
	}
	if sum != pin {
		return Result{}, fmt.Errorf("%w: %w: %s holds sha256 %s, not the pinned %s",
			ErrRefused, ErrPinMismatch, Render(resolved), sum, pin)
	}
	pinned := spec
	pinned.Argv = append([]string{resolved}, spec.Argv[1:]...)
	return Run(pinned)
}

// FileSHA256 returns the hex-encoded SHA-256 of the regular file at path. It is
// the one content hasher: trust add pins with it, Lookup checks with it, and
// RunPinned verifies with it, so the three cannot disagree about which bytes a
// pin names.
//
// path must already be symlink-resolved; a symlink in its final component is
// refused rather than followed. The open is non-blocking and the type check is
// an fstat of the open descriptor, so a FIFO or device swapped in at that path
// is refused rather than waited on, and nothing can be swapped between the
// check and the read.
func FileSHA256(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", Render(path), err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", Render(path), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is %s, not a regular file", Render(path), DescribeMode(info.Mode()))
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s: %w", Render(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DescribeMode names what a non-regular file is, so a refusal tells the
// operator what it found rather than only that it refused.
func DescribeMode(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&os.ModeNamedPipe != 0:
		return "a FIFO"
	case m&os.ModeSocket != 0:
		return "a socket"
	case m&os.ModeDevice != 0:
		return "a device"
	default:
		return "not a regular file"
	}
}
