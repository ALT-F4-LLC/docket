package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// PACKET COMPOSITION — the resolution half.
//
// The engine resolves a step's declared packet files to their BYTES, verified
// against the hash activation recorded, and hands them to the template. Before
// this, `== PINNED` emitted a path and a checksum and nothing opened the file:
// an executor received a filename for a document it was supposed to work from.
//
// The ladder below is `templateSource`'s, applied to a second class of file —
// deliberately, because that reasoning is already the repo's answer to "read a
// pinned file safely".
//
// GENERICITY: the fixtures are a print shop's checklists. Core reads bytes and
// one declared key; it never learns what a file says.

// writeFixture writes a file under root and returns its path.
func writeFixture(t *testing.T, root, name, body string) string {
	t.Helper()
	path := filepath.Join(root, name)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	testsupport.Must(t, err, "mkdir: %v", err)
	err = os.WriteFile(path, []byte(body), 0o644)
	testsupport.Must(t, err, "writing %s: %v", name, err)
	return path
}

// TestPacketFrontmatter pins that the engine reads ONE declared-includes key and
// ignores every other.
//
// This is the concession the design review approved, and its boundary is the
// whole argument. Core learns that a file may open with a `---` block and that
// one key inside it holds paths to more files. It learns nothing about any
// other key, which is why an instance's own frontmatter passes through
// untouched.
func TestPacketFrontmatter(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantIncludes []string
		wantBody     string
		wantErr      string
	}{
		{
			name:     "no frontmatter",
			body:     "# Proofing\nCheck the margins.\n",
			wantBody: "# Proofing\nCheck the margins.\n",
		},
		{
			name:         "the declared key is read and stripped",
			body:         "---\npacket_includes:\n  - fragments/house-style.md\n---\n# Proofing\n",
			wantIncludes: []string{"fragments/house-style.md"},
			wantBody:     "# Proofing\n",
		},
		{
			name:     "other keys are ignored, not validated",
			body:     "---\ntitle: Proofing\nowner: the-desk\nrevision: 4\n---\n# Proofing\n",
			wantBody: "# Proofing\n",
		},
		{
			name:         "the declared key alongside ignored keys",
			body:         "---\ntitle: Proofing\npacket_includes:\n  - a.md\n  - b.md\nowner: x\n---\nBody\n",
			wantIncludes: []string{"a.md", "b.md"},
			wantBody:     "Body\n",
		},
		{
			name:    "unterminated frontmatter",
			body:    "---\npacket_includes:\n  - a.md\n# Proofing\n",
			wantErr: "frontmatter",
		},
		{
			name:    "the declared key is a scalar",
			body:    "---\npacket_includes: a.md\n---\nBody\n",
			wantErr: "list",
		},
		{
			name:    "an include escapes the config directory",
			body:    "---\npacket_includes:\n  - ../../secrets.md\n---\nBody\n",
			wantErr: "..",
		},
		{
			name:    "an include is empty",
			body:    "---\npacket_includes:\n  - \"\"\n---\nBody\n",
			wantErr: "empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			includes, body, err := parsePacketFrontmatter("checklists/proofing.md", tc.body)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parsed without error, want a refusal mentioning %q", tc.wantErr)
				}
				if code, _ := CodeOf(err); code != CodeValidation {
					t.Errorf("code = %q, want %q", code, CodeValidation)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %q, want it to mention %q", err.Error(), tc.wantErr)
				}
				// Every refusal names the file — a malformed include is
				// otherwise very hard to locate in a corpus.
				if !strings.Contains(err.Error(), "checklists/proofing.md") {
					t.Errorf("err = %q, want it to name the file", err.Error())
				}
				return
			}
			testsupport.Must(t, err, "unexpected refusal: %v", err)
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q — frontmatter must be stripped", body, tc.wantBody)
			}
			if len(includes) != len(tc.wantIncludes) {
				t.Fatalf("includes = %v, want %v", includes, tc.wantIncludes)
			}
			for i := range tc.wantIncludes {
				if includes[i] != tc.wantIncludes[i] {
					t.Errorf("includes[%d] = %q, want %q", i, includes[i], tc.wantIncludes[i])
				}
			}
		})
	}
}

// TestPacketFrontmatterIgnoresRetiringKey is the INTERIM rule, as a proven
// property rather than a hope.
//
// The corpus currently declares fragment relationships under a different key,
// authored as bare names. That key retires in a corpus wave after this patch;
// until then a file still carrying it contributes NOTHING from it — the engine
// does not read that key and will not grow a compatibility path that does.
//
// The file is neither refused nor misread. Packets in the interim are
// correct-but-thin: the declared file itself inlines, and its fragments arrive
// when the corpus is re-authored.
func TestPacketFrontmatterIgnoresRetiringKey(t *testing.T) {
	const body = "---\nfragments:\n  - house-style\n  - margins\n---\n# Proofing\n"

	includes, stripped, err := parsePacketFrontmatter("checklists/proofing.md", body)
	testsupport.Must(t, err, "a file carrying the retiring key was refused: %v", err)
	if len(includes) != 0 {
		t.Errorf("includes = %v, want none — only the declared key is read", includes)
	}
	if stripped != "# Proofing\n" {
		t.Errorf("body = %q, want the body with frontmatter stripped", stripped)
	}
}

// TestPacketIncludesAreOneLevelDeep is the depth hard stop, asserted explicitly so
// it is a tested property rather than an implementation accident.
//
// Unbounded recursion would need cycle detection and a depth cap, and every
// level makes the closure size harder for an author to predict at the moment
// the caps need it predictable. One level matches the packet's structure exactly: the
// file, and the files it declares.
func TestPacketIncludesAreOneLevelDeep(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "checklists/proofing.md",
		"---\npacket_includes:\n  - fragments/house-style.md\n---\nTOP\n")
	writeFixture(t, root, "fragments/house-style.md",
		"---\npacket_includes:\n  - fragments/deeper.md\n---\nMIDDLE\n")
	writeFixture(t, root, "fragments/deeper.md", "DEEPEST\n")

	files, err := resolvePacketFiles("RUN-1", testPinSet(t, root,
		"checklists/proofing.md", "fragments/house-style.md", "fragments/deeper.md"),
		[]string{root}, []string{"checklists/proofing.md"})
	testsupport.Must(t, err, "resolvePacketFiles: %v", err)

	var bodies []string
	for _, f := range files {
		bodies = append(bodies, strings.TrimSpace(f.Body))
	}
	joined := strings.Join(bodies, "|")
	if joined != "TOP|MIDDLE" {
		t.Errorf("resolved %q, want TOP|MIDDLE — includes do not themselves include",
			joined)
	}
}

// TestPacketIncludesDedupe pins de-duplication: a file reachable twice is inlined ONCE, at
// its first position. Repeating it would inflate the closure for no gain.
func TestPacketIncludesDedupe(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "checklists/a.md",
		"---\npacket_includes:\n  - fragments/shared.md\n---\nA\n")
	writeFixture(t, root, "checklists/b.md",
		"---\npacket_includes:\n  - fragments/shared.md\n---\nB\n")
	writeFixture(t, root, "fragments/shared.md", "SHARED\n")

	files, err := resolvePacketFiles("RUN-1", testPinSet(t, root,
		"checklists/a.md", "checklists/b.md", "fragments/shared.md"),
		[]string{root}, []string{"checklists/a.md", "checklists/b.md"})
	testsupport.Must(t, err, "resolvePacketFiles: %v", err)

	var order []string
	shared := 0
	for _, f := range files {
		order = append(order, f.Path)
		if strings.Contains(f.Path, "shared") {
			shared++
		}
	}
	if shared != 1 {
		t.Errorf("the shared fragment appears %d times, want 1", shared)
	}
	// Declared order, each entry followed immediately by its own includes.
	want := "checklists/a.md,fragments/shared.md,checklists/b.md"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
}

// TestPacketResolutionLadder is the resolution ladder — the four states of a declared entry.
//
// The first three rows are templateSource's ladder applied to a second class of
// file. The fourth differs, and the difference is the point: an unpinned
// `--template F` renders unverified because the OPERATOR chose that file at
// render time, while a packet entry is chosen at AUTHORING time and pinned at
// activation. An unpinned one means the run holds no snapshot, so reading the
// live tree would break the byte-identical property outright.
func TestPacketResolutionLadder(t *testing.T) {
	t.Run("pinned and matching resolves", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "checklists/a.md", "BODY\n")
		files, err := resolvePacketFiles(
			"RUN-1", testPinSet(t, root, "checklists/a.md"),
			[]string{root}, []string{"checklists/a.md"})
		testsupport.Must(t, err, "resolvePacketFiles: %v", err)
		if len(files) != 1 || strings.TrimSpace(files[0].Body) != "BODY" {
			t.Errorf("files = %+v, want the file's bytes inlined", files)
		}
		if files[0].SHA256 == "" {
			t.Error("the hash must ride along as the provenance record")
		}
	})

	t.Run("edited after activation is a CONFLICT naming both hashes", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "checklists/a.md", "BODY\n")
		pins := testPinSet(t, root, "checklists/a.md")
		pinnedHash := pins[filepath.Join(root, "checklists/a.md")].SHA256

		writeFixture(t, root, "checklists/a.md", "EDITED\n")

		_, err := resolvePacketFiles(
			"RUN-1", pins, []string{root}, []string{"checklists/a.md"})
		if err == nil {
			t.Fatal("an edited file resolved, want CONFLICT")
		}
		if code, _ := CodeOf(err); code != CodeConflict {
			t.Errorf("code = %q, want %q", code, CodeConflict)
		}
		if !strings.Contains(err.Error(), pinnedHash) {
			t.Errorf("err = %q, want the PINNED hash named", err.Error())
		}
	})

	t.Run("deleted after activation is NOT_FOUND", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "checklists/a.md", "BODY\n")
		pins := testPinSet(t, root, "checklists/a.md")
		err := os.Remove(filepath.Join(root, "checklists/a.md"))
		testsupport.Must(t, err, "removing the fixture: %v", err)

		_, err = resolvePacketFiles("RUN-1", pins, []string{root}, []string{"checklists/a.md"})
		if code, _ := CodeOf(err); code != CodeNotFound {
			t.Errorf("code = %q (err %v), want %q", code, err, CodeNotFound)
		}
	})

	t.Run("present but never pinned is refused", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "checklists/a.md", "BODY\n")

		_, err := resolvePacketFiles(
			"RUN-1", map[string]db.Pin{}, []string{root},
			[]string{"checklists/a.md"})
		if err == nil {
			t.Fatal("an unpinned file was read, want a refusal")
		}
		if code, _ := CodeOf(err); code != CodeValidation {
			t.Errorf("code = %q, want %q — an unpinned entry has no snapshot", code, CodeValidation)
		}
	})

	t.Run("a dangling include is refused, never silently skipped", func(t *testing.T) {
		root := t.TempDir()
		writeFixture(t, root, "checklists/a.md",
			"---\npacket_includes:\n  - fragments/missing.md\n---\nA\n")

		_, err := resolvePacketFiles(
			"RUN-1", testPinSet(t, root, "checklists/a.md"),
			[]string{root}, []string{"checklists/a.md"})
		if err == nil {
			t.Fatal("a dangling include was skipped — that is DKT-70's failure signature")
		}
	})
}

// TestPacketRendersFromPersistedPinBytes: once activation has stored a packet
// file's bytes, the packet renders from the store, so deleting the source file
// leaves the rendered packet byte-identical.
func TestPacketRendersFromPersistedPinBytes(t *testing.T) {
	conn, shared, _ := unionRepo(t)
	writeConfigFile(t, shared, "workflows/auto-dev.toml", packetWorkflow("contracts/fix.md"))
	source := writeConfigFile(t, shared, "contracts/fix.md", "the pinned contract\n")

	issue := createIssue(t, conn, "persisted packet", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	stepID := stepIDByInstance(t, conn, "implement@0")

	before, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep before the delete: %v", err)
	if !strings.Contains(before.Packet, "the pinned contract") {
		t.Fatalf("premise: the packet does not inline the pinned file:\n%s", before.Packet)
	}

	err = os.Remove(source)
	testsupport.Must(t, err, "deleting the pinned source file: %v", err)

	after, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep after the source file was deleted: %v", err)
	if after.Packet != before.Packet {
		t.Errorf("the packet changed after the source file was deleted:\nbefore:\n%s\nafter:\n%s",
			before.Packet, after.Packet)
	}
}

// TestPacketPersistedBytesHashMismatchConflicts: stored bytes that do not hash
// to the recorded pin are a CONFLICT naming both hashes, never inlined.
func TestPacketPersistedBytesHashMismatchConflicts(t *testing.T) {
	const ref = "checklists/a.md"
	pinnedHash := sha256Hex([]byte("BODY\n"))
	storedHash := sha256Hex([]byte("TAMPERED\n"))
	pins := map[string]db.Pin{ref: {
		Kind: db.PinKindFile, Ref: ref, SHA256: pinnedHash,
		Content: sql.NullString{String: "TAMPERED\n", Valid: true},
	}}

	_, err := resolvePacketFiles("RUN-1", pins, []string{t.TempDir()}, []string{ref})
	if err == nil {
		t.Fatal("persisted bytes that miss the pin hash resolved, want CONFLICT")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("code = %q (err %v), want %q", code, err, CodeConflict)
	}
	for _, hash := range []string{pinnedHash, storedHash} {
		if !strings.Contains(err.Error(), hash) {
			t.Errorf("err = %q, want it to name %s", err.Error(), hash)
		}
	}
}

// TestPacketResolutionIsDeterministic is determinism through the assembler:
// same inputs, byte-identical output, so the goldens do not flap.
func TestPacketResolutionIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "checklists/a.md",
		"---\npacket_includes:\n  - fragments/x.md\n  - fragments/y.md\n---\nA\n")
	writeFixture(t, root, "fragments/x.md", "X\n")
	writeFixture(t, root, "fragments/y.md", "Y\n")

	pins := testPinSet(t, root, "checklists/a.md", "fragments/x.md", "fragments/y.md")

	var first string
	for i := 0; i < 16; i++ {
		files, err := resolvePacketFiles(
			"RUN-1", pins, []string{root}, []string{"checklists/a.md"})
		testsupport.Must(t, err, "resolvePacketFiles: %v", err)
		var b strings.Builder
		for _, f := range files {
			b.WriteString(f.Path)
			b.WriteString("\x00")
			b.WriteString(f.Body)
		}
		if i == 0 {
			first = b.String()
			continue
		}
		if b.String() != first {
			t.Fatal("resolution is not deterministic across identical calls")
		}
	}
}

// testPinSet hashes the named fixtures into the ref -> pin map the resolver
// verifies against, in the legacy shape: keyed by the FULL path and stored
// without bytes, so the resolver reads each file from disk.
//
// The declared entry is relative to the config directory, and the resolver
// joins the two. Keying this helper the same way is what keeps it an honest
// stand-in for a real legacy pin set rather than a shape only these tests accept.
func testPinSet(t *testing.T, root string, names ...string) map[string]db.Pin {
	t.Helper()
	pins := make(map[string]db.Pin, len(names))
	for _, name := range names {
		full := filepath.Join(root, name)
		body, err := os.ReadFile(full)
		testsupport.Must(t, err, "reading fixture %s: %v", name, err)
		pins[full] = db.Pin{Kind: db.PinKindFile, Ref: full, SHA256: sha256Hex(body)}
	}
	return pins
}

// TestStepPacketFilesSubstitutesTheResolvedHint pins DKT-70's seam: the
// `{executor}` substitution binds the RESOLVED hint when a caller supplies
// one, and the declared hint otherwise. The assertion reads the refusal —
// with nothing pinned, resolution refuses NAMING THE EXACT PATH it derived,
// which is precisely the substituted entry.
//
// Before the override existed, a policy table resolving the declared hint by
// issue label could never influence which contract rendered: the corpus
// shipped per-resolved-hint contracts no packet could ever name.
func TestStepPacketFilesSubstitutesTheResolvedHint(t *testing.T) {
	conn := mustDB(t)
	step := &db.Step{RunID: 1, Executor: "declared-router"}
	spec := &workflow.Step{Packet: []string{"contracts/{executor}.md"}}

	_, err := stepPacketFiles(conn, step, spec, "")
	if err == nil || !strings.Contains(err.Error(), "contracts/declared-router.md") {
		t.Errorf("declared-hint resolution error = %v, want it to name "+
			"contracts/declared-router.md", err)
	}

	_, err = stepPacketFiles(conn, step, spec, "resolved-specialist")
	if err == nil || !strings.Contains(err.Error(), "contracts/resolved-specialist.md") {
		t.Errorf("resolved-hint resolution error = %v, want it to name "+
			"contracts/resolved-specialist.md — the override must reach the "+
			"substitution", err)
	}
}

// TestRenderStepAsNamesTheResolvedExecutor is the packet-facing half: the
// override lands on the rendered target line, so the packet says who the work
// is actually for, and the default path stays byte-identical to before.
func TestRenderStepAsNamesTheResolvedExecutor(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	plain, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if strings.Contains(plain.Packet, "resolved-specialist") {
		t.Fatal("the un-overridden packet already names the resolved hint")
	}

	resolved, err := RenderStepAs(conn, stepID, "", "resolved-specialist", nowMS)
	testsupport.Must(t, err, "RenderStepAs: %v", err)
	if !strings.Contains(resolved.Packet, "target: resolved-specialist") {
		t.Errorf("the packet does not name the resolved executor:\n%s",
			resolved.Packet)
	}

	// The empty override IS RenderStep — one behavior, two entry points.
	same, err := RenderStepAs(conn, stepID, "", "", nowMS)
	testsupport.Must(t, err, "RenderStepAs(\"\"): %v", err)
	if same.Packet != plain.Packet {
		t.Error("RenderStepAs with no override differs from RenderStep")
	}
}

// attachedStep activates an `issue.files` run whose exec root is root, with
// paths attached to its issue, and returns the store, the step, and a spec
// declaring the form.
func attachedStep(
	t *testing.T, root string, paths []string,
) (*sql.DB, *db.Step, *workflow.Step) {
	t.Helper()
	conn := mustDB(t)
	activateIssueFilesRun(t, conn, root, paths)
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "transcribe@0"))
	testsupport.Must(t, err, "GetStep: %v", err)
	return conn, step, &workflow.Step{Inputs: []string{workflow.InputIssueFiles}}
}

// TestIssueAttachmentFilesRefusesAnEscapingPath: an attachment that is
// absolute, starts with `~`, or has a `..` segment is refused before any read,
// so a row naming a host file outside the run's checkout cannot inline it.
// The escape target exists, so an unguarded read of the `..` rows succeeds.
func TestIssueAttachmentFilesRefusesAnEscapingPath(t *testing.T) {
	for _, path := range []string{
		"../escape.txt",
		"docs/../../escape.txt",
		"/etc/passwd",
		"~/escape.txt",
	} {
		t.Run(path, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "root")
			writeFixture(t, base, "escape.txt", "ESCAPED host bytes\n")
			conn, step, spec := attachedStep(t, root, []string{path})

			out, err := issueAttachmentFiles(conn, step, spec)
			if err == nil {
				t.Fatalf("issueAttachmentFiles accepted %q; got %+v", path, out)
			}
			if code, ok := CodeOf(err); !ok || code != CodeValidation {
				t.Errorf("CodeOf(err) = %q, want %q", code, CodeValidation)
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("refusal does not name %q:\n  %s", path, msg)
			}
			if !strings.Contains(msg, "not a relative path inside") &&
				!strings.Contains(msg, "escapes") {
				t.Errorf("refusal lacks the containment wording:\n  %s", msg)
			}
			if strings.Contains(msg, "could not read") {
				t.Errorf("refusal comes from a read, not the guard:\n  %s", msg)
			}
			if out != nil {
				t.Errorf("refusal returned files: %+v", out)
			}
		})
	}
}

// TestIssueAttachmentFilesInlinesContainedPath: a relative path inside the
// run's checkout still arrives with its bytes and their sha256 unchanged.
func TestIssueAttachmentFilesInlinesContainedPath(t *testing.T) {
	root := t.TempDir()
	body := "contained attachment body\n"
	writeFixture(t, root, "docs/notes.md", body)
	conn, step, spec := attachedStep(t, root, []string{"docs/notes.md"})

	got, err := issueAttachmentFiles(conn, step, spec)
	testsupport.Must(t, err, "issueAttachmentFiles: %v", err)

	digest := sha256.Sum256([]byte(body))
	want := []PacketFile{{
		Path: "docs/notes.md", SHA256: hex.EncodeToString(digest[:]), Body: body,
	}}
	if !slices.Equal(got, want) {
		t.Errorf("issueAttachmentFiles = %+v, want %+v", got, want)
	}
}

// TestIssueAttachmentFilesRefusesASymlinkOutOfTheRoot: an attached path with no
// `..` passes the lexical guard, but a symlink inside the run's checkout that
// resolves outside it is refused, whether the escape is a relative or absolute
// final component or an intermediate directory, and none of the target's bytes
// reach the error or the returned files.
func TestIssueAttachmentFilesRefusesASymlinkOutOfTheRoot(t *testing.T) {
	const sentinel = "ESCAPED host bytes via symlink\n"
	cases := []struct {
		name   string
		attach string
		// link returns the symlink's path relative to the checkout and its
		// target, given base, the checkout's parent, which holds escape.txt.
		link func(base string) (path, target string)
	}{{
		name:   "relative_final_component",
		attach: "docs/link.md",
		link: func(string) (string, string) {
			return "docs/link.md", "../../escape.txt"
		},
	}, {
		name:   "absolute_target",
		attach: "docs/abslink.md",
		link: func(base string) (string, string) {
			return "docs/abslink.md", filepath.Join(base, "escape.txt")
		},
	}, {
		name:   "directory_component",
		attach: "docs/dirlink/escape.txt",
		link: func(string) (string, string) {
			return "docs/dirlink", "../.."
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "root")
			writeFixture(t, base, "escape.txt", sentinel)
			testsupport.Must(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755), "mkdir: %v", nil)
			linkPath, target := tc.link(base)
			testsupport.Must(t,
				os.Symlink(target, filepath.Join(root, linkPath)),
				"symlink: %v", nil)
			// An unguarded read must reach the sentinel, so a refusal below is
			// containment and not a missing fixture.
			if escaped, err := os.ReadFile(filepath.Join(root, tc.attach)); err != nil ||
				string(escaped) != sentinel {
				t.Fatalf("fixture %s does not reach the sentinel: %q, %v",
					tc.attach, escaped, err)
			}
			conn, step, spec := attachedStep(t, root, []string{tc.attach})

			out, err := issueAttachmentFiles(conn, step, spec)
			if err == nil {
				t.Fatalf("issueAttachmentFiles followed the escaping symlink; got %+v", out)
			}
			if code, ok := CodeOf(err); !ok || code != CodeValidation {
				t.Errorf("CodeOf(err) = %q, want %q", code, CodeValidation)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.attach) {
				t.Errorf("refusal does not name %s:\n  %s", tc.attach, msg)
			}
			if strings.Contains(msg, strings.TrimSpace(sentinel)) {
				t.Errorf("refusal carries the escaped target's bytes:\n  %s", msg)
			}
			if out != nil {
				t.Errorf("refusal returned files: %+v", out)
			}
		})
	}
}

// TestIssueAttachmentFilesInlinesASymlinkInsideTheRoot: a symlink whose target
// stays inside the run's checkout still inlines its target's bytes, under the
// attached alias path.
func TestIssueAttachmentFilesInlinesASymlinkInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	body := "real attachment body\n"
	writeFixture(t, root, "real.md", body)
	testsupport.Must(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755), "mkdir: %v", nil)
	testsupport.Must(t,
		os.Symlink("../real.md", filepath.Join(root, "docs", "alias.md")),
		"symlink: %v", nil)
	conn, step, spec := attachedStep(t, root, []string{"docs/alias.md"})

	got, err := issueAttachmentFiles(conn, step, spec)
	testsupport.Must(t, err, "issueAttachmentFiles: %v", err)

	digest := sha256.Sum256([]byte(body))
	want := []PacketFile{{
		Path: "docs/alias.md", SHA256: hex.EncodeToString(digest[:]), Body: body,
	}}
	if !slices.Equal(got, want) {
		t.Errorf("issueAttachmentFiles = %+v, want %+v", got, want)
	}
}
