package engine

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
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
		out, err := c.CombinedOutput()
		testsupport.Must(t, err, "git %v: %v\n%s", cmd, err, out)
	}
	return root
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
// recorded exec root is `root`.
func activateIssueFilesRun(t *testing.T, conn *sql.DB, root string, paths []string) {
	t.Helper()
	registerSource(t, conn, []byte(issueFilesWorkflowSrc), "attachment-transcribe.toml")
	issue := createIssue(t, conn, "transcribe the attachments", "the issue body", "task", nil)
	testsupport.Must(t, db.AttachFiles(conn, issue, paths, ""), "AttachFiles: %v", nil)

	run, err := db.InsertRunWithContext(conn, 1, "attachment run", 0, nowMS,
		db.RunContext{ExecRoot: root})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", nil)

	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
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
