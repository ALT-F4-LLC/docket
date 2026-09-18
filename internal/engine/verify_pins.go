package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Pin verdicts. They are constants rather than free strings because a consumer
// branches on them, and `--json` makes them part of the wire contract.
const (
	// PinOK: the pinned bytes are still what the pin recorded.
	PinOK = "ok"
	// PinChanged: the ref resolves, and to different bytes. This is the one
	// that blocks work — every verb that reads the ref refuses (CONFLICT).
	PinChanged = "changed"
	// PinMissing: the run depends on the ref and it is no longer there.
	PinMissing = "missing"
	// PinUnpinnedReference: not a verdict on a pin — there IS no pin. The
	// pinned bytes reach a file this run never snapshotted, so every step whose
	// packet resolves it refuses at claim (VALIDATION_ERROR).
	PinUnpinnedReference = "unpinned-reference"
)

// PinVerdict is one pin, checked.
type PinVerdict struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Status string `json:"status"`
	// Pinned is the hash the run recorded at activation.
	Pinned string `json:"pinned"`
	// Found is the hash the ref resolves to now — empty when it does not
	// resolve at all.
	Found string `json:"found,omitempty"`
	// Path is where a file pin actually resolved, so an operator restoring a
	// file does not have to guess which config root won.
	Path string `json:"path,omitempty"`
}

// ReferenceVerdict is one ref the run's own pinned bytes reach that the run
// does not pin (DKT-821).
//
// It is deliberately NOT a PinVerdict. A pin verdict answers "do these bytes
// still match", and there are no pinned bytes here to match; conflating the two
// would also hand every consumer of `Pins` — repin's dispositions, the
// `run status` drift advisory — a row with no pinned hash to reason about.
type ReferenceVerdict struct {
	// Status is always PinUnpinnedReference; it is carried per row so a
	// consumer branches on one field name across both lists.
	Status string `json:"status"`
	Ref    string `json:"ref"`
	// IncludedBy is the closure file(s) whose `packet_includes` name the ref —
	// the contract an operator actually edits. Empty when a step's own `packet`
	// entry is what reaches it.
	IncludedBy []string `json:"included_by,omitempty"`
	// RequiredBy is the pending step(s) (and unexpanded phases) that can still
	// open the ref — the claims that will refuse.
	RequiredBy []string `json:"required_by"`
	// Path is where the file sits on disk, when it does. Present-but-unpinned
	// is the RUN-59 case and its remedy differs from absent-and-unpinned, the
	// same distinction DKT-818 drew in the claim-time refusal.
	Path string `json:"path,omitempty"`
}

// PinReport is `docket run verify-pins`.
type PinReport struct {
	Run string `json:"run"`
	// Pins is every pin the run holds, in a total order (kind, then ref), so
	// two checks of one unchanged run produce identical output.
	Pins []PinVerdict `json:"pins"`
	// References is the CLOSURE check, kept beside the per-pin check rather
	// than mixed into it: a conductor reading this needs to know which of the
	// two failed, because they have different remedies and only one of them is
	// about the filesystem. Empty (never nil), in ref order.
	References []ReferenceVerdict `json:"references"`
	// Changed, Missing and Unpinned are the counts a caller branches on without
	// re-walking the lists.
	Changed  int `json:"changed"`
	Missing  int `json:"missing"`
	Unpinned int `json:"unpinned"`
}

// Sound reports whether every pin still matches AND the pin set is closed —
// the two halves of "is this run's pin story healthy". A run can fail either
// half alone: RUN-59 had all 30 pins matching disk and four judge steps that
// could not be claimed.
func (r *PinReport) Sound() bool {
	return r.Changed == 0 && r.Missing == 0 && r.Unpinned == 0
}

// VerifyPins checks a run's WHOLE pin set against what those refs resolve to
// now (DKT-297).
//
// It exists because no verb answered "is this run's pin state sound". The verbs
// that check pins each check ONLY the pins they themselves read — `step render`
// verifies the template and its own step's packet files, `pinnedSchema` the one
// schema a step declares — which is right for them and made `step render` a
// misleading answer to the whole-run question. Harness RUN-14 had
// `contracts/synthesize-findings.md` drift at 21:20Z; `step render` returned
// exit 0 and a full packet for two steps that did not read that file, and an
// hour later every `synthesize` step in the run was unclaimable for exactly
// that reason. The conductor diagnosed it by comparing `run status --json`
// against `shasum` by hand, which is the ten-line check every seat was
// rewriting.
//
// IT IS A READ, AND IT IS THE ONLY PLACE THE WHOLE SET IS COMPARED. Context
// assembly must never do this: §6.6 is explicit that assembly "never re-reads a
// file a pin names", because the hash IS the contract and re-reading would make
// a bundle depend on the working tree. This verb is the deliberate opposite —
// it asks about the tree, on purpose, and writes nothing.
func VerifyPins(conn *sql.DB, runID int) (*PinReport, error) {
	return verifyPinsClosedIn(conn, runID, instanceConfigRoots())
}

// verifyPinsClosedIn is the whole-run answer: the per-pin check, plus the
// CLOSURE check the per-pin check cannot see (DKT-821).
//
// Matching every pinned ref against disk is not the same question as "is this
// run's pin story healthy", and RUN-59 is the run where the two answers
// diverged. A repin had adopted contract bytes whose `packet_includes` reached
// two fragments the run never pinned; every one of those 30 pins matched its
// file exactly, `verify-pins` said exit 0, and minutes earlier four review
// claims had already died on `packet file ... is not pinned by this run`. The
// verb whose job is the whole-run question reported a structurally wedged run
// as healthy, and a conductor used it as a pre-dispatch health check.
//
// So after checking the pins, resolve what those pinned bytes REFERENCE and
// report anything the pin set does not hold. The walk is unpinnedClosureRefs,
// shared verbatim with the additions repin computes when it adopts bytes
// (DKT-805): one computation, so the verb that detects the hole and the verb
// that closes it can never name different refs.
func verifyPinsClosedIn(conn *sql.DB, runID int, roots []string) (*PinReport, error) {
	report, err := verifyPinsIn(conn, runID, roots)
	if err != nil {
		return nil, err
	}
	closure, err := pendingPacketClosure(conn, runID, roots)
	if err != nil {
		return nil, err
	}
	for _, u := range unpinnedClosureRefs(report.Pins, closure, roots) {
		// A ref that is there and unreadable is still an unpinned reference —
		// readability is the claim-time question, and this one is about the pin
		// set. Naming the path is what lets a reader tell the two apart.
		report.References = append(report.References, ReferenceVerdict{
			Status:     PinUnpinnedReference,
			Ref:        u.ref,
			IncludedBy: u.includedBy,
			RequiredBy: u.requiredBy,
			Path:       u.path,
		})
		report.Unpinned++
	}
	return report, nil
}

// verifyPinsIn is VerifyPins over an explicit root list, which is the same
// seam resolvePacketFiles takes for the same reason: the resolution order is
// the thing under test, and a check that could only run against the process's
// real config directories could not be tested at all.
func verifyPinsIn(conn *sql.DB, runID int, roots []string) (*PinReport, error) {
	pins, err := db.ListPins(conn, runID)
	if err != nil {
		return nil, err
	}
	projectID, err := db.RunProjectID(conn, runID)
	if err != nil {
		return nil, err
	}

	report := &PinReport{
		Run: model.FormatRunID(runID),
		// Both lists are empty rather than nil so `--json` emits arrays on a
		// clean run — the same wire shape a consumer parses either way.
		Pins: []PinVerdict{}, References: []ReferenceVerdict{},
	}

	for _, p := range pins {
		v := PinVerdict{Kind: p.Kind, Ref: p.Ref, Pinned: p.SHA256}
		switch p.Kind {
		case db.PinKindFile:
			verifyFilePin(&v, roots, p)
		case db.PinKindSchema:
			verifySchemaPin(conn, &v, projectID, p)
		case db.PinKindWorkflow:
			verifyWorkflowPin(conn, &v, projectID, p)
		default:
			// An unknown kind is reported, never silently passed: the honest
			// answer to "is this sound" is "there is something here I cannot
			// check", and calling it `ok` would be the wrong default.
			v.Status = PinMissing
		}
		switch v.Status {
		case PinChanged:
			report.Changed++
		case PinMissing:
			report.Missing++
		}
		report.Pins = append(report.Pins, v)
	}

	// A TOTAL ORDER, so an unchanged run produces byte-identical output twice
	// — the same golden-stability discipline every other report here follows.
	sort.SliceStable(report.Pins, func(i, j int) bool {
		if report.Pins[i].Kind != report.Pins[j].Kind {
			return report.Pins[i].Kind < report.Pins[j].Kind
		}
		return report.Pins[i].Ref < report.Pins[j].Ref
	})
	return report, nil
}

// verifyFilePin resolves a file ref the way the packet resolver does — the
// config-relative ref at each root, then the legacy absolute form — so this
// check and the refusal it predicts agree about which file is meant.
func verifyFilePin(v *PinVerdict, roots []string, p db.Pin) {
	candidates := make([]string, 0, len(roots)+1)
	if filepath.IsAbs(p.Ref) {
		// A run activated before v12 pinned the full walked path.
		candidates = append(candidates, p.Ref)
	}
	for _, root := range roots {
		candidates = append(candidates, filepath.Join(root, p.Ref))
	}

	for _, full := range candidates {
		content, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// Unreadable is not "unchanged". Report it as missing rather than
			// letting an EACCES read as sound.
			v.Status, v.Path = PinMissing, full
			return
		}
		// FIRST ROOT THAT HOLDS IT WINS, matching readPinnedPacketFile: falling
		// through on a mismatch would let a stale copy at a later root stand in
		// for the edited one this check exists to find.
		v.Path, v.Found = full, workflow.SHA256(content)
		v.Status = PinOK
		if v.Found != p.SHA256 {
			v.Status = PinChanged
		}
		return
	}
	v.Status = PinMissing
}

// verifySchemaPin checks a registered payload schema against the registry, the
// same comparison `pinnedSchema` makes before validating a payload.
func verifySchemaPin(conn *sql.DB, v *PinVerdict, projectID int, p db.Pin) {
	name, version, err := workflow.ParsePayloadRef(p.Ref)
	if err != nil {
		v.Status = PinMissing
		return
	}
	row, err := db.GetSchema(conn, projectID, name, version)
	if err != nil {
		v.Status = PinMissing
		return
	}
	v.Found, v.Status = row.SourceSHA256, PinOK
	if row.SourceSHA256 != p.SHA256 {
		v.Status = PinChanged
		return
	}
	// A schema whose bytes match but no longer compiles is not sound either,
	// and `pinnedSchema` refuses on exactly that.
	if _, err := schema.Compile(name, version, []byte(row.Body)); err != nil {
		v.Status = PinChanged
	}
}

// verifyWorkflowPin checks a registered definition against the registry.
func verifyWorkflowPin(conn *sql.DB, v *PinVerdict, projectID int, p db.Pin) {
	name, version, err := workflow.ParsePayloadRef(p.Ref)
	if err != nil {
		v.Status = PinMissing
		return
	}
	row, err := db.GetWorkflow(conn, projectID, name, version)
	if err != nil {
		v.Status = PinMissing
		return
	}
	v.Found, v.Status = row.SourceSHA256, PinOK
	if row.SourceSHA256 != p.SHA256 {
		v.Status = PinChanged
	}
}

// PinReportReason renders the unsound pins for a refusal, one clause each, with
// BOTH hashes — an operator needs them to decide between restoring the file and
// starting a new run, which is the same pair every pin refusal already names.
func PinReportReason(r *PinReport) string {
	var out []string
	for _, v := range r.Pins {
		switch v.Status {
		case PinChanged:
			out = append(out, fmt.Sprintf(
				"%s %s changed: pinned %s, on disk %s", v.Kind, v.Ref, v.Pinned, v.Found))
		case PinMissing:
			out = append(out, fmt.Sprintf(
				"%s %s is pinned at %s but does not resolve", v.Kind, v.Ref, v.Pinned))
		}
	}
	// The closure clauses come after the per-pin ones and read differently on
	// purpose: nothing drifted, so neither hash belongs here — what a reader
	// needs is the file that wrote the reference and the ref it names.
	for _, v := range r.References {
		by := strings.Join(v.IncludedBy, ", ")
		if by == "" {
			by = strings.Join(v.RequiredBy, ", ")
		}
		where := "and no instance-config root holds it"
		if v.Path != "" {
			where = "though " + v.Path + " holds it"
		}
		out = append(out, fmt.Sprintf(
			"%s references %s, which %s does not pin (%s)", by, v.Ref, r.Run, where))
	}
	return joinClauses(out)
}

// joinClauses is the "; "-separated form every refusal in this package uses.
func joinClauses(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
