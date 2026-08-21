package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DKT-405 item 1: the abandon's worktree warning is read from `steps.work_root`
// and used to be printed unqualified. Abandoning RUN-14 flagged 20 outstanding
// worktrees that `git worktree list` showed were already swept — the recorded
// rows outlive the directories a relay removed at close time. The warning now
// stats what it is about.

// seedWorktrees makes `present` real directories under a temp root and returns
// them alongside `absent` paths under the same root that were never created.
func seedWorktrees(t *testing.T, present, absent int) (all, live, gone []string) {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < present; i++ {
		p := filepath.Join(root, "live", string(rune('a'+i)))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("creating %s: %v", p, err)
		}
		all, live = append(all, p), append(live, p)
	}
	for i := 0; i < absent; i++ {
		p := filepath.Join(root, "swept", string(rune('a'+i)))
		all, gone = append(all, p), append(gone, p)
	}
	return all, live, gone
}

// TestWorktreeNoticeSplitsPresentFromSwept is the acceptance criterion: a
// mixed recorded list names only the checkouts that are still there, and says
// how many of the rest are already gone.
func TestWorktreeNoticeSplitsPresentFromSwept(t *testing.T) {
	all, live, gone := seedWorktrees(t, 2, 3)

	notice := worktreeNotice(all, "this run's steps")

	for _, p := range live {
		if !strings.Contains(notice, p) {
			t.Errorf("a worktree that IS on disk is missing from the warning:\n  %s\n%s", p, notice)
		}
	}
	for _, p := range gone {
		if strings.Contains(notice, p) {
			t.Errorf("an already-swept worktree is still listed as outstanding:\n  %s\n%s", p, notice)
		}
	}
	if !strings.Contains(notice, "still on disk") {
		t.Errorf("the outstanding list does not say the paths were checked:\n%s", notice)
	}
	if !strings.Contains(notice, "3 further recorded worktree(s) are already gone") {
		t.Errorf("the swept ones are not accounted for, so the list reads as "+
			"truncated:\n%s", notice)
	}
}

// TestWorktreeNoticeAllSweptSaysNothingToDo is the RUN-14 case exactly: every
// recorded path is gone, and the warning must not send a reader hunting.
func TestWorktreeNoticeAllSweptSaysNothingToDo(t *testing.T) {
	all, _, _ := seedWorktrees(t, 0, 20)

	notice := worktreeNotice(all, "this run's steps")

	if strings.Contains(notice, "Outstanding worktrees") {
		t.Errorf("nothing is on disk, and the warning still says outstanding:\n%s", notice)
	}
	if !strings.Contains(notice, "all 20 recorded") ||
		!strings.Contains(notice, "nothing to sweep") {
		t.Errorf("the warning does not say the recorded paths are already gone:\n%s", notice)
	}
	for _, line := range strings.Split(notice, "\n") {
		if strings.HasPrefix(line, "  /") {
			t.Errorf("a swept path is listed for the reader to act on: %q", line)
		}
	}
}

// TestWorktreeNoticeAllPresentIsTheOldWarning: when the debris is real, the
// message is the DKT-116 one — a list, and no talk of removals.
func TestWorktreeNoticeAllPresentIsTheOldWarning(t *testing.T) {
	all, live, _ := seedWorktrees(t, 2, 0)

	notice := worktreeNotice(all, "the issue's steps")

	if !strings.Contains(notice, "Outstanding worktrees") {
		t.Fatalf("real debris is not reported as outstanding:\n%s", notice)
	}
	if !strings.Contains(notice, "the issue's steps") {
		t.Errorf("the warning does not say whose steps recorded them:\n%s", notice)
	}
	for _, p := range live {
		if !strings.Contains(notice, p) {
			t.Errorf("%s is missing from the warning:\n%s", p, notice)
		}
	}
	if strings.Contains(notice, "already gone") {
		t.Errorf("nothing was swept and the warning claims something was:\n%s", notice)
	}
}

// TestWorktreeNoticeSaysNothingWhenNothingWasRecorded keeps the pre-DKT-116
// silence for a run whose steps never declared a worktree.
func TestWorktreeNoticeSaysNothingWhenNothingWasRecorded(t *testing.T) {
	if notice := worktreeNotice(nil, "this run's steps"); notice != "" {
		t.Errorf("worktreeNotice(nil) = %q, want the empty tail", notice)
	}
}

// TestUnstattableWorktreeCountsAsPresent: only "does not exist" supports the
// claim that there is nothing to clean up. A path docket could not look at is
// reported as outstanding, because a failure to look is not an absence.
func TestUnstattableWorktreeCountsAsPresent(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "no-search")
	if err := os.MkdirAll(filepath.Join(blocked, "wf_a"), 0o755); err != nil {
		t.Fatalf("creating the fixture: %v", err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("removing search permission: %v", err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o755) })

	path := filepath.Join(blocked, "wf_a")
	if _, err := os.Stat(path); err == nil {
		t.Skip("this filesystem (or this uid) stats through a 0000 directory")
	}

	present, removed := splitRecordedWorktrees([]string{path})
	if len(removed) != 0 {
		t.Errorf("an unstattable path was reported as already removed: %v", removed)
	}
	if len(present) != 1 {
		t.Errorf("present = %v, want the unstattable path kept as outstanding", present)
	}
}
