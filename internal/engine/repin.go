package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// REPIN — the recovery half of the pin story (DKT-408).
//
// `verify-pins` (DKT-297) made drift visible; nothing made it survivable. When
// a corpus install replaces files under an instance-config root while a run is
// active or parked, every pin naming a replaced file mismatches disk forever:
// re-activation deliberately INHERITS the original pin set (RA2), so the only
// disposition was abandon + full re-plan. Four runs across three projects died
// exactly that way, one of them forfeiting 76 completed steps, and one of them
// existing only to reconcile the previous casualty of the same mechanism.
//
// RepinRun is the explicit, operator-gated opposite of RA2's inheritance: it
// rewrites the run's CURRENT agreement — the `pins` rows — to what the refs
// resolve to on disk now, so the steps not yet claimed proceed under the new
// bytes. It is a separate verb rather than a flag on re-activation on purpose:
// re-activation inheriting pins is a guarantee in-flight work relies on, and a
// repin is a person deciding, with a recorded reason, that the agreement moves.
//
// COMPLETED WORK'S PROVENANCE IS NEVER REWRITTEN. Two mechanisms enforce it at
// the data level rather than by convention:
//
//   - The transaction refuses unless the run is quiesced: no step `claimed`
//     (an executor mid-flight claimed under the old agreement, and its packet
//     re-renders would silently flip contract mid-execution) and no dispatch
//     open (a manifest is a frozen offer; its relay must not straddle the
//     transition). It also refuses when every step is terminal — a run whose
//     pins are referenced only by completed work has nothing left to recover,
//     so a repin could only falsify history. Under those guards every step is
//     either terminal (it consumed the old bytes wholly, before the repin
//     event's seq) or pre-claim (it will consume the new bytes wholly) — no
//     step ever straddles the old and new agreement.
//
//   - The write touches ONLY the `pins` table. The tables that record what
//     completed steps did — `steps`, `artifacts`, `step_inputs`, `events` —
//     are never updated; the old sha is preserved in the same transaction as
//     a `run-repinned` event per changed ref (old sha -> new sha), so the
//     agreement a completed step worked under stays recoverable from the trail
//     even after the current agreement moves. The event log is already the
//     package's history mechanism (§9 item 2); a parallel pin-history table
//     would be a second source of the same fact.
//
// DROPPING A REF THAT NO LONGER RESOLVES (DKT-582). "Adopt current disk bytes"
// has nothing to say about a ref with no current bytes, so repin refused the
// whole set on one NOT_FOUND — and corpus commits that DELETE a contract are as
// ordinary as commits that edit one. RUN-42 died on `contracts/test-infra.md`
// and `contracts/pr-comment-author.md` being deleted; RUN-36 died the same way
// after surviving two earlier repins. Neither run had a step left that would
// ever open either file.
//
// So there is a second disposition, and its guard is the pending packet closure
// (pending_closure.go): a NOT_FOUND FILE pin that NO non-terminal step can
// reach may be RETIRED — the row deleted, a `run-repinned` event recorded with
// a null `new_sha256`. The properties above are unchanged by it. Completed
// steps' provenance still is not rewritten (the old sha is in the event, and
// their own rows are untouched), and the run stays whole rather than partially
// recovered, because a ref nothing left to run can read is not part of the
// agreement the remaining steps work under.
//
// It is OPT-IN — `--drop REF` or `--drop-unresolvable` — for the same reason
// repin itself is a separate verb: deciding a pinned file is gone for good is a
// person's decision, and an engine that made it silently would delete the pin
// that was about to explain a refusal. A NOT_FOUND ref a pending step DOES
// read still refuses, naming the steps, however the flags are spelled.
//
// ADOPTED BYTES BRING THEIR OWN CLOSURE (DKT-805). "Adopt current disk bytes"
// used to re-hash the run's EXISTING pin set and nothing more — but the pin set
// is the packet closure of the bytes the run froze (DKT-581), and the adopted
// bytes can have a DIFFERENT closure. A corpus edit that makes a contract
// include a new fragment left repin reporting full success while every step
// reading that contract became unrenderable: the fragment was not pinned, so
// render refused (VALIDATION_ERROR) with "start a new run" as its remedy — the
// disposition repin exists to avoid. RUN-56 lost a whole 18-row dispatch to
// exactly that, the day after an operator-approved repin reported
// "Repinned 10 pin(s) (0 dropped, 19 already matched)".
//
// So a repin that adopts changed bytes also walks the pending packet closure
// those bytes reach (pending_closure.go — the same walk the drop guard uses)
// and PINS every reachable file ref the run does not already hold, at its
// current disk hash, in the same transaction. Adding a pin can never rewrite
// completed provenance: no recorded step ever read an unpinned ref (render
// refuses on exactly that), so there is no history for the new row to
// contradict — the same argument RA3's re-activation additions rest on. Each
// added ref records its own `run-repinned` event with a null `old_sha256` and
// `added: true`, the mirror of the drop's signature: "there were no pinned
// bytes before" is a different fact from "the bytes were these".
//
// A newly-required ref with NO bytes on disk refuses the whole set up front,
// naming the ref and the steps that read it — pinning it is impossible and
// repinning around it would trade the CONFLICT for a render-time refusal, which
// is the exact failure this closure walk exists to close. The dispositions are
// the missing-refusal's: restore the file, or abandon the run and re-plan.
type RepinChange struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	OldSHA256 string `json:"old_sha256"`
	NewSHA256 string `json:"new_sha256"`
	// Path is where the file pin resolved on disk, "" for registered-object
	// pins — the same field PinVerdict carries, for the same operator.
	Path string `json:"path,omitempty"`
	// Dropped marks the DKT-582 disposition: the ref no longer resolves at all
	// and nothing non-terminal reads it, so the pin was retired rather than
	// moved. NewSHA256 is "" on exactly these, and the recorded event carries a
	// null `new_sha256` — "there are no bytes now" said in the trail, which is
	// a different fact from "the bytes are these".
	Dropped bool `json:"dropped,omitempty"`
	// Added marks the DKT-805 disposition: the adopted bytes' packet closure
	// reaches a file ref the run never pinned, so a pin was created rather than
	// moved. OldSHA256 is "" on exactly these, and the recorded event carries a
	// null `old_sha256` — "there were no pinned bytes before" said in the
	// trail, the mirror of the drop's signature.
	Added bool `json:"added,omitempty"`
}

// RepinOutcome reports what RepinRun did.
type RepinOutcome struct {
	Run string `json:"run"`
	// Repinned is one entry per pin whose recorded hash moved, in the
	// (kind, ref) order the report walks — empty (never nil) on a no-op.
	Repinned []RepinChange `json:"repinned"`
	// Dropped is one entry per pin RETIRED because its ref no longer resolves
	// and no non-terminal step reads it — empty (never nil) when none were.
	Dropped []RepinChange `json:"dropped"`
	// Added is one entry per pin CREATED because the adopted bytes' packet
	// closure reaches a ref the run never pinned (DKT-805) — empty (never nil)
	// when the adopted closure was already covered.
	Added []RepinChange `json:"added"`
	// Unchanged counts the pins that already matched disk.
	Unchanged int `json:"unchanged"`
}

// RepinOptions carries the operator's dispositions for pins that "adopt the
// current disk bytes" cannot fix on its own (DKT-582).
//
// DROPPING IS OPT-IN, ALWAYS. A ref that stopped resolving is either a deletion
// the run can survive or a deletion that wedges it, and only the pending
// closure can tell the two apart — so the verb refuses by default and these
// fields are how a person says "retire it", never how the engine decides to.
type RepinOptions struct {
	// Reason is --reason, recorded verbatim on every event.
	Reason string
	// Drop names refs to retire individually. A named ref that still resolves,
	// or that this run does not pin, is a refusal rather than a no-op: it means
	// the operator and the run disagree about what is wrong.
	Drop []string
	// DropUnresolvable applies the same disposition to EVERY currently drifted
	// file pin that no longer resolves and that no non-terminal step reads,
	// without naming each. Pins that resolve to different bytes are untouched
	// by it — those are repin's ordinary business.
	DropUnresolvable bool
}

// RepinRun re-pins a run's drifted pins to what their refs resolve to now.
func RepinRun(conn *sql.DB, runID int, reason string, nowMS int64) (*RepinOutcome, error) {
	return RepinRunWith(conn, runID, RepinOptions{Reason: reason}, nowMS)
}

// RepinRunWith is RepinRun with the DKT-582 dispositions.
func RepinRunWith(
	conn *sql.DB, runID int, opts RepinOptions, nowMS int64,
) (*RepinOutcome, error) {
	return repinRunOptsIn(conn, runID, opts, nowMS, instanceConfigRoots())
}

// repinRunIn is RepinRun over an explicit root list — the same seam
// verifyPinsIn takes, for the same reason: which file a ref resolves to is the
// thing under test.
func repinRunIn(
	conn *sql.DB, runID int, reason string, nowMS int64, roots []string,
) (*RepinOutcome, error) {
	return repinRunOptsIn(conn, runID, RepinOptions{Reason: reason}, nowMS, roots)
}

// repinRunOptsIn is repinRunIn with dispositions.
func repinRunOptsIn(
	conn *sql.DB, runID int, opts RepinOptions, nowMS int64, roots []string,
) (*RepinOutcome, error) {
	reason := opts.Reason
	// The status gate runs BEFORE the disk walk so a repin of a finished run
	// refuses cleanly rather than reporting drift it would then refuse to fix.
	// It is re-checked inside the transaction below; this read exists for the
	// error, not for the guarantee.
	run, err := db.GetRun(conn, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	if err := repinStatusGuard(run); err != nil {
		return nil, err
	}

	// The SAME comparison `verify-pins` reports — one definition of drift, so
	// the detection verb and the recovery verb can never disagree about what
	// is drifted.
	report, err := verifyPinsIn(conn, runID, roots)
	if err != nil {
		return nil, err
	}

	// MISSING REFUSES THE WHOLE SET, UNLESS IT IS DROPPED. Repin's contract is
	// "adopt current disk bytes", and a missing ref has no bytes to adopt;
	// repinning the changed pins around it would report recovery while the run
	// stays wedged on the rest. All-or-nothing is the same rule activation's own
	// pinning follows ("pinning is never partial"). DKT-582 adds the one
	// disposition that is not a partial recovery but a complete one: a ref
	// nothing left to run can read is not a hole in the agreement, and retiring
	// it — on the operator's explicit say-so — leaves the run whole.
	var missing []string
	// A changed verdict that repinning cannot fix: no resolved hash, or the
	// same hash (a schema that matches its pin byte-for-byte but no longer
	// compiles reports `changed` with Found == Pinned — new bytes are not the
	// remedy for that).
	var unfixable []string
	// A ref covered by --drop/--drop-unresolvable that a non-terminal step can
	// still read: the drop that would wedge the run, refused by name.
	var stillNeeded []string
	var changes []RepinChange
	var drops []RepinChange

	dropNamed := make(map[string]bool, len(opts.Drop))
	for _, ref := range opts.Drop {
		dropNamed[normalizePinRef(ref)] = true
	}
	dropMatched := make(map[string]bool, len(opts.Drop))

	// The pending closure is computed ONLY when a disposition could use it — a
	// plain repin must not start reading step definitions and config files it
	// has no question for.
	var closure pendingClosure
	if len(dropNamed) > 0 || opts.DropUnresolvable {
		closure, err = pendingPacketClosure(conn, runID, roots)
		if err != nil {
			return nil, err
		}
	}

	for _, v := range report.Pins {
		switch v.Status {
		case PinMissing:
			named := dropNamed[normalizePinRef(v.Ref)]
			if named {
				dropMatched[normalizePinRef(v.Ref)] = true
			}
			if !named && !opts.DropUnresolvable {
				missing = append(missing, fmt.Sprintf("%s %s", v.Kind, v.Ref))
				continue
			}
			// ONLY FILE PINS ARE DROPPABLE. A workflow or schema pin names a
			// REGISTERED object every step of the run is expanded from or
			// validates against; the packet closure says nothing about those,
			// so there is no reading of "unreferenced" that could justify
			// retiring one. --drop-unresolvable leaves them to the refusal
			// below rather than quietly widening its own meaning.
			if v.Kind != db.PinKindFile {
				if named {
					return nil, validationErr(
						"--drop %s names a %s pin; only file pins can be dropped — a "+
							"registered %s is what the run's steps are expanded from or "+
							"validated against, and no packet closure can say it is unread",
						v.Ref, v.Kind, v.Kind)
				}
				missing = append(missing, fmt.Sprintf("%s %s", v.Kind, v.Ref))
				continue
			}
			// A file pin can also read `missing` because the file IS there and
			// could not be READ — verifyFilePin reports the path in exactly that
			// case and in no other. That is a permission to restore, not a
			// deletion to retire, and retiring it would throw away the pin that
			// still describes the file sitting on disk.
			if v.Path != "" {
				return nil, conflictErr(
					"%s cannot be dropped: %s exists but could not be read; fix its "+
						"permissions — dropping retires a ref with no file at all",
					v.Ref, v.Path)
			}
			if by := closure.referencedBy(v.Ref); len(by) > 0 {
				stillNeeded = append(stillNeeded, fmt.Sprintf(
					"%s (read by %s)", v.Ref, strings.Join(by, ", ")))
				continue
			}
			drops = append(drops, RepinChange{
				Kind: v.Kind, Ref: v.Ref,
				OldSHA256: v.Pinned, NewSHA256: "", Path: v.Path, Dropped: true,
			})
		case PinChanged:
			if dropNamed[normalizePinRef(v.Ref)] {
				return nil, conflictErr(
					"--drop %s names a ref that still resolves (pinned %s, on disk %s); "+
						"drop retires a ref with no current bytes at all — repin adopts "+
						"this one's new bytes without it",
					v.Ref, v.Pinned, v.Found)
			}
			if v.Found == "" || v.Found == v.Pinned {
				unfixable = append(unfixable, fmt.Sprintf("%s %s", v.Kind, v.Ref))
				continue
			}
			changes = append(changes, RepinChange{
				Kind: v.Kind, Ref: v.Ref,
				OldSHA256: v.Pinned, NewSHA256: v.Found, Path: v.Path,
			})
		case PinOK:
			if dropNamed[normalizePinRef(v.Ref)] {
				return nil, conflictErr(
					"--drop %s names a ref that still resolves and still matches its "+
						"pin; there is nothing to retire", v.Ref)
			}
		}
	}

	// A --drop that matched no pin at all is the operator and the run
	// disagreeing about what this run holds — never a silent no-op.
	for _, ref := range opts.Drop {
		if !dropMatched[normalizePinRef(ref)] {
			return nil, notFoundErr(nil,
				"--drop %s names a ref %s does not pin; run `docket run verify-pins %s` "+
					"for the refs this run actually holds", ref, run.Ref(), run.Ref())
		}
	}
	if len(stillNeeded) > 0 {
		return nil, conflictErr(
			"%s cannot drop %s: the ref(s) no longer resolve, but non-terminal steps "+
				"still read them — restore the file(s); dropping retires only a ref "+
				"nothing left to run can open",
			run.Ref(), strings.Join(stillNeeded, "; "))
	}
	if len(missing) > 0 {
		return nil, notFoundErr(nil,
			"%s cannot be repinned: %s no longer resolve(s) at all — restore the "+
				"file(s), abandon the run, or retire the ref(s) with `--drop`/"+
				"`--drop-unresolvable` if no pending step reads them; repin adopts "+
				"current disk bytes and a missing ref has none",
			run.Ref(), strings.Join(missing, ", "))
	}
	if len(unfixable) > 0 {
		return nil, conflictErr(
			"%s cannot be repinned: %s report(s) drift that new bytes do not "+
				"explain (run `docket run verify-pins %s` for the verdicts)",
			run.Ref(), strings.Join(unfixable, ", "), run.Ref())
	}

	// THE ADOPTED BYTES' OWN CLOSURE (DKT-805). Adopting changed bytes adopts
	// their packet closure too, and that closure can reach refs the run never
	// pinned — the walk below finds them, so the transaction can pin them or
	// this verb can refuse NOW, naming them, instead of reporting success and
	// letting the next render refuse with "start a new run". Computed only when
	// bytes were actually adopted: a drop cannot introduce a reference, and a
	// no-op repin must stay a no-op.
	var adds []RepinChange
	if len(changes) > 0 {
		if closure == nil {
			closure, err = pendingPacketClosure(conn, runID, roots)
			if err != nil {
				return nil, err
			}
		}
		adds, err = closureAdditions(report, closure, roots, run.Ref())
		if err != nil {
			return nil, err
		}
	}

	outcome := &RepinOutcome{
		Run: run.Ref(), Repinned: []RepinChange{}, Dropped: []RepinChange{},
		Added:     []RepinChange{},
		Unchanged: len(report.Pins) - len(changes) - len(drops),
	}
	if len(changes) == 0 && len(drops) == 0 {
		// Nothing drifted: a clean no-op, so running repin twice is safe and
		// the second run says so instead of inventing an event.
		return outcome, nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("repinning %s: %w", run.Ref(), err)
	}
	defer tx.Rollback()

	// The guards re-run against the transaction's snapshot: everything below
	// holds at the moment the agreement moves, not merely when the verb began.
	fresh, err := db.GetRunTx(tx, runID)
	if err != nil {
		return nil, err
	}
	if err := repinStatusGuard(fresh); err != nil {
		return nil, err
	}
	if err := repinQuiescenceGuard(tx, runID, fresh.Ref()); err != nil {
		return nil, err
	}

	for _, c := range changes {
		// The UPDATE is keyed on the OLD hash — the compare-and-swap that makes
		// "never rewrite completed provenance" a property of the statement
		// rather than of the caller's timing. A pin that moved between the
		// disk walk and this write hits zero rows and the whole repin rolls
		// back, instead of overwriting an agreement this verb never inspected.
		res, err := tx.Exec(
			`UPDATE pins SET sha256 = ? WHERE run_id = ? AND kind = ? AND ref = ? AND sha256 = ?`,
			c.NewSHA256, runID, c.Kind, c.Ref, c.OldSHA256,
		)
		if err != nil {
			return nil, fmt.Errorf("repinning %s %s: %w", c.Kind, c.Ref, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil, conflictErr(
				"%s %s changed while repinning %s; re-run `docket run verify-pins %s` and retry",
				c.Kind, c.Ref, fresh.Ref(), fresh.Ref())
		}

		// One event PER CHANGED REF, in the same transaction as its UPDATE:
		// the old sha's survival must not depend on a second commit landing.
		data, err := json.Marshal(map[string]any{
			"kind":       c.Kind,
			"ref":        c.Ref,
			"old_sha256": c.OldSHA256,
			"new_sha256": c.NewSHA256,
			"path":       c.Path,
			"reason":     reason,
		})
		if err != nil {
			return nil, fmt.Errorf("recording the repin of %s %s: %w", c.Kind, c.Ref, err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventRunRepinned, RunID: runID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return nil, err
		}
		outcome.Repinned = append(outcome.Repinned, c)
	}

	for _, d := range drops {
		// The DELETE is keyed on the OLD hash for the same reason the UPDATE is:
		// a pin that moved between the disk walk and this write is an agreement
		// this verb never inspected, and retiring it blind would be exactly the
		// silent removal the opt-in gate exists to prevent.
		res, err := tx.Exec(
			`DELETE FROM pins WHERE run_id = ? AND kind = ? AND ref = ? AND sha256 = ?`,
			runID, d.Kind, d.Ref, d.OldSHA256,
		)
		if err != nil {
			return nil, fmt.Errorf("dropping %s %s: %w", d.Kind, d.Ref, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil, conflictErr(
				"%s %s changed while repinning %s; re-run `docket run verify-pins %s` and retry",
				d.Kind, d.Ref, fresh.Ref(), fresh.Ref())
		}

		// A NULL `new_sha256` is the drop's signature in the trail: the same
		// event kind, the same old sha preserved for completed steps'
		// provenance, and the one field that says there are no current bytes
		// rather than these current bytes. `dropped` states it in a form a
		// reader can branch on without inferring intent from an absence.
		data, err := json.Marshal(map[string]any{
			"kind":       d.Kind,
			"ref":        d.Ref,
			"old_sha256": d.OldSHA256,
			"new_sha256": nil,
			"path":       d.Path,
			"dropped":    true,
			"reason":     reason,
		})
		if err != nil {
			return nil, fmt.Errorf("recording the drop of %s %s: %w", d.Kind, d.Ref, err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventRunRepinned, RunID: runID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return nil, err
		}
		outcome.Dropped = append(outcome.Dropped, d)
	}

	for _, a := range adds {
		// A plain INSERT would race a concurrent activation's `INSERT OR
		// IGNORE` of the same ref; the OR IGNORE plus the zero-rows check makes
		// the collision a visible CONFLICT rather than either a constraint
		// error or a silent adoption of a hash this verb never inspected —
		// the same discipline the UPDATE's compare-and-swap enforces.
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO pins (run_id, kind, ref, sha256) VALUES (?, ?, ?, ?)`,
			runID, a.Kind, a.Ref, a.NewSHA256,
		)
		if err != nil {
			return nil, fmt.Errorf("pinning %s %s: %w", a.Kind, a.Ref, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil, conflictErr(
				"%s %s changed while repinning %s; re-run `docket run verify-pins %s` and retry",
				a.Kind, a.Ref, fresh.Ref(), fresh.Ref())
		}

		// A NULL `old_sha256` is the addition's signature in the trail, the
		// mirror of the drop's null `new_sha256`: same event kind, and the one
		// field that says the run held no bytes for this ref before. `added`
		// states it in a form a reader can branch on, and `required_by` names
		// what reaches the ref, so provenance says WHY the agreement grew.
		data, err := json.Marshal(map[string]any{
			"kind":        a.Kind,
			"ref":         a.Ref,
			"old_sha256":  nil,
			"new_sha256":  a.NewSHA256,
			"path":        a.Path,
			"added":       true,
			"required_by": closure.referencedBy(a.Ref),
			"reason":      reason,
		})
		if err != nil {
			return nil, fmt.Errorf("recording the pin of %s %s: %w", a.Kind, a.Ref, err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventRunRepinned, RunID: runID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return nil, err
		}
		outcome.Added = append(outcome.Added, a)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("repinning %s: %w", run.Ref(), err)
	}
	return outcome, nil
}

// closureAdditions computes the pins a repin must CREATE: every file ref the
// pending packet closure reaches that the run does not already hold, resolved
// and hashed at its current disk bytes (DKT-805).
//
// The walk itself is unpinnedClosureRefs, shared verbatim with `verify-pins`'
// closure check (DKT-821) — the detection verb and the recovery verb must never
// disagree about which refs a pin set is missing, which is the disagreement
// RUN-59 was.
//
// It refuses — before any write — when a reachable ref has no bytes on disk at
// all: there is nothing to pin, and a repin that proceeded around it would
// report success while the steps reading it stay unrenderable, which is the
// exact outcome this walk exists to close.
func closureAdditions(
	report *PinReport, closure pendingClosure, roots []string, runRef string,
) ([]RepinChange, error) {
	var adds []RepinChange
	var unpinnable []string
	for _, u := range unpinnedClosureRefs(report.Pins, closure, roots) {
		if u.readErr != nil {
			return nil, conflictErr(
				"%s cannot be repinned: the adopted bytes require %q, and %s "+
					"exists but could not be read; fix its permissions",
				runRef, u.ref, u.path)
		}
		if u.path == "" {
			unpinnable = append(unpinnable, fmt.Sprintf(
				"%s (read by %s)", u.ref, strings.Join(u.requiredBy, ", ")))
			continue
		}
		adds = append(adds, RepinChange{
			Kind: db.PinKindFile, Ref: u.ref,
			OldSHA256: "", NewSHA256: u.sha256,
			Path: u.path, Added: true,
		})
	}
	if len(unpinnable) > 0 {
		return nil, notFoundErr(nil,
			"%s cannot be repinned: the adopted bytes require ref(s) this run does "+
				"not pin and that do not resolve on disk — %s; restore the file(s) "+
				"under an instance-config root, or abandon the run and re-plan; a "+
				"repin that proceeded would leave those steps unable to render "+
				"their packets",
			runRef, strings.Join(unpinnable, "; "))
	}
	return adds, nil
}

// repinStatusGuard refuses every run status with no pending-side work.
//
// `done` and `abandoned` are the acceptance criterion's hard case stated as a
// status: every pin of a terminal run is referenced only by completed steps'
// history, so a repin could not help any future claim and could only make the
// recorded agreement disagree with the bytes that work actually consumed.
// `planning` has no frozen agreement yet — activation is where pinning happens.
func repinStatusGuard(run *model.Run) error {
	if run.Status == model.RunActive || run.Status == model.RunWaitingHuman {
		return nil
	}
	return conflictErr(
		"run %s is %s; repin applies to a run that is %s — a run with no "+
			"remaining steps has only completed steps' pins, and those are "+
			"history, not an agreement to move",
		run.Ref(), run.Status,
		orStatusList([]model.RunStatus{model.RunActive, model.RunWaitingHuman}))
}

// repinQuiescenceGuard is the transaction-side half of the provenance rule: the
// agreement may move only when no step could consume both sides of it.
func repinQuiescenceGuard(tx *sql.Tx, runID int, runRef string) error {
	// In-flight executors first, because theirs is the sharper refusal: a
	// claimed step's packet was rendered under the old agreement, and its later
	// renders and its completion would land under the new one — the exact
	// straddle that would falsify its provenance the moment it recorded.
	rows, err := tx.Query(
		`SELECT instance FROM steps WHERE run_id = ? AND status = ? ORDER BY id`,
		runID, db.StepClaimed)
	if err != nil {
		return fmt.Errorf("collecting %s's claimed steps: %w", runRef, err)
	}
	claimed, err := scanTxRows(rows, func(r *sql.Rows) (string, error) {
		var s string
		return s, r.Scan(&s)
	})
	if err != nil {
		return err
	}
	if len(claimed) > 0 {
		return conflictErr(
			"%d step(s) of %s are claimed and mid-flight (%s); a repin under a "+
				"live claim would change what the executor's packet means "+
				"mid-execution — wait for them to record or for their leases "+
				"to be reaped, then retry",
			len(claimed), runRef, strings.Join(claimed, ", "))
	}

	// A manifest is a frozen copy of one `next` answer. Repinning under an open
	// one would hand its relay rows whose packets no longer mean what the
	// manifest's reader saw at open time.
	if open, err := db.OpenDispatchTx(tx, runID); err == nil {
		return conflictErr(
			"a dispatch is open for %s (%s, expiring at %d); close or abandon it "+
				"before repinning — its manifest was offered under the current pins",
			runRef, FormatDispatchID(open.ID), open.ExpiresMS)
	} else if !errors.Is(err, db.ErrNoOpenDispatch) {
		return err
	}

	// And there must be something left for the new agreement to govern. The
	// run-status guard usually implies this, but the rollup that retires a
	// finished run runs lazily — this is the direct form of "completed steps'
	// pins are never rewritten", asked of the steps themselves.
	var remaining int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM steps
		  WHERE run_id = ? AND status NOT IN (?, ?, ?, ?)`,
		runID, db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	).Scan(&remaining); err != nil {
		return fmt.Errorf("counting %s's remaining steps: %w", runRef, err)
	}
	if remaining == 0 {
		return conflictErr(
			"every step of %s is terminal; its pins are referenced only by "+
				"completed steps' history now, and repinning them would rewrite "+
				"that record without helping any future claim", runRef)
	}
	return nil
}

// PinDrift returns the unsound subset of a run's pin report — the rows a read
// surface states when it warns (DKT-408's remedy 2). nil when every pin is
// sound, so callers can gate rendering on emptiness alone.
//
// It is the pin check minus the sound rows rather than its own walk, so the
// warning a surface prints and the report `verify-pins` exits 4 on can never
// name different pins.
//
// It is the PER-PIN half only. `verify-pins` also reports refs the pinned bytes
// reference and the pin set does not hold (DKT-821), and that question has no
// per-pin verdict to render here — it is the verb's answer, not a drift line —
// so this advisory keeps to the pins and keeps to a hash-per-pin of work on
// every `run status`.
func PinDrift(conn *sql.DB, runID int) ([]PinVerdict, error) {
	report, err := verifyPinsIn(conn, runID, instanceConfigRoots())
	if err != nil {
		return nil, err
	}
	var out []PinVerdict
	for _, v := range report.Pins {
		if v.Status != PinOK {
			out = append(out, v)
		}
	}
	return out, nil
}

// pinDriftAdvisory is PinDrift as a best-effort advisory, the same posture
// staleTargets takes: the caller has already done its real work, and a failed
// advisory computation must not turn a committed answer into an error. The
// authoritative surfaces — `verify-pins`, and the CONFLICT every pinned read
// raises — do not run through this.
func pinDriftAdvisory(conn *sql.DB, runID int) []PinVerdict {
	drift, err := PinDrift(conn, runID)
	if err != nil {
		return nil
	}
	return drift
}

// PinDriftNotice renders drifted pins for a human surface, one line per pin,
// naming both hashes and the recovery verb. Empty for no drift.
func PinDriftNotice(drift []PinVerdict, runRef string) string {
	if len(drift) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b,
		"Pin drift: %d pin(s) no longer match what this run froze at activation:\n",
		len(drift))
	for _, v := range drift {
		switch v.Status {
		case PinChanged:
			fmt.Fprintf(&b, "  %s %s changed: pinned %s, on disk %s\n",
				v.Kind, v.Ref, v.Pinned, v.Found)
		case PinMissing:
			fmt.Fprintf(&b, "  %s %s is pinned at %s but no longer resolves\n",
				v.Kind, v.Ref, v.Pinned)
		}
	}
	fmt.Fprintf(&b,
		"Steps reading these refs will refuse (CONFLICT). Restore the files, or "+
			"run `docket run repin %s --reason ...` to adopt current disk bytes "+
			"for the remaining steps.", runRef)
	return b.String()
}
