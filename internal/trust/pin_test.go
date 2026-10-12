package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// TestRepinUpgradesALegacyEntry is the remedy the legacy warning names: re-running
// the same add over an entry written before pinning existed pins the file's
// current bytes, records and discloses the change from "none", and a later
// edit to the script no longer matches.
func TestRepinUpgradesALegacyEntry(t *testing.T) {
	const body = "#!/bin/sh\nexit 0\n"

	f := newPinFixture(t)
	script := writeScript(t, t.TempDir(), "gate.sh", body)
	argv := []string{script, "small"}
	writeStoreFile(t, f.storePath, fmt.Sprintf(`version = 1

[[entry]]
name = "checks"
argv = [%q, %q]
argv_sha256 = %q
repo = %q
`, argv[0], argv[1], ArgvSHA256(argv), f.identity), storeFileMode)

	if m := f.lookup(t, "checks", nil); !m.Matched || m.Warning == "" {
		t.Fatalf("the fixture must be a legacy entry that matches with a warning; got %+v", m)
	}

	var recorded []Entry
	res := f.add(t, AddRequest{
		Name:     "checks",
		Argv:     argv,
		OnChange: func(e Entry) error { recorded = append(recorded, e); return nil },
	})
	pin := sha256Hex(body)
	if res.Idempotent {
		t.Error("pinning a legacy entry changes the store; it must not report itself idempotent")
	}
	if len(recorded) != 1 || recorded[0].Argv0SHA256 != pin {
		t.Errorf("the upgrade must be recorded once with the file's hash %s; got %+v", pin, recorded)
	}
	want := fmt.Sprintf("re-pinned %s: argv0_sha256 none to %s", script, pin)
	if disclosure := strings.Join(res.Warnings, "\n"); !strings.Contains(disclosure, want) {
		t.Errorf("the disclosure must read %q; got %q", want, disclosure)
	}
	assertStoredPin(t, f.storePath, pin)

	err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	testsupport.Must(t, err, "rewriting the script: %v", err)
	if m := f.lookup(t, "checks", nil); m.Matched {
		t.Errorf("after the upgrade a one-byte edit must be unmatched; got %+v", m)
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

// --- Content pins on declared files under an explicit root ------------------

const (
	declaredMakefile = "check:\n\t. scripts/gate-range && echo ok\n"
	declaredScript   = "#!/bin/sh\nBASE=main\n"
)

// declaredRoot is a temporary repository holding a makefile and the script it
// sources, the shape the declared-file pin exists for.
func declaredRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testsupport.Must(t, os.MkdirAll(filepath.Join(root, "scripts"), 0o755), "creating scripts/")
	writeScript(t, root, "Makefile", declaredMakefile)
	writeScript(t, root, filepath.Join("scripts", "gate-range"), declaredScript)
	return root
}

func declaredRequest(root string) AddRequest {
	return AddRequest{
		Name:          "diff-scope-small",
		Argv:          []string{"make", "check"},
		DeclaredFiles: []string{"Makefile", "scripts/gate-range"},
		DeclaredRoot:  root,
	}
}

func (f pinFixture) lookupAt(t *testing.T, gate, root string) Match {
	t.Helper()
	st, err := loadAt(f.storePath)
	testsupport.Must(t, err, "loadAt: %v", err)
	return st.LookupAt(f.identity, gate, nil, root)
}

// TestDeclaredFilePinMismatch: one changed byte in a declared file under the
// root the gate will run in refuses the match, naming the repo-relative path
// and both hashes. The argv[0] is a bare name, so the check cannot hide behind
// the absolute-only argv[0] pin.
func TestDeclaredFilePinMismatch(t *testing.T) {
	f := newPinFixture(t)
	root := declaredRoot(t)
	f.add(t, declaredRequest(root))

	m := f.lookupAt(t, "diff-scope-small", root)
	if !m.Matched || !slices.Equal(m.Argv, []string{"make", "check"}) {
		t.Fatalf("unchanged declared files must match with the entry's argv; got %+v", m)
	}

	if m := f.lookup(t, "diff-scope-small", nil); m.Matched || m.Argv != nil || !strings.Contains(m.Reason, "root") {
		t.Errorf("Lookup has no root to verify declared files against, so it must refuse naming the root check; got %+v", m)
	}
	if m := f.lookupAt(t, "diff-scope-small", "relative/root"); m.Matched {
		t.Errorf("a relative root must refuse; got %+v", m)
	}

	const drifted = "#!/bin/sh\nBASE=main~\n"
	script := filepath.Join(root, "scripts", "gate-range")
	testsupport.Must(t, os.WriteFile(script, []byte(drifted), 0o755), "rewriting the sourced script")

	m = f.lookupAt(t, "diff-scope-small", root)
	if m.Matched || m.Argv != nil || m.Entry != nil {
		t.Fatalf("a drifted declared file must be unmatched with nothing to execute; got %+v", m)
	}
	for _, want := range []string{"scripts/gate-range", sha256Hex(declaredScript), sha256Hex(drifted)} {
		if !strings.Contains(m.Reason, want) {
			t.Errorf("the reason must name %q; got %q", want, m.Reason)
		}
	}

	// An entry that declares no files is unaffected by the root.
	f.add(t, AddRequest{Name: "plain", Argv: []string{"make", "test"}})
	if got, want := f.lookupAt(t, "plain", root), f.lookup(t, "plain", nil); !got.Matched || !slices.Equal(got.Argv, want.Argv) {
		t.Errorf("an entry without declared files must match as Lookup does; got %+v, want %+v", got, want)
	}
}

// TestDeclaredFileRepin: re-adding the same entry after an intended edit
// replaces every stored declared-file hash, discloses each change, and the
// check against the same root matches again. Dropping a declared file is a
// conflict, never a silent unpin.
func TestDeclaredFileRepin(t *testing.T) {
	const makefile2 = "check:\n\t. scripts/gate-range && echo fine\n"
	const script2 = "#!/bin/sh\nBASE=origin/main\n"

	f := newPinFixture(t)
	root := declaredRoot(t)
	f.add(t, declaredRequest(root))

	testsupport.Must(t, os.WriteFile(filepath.Join(root, "Makefile"), []byte(makefile2), 0o644), "editing Makefile")
	testsupport.Must(t, os.WriteFile(filepath.Join(root, "scripts", "gate-range"), []byte(script2), 0o755), "editing the script")
	if m := f.lookupAt(t, "diff-scope-small", root); m.Matched {
		t.Fatal("before the re-pin, edited declared files must be unmatched")
	}

	var recorded []Entry
	req := declaredRequest(root)
	req.OnChange = func(e Entry) error { recorded = append(recorded, e); return nil }
	res := f.add(t, req)
	if res.Idempotent || len(recorded) != 1 {
		t.Fatalf("a re-pin is a recorded change; idempotent=%t records=%d", res.Idempotent, len(recorded))
	}
	disclosure := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"Makefile", sha256Hex(declaredMakefile), sha256Hex(makefile2),
		"scripts/gate-range", sha256Hex(declaredScript), sha256Hex(script2)} {
		if !strings.Contains(disclosure, want) {
			t.Errorf("the re-pin disclosure must name %q; got %q", want, disclosure)
		}
	}

	want := []DeclaredFile{{Path: "Makefile", SHA256: sha256Hex(makefile2)}, {Path: "scripts/gate-range", SHA256: sha256Hex(script2)}}
	st, err := loadAt(f.storePath)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 1 || !slices.Equal(st.Entries[0].DeclaredFiles, want) {
		t.Fatalf("the store must hold the current hashes %+v; got %+v", want, st.Entries)
	}
	if m := f.lookupAt(t, "diff-scope-small", root); !m.Matched {
		t.Errorf("after the re-pin the check must match; reason: %s", m.Reason)
	}

	again := declaredRequest(root)
	var calls int
	again.OnChange = func(Entry) error { calls++; return nil }
	if res := f.add(t, again); !res.Idempotent || calls != 0 {
		t.Errorf("a re-add with unchanged files must be idempotent and unrecorded; idempotent=%t calls=%d", res.Idempotent, calls)
	}

	dropped := declaredRequest(root)
	dropped.DeclaredFiles = []string{"Makefile"}
	if _, err := addAt(f.storePath, withRepo(dropped, f.identity)); !errors.Is(err, ErrConflict) {
		t.Errorf("dropping a declared file must conflict; got %v", err)
	}
}

// TestDeclaredFilesAreValidated refuses a declared path that could name a file
// other than the one intended, at add and in a hand-edited store, and refuses
// declared files on a global entry, which has no repository to bind them to.
func TestDeclaredFilesAreValidated(t *testing.T) {
	root := declaredRoot(t)
	outside := writeScript(t, t.TempDir(), "outside.sh", "#!/bin/sh\n")
	testsupport.Must(t, os.Symlink(outside, filepath.Join(root, "escape")), "planting an escaping symlink")

	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"parent", []string{"../x"}},
		{"absolute", []string{"/etc/passwd"}},
		{"dot-prefixed", []string{"./Makefile"}},
		{"unclean", []string{"scripts/../Makefile"}},
		{"dot", []string{"."}},
		{"empty", []string{""}},
		{"duplicate", []string{"Makefile", "Makefile"}},
		{"missing", []string{"absent"}},
		{"escaping symlink", []string{"escape"}},
	} {
		t.Run("add "+tc.name, func(t *testing.T) {
			f := newPinFixture(t)
			req := declaredRequest(root)
			req.DeclaredFiles = tc.paths
			if _, err := addAt(f.storePath, withRepo(req, f.identity)); err == nil {
				t.Fatalf("declared files %q must refuse the add", tc.paths)
			}
			st, err := loadAt(f.storePath)
			testsupport.Must(t, err, "loadAt: %v", err)
			if len(st.Entries) != 0 {
				t.Errorf("a refused add must write nothing; got %+v", st.Entries)
			}
		})
	}

	t.Run("add global", func(t *testing.T) {
		f := newPinFixture(t)
		req := declaredRequest(root)
		req.Global = true
		if _, err := addAt(f.storePath, req); !errors.Is(err, ErrParse) {
			t.Errorf("a global entry with declared files must be refused; got %v", err)
		}
	})

	t.Run("add relative root", func(t *testing.T) {
		f := newPinFixture(t)
		req := declaredRequest("relative")
		if _, err := addAt(f.storePath, withRepo(req, f.identity)); err == nil {
			t.Error("a relative declared root must refuse the add")
		}
	})

	valid := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, binding, files, wantErr string
	}{
		{"parent", `repo = "/src/x"`, `{ path = "../x", sha256 = "` + valid + `" }`, "declared file"},
		{"duplicate", `repo = "/src/x"`, `{ path = "a", sha256 = "` + valid + `" }, { path = "a", sha256 = "` + valid + `" }`, "more than once"},
		{"bad hash", `repo = "/src/x"`, `{ path = "a", sha256 = "` + valid[:63] + `" }`, "64 lowercase hex"},
		{"global", `global = true`, `{ path = "a", sha256 = "` + valid + `" }`, "global"},
		{"unknown subkey", `repo = "/src/x"`, `{ path = "a", sha256 = "` + valid + `", mode = "x" }`, "unknown key"},
	} {
		t.Run("parse "+tc.name, func(t *testing.T) {
			path := sandbox(t)
			writeStoreFile(t, path, "version = 1\n\n[[entry]]\nname = \"g\"\nargv = [\"make\"]\n"+tc.binding+
				"\ndeclared_files = ["+tc.files+"]\n", storeFileMode)
			_, err := loadAt(path)
			if !errors.Is(err, ErrParse) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("want a parse error mentioning %q; got %v", tc.wantErr, err)
			}
		})
	}
}
