package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-1079 at the CLI boundary: `run note add` records the note and reports
// it on both channels, `run note list` reads it back, and the verb refuses a
// note with no source (or two) naming the flags. WHICH packets a note then
// renders in is the engine's to prove (internal/engine/note_test.go); what
// this file asserts is the verb.

// runNoteCmdWithDB answers --authority operator, so a test that is not about
// authority states one; runNoteCmdNoAuthority leaves it unanswered.
func runNoteCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := runNoteCmdNoAuthority(conn)
	_ = cmd.Flags().Set("authority", engine.AuthorityOperator)
	return cmd
}

func runNoteCmdNoAuthority(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("text", "", "")
	cmd.Flags().String("file", "", "")
	cmd.Flags().String("authority", "", "")
	return cmd
}

func TestRunNoteAddCLIRecordsAndReports(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)

	w, buf := bufWriter(true)
	cmd := runNoteCmdWithDB(conn)
	err := cmd.Flags().Set("text", "tests fails on clean HEAD; tracked as DKT-1075; override-pass")
	testsupport.Must(t, err, "setting --text: %v", err)
	err = runRunNoteAdd(cmd, runRef, w)
	testsupport.Must(t, err, "run note add: %v", err)

	var env struct {
		Data struct {
			Run  string         `json:"run"`
			Note engine.RunNote `json:"note"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Data.Run != runRef || env.Data.Note.ID == 0 ||
		env.Data.Note.Text != "tests fails on clean HEAD; tracked as DKT-1075; override-pass" ||
		env.Data.Note.RecordedAtMS == 0 {
		t.Fatalf("payload = %+v, want the note on %s with a real id and timestamp", env.Data, runRef)
	}

	// The row really landed, run-scoped, and its event beside it.
	var count int
	err = conn.QueryRow(`SELECT COUNT(*) FROM run_notes WHERE run_id = ?`, runID).Scan(&count)
	testsupport.Must(t, err, "counting notes: %v", err)
	if count != 1 {
		t.Errorf("run_notes rows = %d, want 1", count)
	}
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = 'run-note-added'`, runID,
	).Scan(&count)
	testsupport.Must(t, err, "counting events: %v", err)
	if count != 1 {
		t.Errorf("run-note-added events = %d, want 1", count)
	}
}

func TestRunNoteAddCLIHumanMessageNamesTheNote(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)

	w, buf := bufWriter(false)
	cmd := runNoteCmdWithDB(conn)
	err := cmd.Flags().Set("text", "a ruling")
	testsupport.Must(t, err, "setting --text: %v", err)
	err = runRunNoteAdd(cmd, runRef, w)
	testsupport.Must(t, err, "run note add: %v", err)

	out := buf.String()
	if !strings.Contains(out, runRef) || !strings.Contains(out, "every packet") {
		t.Errorf("the human message does not name the run and what the note does:\n%s", out)
	}
}

func TestRunNoteAddCLIReadsAFile(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	path := filepath.Join(t.TempDir(), "note.txt")
	err := os.WriteFile(path, []byte("line one\nline two\n"), 0o644)
	testsupport.Must(t, err, "writing the note file: %v", err)

	w, buf := bufWriter(true)
	cmd := runNoteCmdWithDB(conn)
	err = cmd.Flags().Set("file", path)
	testsupport.Must(t, err, "setting --file: %v", err)
	err = runRunNoteAdd(cmd, model.FormatRunID(runID), w)
	testsupport.Must(t, err, "run note add --file: %v", err)

	var env struct {
		Data struct {
			Note engine.RunNote `json:"note"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	// One trailing newline dropped, the rest verbatim.
	if env.Data.Note.Text != "line one\nline two" {
		t.Errorf("note text = %q, want the file's contents less one trailing newline",
			env.Data.Note.Text)
	}
}

func TestRunNoteAddCLIRefusesNoSourceOrTwo(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)

	w, _ := bufWriter(true)
	err := runRunNoteAdd(runNoteCmdWithDB(conn), runRef, w)
	if err == nil {
		t.Fatal("a note with no --text and no --file was accepted")
	}
	if !strings.Contains(err.Error(), "--text") || !strings.Contains(err.Error(), "--file") {
		t.Errorf("the refusal %q does not name both flags", err)
	}
	var ce *CmdError
	if !asCmdErr(err, &ce) || ce.Code != output.ErrValidation {
		t.Errorf("the refusal is not VALIDATION_ERROR: %v", err)
	}

	cmd := runNoteCmdWithDB(conn)
	_ = cmd.Flags().Set("text", "x")
	_ = cmd.Flags().Set("file", "y")
	err = runRunNoteAdd(cmd, runRef, w)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("both flags at once: err = %v, want a refusal naming the conflict", err)
	}

	var count int
	err = conn.QueryRow(`SELECT COUNT(*) FROM run_notes`).Scan(&count)
	testsupport.Must(t, err, "counting notes: %v", err)
	if count != 0 {
		t.Errorf("run_notes rows after two refusals = %d, want 0", count)
	}
}

func TestRunNoteAddCLIMapsEngineCodes(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)

	// Unknown run: NOT_FOUND.
	w, _ := bufWriter(true)
	cmd := runNoteCmdWithDB(conn)
	_ = cmd.Flags().Set("text", "a ruling")
	err := runRunNoteAdd(cmd, model.FormatRunID(runID+99), w)
	var ce *CmdError
	if !asCmdErr(err, &ce) || ce.Code != output.ErrNotFound {
		t.Errorf("unknown run: err = %v, want NOT_FOUND", err)
	}

	// Empty text: VALIDATION_ERROR from the engine, mapped.
	cmd = runNoteCmdWithDB(conn)
	_ = cmd.Flags().Set("text", "   ")
	err = runRunNoteAdd(cmd, model.FormatRunID(runID), w)
	if !asCmdErr(err, &ce) || ce.Code != output.ErrValidation {
		t.Errorf("blank note: err = %v, want VALIDATION_ERROR", err)
	}
}

// runNoteWrites counts a run's note rows and run-note-added events, the two
// writes a refused `run note add` must leave untouched.
func runNoteWrites(t *testing.T, conn *sql.DB, runID int) (notes, events int) {
	t.Helper()
	err := conn.QueryRow(`SELECT COUNT(*) FROM run_notes WHERE run_id = ?`, runID).Scan(&notes)
	testsupport.Must(t, err, "counting notes: %v", err)
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = 'run-note-added'`, runID,
	).Scan(&events)
	testsupport.Must(t, err, "counting events: %v", err)
	return notes, events
}

// TestRunNoteAddCLIRequiresTheConductorCapability: on a bound run the verb
// reads the capability from DOCKET_TOKEN and refuses without it or with any
// other token, writing nothing; an unbound run needs none.
func TestRunNoteAddCLIRequiresTheConductorCapability(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)
	capability := seatConductor(t, conn, runID)

	addNote := func() error {
		cmd := runNoteCmdWithDB(conn)
		testsupport.Must(t, cmd.Flags().Set("text", "a ruling"), "setting --text: %v", nil)
		w, _ := bufWriter(true)
		return runRunNoteAdd(cmd, runRef, w)
	}
	refused := func(name string, want output.ErrorCode) {
		t.Helper()
		notesBefore, eventsBefore := runNoteWrites(t, conn, runID)
		assertCmdCode(t, addNote(), want, name)
		notesAfter, eventsAfter := runNoteWrites(t, conn, runID)
		if notesAfter != notesBefore || eventsAfter != eventsBefore {
			t.Errorf("%s wrote: notes %d -> %d, events %d -> %d",
				name, notesBefore, notesAfter, eventsBefore, eventsAfter)
		}
	}

	withStdin(t, "")
	t.Setenv(TokenEnvVar, "")
	refused("no token", output.ErrValidation)
	t.Setenv(TokenEnvVar, "deadbeef")
	refused("a wrong token", output.ErrAuth)

	t.Setenv(TokenEnvVar, capability)
	testsupport.Must(t, addNote(), "run note add with the capability: %v", nil)
	if notes, events := runNoteWrites(t, conn, runID); notes != 1 || events != 1 {
		t.Errorf("notes = %d, events = %d after the authorized add; want 1 and 1", notes, events)
	}

	t.Run("an unbound run", func(t *testing.T) {
		unbound, _ := seedRun(t, conn)
		t.Setenv(TokenEnvVar, "")
		cmd := runNoteCmdWithDB(conn)
		testsupport.Must(t, cmd.Flags().Set("text", "a ruling"), "setting --text: %v", nil)
		w, _ := bufWriter(true)
		err := runRunNoteAdd(cmd, model.FormatRunID(unbound), w)
		testsupport.Must(t, err, "run note add on an unbound run: %v", err)
		if notes, events := runNoteWrites(t, conn, unbound); notes != 1 || events != 1 {
			t.Errorf("notes = %d, events = %d; want 1 and 1", notes, events)
		}
	})
}

// TestRunNoteAddCLIStdinNoteTakesTokenOnlyFromEnv: under --file - stdin is
// the note, never a token, so the capability comes only from DOCKET_TOKEN.
// withStdin backs stdin with a file rather than a pipe, as elsewhere in the
// suite, so a read to EOF cannot hang the test.
func TestRunNoteAddCLIStdinNoteTakesTokenOnlyFromEnv(t *testing.T) {
	const piped = "tests fails on clean HEAD; override-pass"
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)
	capability := seatConductor(t, conn, runID)

	addFromStdin := func() (*bytes.Buffer, error) {
		withStdin(t, piped)
		cmd := runNoteCmdWithDB(conn)
		testsupport.Must(t, cmd.Flags().Set("file", "-"), "setting --file: %v", nil)
		w, buf := bufWriter(true)
		return buf, runRunNoteAdd(cmd, runRef, w)
	}

	t.Run("DOCKET_TOKEN holds the capability", func(t *testing.T) {
		t.Setenv(TokenEnvVar, capability)
		buf, err := addFromStdin()
		testsupport.Must(t, err, "run note add --file -: %v", err)
		var env struct {
			Data struct {
				Note engine.RunNote `json:"note"`
			} `json:"data"`
		}
		testsupport.Must(t, json.Unmarshal(buf.Bytes(), &env), "unmarshal: %v\n%s", nil, buf.String())
		if env.Data.Note.Text != piped {
			t.Errorf("recorded note = %q, want the piped text %q", env.Data.Note.Text, piped)
		}
	})

	t.Run("DOCKET_TOKEN unset", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "")
		notesBefore, eventsBefore := runNoteWrites(t, conn, runID)
		_, err := addFromStdin()
		assertCmdCode(t, err, output.ErrValidation, "run note add --file - with no DOCKET_TOKEN")
		// The capability refusal, not the empty-note one a stdin read taken as
		// a token would leave behind.
		if !strings.Contains(err.Error(), "conductor capability") {
			t.Errorf("the refusal %q is not the missing-capability refusal", err)
		}
		notesAfter, eventsAfter := runNoteWrites(t, conn, runID)
		if notesAfter != notesBefore || eventsAfter != eventsBefore {
			t.Errorf("the refusal wrote: notes %d -> %d, events %d -> %d",
				notesBefore, notesAfter, eventsBefore, eventsAfter)
		}
	})
}

// runNoteEventAuthority reads the authority off the run-note-added event that
// recorded note id.
func runNoteEventAuthority(t *testing.T, conn *sql.DB, noteID int) any {
	t.Helper()
	var data string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE kind = 'run-note-added' AND json_extract(data, '$.note') = ?`,
		noteID,
	).Scan(&data)
	testsupport.Must(t, err, "reading note %d's event: %v", noteID, err)
	var payload map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(data), &payload), "event data %q: %v", data, nil)
	return payload["authority"]
}

// TestRunNoteAddCLIRecordsTheStatedAuthority: --authority operator and
// --authority conductor each land on the note's event as stated.
func TestRunNoteAddCLIRecordsTheStatedAuthority(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)
	t.Setenv(TokenEnvVar, seatConductor(t, conn, runID))

	for _, kind := range []string{"operator", "conductor"} {
		cmd := runNoteCmdNoAuthority(conn)
		testsupport.Must(t, cmd.Flags().Set("authority", kind), "setting --authority: %v", nil)
		testsupport.Must(t, cmd.Flags().Set("text", "ruled by "+kind), "setting --text: %v", nil)
		w, buf := bufWriter(true)
		err := runRunNoteAdd(cmd, runRef, w)
		testsupport.Must(t, err, "run note add --authority %s: %v", kind, err)

		var env struct {
			Data struct {
				Note engine.RunNote `json:"note"`
			} `json:"data"`
		}
		testsupport.Must(t, json.Unmarshal(buf.Bytes(), &env), "unmarshal: %v\n%s", nil, buf.String())
		if got := runNoteEventAuthority(t, conn, env.Data.Note.ID); got != kind {
			t.Errorf("--authority %s: event authority = %v, want %q", kind, got, kind)
		}
	}
}

// TestRunNoteAddCLIRefusesAnAuthorityItCannotRecord: a standing grant, an
// unknown value, and no --authority at all are each VALIDATION_ERROR and
// write nothing. The run is bound and the capability supplied, so the refusal
// is the authority's and not the token's.
func TestRunNoteAddCLIRefusesAnAuthorityItCannotRecord(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)
	t.Setenv(TokenEnvVar, seatConductor(t, conn, runID))

	for _, tc := range []struct {
		name      string
		authority string
		set       bool
	}{
		{"standing-grant", "standing-grant", true},
		{"an unknown value", "admin", true},
		{"no --authority", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := runNoteCmdNoAuthority(conn)
			if tc.set {
				testsupport.Must(t, cmd.Flags().Set("authority", tc.authority), "setting --authority: %v", nil)
			}
			testsupport.Must(t, cmd.Flags().Set("text", "a ruling"), "setting --text: %v", nil)
			notesBefore, eventsBefore := runNoteWrites(t, conn, runID)
			w, _ := bufWriter(true)
			err := runRunNoteAdd(cmd, runRef, w)
			assertCmdCode(t, err, output.ErrValidation, "run note add with "+tc.name)
			if err != nil && !strings.Contains(err.Error(), "--authority") {
				t.Errorf("the refusal %q does not name --authority", err)
			}
			notesAfter, eventsAfter := runNoteWrites(t, conn, runID)
			if notesAfter != notesBefore || eventsAfter != eventsBefore {
				t.Errorf("the refusal wrote: notes %d -> %d, events %d -> %d",
					notesBefore, notesAfter, eventsBefore, eventsAfter)
			}
		})
	}
}

// TestRunNoteAddCLIRefusesTheAuthorityBeforeReadingStdin: under --file - an
// invalid --authority is refused before the note is read, so the caller's
// stdin is left unconsumed.
func TestRunNoteAddCLIRefusesTheAuthorityBeforeReadingStdin(t *testing.T) {
	const piped = "tests fails on clean HEAD; override-pass"
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	t.Setenv(TokenEnvVar, seatConductor(t, conn, runID))
	withStdin(t, piped)

	cmd := runNoteCmdNoAuthority(conn)
	testsupport.Must(t, cmd.Flags().Set("authority", "bogus"), "setting --authority: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("file", "-"), "setting --file: %v", nil)
	w, _ := bufWriter(true)
	err := runRunNoteAdd(cmd, model.FormatRunID(runID), w)
	assertCmdCode(t, err, output.ErrValidation, "run note add --file - --authority bogus")
	if notes, events := runNoteWrites(t, conn, runID); notes != 0 || events != 0 {
		t.Errorf("the refusal wrote: notes = %d, events = %d", notes, events)
	}
	rest, readErr := io.ReadAll(os.Stdin)
	testsupport.Must(t, readErr, "reading stdin after the refusal: %v", readErr)
	if string(rest) != piped {
		t.Errorf("stdin after the refusal = %q, want it unread (%q)", rest, piped)
	}
}

func TestRunNoteListCLI(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	runRef := model.FormatRunID(runID)

	// Empty first: the human line says so rather than printing nothing.
	w, buf := bufWriter(false)
	err := runRunNoteList(cmdWithDB(conn), runRef, w)
	testsupport.Must(t, err, "run note list (empty): %v", err)
	if !strings.Contains(buf.String(), "no notes") {
		t.Errorf("the empty listing does not say so:\n%s", buf.String())
	}

	for _, text := range []string{"first ruling", "second ruling\nwith a second line"} {
		cmd := runNoteCmdWithDB(conn)
		_ = cmd.Flags().Set("text", text)
		wa, _ := bufWriter(true)
		err := runRunNoteAdd(cmd, runRef, wa)
		testsupport.Must(t, err, "run note add: %v", err)
	}

	w, buf = bufWriter(true)
	err = runRunNoteList(cmdWithDB(conn), runRef, w)
	testsupport.Must(t, err, "run note list: %v", err)
	var env struct {
		Data struct {
			Run   string           `json:"run"`
			Notes []engine.RunNote `json:"notes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Data.Run != runRef || len(env.Data.Notes) != 2 ||
		env.Data.Notes[0].Text != "first ruling" ||
		env.Data.Notes[1].Text != "second ruling\nwith a second line" ||
		env.Data.Notes[0].ID >= env.Data.Notes[1].ID {
		t.Errorf("listing = %+v, want both notes in insertion order", env.Data)
	}

	w, buf = bufWriter(false)
	err = runRunNoteList(cmdWithDB(conn), runRef, w)
	testsupport.Must(t, err, "run note list (human): %v", err)
	out := buf.String()
	for _, want := range []string{"2 note(s)", "first ruling", "second ruling", "      with a second line"} {
		if !strings.Contains(out, want) {
			t.Errorf("the human listing lacks %q:\n%s", want, out)
		}
	}
}

// TestRunNoteVerbsAreRegistered: `run note add|list` hang off `run`, so
// `run --help` and the derived unknown-verb list both name them.
func TestRunNoteVerbsAreRegistered(t *testing.T) {
	var note *cobra.Command
	for _, sub := range runCmd.Commands() {
		if sub.Name() == "note" {
			note = sub
		}
	}
	if note == nil {
		t.Fatalf("run has no `note` subcommand; registered: %s", runVerbList(runCmd))
	}
	got := map[string]bool{}
	for _, sub := range note.Commands() {
		got[sub.Name()] = true
	}
	for _, want := range []string{"add", "list"} {
		if !got[want] {
			t.Errorf("run note has no %q subcommand", want)
		}
	}
	for _, flag := range []string{"text", "file", "authority"} {
		if runNoteAddCmd.Flags().Lookup(flag) == nil {
			t.Errorf("run note add has no --%s flag", flag)
		}
	}
}

// asCmdErr unwraps to the CLI's coded error, so a test asserts the exit code a
// script would see rather than a message.
func asCmdErr(err error, target **CmdError) bool {
	if err == nil {
		return false
	}
	ce, ok := err.(*CmdError)
	if !ok {
		return false
	}
	*target = ce
	return true
}
