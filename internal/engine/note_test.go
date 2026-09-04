package engine

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1079: the run note — a statement recorded once against a run that every
// packet of the run renders from then on.
//
// The acceptance criteria, each falsified separately: a note added before
// dispatch appears VERBATIM in `step render` for EVERY step of the run; `step
// context` carries it as `notes`; and a run with no notes renders every bundle
// and packet byte-identically to before the field existed.

const noteText = "Gate `tests` fails on clean HEAD (routing_sweep_test.go): " +
	"pre-existing, tracked as DKT-1075; disposition override-pass.\n" +
	"Do not re-derive it and do not file a gap for it."

// noteRunSteps lists every step of a run, id and instance, so an assertion over
// "every packet of the run" enumerates the run rather than a hand-picked pair.
func noteRunSteps(t *testing.T, conn *sql.DB, runID int) map[int]string {
	t.Helper()
	rows, err := conn.Query(`SELECT id, instance FROM steps WHERE run_id = ? ORDER BY id`, runID)
	testsupport.Must(t, err, "listing steps: %v", err)
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var instance string
		err := rows.Scan(&id, &instance)
		testsupport.Must(t, err, "scanning a step: %v", err)
		out[id] = instance
	}
	testsupport.Must(t, rows.Err(), "iterating steps: %v", rows.Err())
	return out
}

func TestRunNoteRendersInEveryPacketOfTheRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	steps := noteRunSteps(t, conn, run.ID)
	if len(steps) < 2 {
		t.Fatalf("the fixture expanded %d step(s); this test needs several", len(steps))
	}

	// BEFORE: no `notes` member at all, so every bundle that predates the
	// field is byte-identical — the omitempty half of the acceptance.
	for id, instance := range steps {
		if strings.Contains(bundleJSON(t, conn, id), `"notes"`) {
			t.Errorf("%s's bundle carries a notes member before any note exists", instance)
		}
	}

	first, err := AddRunNote(conn, run.ID, noteText, nowMS)
	testsupport.Must(t, err, "AddRunNote: %v", err)
	second, err := AddRunNote(conn, run.ID, "Second ruling: also pre-existing.", nowMS+1)
	testsupport.Must(t, err, "AddRunNote (second): %v", err)
	if second.ID <= first.ID {
		t.Fatalf("note ids = %d then %d, want ascending", first.ID, second.ID)
	}

	for id, instance := range steps {
		// `step context`: the bundle carries both notes, in order, verbatim.
		bundle, err := ReadContext(conn, id, nowMS)
		testsupport.Must(t, err, "ReadContext(%s): %v", instance, err)
		if len(bundle.Notes) != 2 {
			t.Fatalf("%s's bundle carries %d note(s), want 2: %+v",
				instance, len(bundle.Notes), bundle.Notes)
		}
		if bundle.Notes[0] != *first || bundle.Notes[1] != *second {
			t.Errorf("%s's bundle notes = %+v, want [%+v %+v] verbatim and in order",
				instance, bundle.Notes, *first, *second)
		}

		// `step render`: the packet carries each note as its own section,
		// verbatim, beside the request.
		result, err := RenderStep(conn, id, "", nowMS)
		testsupport.Must(t, err, "RenderStep(%s): %v", instance, err)
		packet := result.Packet
		for _, n := range []*RunNote{first, second} {
			section := "\n== RUN NOTE " + itoa(n.ID) + "\n" + n.Text + "\n"
			if !strings.Contains(packet, section) {
				t.Errorf("%s's packet lacks note %d verbatim as its own section:\n%s",
					instance, n.ID, packet)
			}
		}
		request := strings.Index(packet, "== REQUEST")
		note := strings.Index(packet, "== RUN NOTE ")
		if request < 0 || note < request {
			t.Errorf("%s's packet renders the notes before the request (%d vs %d)",
				instance, note, request)
		}
		if strings.Contains(packet, "== FILE") && note > strings.Index(packet, "== FILE") {
			t.Errorf("%s's packet renders the notes after the packet files; they belong "+
				"beside the request", instance)
		}
	}
}

// TestRunNoteIsRunScoped: a note on one run reaches none of another run's
// packets — the run_id foreign key is the whole scope rule.
func TestRunNoteIsRunScoped(t *testing.T) {
	conn := mustDB(t)
	first, _ := activatedRun(t, conn)

	other := createIssue(t, conn, "another issue", "another body", "task", nil)
	second := startRun(t, conn, other)
	_, err := activate(conn, second.ID)
	testsupport.Must(t, err, "activating the second run: %v", err)

	_, err = AddRunNote(conn, first.ID, noteText, nowMS)
	testsupport.Must(t, err, "AddRunNote: %v", err)

	for id, instance := range noteRunSteps(t, conn, second.ID) {
		if strings.Contains(bundleJSON(t, conn, id), `"notes"`) {
			t.Errorf("%s (run %s) carries a note recorded against %s",
				instance, second.Ref(), first.Ref())
		}
	}
	for id := range noteRunSteps(t, conn, first.ID) {
		bundle, err := ReadContext(conn, id, nowMS)
		testsupport.Must(t, err, "ReadContext: %v", err)
		if len(bundle.Notes) != 1 {
			t.Fatalf("the noted run's own bundle carries %d note(s), want 1", len(bundle.Notes))
		}
	}
}

// TestRunNoteIsDeterministicAtFixedState: two reads at the same run state are
// byte-identical, and a note is RECORDED run state — the mid-run edits the
// snapshot discipline immunizes against still move nothing.
func TestRunNoteIsDeterministicAtFixedState(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	_, err := AddRunNote(conn, run.ID, noteText, nowMS)
	testsupport.Must(t, err, "AddRunNote: %v", err)
	stepID := stepIDByInstance(t, conn, "implement@0")

	before := bundleJSON(t, conn, stepID)
	execSQL(t, conn, `UPDATE issues SET description = ? WHERE id = ?`, "EDITED BODY", issue)
	after := bundleJSON(t, conn, stepID)
	if before != after {
		t.Errorf("a live issue edit moved a bundle carrying a note:\nbefore: %s\nafter:  %s",
			before, after)
	}

	r1, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	r2, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if r1.Packet != r2.Packet {
		t.Error("two renders at one run state differ")
	}
}

func TestRunNoteRefusals(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	cases := []struct {
		name string
		run  int
		text string
		code ErrorCode
		want string
	}{
		{"empty", run.ID, "", CodeValidation, "empty"},
		{"blank", run.ID, "  \n\t\n", CodeValidation, "empty"},
		{"oversized", run.ID, strings.Repeat("x", RunNoteMaxBytes+1), CodeValidation, "cap"},
		{"unknown run", run.ID + 99, noteText, CodeNotFound, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AddRunNote(conn, tc.run, tc.text, nowMS)
			if err == nil {
				t.Fatal("the note was accepted")
			}
			if code, ok := CodeOf(err); !ok || code != tc.code {
				t.Errorf("code = %v (%v), want %s: %v", code, ok, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not say %q", err, tc.want)
			}
		})
	}

	// Exactly at the cap is fine: the cap is a ceiling, not a strict bound.
	if _, err := AddRunNote(conn, run.ID, strings.Repeat("y", RunNoteMaxBytes), nowMS); err != nil {
		t.Errorf("a note exactly at the cap was refused: %v", err)
	}

	// A terminal run renders no more packets, so a note against it is CONFLICT.
	err := db.SetRunStatus(conn, run.ID, model.RunAbandoned, "test", nowMS)
	testsupport.Must(t, err, "abandoning the run: %v", err)
	_, err = AddRunNote(conn, run.ID, noteText, nowMS)
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("a note on an abandoned run: code = %v (%v), want CONFLICT: %v", code, ok, err)
	}
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Errorf("the refusal does not name the run's status: %v", err)
	}

	// Nothing above landed: the refusals left the note table as it was.
	notes, err := ListRunNotes(conn, run.ID)
	testsupport.Must(t, err, "ListRunNotes: %v", err)
	if len(notes) != 1 {
		t.Errorf("run_notes holds %d note(s) after the refusals, want the one at-cap note",
			len(notes))
	}
}

// TestRunNoteOnAPlanningRun: the motivating note is written BEFORE the first
// dispatch, and a planning run — not yet activated — must accept it and then
// render it once activated.
func TestRunNoteOnAPlanningRun(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	note, err := AddRunNote(conn, run.ID, noteText, nowMS)
	testsupport.Must(t, err, "AddRunNote on a planning run: %v", err)

	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	stepID := stepIDByInstance(t, conn, "implement@0")
	result, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if !strings.Contains(result.Packet, "== RUN NOTE "+itoa(note.ID)+"\n"+noteText) {
		t.Errorf("a note recorded before activation does not render after it:\n%s",
			result.Packet)
	}
}

func TestRunNoteIsEventLogged(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	note, err := AddRunNote(conn, run.ID, noteText, nowMS)
	testsupport.Must(t, err, "AddRunNote: %v", err)

	var runID int
	var data string
	err = conn.QueryRow(
		`SELECT run_id, data FROM events WHERE kind = ?`, EventRunNoteAdded,
	).Scan(&runID, &data)
	testsupport.Must(t, err, "reading the run-note-added event: %v", err)
	if runID != run.ID {
		t.Errorf("event run_id = %d, want %d", runID, run.ID)
	}
	var payload struct {
		Note int    `json:"note"`
		Text string `json:"text"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(data), &payload), "event data %q: %v", data, err)
	if payload.Note != note.ID || payload.Text != noteText {
		t.Errorf("event data = %s, want note %d with the text verbatim", data, note.ID)
	}

	actor, ok := ActorFor(EventRunNoteAdded)
	if !ok || actor != ActorHuman {
		t.Errorf("ActorFor(run-note-added) = %v/%v, want ActorHuman", actor, ok)
	}
}

func TestRunNoteMetaCountsNotes(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	without, err := ReadContext(conn, stepID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)
	_, err = AddRunNote(conn, run.ID, noteText, nowMS)
	testsupport.Must(t, err, "AddRunNote: %v", err)
	with, err := ReadContext(conn, stepID, nowMS)
	testsupport.Must(t, err, "ReadContext: %v", err)

	before, after := without.Meta(), with.Meta()
	if before.NotesBytes != 0 {
		t.Errorf("notes_bytes before any note = %d, want 0", before.NotesBytes)
	}
	if after.NotesBytes != len(noteText) {
		t.Errorf("notes_bytes = %d, want %d", after.NotesBytes, len(noteText))
	}
	if after.TotalBytes != before.TotalBytes+len(noteText) {
		t.Errorf("total_bytes grew by %d, want %d — a note rides every packet and "+
			"the cap must see it", after.TotalBytes-before.TotalBytes, len(noteText))
	}
}

// TestRunNoteDropsOneTrailingNewline: a file-fed note renders exactly as the
// same words passed inline, and nothing else about the text is touched.
func TestRunNoteDropsOneTrailingNewline(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	note, err := AddRunNote(conn, run.ID, "  keep my leading spaces\nand my blank line\n\n", nowMS)
	testsupport.Must(t, err, "AddRunNote: %v", err)
	if note.Text != "  keep my leading spaces\nand my blank line\n" {
		t.Errorf("stored text = %q, want exactly one trailing newline dropped", note.Text)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
