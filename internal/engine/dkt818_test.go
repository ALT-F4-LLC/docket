package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-818 — the "not pinned" refusal named a remedy that was already
// satisfied. On RUN-59 a repin adopted contract bytes reaching two fragments
// the run had never snapshotted, and every judge claim then died with "add it
// under an instance-config root and start a new run" — while BOTH fragments sat
// under `~/.docket/config/fragments/`. The conductor went looking for a missing
// file, found it present, and had to re-derive the real cause: the pin set, not
// the filesystem. These tests hold the refusal to a TRUE sentence about which of
// the two unpinned causes it actually has.
//
// The fixture is DKT-804's shape, which is the one way a live run still reaches
// an unpinned packet file at render time: a `{executor}` entry resolved to a
// hint whose contract activation never pinned. Whether that contract exists on
// disk is the whole variable under test.

// TestUnpinnedButPresentPacketFileNamesThePinSetNotTheFilesystem is RUN-59's
// half: the file IS on disk, so the old remedy was a dead end. The refusal must
// name the run's pin set, say the file is present and where, and offer the
// remedy that actually applies.
func TestUnpinnedButPresentPacketFileNamesThePinSetNotTheFilesystem(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/{executor}.md\"]\n")
	// The DECLARED hint's contract is what activation pins (DKT-581's
	// closure). The resolved hint's contract is written too — present under an
	// instance-config root, and still outside the run's frozen pin set.
	writeConfigFile(t, configDir, "contracts/w.md", "the declared contract\n")
	writeConfigFile(t, configDir, "contracts/rogue.md", "the resolved contract\n")

	issue := createIssue(t, conn, "present but unpinned", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")
	_, _, err = NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	}, "", "rogue")
	if err == nil {
		t.Fatal("a claim whose packet references an unpinned file succeeded")
	}
	// The DISPOSITION is unchanged — an unpinned entry has no snapshot, and
	// only the sentence explaining it moves.
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("code = %q, want %q", code, CodeValidation)
	}

	full := filepath.Join(configDir, "contracts/rogue.md")
	for _, want := range []string{
		"contracts/rogue.md",
		"is not in " + run.Ref() + "'s pin set",
		"froze at activation",
		full,
		"start a new run to pin it",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to say %q", err.Error(), want)
		}
	}
	// The RUN-59 defect itself: the refusal must not send anyone to the
	// filesystem for a file that is already sitting there.
	if strings.Contains(err.Error(), "add it under an instance-config root") {
		t.Errorf("err = %q still names the already-satisfied remedy", err.Error())
	}
}

// TestUnpinnedAndAbsentPacketFileStillSaysToAddIt is the other half: nothing
// wrote the file, so writing it IS the remedy and the message must keep saying
// so. Splitting the sentence must not cost the honest case its answer.
func TestUnpinnedAndAbsentPacketFileStillSaysToAddIt(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/{executor}.md\"]\n")
	writeConfigFile(t, configDir, "contracts/w.md", "the declared contract\n")

	issue := createIssue(t, conn, "absent and unpinned", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")
	_, _, err = NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	}, "", "rogue")
	if err == nil {
		t.Fatal("a claim whose packet references an absent file succeeded")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("code = %q, want %q", code, CodeValidation)
	}
	for _, want := range []string{
		"contracts/rogue.md",
		"is not in " + run.Ref() + "'s pin set",
		"resolves under no instance-config root",
		"add it under one and start a new run to pin it",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to say %q", err.Error(), want)
		}
	}
}
