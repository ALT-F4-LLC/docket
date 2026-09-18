package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket run note add|list` — DKT-1079. The dispatcher's channel into every
// packet of a run: a statement recorded once against the run, rendered as
// `== RUN NOTE N` beside the request in every packet the run renders from
// then on, and carried by `step context` as `notes`.

var runNoteCmd = &cobra.Command{
	Use:   "note",
	Short: "Record standing statements that render in every packet of a run",
	Long: `Record and list a run's notes.

A packet draws from frozen and recorded state only: the issue as snapshotted at
activation, the recorded artifacts, the pins, the step's packet files, and the
step's own routing record. Issue comments never render, a mid-run description
edit never renders, and a ` + "`step resolve -m`" + ` note reaches only the step it rules
on. A fact about the WHOLE RUN had no channel — so a dispatcher that learned
before dispatch that a required gate fails on clean HEAD, got the operator's
disposition, and filed the tracking issue, could tell none of it to the workers,
and each one rediscovered the failure and filed a duplicate.

A run note is that channel. ` + "`run note add`" + ` records the statement once; every
packet the run renders afterwards — every step, every issue, every round —
carries it verbatim as a ` + "`== RUN NOTE N`" + ` section right after ` + "`== REQUEST`" + `,
and ` + "`step context`" + ` exposes it as ` + "`notes`" + ` so a contract can name it. Notes are
append-only and each is recorded as a ` + "`run-note-added`" + ` event carrying its text,
so the feed says what every later worker was told.

The shipped packet template renders notes; a custom ` + "`step render --template F`" + `
renders them only where it ranges over ` + "`.Notes`" + ` (each has ` + "`.ID`" + ` and ` + "`.Text`" + `).`,
}

var runNoteAddCmd = &cobra.Command{
	Use:   "add RUN-N (--text T | --file F)",
	Short: "Record a note that every later packet of the run carries",
	Long: `Record one note against a run.

Put in it what a worker would otherwise rediscover: the gate, why its failure
is pre-existing, the issue tracking it, and the disposition already given —
for example:

  docket run note add RUN-70 --text "Gate tests fails on clean HEAD \
      (routing_sweep_test.go), pre-existing and tracked as DKT-1075; \
      disposition: override-pass. Do not re-derive it and do not file a gap."

--file reads the note from a file, or from stdin with "-", for a note that
does not fit a shell argument. One trailing newline is dropped so a file-fed
note renders exactly as the same words passed inline; nothing else is touched.

The note is capped at 16 KiB because it rides every packet of the run; keep
the detail on the issue the note cites. Legal while the run is planning,
active, or parked — the motivating note is written BEFORE the first dispatch —
and refused on a done or abandoned run, which renders no more packets. A note
cannot be edited or removed: a packet is the record of what a worker was
told, so a changed ruling is recorded as a second note, which renders after
the first.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunNoteAdd(cmd, args[0], getWriter(cmd))
	},
}

var runNoteListCmd = &cobra.Command{
	Use:   "list RUN-N",
	Short: "List a run's notes in the order they render",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunNoteList(cmd, args[0], getWriter(cmd))
	},
}

// runNoteOutcome is `add`'s answer: the run and the note as recorded — the
// SAME shape the bundle's `notes` element carries, so a caller that reads one
// can read the other.
type runNoteOutcome struct {
	Run  string         `json:"run"`
	Note engine.RunNote `json:"note"`
}

// runNoteListing is `list`'s answer. The notes ride under a named key rather
// than as a bare array so the envelope can gain a sibling field later without
// changing the shape a caller already parses.
type runNoteListing struct {
	Run   string           `json:"run"`
	Notes []engine.RunNote `json:"notes"`
}

func runRunNoteAdd(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	text, err := runNoteText(cmd)
	if err != nil {
		return err
	}

	note, err := engine.AddRunNote(conn, runID, text, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	outcome := runNoteOutcome{Run: model.FormatRunID(runID), Note: *note}
	var message string
	if !w.JSONMode {
		message = fmt.Sprintf(
			"Recorded note %d on %s (%d bytes); every packet the run renders "+
				"from now on carries it", note.ID, outcome.Run, len(note.Text))
	}
	w.Success(outcome, message)
	return nil
}

// runNoteText resolves `--text` or `--file`, refusing both and neither: a
// note has exactly one source, and a verb that silently preferred one flag
// over the other would record something the caller did not write.
func runNoteText(cmd *cobra.Command) (string, error) {
	text, _ := cmd.Flags().GetString("text")
	path, _ := cmd.Flags().GetString("file")
	hasText := cmd.Flags().Changed("text")
	hasFile := cmd.Flags().Changed("file")

	switch {
	case hasText && hasFile:
		return "", cmdErr(fmt.Errorf("pass --text or --file, not both"), output.ErrValidation)
	case !hasText && !hasFile:
		return "", cmdErr(fmt.Errorf(
			"a note needs its text: pass --text \"...\" or --file F (\"-\" reads stdin)"),
			output.ErrValidation)
	case hasText:
		return text, nil
	}

	// One byte over the cap is enough to let the engine refuse with the cap
	// named; reading the rest of an oversized stream buys nothing.
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, engine.RunNoteMaxBytes+1))
		if err != nil {
			return "", cmdErr(fmt.Errorf("reading the note from stdin: %w", err),
				output.ErrGeneral)
		}
	} else {
		raw, err = os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return "", cmdErr(fmt.Errorf("note file %s not found", path), output.ErrNotFound)
			}
			return "", cmdErr(fmt.Errorf("reading the note file %s: %w", path, err),
				output.ErrGeneral)
		}
	}
	return string(raw), nil
}

func runRunNoteList(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	notes, err := engine.ListRunNotes(conn, runID)
	if err != nil {
		return runErr(err)
	}

	listing := runNoteListing{Run: model.FormatRunID(runID), Notes: notes}
	var message string
	if !w.JSONMode {
		message = renderRunNotes(listing)
	}
	w.Success(listing, message)
	return nil
}

// renderRunNotes is the listing's human form: one block per note, dated, the
// text indented under its header so a multi-line note reads as one entry.
func renderRunNotes(l runNoteListing) string {
	if len(l.Notes) == 0 {
		return fmt.Sprintf("%s has no notes", l.Run)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d note(s)", l.Run, len(l.Notes))
	for _, n := range l.Notes {
		when := time.UnixMilli(n.RecordedAtMS).UTC().Format(time.RFC3339)
		fmt.Fprintf(&b, "\n  [%d] %s", n.ID, when)
		for _, line := range strings.Split(n.Text, "\n") {
			fmt.Fprintf(&b, "\n      %s", line)
		}
	}
	return b.String()
}

func init() {
	runNoteAddCmd.Flags().String("text", "",
		"The note, inline (exactly one of --text or --file)")
	runNoteAddCmd.Flags().String("file", "",
		"Read the note from this file; \"-\" reads stdin")

	runNoteCmd.AddCommand(runNoteAddCmd, runNoteListCmd)
	runCmd.AddCommand(runNoteCmd)
}
