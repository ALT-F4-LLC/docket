package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `run activate --pin PATH` used to record PATH verbatim, while every
// reader resolves a file pin config-relative at each root and then absolute —
// never relative to the cwd activation ran in. A repo-relative path to a file
// under an instance-config root therefore activated cleanly and never resolved
// again. These tests pin the writer to the readers' rules.

// TestConfigRelativeRefForms is the helper's table: every spelling of a path
// under a root yields the same ref, and a path outside every root yields none.
func TestConfigRelativeRefForms(t *testing.T) {
	work := t.TempDir()
	writeConfigFile(t, filepath.Join(work, ".docket", "config"), "workflows/x.toml", "x")
	// Resolved AFTER the tree exists, the way the scan resolves a root (macOS
	// puts every temp directory under a /var -> /private/var symlink).
	root := canonical(t, filepath.Join(work, ".docket", "config"))
	elsewhere := t.TempDir()

	t.Chdir(work)

	for _, tc := range []struct {
		name, path, want string
		ok               bool
	}{
		{"repo-relative", filepath.Join(".docket", "config", "workflows", "x.toml"), filepath.Join("workflows", "x.toml"), true},
		{"absolute", filepath.Join(root, "workflows", "x.toml"), filepath.Join("workflows", "x.toml"), true},
		{"the root itself", root, "", false},
		{"outside every root", filepath.Join(elsewhere, "x.toml"), "", false},
		{"sibling with the root as a prefix", root + "-other/x.toml", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := configRelativeRef([]string{root}, tc.path)
			if ok != tc.ok || got != tc.want {
				t.Errorf("configRelativeRef(%q) = (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.ok)
			}
		})
	}

	// From the config root, the bare relative form resolves too.
	t.Chdir(root)
	if got, ok := configRelativeRef([]string{root}, filepath.Join("workflows", "x.toml")); !ok || got != filepath.Join("workflows", "x.toml") {
		t.Errorf("from the config root: got (%q, %v)", got, ok)
	}
}

// TestReadFilePinsAgreeWithReaders is the by-construction check: the pin
// activation records for a repo-relative path is one verifyFilePin and the
// packet resolver find at the same roots.
func TestReadFilePinsAgreeWithReaders(t *testing.T) {
	work := t.TempDir()
	body := "a contract\n"
	writeConfigFile(t, filepath.Join(work, ".docket", "config"), "contracts/pinned.md", body)
	root := canonical(t, filepath.Join(work, ".docket", "config"))
	t.Chdir(work)

	pins, err := readFilePins([]string{filepath.Join(".docket", "config", "contracts", "pinned.md")}, []string{root})
	testsupport.Must(t, err, "readFilePins: %v", err)
	if len(pins) != 1 || pins[0].Ref != filepath.Join("contracts", "pinned.md") {
		t.Fatalf("recorded %+v, want one pin at the config-relative ref", pins)
	}

	// The pin verifies from a cwd that is NOT the repository.
	t.Chdir(t.TempDir())
	v := PinVerdict{Kind: pins[0].Kind, Ref: pins[0].Ref, Pinned: pins[0].SHA256}
	verifyFilePin(&v, []string{root}, pins[0])
	if v.Status != PinOK {
		t.Errorf("verifyFilePin status = %q (path %q), want %q", v.Status, v.Path, PinOK)
	}

	got, _, err := readPinnedPacketFile("RUN-1", packetPinsForRun(pins), []string{root}, filepath.Join("contracts", "pinned.md"))
	testsupport.Must(t, err, "readPinnedPacketFile: %v", err)
	if got != body {
		t.Errorf("packet resolver read %q, want %q", got, body)
	}
}

// TestReadFilePinsKeepsAbsoluteOutsideRoots preserves the arbitrary-file pin:
// an absolute path under no root is recorded as given, which is the form the
// readers honor for it.
func TestReadFilePinsKeepsAbsoluteOutsideRoots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.md")
	err := os.WriteFile(path, []byte("notes"), 0o644)
	testsupport.Must(t, err, "writing pin file: %v", err)

	pins, err := readFilePins([]string{path}, []string{canonical(t, t.TempDir())})
	testsupport.Must(t, err, "readFilePins: %v", err)
	if len(pins) != 1 || pins[0].Ref != path {
		t.Fatalf("recorded %+v, want the absolute path as given", pins)
	}
	v := PinVerdict{Kind: pins[0].Kind, Ref: pins[0].Ref, Pinned: pins[0].SHA256}
	verifyFilePin(&v, nil, pins[0])
	if v.Status != PinOK {
		t.Errorf("verifyFilePin status = %q, want %q", v.Status, PinOK)
	}
}

// TestReadFilePinsRefusesRelativeOutsideRoots is the refusal: a relative path
// under no root could never resolve once recorded, so it is VALIDATION_ERROR
// naming the absolute form, and nothing is read into the pin set.
func TestReadFilePinsRefusesRelativeOutsideRoots(t *testing.T) {
	work := t.TempDir()
	err := os.WriteFile(filepath.Join(work, "notes.md"), []byte("notes"), 0o644)
	testsupport.Must(t, err, "writing pin file: %v", err)
	t.Chdir(work)

	pins, err := readFilePins([]string{"notes.md"}, []string{canonical(t, t.TempDir())})
	if err == nil {
		t.Fatalf("readFilePins accepted a relative path outside every root: %+v", pins)
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	abs, _ := filepath.Abs("notes.md")
	if !strings.Contains(err.Error(), abs) {
		t.Errorf("error does not name the absolute form %q: %s", abs, err)
	}
}

// TestActivateRepoRelativePinResolvesEverywhere is the acceptance criterion
// end to end, the issue's own repro: activate from the repository root with a
// repo-relative path to a file under the repo's config root, then verify-pins
// reports ok and pin show prints the bytes. Both readers resolve at the
// instance-config roots of the invoking checkout, which is what the recorded
// ref must agree with; cwd-independence given the roots is
// TestReadFilePinsAgreeWithReaders' half.
func TestActivateRepoRelativePinResolvesEverywhere(t *testing.T) {
	conn, shared, repo := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	body := "pinned by path\n"
	writeConfigFile(t, repo, "contracts/pinned.md", body)

	issue := createIssue(t, conn, "pin by repo path", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID, filepath.Join(".docket", "config", "contracts", "pinned.md"))
	testsupport.Must(t, err, "activate: %v", err)

	pins, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)
	var ref string
	for _, p := range pins {
		if p.Kind == db.PinKindFile && strings.HasSuffix(p.Ref, "pinned.md") {
			ref = p.Ref
		}
	}
	if want := filepath.Join("contracts", "pinned.md"); ref != want {
		t.Fatalf("file pin ref = %q, want the config-relative %q (pins: %+v)", ref, want, pins)
	}

	report, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	for _, v := range report.Pins {
		if v.Ref == ref && v.Status != PinOK {
			t.Errorf("verify-pins reports %q as %q, want %q", ref, v.Status, PinOK)
		}
	}
	if report.Missing != 0 {
		t.Errorf("verify-pins reports %d missing pins: %+v", report.Missing, report.Pins)
	}

	got, err := PinContent(conn, run.ID, ref)
	testsupport.Must(t, err, "PinContent(%s): %v", ref, err)
	if got != body {
		t.Errorf("pin show printed %q, want %q", got, body)
	}
}

// TestActivateRefusesRelativePinOutsideRoots is the refusal at the
// activation seam: VALIDATION_ERROR, and no run rows, steps, or pins written.
func TestActivateRefusesRelativePinOutsideRoots(t *testing.T) {
	conn, shared, _ := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", autoWorkflowSrc)
	err := os.WriteFile("notes.md", []byte("notes"), 0o644)
	testsupport.Must(t, err, "writing pin file: %v", err)

	issue := createIssue(t, conn, "relative pin", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID, "notes.md")
	if err == nil {
		t.Fatal("activate accepted a relative --pin path outside every instance-config root")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	assertNothingWritten(t, conn)
	after, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "reading run: %v", err)
	if after.Status != model.RunPlanning {
		t.Errorf("run status = %q after a refused activation, want %q", after.Status, model.RunPlanning)
	}
}
