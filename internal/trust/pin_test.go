package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// --- Content pins on an absolute argv[0] (§3.1 argv0_sha256) -----------------

// writeScript writes body to dir/name, executable, and returns the file's
// symlink-resolved absolute path: the path a pin reason must name.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	err := os.WriteFile(p, []byte(body), 0o755)
	testsupport.Must(t, err, "writing %s: %v", p, err)
	resolved, err := filepath.EvalSymlinks(p)
	testsupport.Must(t, err, "resolving %s: %v", p, err)
	return resolved
}

// sha256Hex is the test's own oracle for a pin: the hash of the bytes the test
// wrote, computed without the production hasher.
func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// pinFixture is a sandboxed store holding entries whose argv[0] is one script.
type pinFixture struct {
	storePath string
	identity  string
}

func newPinFixture(t *testing.T) pinFixture {
	t.Helper()
	path := sandbox(t)
	repo := t.TempDir()
	identity, err := RepoIdentity(repo)
	testsupport.Must(t, err, "RepoIdentity: %v", err)
	return pinFixture{storePath: path, identity: identity}
}

func (f pinFixture) add(t *testing.T, req AddRequest) *AddResult {
	t.Helper()
	req.RepoRoot = f.identity
	res, err := addAt(f.storePath, req)
	testsupport.Must(t, err, "add %s: %v", req.Name, err)
	return res
}

func (f pinFixture) lookup(t *testing.T, gate string, candidate []string) Match {
	t.Helper()
	st, err := loadAt(f.storePath)
	testsupport.Must(t, err, "loadAt: %v", err)
	return st.Lookup(f.identity, gate, candidate)
}

// TestContentPinMismatch is the enforcement row of §3.1's pin table: one
// changed byte in a pinned script refuses every match branch, and the reason
// names the resolved path and both hashes so the operator can see what moved.
func TestContentPinMismatch(t *testing.T) {
	const original = "#!/bin/sh\nexit 0\n"
	const changed = "#!/bin/sh\nexit 1\n"

	f := newPinFixture(t)
	script := writeScript(t, t.TempDir(), "gate.sh", original)

	f.add(t, AddRequest{Name: "named", Argv: []string{script, "small"}})
	f.add(t, AddRequest{Name: "exact", Argv: []string{script, "a"}})
	f.add(t, AddRequest{Name: "prefixed", Argv: []string{script}, Prefix: true})

	branches := []struct {
		name      string
		gate      string
		candidate []string
	}{
		{"named gate", "named", nil},
		{"fence exact", "exact", []string{script, "a"}},
		{"fence prefix", "prefixed", []string{script, "x"}},
	}

	for _, b := range branches {
		m := f.lookup(t, b.gate, b.candidate)
		if !m.Matched {
			t.Fatalf("%s: an unchanged pinned script must match; reason: %s", b.name, m.Reason)
		}
		if m.Warning != "" {
			t.Errorf("%s: a pinned entry must not carry the legacy warning; got %q", b.name, m.Warning)
		}
	}

	err := os.WriteFile(script, []byte(changed), 0o755)
	testsupport.Must(t, err, "rewriting the script: %v", err)

	for _, b := range branches {
		m := f.lookup(t, b.gate, b.candidate)
		if m.Matched || m.Argv != nil || m.Entry != nil {
			t.Errorf("%s: a changed script must be unmatched with nothing to execute; got %+v", b.name, m)
			continue
		}
		for _, want := range []string{script, sha256Hex(original), sha256Hex(changed), "docket trust add"} {
			if !strings.Contains(m.Reason, want) {
				t.Errorf("%s: the reason must name %q; got %q", b.name, want, m.Reason)
			}
		}
	}
}

// TestContentPinFollowsTheSymlink pins the hashed file to the symlink-resolved
// argv[0]: retargeting a trusted link at different bytes is a change to what
// executes, so it refuses exactly as an in-place edit does.
func TestContentPinFollowsTheSymlink(t *testing.T) {
	f := newPinFixture(t)
	dir := t.TempDir()
	first := writeScript(t, dir, "first.sh", "#!/bin/sh\nexit 0\n")
	second := writeScript(t, dir, "second.sh", "#!/bin/sh\nexit 1\n")
	link := filepath.Join(dir, "gate")
	testsupport.Must(t, os.Symlink(first, link), "symlink")

	f.add(t, AddRequest{Name: "checks", Argv: []string{link}})
	if m := f.lookup(t, "checks", nil); !m.Matched {
		t.Fatalf("an unchanged link target must match; reason: %s", m.Reason)
	}

	testsupport.Must(t, os.Remove(link), "removing the link")
	testsupport.Must(t, os.Symlink(second, link), "retargeting the link")

	m := f.lookup(t, "checks", nil)
	if m.Matched {
		t.Fatal("a link retargeted at different bytes must be unmatched")
	}
	if !strings.Contains(m.Reason, second) {
		t.Errorf("the reason must name the resolved path %s; got %q", second, m.Reason)
	}
}

// TestContentPinFailsClosedOnAnUnreadableTarget covers the pinned path no
// longer being a regular file: a FIFO must refuse without blocking on open,
// and a missing file must refuse naming the path.
func TestContentPinFailsClosedOnAnUnreadableTarget(t *testing.T) {
	f := newPinFixture(t)
	script := writeScript(t, t.TempDir(), "gate.sh", "#!/bin/sh\nexit 0\n")
	f.add(t, AddRequest{Name: "checks", Argv: []string{script}})

	testsupport.Must(t, os.Remove(script), "removing the script")
	m := f.lookup(t, "checks", nil)
	if m.Matched || !strings.Contains(m.Reason, script) {
		t.Errorf("a missing pinned file must be unmatched naming %s; got %+v", script, m)
	}

	if err := mkfifo(script); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	m = f.lookup(t, "checks", nil)
	if m.Matched || !strings.Contains(m.Reason, "a FIFO") {
		t.Errorf("a FIFO at the pinned path must be unmatched as a FIFO; got %+v", m)
	}
}

// TestRepinAfterIntendedChange is §3.5's re-pin row: re-running the same add
// after an intended change replaces the stored hash, records the change through
// OnChange before the write, and the next lookup matches.
func TestRepinAfterIntendedChange(t *testing.T) {
	const original = "#!/bin/sh\nexit 0\n"
	const changed = "#!/bin/sh\necho changed\nexit 0\n"

	f := newPinFixture(t)
	script := writeScript(t, t.TempDir(), "gate.sh", original)
	base := AddRequest{Name: "checks", Argv: []string{script, "small"}, Tree: true}

	first := f.add(t, base)
	if first.Entry.Argv0SHA256 != sha256Hex(original) {
		t.Fatalf("add must pin the file's hash; stored %q, want %q", first.Entry.Argv0SHA256, sha256Hex(original))
	}

	err := os.WriteFile(script, []byte(changed), 0o755)
	testsupport.Must(t, err, "rewriting the script: %v", err)
	if m := f.lookup(t, "checks", nil); m.Matched {
		t.Fatal("before the re-pin, the changed script must be unmatched")
	}

	// A conflicting re-add is still a conflict, and leaves the old pin alone.
	conflicting := base
	conflicting.Tree = false
	if _, err := addAt(f.storePath, withRepo(conflicting, f.identity)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a differing flag must still conflict; got %v", err)
	}
	assertStoredPin(t, f.storePath, sha256Hex(original))

	// A failing record aborts the re-pin with nothing written.
	failing := base
	failing.OnChange = func(Entry) error { return errors.New("record failed") }
	if _, err := addAt(f.storePath, withRepo(failing, f.identity)); err == nil {
		t.Fatal("a re-pin whose record fails must fail")
	}
	assertStoredPin(t, f.storePath, sha256Hex(original))

	var recorded []Entry
	repin := base
	repin.OnChange = func(e Entry) error { recorded = append(recorded, e); return nil }
	res := f.add(t, repin)
	if res.Idempotent {
		t.Error("a re-pin changes the store; it must not report itself idempotent")
	}
	if len(recorded) != 1 || recorded[0].Argv0SHA256 != sha256Hex(changed) {
		t.Errorf("the re-pin must be recorded once with the new hash; got %+v", recorded)
	}
	disclosure := strings.Join(res.Warnings, "\n")
	for _, want := range []string{script, sha256Hex(original), sha256Hex(changed)} {
		if !strings.Contains(disclosure, want) {
			t.Errorf("the re-pin disclosure must name %q; got %q", want, disclosure)
		}
	}
	assertStoredPin(t, f.storePath, sha256Hex(changed))

	if m := f.lookup(t, "checks", nil); !m.Matched {
		t.Errorf("after the re-pin the script must match; reason: %s", m.Reason)
	}

	// With the pin current, the same add is the idempotent row again.
	again := base
	var calls int
	again.OnChange = func(Entry) error { calls++; return nil }
	if res := f.add(t, again); !res.Idempotent || calls != 0 {
		t.Errorf("a re-add with an unchanged file must be idempotent and unrecorded; idempotent=%t calls=%d", res.Idempotent, calls)
	}
}

func withRepo(req AddRequest, identity string) AddRequest {
	req.RepoRoot = identity
	return req
}

func assertStoredPin(t *testing.T, storePath, want string) {
	t.Helper()
	st, err := loadAt(storePath)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 1 {
		t.Fatalf("expected one stored entry, got %d", len(st.Entries))
	}
	if got := st.Entries[0].Argv0SHA256; got != want {
		t.Errorf("stored argv0_sha256 = %q, want %q", got, want)
	}
}

// TestAddPinsOnlyAnAbsoluteArgv0AndFailsClosed: a bare name gets no pin, and
// an absolute argv[0] that cannot be hashed refuses the add rather than
// writing an entry that would read as an unpinned legacy one.
func TestAddPinsOnlyAnAbsoluteArgv0AndFailsClosed(t *testing.T) {
	f := newPinFixture(t)
	if res := f.add(t, AddRequest{Name: "bare", Argv: []string{"make", "test"}}); res.Entry.Argv0SHA256 != "" {
		t.Errorf("a bare-name argv[0] must not be pinned; got %q", res.Entry.Argv0SHA256)
	}

	missing := filepath.Join(t.TempDir(), "absent.sh")
	_, err := addAt(f.storePath, AddRequest{Name: "missing", Argv: []string{missing}, RepoRoot: f.identity})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("an unhashable absolute argv[0] must refuse the add naming %s; got %v", missing, err)
	}
	st, err := loadAt(f.storePath)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 1 {
		t.Errorf("a refused add must write nothing; store has %d entries", len(st.Entries))
	}
}

// TestLegacyEntryWithoutContentHash is the compatibility row: a store written
// before pinning existed still parses, and its absolute-argv entry matches with
// a warning naming the entry and the re-pin command.
func TestLegacyEntryWithoutContentHash(t *testing.T) {
	path := sandbox(t)
	writeStoreFile(t, path, `version = 1

[[entry]]
name = "secret-scan"
argv = ["/usr/bin/true"]
global = true

[[entry]]
name = "checks"
argv = ["make", "test"]
global = true
`, storeFileMode)

	st, err := loadAt(path)
	testsupport.Must(t, err, "a legacy store without argv0_sha256 must parse: %v", err)

	m := st.Lookup("/src/example", "secret-scan", nil)
	if !m.Matched {
		t.Fatalf("a legacy absolute entry must still match; reason: %s", m.Reason)
	}
	for _, want := range []string{"secret-scan", "docket trust add"} {
		if !strings.Contains(m.Warning, want) {
			t.Errorf("the legacy warning must name %q; got %q", want, m.Warning)
		}
	}

	if bare := st.Lookup("/src/example", "checks", nil); !bare.Matched || bare.Warning != "" {
		t.Errorf("a bare-name entry is never pinned and carries no warning; got %+v", bare)
	}
}

// TestParseRefusesAMalformedContentPin keeps a hand-edited argv0_sha256 from
// meaning anything the ruling does not cover.
func TestParseRefusesAMalformedContentPin(t *testing.T) {
	valid := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, argv, pin, wantErr string
	}{
		{"bare-name argv[0]", `["make", "test"]`, valid, "absolute"},
		{"short", `["/usr/bin/true"]`, valid[:63], "64 lowercase hex"},
		{"uppercase", `["/usr/bin/true"]`, strings.Repeat("A", 64), "64 lowercase hex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := sandbox(t)
			writeStoreFile(t, path, "version = 1\n\n[[entry]]\nname = \"g\"\nargv = "+tc.argv+
				"\nargv0_sha256 = \""+tc.pin+"\"\nglobal = true\n", storeFileMode)
			_, err := loadAt(path)
			if !errors.Is(err, ErrParse) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("want a parse error mentioning %q; got %v", tc.wantErr, err)
			}
		})
	}
}
