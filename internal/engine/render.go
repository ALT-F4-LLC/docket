package engine

import (
	"bytes"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"text/template"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Rendering — TDD §6.11 and §6.11.1.
//
// `step render` formats the context bundle into a work packet through a
// template: the shipped generic default, or `--template F`.
//
// PACKET LAYOUT IS CORE MECHANICS; PACKET CONTENT IS INSTANCE DATA (§2). The
// default template below emits framing (run/issue/step ids, scope), the input
// artifacts each delimited and labeled in DECLARED order, the pinned-file list
// with hashes, and the output instruction — and it names NO instance concept.
// The genericity gate checks its bytes like any other core surface.
//
// WHAT A PACKET DRAWS FROM — the exhaustive list, for anyone deciding where
// to put words they need a worker to read (DKT-725). A packet is the context
// bundle (§6.6's five sources: the pinned step definition, the issue's
// ACTIVATION-FROZEN body_snapshot and issue_snapshot, recorded input
// artifacts, and the pin list) plus the step's declared packet files and the
// step's OWN routing record, rendered as `== RESOLUTION`. Two consequences
// operators repeatedly discover the hard way:
//
//   - issue COMMENTS never render. They are an audit surface, not a context
//     source, and no template can reach them.
//   - a mid-run `description` edit never renders. The packet reads
//     `body_snapshot`, frozen at activation — §9 item 5's edit immunity.
//   - a mid-run `--scope` edit never renders either, and for the same reason:
//     the brief's `scope:` line reads `issue_snapshot`, and so does the scope
//     the step's `issue.diff` is recorded over (DKT-741). There is no verb
//     that refreshes it; an authorized mid-run widen is made real by taking
//     the issue out of the run and re-planning it, and `issue edit --scope`
//     says so when it lands on an issue with live steps in a live run.
//
// The sanctioned steering channel is the resolve note: `step resolve --as
// retry|rerun-gates -m` renders on the same step's re-execution, and `--as
// fix-round -m` is stamped onto the new round's rows (stampEntryRouting) so
// the authorization's remedy reaches the round it paid for.

// defaultPacket is the shipped template. It lives under internal/ because the
// Vorpal build's include list requires embeds there — the same constraint the
// `workflow init` templates are under.
//
//go:embed packets/default.tmpl
var defaultPacket string

// packetData is what the template sees: the context bundle plus the two facts
// about the step's OUTPUT that the bundle does not carry, since the bundle
// describes inputs.
type packetData struct {
	*Context
	// Emits is the artifact kind the step must produce (§4.3.1).
	Emits string
	// PayloadSchema is `payload`'s `schema@ver`, or "" — carried so the packet
	// can state the contract even though the SCHEMA REGISTER is S5's.
	PayloadSchema string
	// Files are the step's declared packet files, resolved in declared order
	// with each entry followed by its own declared includes (§1.4).
	//
	// Core attaches no meaning to any entry: the path, the hash, and the bytes
	// are handed to the template, and the TEMPLATE decides how they appear.
	// This is the field whose absence made the corpus inert — `--template F`
	// could not recover the content because no field carried it.
	Files []PacketFile
}

// RenderResult is one rendered work packet, with the provenance of the template
// that produced it.
type RenderResult struct {
	Packet string `json:"packet"`
	// Template names the template used: "default" for the embedded one, or the
	// file path.
	Template string `json:"template"`
	// TemplatePinned reports whether the template's bytes are pinned by the run.
	// It rides on the result so `--meta` can report an UNPINNED template and the
	// reproducibility gap is VISIBLE rather than assumed (§6.11.1) — a packet
	// rendered through an unpinned file is reproducible only to the extent the
	// operator chose.
	TemplatePinned bool `json:"template_pinned"`
}

// RenderStep renders a step's work packet.
//
// `templatePath` is `--template F`, or "" for the shipped default.
func RenderStep(
	conn *sql.DB, stepID int, templatePath string, nowMS int64,
) (*RenderResult, error) {
	return RenderStepAs(conn, stepID, templatePath, "", nowMS)
}

// RenderStepAs is RenderStep with the `{executor}` substitution resolved to a
// caller-supplied hint (DKT-70).
//
// The declared hint is a DECLARATION, and some instances resolve it further
// at dispatch time — a policy table keyed by issue labels picks the actual
// executor after the engine has offered the step. Substitution used to bind
// to the declared hint alone, so a label-resolved executor could never
// receive its own contract: the corpus shipped per-resolved-hint files that
// no packet could ever name. The resolved hint arrives here, at render time,
// which is where substitution ALREADY re-derives — and the resolved contract
// verifies against its pin like any other entry, while a hint whose contract
// does not exist refuses loudly naming the exact path. Since DKT-581,
// activation pins the packet CLOSURE rather than the whole config tree —
// entries substituted with the declared executor and fanout hints — so a
// resolved hint outside the workflow's own declarations resolves only if the
// corpus declares it somewhere in the bound definition (the shipped corpus
// declares label-resolved executors as `when`-gated steps, which the closure
// covers) or the operator pinned its contract with `--pin`.
//
// An empty executor is the declared behavior, unchanged. The override also
// lands on the rendered step row's `executor`, so the packet's `target:`
// line names who the work is actually for.
func RenderStepAs(
	conn *sql.DB, stepID int, templatePath, executor string, nowMS int64,
) (*RenderResult, error) {
	bundle, err := ReadContext(conn, stepID, nowMS)
	if err != nil {
		return nil, err
	}
	if executor != "" {
		bundle.Step.Executor = executor
	}

	step, err := db.GetStep(conn, stepID)
	if err != nil {
		return nil, err
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	tally, err := loadHoldTally(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	spec := materializedSpec(defs[step.WorkflowID], step, tally)
	if spec == nil {
		return nil, validationErr("step %s: %q is not a step of its pinned workflow",
			step.Instance, step.StepName)
	}

	source, name, pinned, err := templateSource(conn, step.RunID, templatePath)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("packet").Parse(source)
	if err != nil {
		return nil, validationErr("template %s is not a valid text/template: %v", name, err)
	}

	// The declared packet files, resolved to VERIFIED BYTES. Before
	// this, the corpus was inert: `== PINNED` listed paths and hashes, and
	// nothing opened them.
	//
	// Substitution is re-derived here rather than stored on the step, so no
	// schema change is needed: the pinned definition and the step's own
	// executor hint are both already in hand, and they are exactly what
	// expansion used.
	files, err := stepPacketFiles(conn, step, spec, executor)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	data := packetData{
		Context: bundle,
		Files:   files,
		Emits:   workflow.ArtifactKind(spec),
		// `payload` names a `schema@ver` (§11.1). Carrying it is not the same as
		// VALIDATING against it: the schema register lands at S5, and the packet
		// stating the contract is what lets a worker satisfy it before the
		// engine can check it.
		PayloadSchema: spec.Payload,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("rendering the packet for %s: %w", step.Instance, err)
	}

	return &RenderResult{
		Packet: buf.String(), Template: name, TemplatePinned: pinned,
	}, nil
}

// templateSource resolves the template's bytes, VERIFYING A PINNED PATH against
// its recorded hash (§6.11.1).
//
// This closes the one hole in the reproducibility story. `--template F` reads a
// file AT RENDER TIME, but activation may have pinned that path. Without a
// check, a post-activation edit silently changes every packet the run renders
// from then on, while `step context` stays byte-identical — so the §9-item-5
// determinism goldens keep passing and the thing actually handed to a worker
// has changed.
//
//	path pinned, bytes match   render proceeds
//	path pinned, bytes differ  CONFLICT (exit 4), BOTH hashes named
//	path pinned, file absent   NOT_FOUND (exit 2) — the pin says the run needs it
//	path not pinned            proceeds unverified; --meta says so
//	the shipped default        always reproducible; it ships in the binary
//
// The refusal is CONFLICT and not VALIDATION_ERROR because the REQUEST is
// well-formed: the state disagrees with the pin. That is the same reading as a
// re-register with differing bytes (§4.5) and an `--if-version` mismatch, so
// the taxonomy stays consistent and no new code is introduced.
func templateSource(
	conn *sql.DB, runID int, path string,
) (source, name string, pinned bool, err error) {
	if path == "" {
		// The embedded default ships in the binary, so there is no file to
		// drift: it is always reproducible.
		return defaultPacket, "default", true, nil
	}

	pins, err := db.ListPins(conn, runID)
	if err != nil {
		return "", "", false, err
	}
	var pinnedHash string
	for _, p := range pins {
		if p.Kind == db.PinKindFile && p.Ref == path {
			pinnedHash = p.SHA256
			break
		}
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if pinnedHash != "" {
				// The pin says the run depends on this file. Its absence is a
				// missing dependency, not a bad request.
				return "", "", false, notFoundErr(err,
					"template %s is pinned by %s but is no longer on disk",
					path, model.FormatRunID(runID))
			}
			return "", "", false, notFoundErr(err, "template %s not found", path)
		}
		return "", "", false, fmt.Errorf("reading template %s: %w", path, err)
	}

	if pinnedHash == "" {
		// Unverified, and REPORTED as such rather than silently accepted.
		return string(content), path, false, nil
	}

	if got := workflow.SHA256(content); got != pinnedHash {
		// Never a warning, never a silent re-pin. Both hashes are named because
		// an operator needs to know whether to restore the file or re-activate.
		return "", "", false, conflictErr(
			"template %s has changed since %s pinned it: pinned %s, on disk %s; "+
				"restore the file or start a new run",
			path, model.FormatRunID(runID), pinnedHash, got)
	}

	return string(content), path, true, nil
}
