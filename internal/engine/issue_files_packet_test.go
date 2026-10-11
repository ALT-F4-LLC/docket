package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	dexec "github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-44 — an issue's attached files had no path into a packet. An isolated
// executor was told its inputs arrive in the packet and that the shared
// checkout is not visible, so an issue whose whole input is three attachments
// was unexecutable by the brief's own rules while the files sat on disk.
//
// `issue.files` is the declared form that carries them: each attachment is read
// from the RUN's recorded exec root — the project checkout the issue's paths are
// relative to, not whichever worktree happens to be claiming — and rendered as a
// `== FILE <path>` section beside the packet's other files. The read is a
// filesystem read, so a tracked path and an untracked one arrive identically;
// that is the point, since the motivating attachments were untracked.

const issueFilesWorkflowSrc = `
[pipeline]
name = "attachment-transcribe"
version = 1

[match]
kind = ["task"]

[[step]]
name = "transcribe"
after = []
executor = "author"
emits = "doc"
inputs = ["issue.body", "issue.files"]
`

// issueFilesCheckout builds a project checkout holding one committed file and
// two untracked ones, and returns its root. The git init is what makes the
// tracked/untracked distinction real rather than asserted.
func issueFilesCheckout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	for _, cmd := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		c := exec.Command("git", cmd...)
		c.Dir = root
		c.Env = gitEnv()
		out, err := c.CombinedOutput()
		testsupport.Must(t, err, "git %v: %v\n%s", cmd, err, out)
	}

	testsupport.Must(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755),
		"mkdir docs: %v", nil)
	for path, body := range issueFilesFixture {
		testsupport.Must(t,
			os.WriteFile(filepath.Join(root, path), []byte(body), 0o644),
			"writing %s: %v", path, nil)
	}

	for _, cmd := range [][]string{
		{"add", "docs/tracked.md"},
		{"commit", "-q", "-m", "tracked"},
	} {
		c := exec.Command("git", cmd...)
		c.Dir = root
		c.Env = gitEnv()
		out, err := c.CombinedOutput()
		testsupport.Must(t, err, "git %v: %v\n%s", cmd, err, out)
	}
	return root
}

// TestIssueFilesCheckoutIgnoresOperatorGitConfig pins the helper's hermetic
// git environment: a global config that demands commit signing through a
// failing program must not reach the helper's commit.
func TestIssueFilesCheckoutIgnoresOperatorGitConfig(t *testing.T) {
	home := t.TempDir()
	gitconfig := "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(gitconfig), 0o644); err != nil {
		t.Fatalf("writing .gitconfig: %v", err)
	}
	t.Setenv("HOME", home)
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "XDG_CONFIG_HOME"} {
		t.Setenv(key, "") // registers the restore
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unsetting %s: %v", key, err)
		}
	}

	issueFilesCheckout(t)
}

// issueFilesFixture is the attachment set: one committed path and two loose
// ones, each with content distinguishable in the rendered packet.
var issueFilesFixture = map[string]string{
	"docs/tracked.md":   "tracked attachment body\n",
	"docs/loose-one.md": "first untracked attachment body\n",
	"docs/loose-two.md": "second untracked attachment body\n",
}

// activateIssueFilesRun registers the declaration through the ordinary
// register path, attaches `paths` to the issue, and activates a run whose
// recorded exec root is `root`. It returns the issue and run ids.
func activateIssueFilesRun(
	t *testing.T, conn *sql.DB, root string, paths []string,
) (issue, runID int) {
	t.Helper()
	registerSource(t, conn, []byte(issueFilesWorkflowSrc), "attachment-transcribe.toml")
	issue = createIssue(t, conn, "transcribe the attachments", "the issue body", "task", nil)
	testsupport.Must(t, db.AttachFiles(conn, issue, paths, ""), "AttachFiles: %v", nil)

	run, err := db.InsertRunWithContext(conn, 1, "attachment run", 0, nowMS,
		db.RunContext{ExecRoot: root})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", nil)

	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return issue, run.ID
}

// TestPacketRendersIssueFiles is AC2: every attached path arrives in the packet
// as its own `== FILE` section carrying the file's content, tracked or not.
func TestPacketRendersIssueFiles(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	attached := []string{"docs/tracked.md", "docs/loose-one.md", "docs/loose-two.md"}
	activateIssueFilesRun(t, conn, root, attached)

	result, err := RenderStep(conn, stepIDByInstance(t, conn, "transcribe@0"), "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)

	for _, path := range attached {
		if !strings.Contains(result.Packet, "== FILE "+path) {
			t.Errorf("packet carries no `== FILE %s` section:\n%s", path, result.Packet)
		}
		if !strings.Contains(result.Packet, issueFilesFixture[path]) {
			t.Errorf("packet does not carry the content of %s", path)
		}
	}
}

// TestPacketRendersIssueFilesRefusesAnUnreadablePath is AC3: a path the engine
// cannot read fails the render — and therefore the claim, whose preflight
// renders before any lease is written — naming the path, rather than handing
// out a packet that silently omits an input the step was told it would have.
func TestPacketRendersIssueFilesRefusesAnUnreadablePath(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	activateIssueFilesRun(t, conn, root,
		[]string{"docs/tracked.md", "docs/never-written.md"})

	_, err := RenderStep(conn, stepIDByInstance(t, conn, "transcribe@0"), "", nowMS)
	if err == nil {
		t.Fatal("RenderStep accepted an unreadable attachment; want a refusal")
	}
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Errorf("CodeOf(err) = %q, want %q", code, CodeValidation)
	}
	if !strings.Contains(err.Error(), "docs/never-written.md") {
		t.Errorf("refusal does not name the unreadable path:\n  %v", err)
	}
}

// writeAttachment writes `body` at `path` under the checkout `root`.
func writeAttachment(t *testing.T, root, path, body string) {
	t.Helper()
	testsupport.Must(t,
		os.WriteFile(filepath.Join(root, path), []byte(body), 0o644),
		"writing %s: %v", path, nil)
}

// renderTranscribe renders the attachment workflow's one step.
func renderTranscribe(t *testing.T, conn *sql.DB) string {
	t.Helper()
	result, err := RenderStep(conn, stepIDByInstance(t, conn, "transcribe@0"), "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	return result.Packet
}

// fileSection is the `== FILE` header the packet carries for `path` with
// content `body`, hashed independently of the engine.
func fileSection(path, body string) string {
	sum := sha256.Sum256([]byte(body))
	return "== FILE " + path + "  " + hex.EncodeToString(sum[:])
}

// TestPacketRendersIssueFilesFrozenAtActivation: the attachment LIST is part of
// the run's activation snapshot, so a path attached mid-run never reaches that
// run's packets.
func TestPacketRendersIssueFilesFrozenAtActivation(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/a.md", "frozen attachment a\n")
	writeAttachment(t, root, "docs/b.md", "late attachment b\n")
	issue, _ := activateIssueFilesRun(t, conn, root, []string{"docs/a.md"})

	testsupport.Must(t, db.AttachFiles(conn, issue, []string{"docs/b.md"}, ""),
		"AttachFiles after activation: %v", nil)

	packet := renderTranscribe(t, conn)
	if !strings.Contains(packet, fileSection("docs/a.md", "frozen attachment a\n")) {
		t.Errorf("packet lacks the attachment frozen at activation:\n%s", packet)
	}
	if strings.Contains(packet, "== FILE docs/b.md") ||
		strings.Contains(packet, "late attachment b") {
		t.Errorf("packet inlines docs/b.md, attached after activation:\n%s", packet)
	}
}

// TestPacketRendersIssueFilesBytesLive: only the path list is frozen. The bytes
// are still read from the run's exec root at render time (DKT-44), so an edit
// after activation is what the packet carries.
func TestPacketRendersIssueFilesBytesLive(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/a.md", "content at activation\n")
	activateIssueFilesRun(t, conn, root, []string{"docs/a.md"})

	writeAttachment(t, root, "docs/a.md", "content after activation\n")

	packet := renderTranscribe(t, conn)
	if !strings.Contains(packet, fileSection("docs/a.md", "content after activation\n")) {
		t.Errorf("packet does not carry docs/a.md's current body and sha256:\n%s", packet)
	}
	if strings.Contains(packet, "content at activation") {
		t.Errorf("packet carries docs/a.md's activation-time body:\n%s", packet)
	}
}

// TestPacketRendersIssueFilesLiveForPreFreezeSnapshot: a run activated before
// the list was frozen has a snapshot with no `files` key, and its packets keep
// reading the live attachment list.
func TestPacketRendersIssueFilesLiveForPreFreezeSnapshot(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/a.md", "attachment a\n")
	writeAttachment(t, root, "docs/b.md", "attachment b\n")
	issue, runID := activateIssueFilesRun(t, conn, root, []string{"docs/a.md"})

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	legacy := `{"title":"transcribe the attachments","kind":"task","labels":[],"scope":[]}`
	testsupport.Must(t, db.SetRunIssueSnapshotTx(tx, runID, issue, legacy),
		"SetRunIssueSnapshotTx: %v", nil)
	testsupport.Must(t, tx.Commit(), "Commit: %v", nil)

	testsupport.Must(t, db.AttachFiles(conn, issue, []string{"docs/b.md"}, ""),
		"AttachFiles: %v", nil)

	packet := renderTranscribe(t, conn)
	for path, body := range map[string]string{
		"docs/a.md": "attachment a\n", "docs/b.md": "attachment b\n",
	} {
		if !strings.Contains(packet, fileSection(path, body)) {
			t.Errorf("pre-freeze snapshot: packet lacks live attachment %s:\n%s", path, packet)
		}
	}
}

// TestPacketRendersIssueFilesRefusesAnUndecodableSnapshot: a snapshot that does
// not decode refuses the render. Falling back to the live list would let a
// corrupt snapshot reopen the mid-run attach path the freeze closes.
//
// The snapshot is well-formed JSON whose `files` is not a list. Truncated JSON
// would be refused earlier by the context read, which ignores `files`, so it
// would never reach the attachment read this test pins.
func TestPacketRendersIssueFilesRefusesAnUndecodableSnapshot(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/a.md", "attachment a\n")
	writeAttachment(t, root, "docs/b.md", "attachment b\n")
	issue, runID := activateIssueFilesRun(t, conn, root, []string{"docs/a.md"})

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	undecodable := `{"title":"transcribe the attachments","kind":"task",` +
		`"labels":[],"scope":[],"files":5}`
	testsupport.Must(t, db.SetRunIssueSnapshotTx(tx, runID, issue, undecodable),
		"SetRunIssueSnapshotTx: %v", nil)
	testsupport.Must(t, tx.Commit(), "Commit: %v", nil)

	testsupport.Must(t, db.AttachFiles(conn, issue, []string{"docs/b.md"}, ""),
		"AttachFiles: %v", nil)

	result, err := RenderStep(conn, stepIDByInstance(t, conn, "transcribe@0"), "", nowMS)
	if result != nil && (strings.Contains(result.Packet, "docs/b.md") ||
		strings.Contains(result.Packet, "attachment b")) {
		t.Errorf("undecodable snapshot: packet inlines docs/b.md from the live list:\n%s",
			result.Packet)
	}
	if err == nil {
		t.Fatal("RenderStep accepted an undecodable issue snapshot; want a refusal")
	}
	want := "reading the issue snapshot for " + model.FormatID(issue)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal does not contain %q:\n  %v", want, err)
	}
}

// TestPacketRendersIssueFilesRefusesAFIFO: an attachment that is not a regular
// file refuses, and the refusal comes back without any writer opening the pipe.
// A plain open of a FIFO blocks until a writer arrives, which stalled the claim.
func TestPacketRendersIssueFilesRefusesAFIFO(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	testsupport.Must(t, syscall.Mkfifo(filepath.Join(root, "docs/pipe.md"), 0o600),
		"mkfifo: %v", nil)
	activateIssueFilesRun(t, conn, root, []string{"docs/pipe.md"})
	stepID := stepIDByInstance(t, conn, "transcribe@0")

	refused := make(chan error, 1)
	go func() {
		_, err := RenderStep(conn, stepID, "", nowMS)
		refused <- err
	}()

	select {
	case err := <-refused:
		if code, ok := CodeOf(err); !ok || code != CodeValidation {
			t.Fatalf("CodeOf(err) = %q, want %q (err: %v)", code, CodeValidation, err)
		}
		if !strings.Contains(err.Error(), "docs/pipe.md") {
			t.Errorf("refusal does not name the FIFO's path:\n  %v", err)
		}
	case <-time.After(2 * time.Second):
		// Unblock the stalled render so the test can tear down its database.
		if w, err := os.OpenFile(filepath.Join(root, "docs/pipe.md"), os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		<-refused
		t.Fatal("RenderStep blocked on a FIFO attachment; want an immediate refusal")
	}
}

// TestPacketRendersIssueFilesRefusesOtherNonRegularFiles: a directory, and a
// character device reached through an in-checkout symlink, refuse like a FIFO.
// /dev/null is the device because reading it is harmless; a check narrowed to
// pipes would render it as an empty section.
func TestPacketRendersIssueFilesRefusesOtherNonRegularFiles(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		make       func(full string) error
	}{
		{"directory", "docs/subdir.md", func(full string) error {
			return os.Mkdir(full, 0o755)
		}},
		{"device via symlink", "docs/null.md", func(full string) error {
			return os.Symlink("/dev/null", full)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			root := issueFilesCheckout(t)
			testsupport.Must(t, tc.make(filepath.Join(root, tc.path)),
				"creating %s: %v", tc.path, nil)
			activateIssueFilesRun(t, conn, root, []string{tc.path})

			_, err := RenderStep(conn, stepIDByInstance(t, conn, "transcribe@0"), "", nowMS)
			if code, ok := CodeOf(err); !ok || code != CodeValidation {
				t.Fatalf("CodeOf(err) = %q, want %q (err: %v)", code, CodeValidation, err)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("refusal does not name %s:\n  %v", tc.path, err)
			}
		})
	}
}

// TestClaimRenderRefusesAFIFOAttachmentWithoutALease: `step claim --render`
// renders before it claims, so a FIFO attachment refuses promptly there and the
// step keeps no lease, spends no attempt, and records no claim.
func TestClaimRenderRefusesAFIFOAttachmentWithoutALease(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	testsupport.Must(t, syscall.Mkfifo(filepath.Join(root, "docs/pipe.md"), 0o600),
		"mkfifo: %v", nil)
	activateIssueFilesRun(t, conn, root, []string{"docs/pipe.md"})
	stepID := stepIDByInstance(t, conn, "transcribe@0")

	refused := make(chan error, 1)
	go func() {
		_, _, err := NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
			Owner: "wave:STEP-1", NowMS: nowMS,
		}, "", "")
		refused <- err
	}()

	select {
	case err := <-refused:
		if code, ok := CodeOf(err); !ok || code != CodeValidation {
			t.Fatalf("CodeOf(err) = %q, want %q (err: %v)", code, CodeValidation, err)
		}
		if !strings.Contains(err.Error(), "docs/pipe.md") {
			t.Errorf("refusal does not name the FIFO's path:\n  %v", err)
		}
	case <-time.After(2 * time.Second):
		if w, err := os.OpenFile(filepath.Join(root, "docs/pipe.md"), os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		<-refused
		t.Fatal("ClaimStepRendered blocked on a FIFO attachment; want an immediate refusal")
	}

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending {
		t.Errorf("status = %q after a refused claim, want %q", step.Status, db.StepPending)
	}
	if step.Owner != "" || step.TokenHash != "" || step.ExpiresMS != 0 {
		t.Errorf("a refused claim left a lease: owner=%q token_hash set=%v expires=%d",
			step.Owner, step.TokenHash != "", step.ExpiresMS)
	}
	if step.Attempt != 0 {
		t.Errorf("attempt = %d after a refused claim, want 0", step.Attempt)
	}
	assertEventCount(t, conn, EventStepClaimed, 0)
}

// countingReader yields an endless run of bytes, counting every one handed out.
type countingReader struct{ consumed int }

func (r *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.consumed += len(p)
	return len(p), nil
}

// TestReadCappedAttachmentConsumesAtMostOnePastTheCap: the cap bounds the
// bytes READ, not only the bytes kept, so an oversized source costs at most
// AttachmentMaxBytes+1 bytes of memory before it refuses. The source stops
// after twice the cap so an unbounded read fails this test instead of hanging.
func TestReadCappedAttachmentConsumesAtMostOnePastTheCap(t *testing.T) {
	source := &countingReader{}
	_, err := readCappedAttachment(io.LimitReader(source, 2*AttachmentMaxBytes))
	if err == nil {
		t.Fatal("readCappedAttachment accepted a source past the cap; want a refusal")
	}
	if source.consumed > AttachmentMaxBytes+1 {
		t.Errorf("read %d bytes from an oversized source, want at most %d",
			source.consumed, AttachmentMaxBytes+1)
	}
}

// TestPacketRendersIssueFilesRefusesAnOverCapFile: an attachment one byte past
// AttachmentMaxBytes refuses, naming the path and the cap, rather than inlining
// the file or a truncated prefix of it.
func TestPacketRendersIssueFilesRefusesAnOverCapFile(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/huge.md", strings.Repeat("x", AttachmentMaxBytes+1))
	activateIssueFilesRun(t, conn, root, []string{"docs/huge.md"})

	_, err := RenderStep(conn, stepIDByInstance(t, conn, "transcribe@0"), "", nowMS)
	if err == nil {
		t.Fatal("RenderStep inlined an attachment over the cap; want a refusal")
	}
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Errorf("CodeOf(err) = %q, want %q", code, CodeValidation)
	}
	for _, want := range []string{"docs/huge.md", strconv.Itoa(AttachmentMaxBytes)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n  %v", want, err)
		}
	}
}

// TestPacketRendersIssueFilesCapIsTheGateCaptureCap: the per-file cap is the
// engine's gate-output capture cap, the bound the recorded cap decision names,
// so a file one byte past it refuses.
func TestPacketRendersIssueFilesCapIsTheGateCaptureCap(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/over.md", strings.Repeat("z", dexec.CaptureCap+1))
	activateIssueFilesRun(t, conn, root, []string{"docs/over.md"})

	_, err := RenderStep(conn, stepIDByInstance(t, conn, "transcribe@0"), "", nowMS)
	if err == nil {
		t.Fatalf("RenderStep inlined a %d-byte attachment, one past the gate "+
			"capture cap; want a refusal", dexec.CaptureCap+1)
	}
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Errorf("CodeOf(err) = %q, want %q", code, CodeValidation)
	}
}

// TestPacketRendersIssueFilesAtExactlyTheCap: a file of exactly
// AttachmentMaxBytes renders whole, with the sha256 of its full body.
func TestPacketRendersIssueFilesAtExactlyTheCap(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	body := strings.Repeat("y", AttachmentMaxBytes)
	writeAttachment(t, root, "docs/full.md", body)
	activateIssueFilesRun(t, conn, root, []string{"docs/full.md"})

	packet := renderTranscribe(t, conn)
	if !strings.Contains(packet, fileSection("docs/full.md", body)) {
		t.Errorf("packet lacks docs/full.md at exactly the cap with its full sha256")
	}
	if !strings.Contains(packet, body) {
		t.Errorf("packet does not carry docs/full.md's whole body")
	}
}

// TestPacketRendersIssueFilesEmptyAtActivation: an issue with no attachments
// freezes an EMPTY list, which is not the absent list of a pre-freeze snapshot,
// so a later attachment stays out of the run's packets.
func TestPacketRendersIssueFilesEmptyAtActivation(t *testing.T) {
	conn := mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/b.md", "late attachment b\n")
	issue, runID := activateIssueFilesRun(t, conn, root, nil)

	snapshot := runIssueOf(t, conn, runID, issue).IssueSnapshot
	if !strings.Contains(snapshot, `"files":[]`) {
		t.Errorf("issue_snapshot does not record an empty attachment list:\n  %s", snapshot)
	}

	testsupport.Must(t, db.AttachFiles(conn, issue, []string{"docs/b.md"}, ""),
		"AttachFiles after activation: %v", nil)

	packet := renderTranscribe(t, conn)
	if strings.Contains(packet, "== FILE docs/b.md") ||
		strings.Contains(packet, "late attachment b") {
		t.Errorf("packet inlines docs/b.md, attached after activation:\n%s", packet)
	}
}
