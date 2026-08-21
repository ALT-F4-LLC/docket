package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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

// PinReport is `docket run verify-pins`.
type PinReport struct {
	Run string `json:"run"`
	// Pins is every pin the run holds, in a total order (kind, then ref), so
	// two checks of one unchanged run produce identical output.
	Pins []PinVerdict `json:"pins"`
	// Changed and Missing are the counts a caller branches on without
	// re-walking Pins.
	Changed int `json:"changed"`
	Missing int `json:"missing"`
}

// Sound reports whether every pin still matches.
func (r *PinReport) Sound() bool { return r.Changed == 0 && r.Missing == 0 }

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
	return verifyPinsIn(conn, runID, instanceConfigRoots())
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

	report := &PinReport{Run: model.FormatRunID(runID), Pins: []PinVerdict{}}

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
