package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The mid-run attachment-list refresh: a detach recorded through `issue file
// remove` reaches a live run's packets only when an operator re-freezes the
// list, and the re-freeze leaves one dated, attributed event behind.

// twoAttachmentRun activates the attachment workflow with docs/a.md and
// docs/b.md attached and both written to the run's checkout.
func twoAttachmentRun(t *testing.T) (conn *sql.DB, issue, runID int) {
	t.Helper()
	conn = mustDB(t)
	root := issueFilesCheckout(t)
	writeAttachment(t, root, "docs/a.md", "attachment a\n")
	writeAttachment(t, root, "docs/b.md", "attachment b\n")
	issue, runID = activateIssueFilesRun(t, conn, root,
		[]string{"docs/a.md", "docs/b.md"})
	return conn, issue, runID
}

func detach(t *testing.T, conn *sql.DB, issue int, paths ...string) {
	t.Helper()
	testsupport.Must(t, db.DetachFiles(conn, issue, paths, ""), "DetachFiles: %v", nil)
}

func refreshFilesAsConductor(conn *sql.DB, runID, issue int, reason string) (*RefreshedFiles, error) {
	if err := seatTestConductor(conn, runID); err != nil {
		return nil, err
	}
	return RefreshIssueFilesInRun(
		conn, runID, issue, reason, testBy, testConductorToken, nowMS)
}

func filesRefreshEvents(t *testing.T, conn *sql.DB, runID int) []string {
	t.Helper()
	rows, err := conn.Query(
		`SELECT data FROM events WHERE kind = ? AND run_id = ? ORDER BY rowid`,
		EventIssueFilesRefreshed, runID)
	testsupport.Must(t, err, "reading files-refresh events: %v", err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var data string
		testsupport.Must(t, rows.Scan(&data), "scanning event: %v", nil)
		out = append(out, data)
	}
	testsupport.Must(t, rows.Err(), "iterating events: %v", nil)
	return out
}

func frozenFiles(t *testing.T, conn *sql.DB, runID, issue int) []string {
	t.Helper()
	var fields issueSnapshotFields
	blob := snapshotBlob(t, conn, runID, issue)
	testsupport.Must(t, json.Unmarshal([]byte(blob), &fields), "decoding snapshot: %v", nil)
	if fields.Files == nil {
		t.Fatalf("snapshot carries no files key: %s", blob)
	}
	return *fields.Files
}

// TestRefreshIssueFilesReachesTheNextPacket: after a detach, the packet keeps
// the frozen list until the refresh, and omits the detached path after it.
func TestRefreshIssueFilesReachesTheNextPacket(t *testing.T) {
	conn, issue, runID := twoAttachmentRun(t)
	before := snapshotBlob(t, conn, runID, issue)
	detach(t, conn, issue, "docs/b.md")

	if packet := renderTranscribe(t, conn); !strings.Contains(packet,
		fileSection("docs/b.md", "attachment b\n")) {
		t.Fatalf("without a refresh the detach reached the packet:\n%s", packet)
	}

	got, err := refreshFilesAsConductor(conn, runID, issue, "operator detached b")
	testsupport.Must(t, err, "RefreshIssueFilesInRun: %v", err)
	if !slices.Equal(got.From, []string{"docs/a.md", "docs/b.md"}) ||
		!slices.Equal(got.To, []string{"docs/a.md"}) {
		t.Errorf("result from/to = %v/%v, want [docs/a.md docs/b.md]/[docs/a.md]",
			got.From, got.To)
	}

	if files := frozenFiles(t, conn, runID, issue); !slices.Equal(files, []string{"docs/a.md"}) {
		t.Errorf("snapshot files = %v, want [docs/a.md]", files)
	}

	var was, now issueSnapshotFields
	testsupport.Must(t, json.Unmarshal([]byte(before), &was), "decoding: %v", nil)
	testsupport.Must(t, json.Unmarshal([]byte(snapshotBlob(t, conn, runID, issue)), &now),
		"decoding: %v", nil)
	was.Files, now.Files = nil, nil
	if fmtJSON(was) != fmtJSON(now) {
		t.Errorf("refresh moved more than files:\n before %s\n after  %s",
			fmtJSON(was), fmtJSON(now))
	}

	packet := renderTranscribe(t, conn)
	if !strings.Contains(packet, fileSection("docs/a.md", "attachment a\n")) {
		t.Errorf("refreshed packet lost docs/a.md:\n%s", packet)
	}
	if strings.Contains(packet, "== FILE docs/b.md") || strings.Contains(packet, "attachment b") {
		t.Errorf("refreshed packet still inlines the detached docs/b.md:\n%s", packet)
	}
}

// TestRefreshIssueFilesRecordsOneHumanEvent: one event, attributed to a
// person, carrying the frozen list, the live list, and the reason.
func TestRefreshIssueFilesRecordsOneHumanEvent(t *testing.T) {
	conn, issue, runID := twoAttachmentRun(t)
	detach(t, conn, issue, "docs/b.md")

	_, err := refreshFilesAsConductor(conn, runID, issue, "operator detached b")
	testsupport.Must(t, err, "RefreshIssueFilesInRun: %v", err)

	events := filesRefreshEvents(t, conn, runID)
	if len(events) != 1 {
		t.Fatalf("%d %s events, want exactly 1", len(events), EventIssueFilesRefreshed)
	}
	if actor, ok := ActorFor(EventIssueFilesRefreshed); !ok || actor != ActorHuman {
		t.Errorf("ActorFor(%s) = %q/%v, want human", EventIssueFilesRefreshed, actor, ok)
	}
	var payload struct {
		From   []string `json:"from"`
		To     []string `json:"to"`
		Reason string   `json:"reason"`
		Actor  string   `json:"actor"`
		Cwd    string   `json:"cwd"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(events[0]), &payload), "decoding: %v", nil)
	if !slices.Equal(payload.From, []string{"docs/a.md", "docs/b.md"}) ||
		!slices.Equal(payload.To, []string{"docs/a.md"}) ||
		payload.Reason != "operator detached b" {
		t.Errorf("event payload = %s, want from [a b], to [a], the reason", events[0])
	}
	if payload.Actor != testBy.Actor || payload.Cwd != testBy.Cwd {
		t.Errorf("event actor/cwd = %q/%q, want %q/%q",
			payload.Actor, payload.Cwd, testBy.Actor, testBy.Cwd)
	}
}

// TestRefreshIssueFilesRefuses: each refusal names its cause and writes
// nothing — the snapshot blob is byte-identical and no event is recorded.
func TestRefreshIssueFilesRefuses(t *testing.T) {
	type refusal struct {
		name    string
		arrange func(t *testing.T, conn *sql.DB, issue, runID int)
		reason  string
		code    ErrorCode
		wants   []string
	}
	cases := []refusal{
		{
			name:    "live list equals the frozen list",
			arrange: func(*testing.T, *sql.DB, int, int) {},
			reason:  "nothing changed",
			code:    CodeConflict,
		},
		{
			name: "the run froze no snapshot for the issue",
			arrange: func(t *testing.T, conn *sql.DB, issue, runID int) {
				mustExec(t, conn,
					`UPDATE run_issues SET issue_snapshot = '' WHERE run_id = ? AND issue_id = ?`,
					runID, issue)
				detach(t, conn, issue, "docs/b.md")
			},
			reason: "detached b",
			code:   CodeConflict,
		},
		{
			name: "the snapshot predates the frozen list",
			arrange: func(t *testing.T, conn *sql.DB, issue, runID int) {
				mustExec(t, conn,
					`UPDATE run_issues SET issue_snapshot = ? WHERE run_id = ? AND issue_id = ?`,
					`{"title":"transcribe the attachments","kind":"task","labels":[],"scope":[]}`,
					runID, issue)
				detach(t, conn, issue, "docs/b.md")
			},
			reason: "detached b",
			code:   CodeConflict,
		},
		{
			name: "no reason",
			arrange: func(t *testing.T, conn *sql.DB, issue, _ int) {
				detach(t, conn, issue, "docs/b.md")
			},
			reason: "  ",
			code:   CodeValidation,
		},
		{
			name: "every step of the issue is terminal",
			arrange: func(t *testing.T, conn *sql.DB, issue, runID int) {
				detach(t, conn, issue, "docs/b.md")
				mustExec(t, conn,
					`UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ?`,
					db.StepDone, runID, issue)
			},
			reason: "detached b",
			code:   CodeConflict,
			wants:  []string{"every step", "terminal", "NEXT run"},
		},
		{
			name: "a dispatch is open",
			arrange: func(t *testing.T, conn *sql.DB, issue, runID int) {
				detach(t, conn, issue, "docs/b.md")
				mustExec(t, conn,
					`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, created_at_ms)
					 VALUES (?, 'open', 0, ?, ?)`, runID, nowMS+60_000, nowMS)
			},
			reason: "detached b",
			code:   CodeConflict,
			wants:  []string{"DISPATCH-"},
		},
	}
	for _, status := range []string{db.StepClaimed, db.StepRunning, db.StepGated} {
		cases = append(cases, refusal{
			name: "a step is " + status,
			arrange: func(t *testing.T, conn *sql.DB, issue, _ int) {
				detach(t, conn, issue, "docs/b.md")
				mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
					status, stepIDByInstance(t, conn, "transcribe@0"))
			},
			reason: "detached b",
			code:   CodeConflict,
			wants:  []string{"transcribe@0", status},
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, issue, runID := twoAttachmentRun(t)
			tc.arrange(t, conn, issue, runID)
			before := snapshotBlob(t, conn, runID, issue)

			_, err := refreshFilesAsConductor(conn, runID, issue, tc.reason)
			if code, ok := CodeOf(err); !ok || code != tc.code {
				t.Fatalf("error = %v (code %v), want %s", err, code, tc.code)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q:\n%s", want, err)
				}
			}
			if after := snapshotBlob(t, conn, runID, issue); after != before {
				t.Errorf("refused refresh rewrote the snapshot:\n before %s\n after  %s",
					before, after)
			}
			if events := filesRefreshEvents(t, conn, runID); len(events) != 0 {
				t.Errorf("refused refresh recorded %d event(s): %v", len(events), events)
			}
		})
	}
}

// TestRefreshIssueFilesRequiresTheConductorCapability: on a bound run the
// refresh takes the conductor token, and a refusal writes nothing.
func TestRefreshIssueFilesRequiresTheConductorCapability(t *testing.T) {
	call := func(t *testing.T, token string) ([]string, []string, error) {
		t.Helper()
		conn, issue, runID := twoAttachmentRun(t)
		hash, err := db.RunConductorHash(conn, runID)
		testsupport.Must(t, err, "reading the conductor hash: %v", err)
		if hash == "" {
			t.Fatal("premise: activation minted no conductor capability")
		}
		detach(t, conn, issue, "docs/b.md")
		err = func() error {
			_, err := RefreshIssueFilesInRun(
				conn, runID, issue, "detached b", testBy, token, nowMS)
			return err
		}()
		return frozenFiles(t, conn, runID, issue), filesRefreshEvents(t, conn, runID), err
	}

	t.Run("no token", func(t *testing.T) {
		files, events, err := call(t, "")
		if code, ok := CodeOf(err); !ok || code != CodeValidation {
			t.Fatalf("error = %v (code %v), want VALIDATION_ERROR", err, code)
		}
		if !strings.Contains(err.Error(), "conductor capability") {
			t.Errorf("refusal does not name the conductor capability: %v", err)
		}
		if !slices.Equal(files, []string{"docs/a.md", "docs/b.md"}) || len(events) != 0 {
			t.Errorf("refused refresh wrote: files %v, events %v", files, events)
		}
	})

	t.Run("a token that is not the run's", func(t *testing.T) {
		files, events, err := call(t, "an-executors-lease-token")
		if code, ok := CodeOf(err); !ok || code != CodeAuth {
			t.Fatalf("error = %v (code %v), want AUTH_ERROR", err, code)
		}
		if !errors.Is(err, ErrNotConductor) {
			t.Errorf("error %v does not wrap ErrNotConductor", err)
		}
		if !slices.Equal(files, []string{"docs/a.md", "docs/b.md"}) || len(events) != 0 {
			t.Errorf("refused refresh wrote: files %v, events %v", files, events)
		}
	})
}
