package exec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// pinnedFixture lays out the two executables a pin distinguishes and a
// symlinked argv[0] pointing at the pinned one. Each script appends its own
// name to a marker file, so a test can see every process that ran, including
// one whose spawn returned an error to the caller.
//
// These are shell scripts rather than the compiled witness: the property under
// test is which FILE's bytes executed, not argv delivery, and two scripts with
// different text are the plainest pair of files with different bytes.
type pinnedFixture struct {
	pinned, unpinned, link, marker string
	// repoRoot is deliberately NOT the scripts' directory, so a mid-Resolve
	// flip cannot fall into R3's containment refusal and stand in for the pin
	// refusal under test.
	repoRoot string
	pin      string
}

func newPinnedFixture(t *testing.T) pinnedFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving the fixture dir: %v", err)
	f := pinnedFixture{
		pinned:   filepath.Join(dir, "pinned.sh"),
		unpinned: filepath.Join(dir, "unpinned.sh"),
		link:     filepath.Join(dir, "argv0"),
		marker:   filepath.Join(dir, "ran.log"),
		repoRoot: t.TempDir(),
	}
	script := func(name string) []byte {
		return []byte("#!/bin/sh\necho " + name + " >> '" + f.marker + "'\necho " + name + "\n")
	}
	pinnedBytes := script("pinned")
	testsupport.Must(t, os.WriteFile(f.pinned, pinnedBytes, 0o755), "writing pinned.sh")
	testsupport.Must(t, os.WriteFile(f.unpinned, script("unpinned"), 0o755), "writing unpinned.sh")
	testsupport.Must(t, os.Symlink(f.pinned, f.link), "linking argv0")
	sum := sha256.Sum256(pinnedBytes)
	f.pin = hex.EncodeToString(sum[:])
	return f
}

// markerLines returns every name a fixture script recorded, in run order.
func (f pinnedFixture) markerLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	testsupport.Must(t, err, "reading the marker: %v", err)
	return strings.Fields(string(b))
}

func (f pinnedFixture) spec() Spec {
	return Spec{Argv: []string{f.link}, Env: []string{}}
}

// TestPinnedSpawnSurvivesSymlinkFlip is the property the pin exists for: the
// bytes that execute are the bytes that were hashed. A goroutine flips the
// symlinked argv[0] between the pinned and the unpinned script for the whole
// run, so the link's target changes between any two path resolutions. An
// entry point that hashes one resolution and executes another runs the
// unpinned script on some of these spawns; one that acts on a single
// resolution either runs the pinned script or refuses.
func TestPinnedSpawnSurvivesSymlinkFlip(t *testing.T) {
	f := newPinnedFixture(t)

	stop := make(chan struct{})
	flipped := make(chan struct{})
	go func() {
		defer close(flipped)
		tmp := f.link + ".next"
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			target := f.pinned
			if i%2 == 1 {
				target = f.unpinned
			}
			// Symlink then rename: the link is replaced atomically, so it
			// always names one of the two scripts and never dangles.
			_ = os.Remove(tmp)
			if err := os.Symlink(target, tmp); err != nil {
				t.Errorf("staging the flip: %v", err)
				return
			}
			if err := os.Rename(tmp, f.link); err != nil {
				t.Errorf("flipping the link: %v", err)
				return
			}
		}
	}()

	const spawns = 300
	var ran, refused int
	for i := 0; i < spawns; i++ {
		res, err := RunPinned(f.spec(), f.pin, f.repoRoot, f.link)
		switch {
		case err == nil:
			ran++
			if res.Exit != 0 || strings.TrimSpace(res.Output) != "pinned" {
				t.Errorf("spawn %d ran with exit %d and output %q, want the pinned script's exit 0 and %q",
					i, res.Exit, res.Output, "pinned")
			}
			if res.Argv[0] != f.pinned {
				t.Errorf("spawn %d reports argv[0] %q, want the pinned script %q", i, res.Argv[0], f.pinned)
			}
		case errors.Is(err, ErrPinMismatch):
			refused++
		case errors.Is(err, ErrRefused):
			// A resolution that lost a race with the flip (readlink on a link
			// being replaced) refuses too. Nothing ran, which is all the pin
			// promises; the marker check below holds it to that.
		default:
			t.Errorf("spawn %d failed with %v, want a run of the pinned script or a refusal", i, err)
		}
	}
	close(stop)
	<-flipped

	for _, name := range f.markerLines(t) {
		if name != "pinned" {
			t.Fatalf("the unpinned script ran: marker recorded %q (ran=%d refused=%d)", name, ran, refused)
		}
	}
	if got := len(f.markerLines(t)); got != ran {
		t.Errorf("the marker records %d runs, but %d spawns reported success", got, ran)
	}
	// Both outcomes must occur, or the run did not exercise the flip: no pin
	// mismatch means the unpinned script was never the one hashed, and no run
	// means the pinned script never ran at all.
	if ran == 0 || refused == 0 {
		t.Fatalf("ran=%d refused=%d over %d spawns; the flip was not exercised", ran, refused, spawns)
	}
}

// TestPinnedSpawnRefusesMismatchWithoutSpawning pins the refusal's shape: a
// file whose bytes differ from the pin is refused as a pin mismatch that is
// also an ErrRefused (so a runner records unmatched, not fail), with an empty
// Result and no process.
func TestPinnedSpawnRefusesMismatchWithoutSpawning(t *testing.T) {
	f := newPinnedFixture(t)
	testsupport.Must(t, os.Remove(f.link), "unlinking argv0")
	testsupport.Must(t, os.Symlink(f.unpinned, f.link), "pointing argv0 at the unpinned script")

	res, err := RunPinned(f.spec(), f.pin, f.repoRoot, f.link)
	if !errors.Is(err, ErrPinMismatch) || !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want one wrapping both ErrPinMismatch and ErrRefused", err)
	}
	if !strings.Contains(err.Error(), Render(f.unpinned)) {
		t.Errorf("the refusal %q does not name the file it hashed, %s", err, Render(f.unpinned))
	}
	if !reflect.DeepEqual(res, Result{}) {
		t.Errorf("a refused spawn returned a result: %+v", res)
	}
	if lines := f.markerLines(t); len(lines) != 0 {
		t.Errorf("a refused spawn ran a process: marker recorded %q", lines)
	}
}
