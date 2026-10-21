package engine

import (
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4269: `step annotate --integrated-sha` accepts a sha only when it carries
// the writer's patch, and re-pins issue.diff over the whole integrated range.

// rangeRepo builds a trunk with one commit and a writer branch of two commits
// touching different files, returning the repo, the trunk base, the writer's
// base (the trunk commit it forked from) and head.
func rangeRepo(t *testing.T) (repo, base, head string) {
	t.Helper()
	repo = gitRepo(t)
	base = gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-q", "-b", "writer")
	writeFile(t, repo, "internal/a.go", "package a\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "writer one")
	writeFile(t, repo, "internal/b.go", "package b\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "writer two")
	head = gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-q", "-")
	// A sibling lands first, so the picks are not a fast-forward.
	writeFile(t, repo, "internal/sibling.go", "package sibling\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "sibling")
	return repo, base, head
}

func statNames(t *testing.T, repo, from, to string) []string {
	t.Helper()
	names := strings.Fields(gitRun(t, repo, "diff", "--name-only", from, to))
	sort.Strings(names)
	return names
}

func patchFiles(body string) []string {
	var names []string
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "diff --git a/"); ok {
			name, _, _ := strings.Cut(rest, " b/")
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// TestIntegratedRangeCoversATwoCommitRangePick: a range cherry-pick carries
// every writer commit; the range starts at the trunk commit before the first
// pick and its patch lists every file the writer changed (the DKT-4112 packet
// omitted four of five).
func TestIntegratedRangeCoversATwoCommitRangePick(t *testing.T) {
	repo, base, head := rangeRepo(t)
	sibling := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "cherry-pick", "-x", base+".."+head)
	tip := gitRun(t, repo, "rev-parse", "HEAD")

	r := gitIntegratedRange(repo, tip, head, base)
	if !r.Known || !r.Carried {
		t.Fatalf("range pick = %+v; want known and carried", r)
	}
	if r.From != sibling {
		t.Errorf("range starts after %s; want the sibling %s, the trunk commit before the first pick", r.From, sibling)
	}
	body, err := GitRangePatch(repo, r.From, tip, nil)
	testsupport.Must(t, err, "GitRangePatch: %v", err)
	got, want := patchFiles(body), statNames(t, repo, r.From, tip)
	if strings.Join(got, ",") != strings.Join(want, ",") || len(got) != 2 {
		t.Errorf("range patch files = %v; want git diff --stat's %v", got, want)
	}
	// The last commit's own patch lists only one: the defect.
	last, err := GitCommitPatch(repo, tip, nil)
	testsupport.Must(t, err, "GitCommitPatch: %v", err)
	if n := len(patchFiles(last)); n != 1 {
		t.Fatalf("premise: the last pick alone lists %d files; want 1", n)
	}
}

// TestIntegratedRangeRefusesAShaLackingTheWriterPatch: the pre-pick HEAD (the
// DKT-4182 incident) and a partial pick are not carried, and name what is
// missing.
func TestIntegratedRangeRefusesAShaLackingTheWriterPatch(t *testing.T) {
	repo, base, head := rangeRepo(t)
	prePick := gitRun(t, repo, "rev-parse", "HEAD")

	r := gitIntegratedRange(repo, prePick, head, base)
	if !r.Known || r.Carried || len(r.Missing) != 2 {
		t.Errorf("pre-pick HEAD = %+v; want known, not carried, two missing", r)
	}

	first := gitRun(t, repo, "rev-parse", head+"~1")
	gitRun(t, repo, "cherry-pick", "-x", first)
	partial := gitRun(t, repo, "rev-parse", "HEAD")
	r = gitIntegratedRange(repo, partial, head, base)
	if !r.Known || r.Carried || len(r.Missing) != 1 {
		t.Errorf("partial pick = %+v; want known, not carried, one missing", r)
	}
}

// TestIntegratedRangeAcceptsAMergeAndASquash: a merge carries the chain
// itself, and a squash carries it as one commit.
func TestIntegratedRangeAcceptsAMergeAndASquash(t *testing.T) {
	repo, base, head := rangeRepo(t)
	sibling := gitRun(t, repo, "rev-parse", "HEAD")

	gitRun(t, repo, "checkout", "-q", "-b", "squashed", sibling)
	gitRun(t, repo, "cherry-pick", "-n", base+".."+head)
	gitRun(t, repo, "commit", "-q", "-m", "writer, squashed")
	squashTip := gitRun(t, repo, "rev-parse", "HEAD")
	r := gitIntegratedRange(repo, squashTip, head, base)
	if !r.Known || !r.Carried || r.From != sibling {
		t.Errorf("squash = %+v; want carried from the sibling %s", r, sibling)
	}

	gitRun(t, repo, "checkout", "-q", "-b", "merged", sibling)
	gitRun(t, repo, "merge", "-q", "--no-ff", "-m", "merge writer", head)
	mergeTip := gitRun(t, repo, "rev-parse", "HEAD")
	r = gitIntegratedRange(repo, mergeTip, head, base)
	if !r.Known || !r.Carried || r.From != base {
		t.Errorf("merge = %+v; want carried from the writer's base %s", r, base)
	}
}

// TestIntegratedRangeUnknownWithoutARepository: git that cannot answer is
// reported as unknown, never as carried.
func TestIntegratedRangeUnknownWithoutARepository(t *testing.T) {
	if r := gitIntegratedRange(t.TempDir(), "a", "b", "c"); r.Known || r.Carried {
		t.Errorf("no repository = %+v; want unknown", r)
	}
	if r := gitIntegratedRange("", "a", "b", "c"); r.Known {
		t.Errorf("no exec root = %+v; want unknown", r)
	}
}

// TestAnnotateRefusesAShaLackingTheWritersPatch is the acceptance check's
// refusal half: it exits nonzero, records nothing, and emits no
// issue-diff-repinned event.
func TestAnnotateRefusesAShaLackingTheWritersPatch(t *testing.T) {
	conn, e, runID := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return true, true }
	e.IntegrationRangeFn = func(_, tip, head, base string) IntegratedRange {
		return IntegratedRange{Known: true, Missing: []string{"50f45b6aaaaa"}}
	}
	stepID := stepIDByInstance(t, conn, "implement@0")
	before, err := stepLatestIssueDiff(conn, stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)

	_, err = e.AnnotateIntegration(conn, stepID, resolvedSHA, "", testBy, testConductorToken, nowMS+1)
	if err == nil || !strings.Contains(err.Error(), "does not carry the writer's patch") ||
		!strings.Contains(err.Error(), "50f45b6aaaaa") {
		t.Fatalf("annotate with a sha lacking the patch = %v; want a refusal naming the missing commit", err)
	}
	after, err := stepLatestIssueDiff(conn, stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
	if after.ID != before.ID {
		t.Errorf("a refused annotation superseded the record: %d -> %d", before.ID, after.ID)
	}
	if n := eventKindCount(t, conn, runID, EventIssueDiffRepinned); n != 0 {
		t.Errorf("%d issue-diff-repinned events after a refusal; want none", n)
	}
	if n := eventKindCount(t, conn, runID, EventStepAnnotated); n != 0 {
		t.Errorf("%d step-annotated events after a refusal; want none", n)
	}
}

// TestAnnotateRefusesWhenCarriageCannotBeEstablished: an unanswerable probe
// refuses, like an unanswerable ancestry question.
func TestAnnotateRefusesWhenCarriageCannotBeEstablished(t *testing.T) {
	conn, e, _ := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return true, true }
	e.IntegrationRangeFn = func(_, _, _, _ string) IntegratedRange { return IntegratedRange{} }

	_, err := e.AnnotateIntegration(conn, stepIDByInstance(t, conn, "implement@0"),
		resolvedSHA, "", testBy, testConductorToken, nowMS+1)
	if err == nil || !strings.Contains(err.Error(), "could not establish") {
		t.Fatalf("annotate with an unanswerable probe = %v; want a refusal", err)
	}
}

// TestAnnotateRepinsOverTheIntegratedRange is the acceptance check's success
// half: the re-pinned issue.diff is the range patch, and its record carries the
// writer chain so a later annotation judges the same chain.
func TestAnnotateRepinsOverTheIntegratedRange(t *testing.T) {
	conn, e, _ := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return true, true }
	const rangeFrom = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e.IntegrationRangeFn = func(_, _, _, _ string) IntegratedRange {
		return IntegratedRange{Known: true, Carried: true, From: rangeFrom}
	}
	e.RangePatchFn = func(_, from, to string, _ []string) (string, error) {
		return "range patch " + from + ".." + to, nil
	}
	e.CommitPatchFn = func(_, sha string, _ []string) (string, error) {
		return "single commit patch of " + sha, nil
	}
	stepID := stepIDByInstance(t, conn, "implement@0")

	_, err := e.AnnotateIntegration(conn, stepID, resolvedSHA, "", testBy, testConductorToken, nowMS+1)
	testsupport.Must(t, err, "annotate: %v", err)

	latest, err := stepLatestIssueDiff(conn, stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
	if want := "range patch " + rangeFrom + ".." + resolvedSHA; latest.Body != want {
		t.Errorf("re-pinned body = %q; want the range patch %q", latest.Body, want)
	}
	for _, want := range []string{`"writer_head":"deadbeef00"`, `"range_from":"` + rangeFrom + `"`} {
		if !strings.Contains(latest.Payload, want) {
			t.Errorf("payload %s lacks %s", latest.Payload, want)
		}
	}
}
