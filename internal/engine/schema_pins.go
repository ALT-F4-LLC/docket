package engine

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// schemaReference is one `payload` declaration, remembered with the step that
// made it so a refusal can name where to look.
type schemaReference struct {
	ref      string
	workflow string
	step     string
}

// schemaPinsFor builds the `pins` rows for every schema a run's bound workflows
// reference.
//
// The hash comes from the ROW, not from the reference: `ref` says which schema,
// `sha256` says which bytes, and the pair is what makes "two runs of the
// same issue at the same pins reach the same verdict on the same payload" true.
//
// A referenced schema that is not registered is a VALIDATION_ERROR naming
// the workflow, the step, and the schema. That is unreachable through
// `workflow register` — V25a refuses it there — and reachable through a database
// restored from elsewhere, which is the same reasoning ParsePredicate keeps its
// own error for.
func schemaPinsFor(
	tx *sql.Tx,
	projectID int,
	runID int,
	runIssues []*db.RunIssue,
	bindings map[int]*boundDefinition,
) ([]db.Pin, error) {
	pins := make([]db.Pin, 0, len(runIssues))
	for _, reference := range referencedSchemas(runIssues, bindings) {
		name, version, err := workflow.ParsePayloadRef(reference.ref)
		if err != nil {
			// Unreachable through `workflow register` (V25 checks the shape) and
			// reachable through a restored database, exactly as the unregistered
			// case is.
			return nil, validationErr(
				"workflow %s, step %q: `payload` %q is not a `name@version` reference",
				reference.workflow, reference.step, reference.ref)
		}

		registered, err := db.GetSchemaTx(tx, projectID, name, version)
		if errors.Is(err, db.ErrSchemaNotFound) {
			return nil, validationErr(
				"workflow %s, step %q declares `payload = %q`, which is not registered; "+
					"register it with `docket schema register %s <file.json>`",
				reference.workflow, reference.step, reference.ref, reference.ref)
		}
		if err != nil {
			return nil, err
		}

		pins = append(pins, db.Pin{
			RunID: runID, Kind: db.PinKindSchema,
			Ref: registered.Ref(), SHA256: registered.SourceSHA256,
		})
	}
	return pins, nil
}

// referencedSchemas lists the schemas a run's bound workflows name, deduped,
// in a DETERMINISTIC order: run issues in their existing order, then each
// definition's steps in declaration order.
//
// Determinism matters because the pins are golden-diffed: an order that
// varied with map iteration would make the golden flap on nothing.
func referencedSchemas(
	runIssues []*db.RunIssue,
	bindings map[int]*boundDefinition,
) []schemaReference {
	var out []schemaReference
	seen := make(map[string]struct{})

	for _, ri := range runIssues {
		bound := bindings[ri.IssueID]
		if bound == nil || bound.definition == nil {
			continue
		}
		for _, step := range bound.definition.Steps {
			for _, ref := range stepSchemaRefs(step) {
				if _, dup := seen[ref]; dup {
					continue
				}
				seen[ref] = struct{}{}
				out = append(out, schemaReference{
					ref:      ref,
					workflow: bound.workflow.Ref(),
					step:     step.Name,
				})
			}
		}
	}
	return out
}

// stepSchemaRefs is which schemas ONE step brings into the pin set.
//
// The builtin pins like any other referenced schema. That it ships in the
// binary changes where its bytes came from, not whether a run records what it
// used — and a run that cannot say which `aggregate@1` it computed against is a
// run that cannot explain its own output.
func stepSchemaRefs(step *workflow.Step) []string {
	var refs []string
	if step.Payload != "" {
		refs = append(refs, step.Payload)
	}
	if step.Action == schema.AggregateName {
		refs = append(refs, schema.AggregateRef())
	}
	return refs
}

// RegistrationOrder sorts config files into the order auto-registration must
// apply.
//
// **Schemas register in full before workflows.** A workflow that names a schema
// which is not registered yet is a hard VALIDATION_ERROR at `workflow register`
// — the only reading under which "validated against the registered
// schema at register time" is true — so the zero-touch path never reaches that
// refusal only because the ORDER guarantees it cannot. That is a contract, not
// luck, and it is written here, beside the code that creates the dependency.
//
// Within each group the order is lexical by path, for determinism.
//
// The DIRECTORY SCAN that feeds this lives in autoregister.go. The scan hands
// its paths here rather than re-implement the ordering, so this stays the ONE
// definition of what the order is — and autoregister_test.go asserts the
// behavior it buys:
// activation over a config tree holding a workflow and the schema it names
// SUCCEEDS, which it could not if the order were lexical across everything.
func RegistrationOrder(paths []string) []string {
	out := make([]string, len(paths))
	copy(out, paths)
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := isSchemaConfigPath(out[i]), isSchemaConfigPath(out[j])
		if si != sj {
			return si
		}
		return out[i] < out[j]
	})
	return out
}

// isSchemaConfigPath reports whether a config path is a schema document.
//
// It classifies by DIRECTORY — `.docket/config/schemas/` — rather than by
// extension, because the config lifecycle names the directories and a
// `policy.toml` beside a `workflows/` tree would otherwise sort by suffix into
// the wrong half.
func isSchemaConfigPath(path string) bool {
	return strings.Contains(normalizedPath(path), "/schemas/")
}

// normalizedPath puts a path in a form the directory test can match on every
// platform: forward slashes, and a leading and trailing one so `/schemas/`
// matches a first or last segment too.
func normalizedPath(path string) string {
	return "/" + strings.Trim(strings.ReplaceAll(path, `\`, "/"), "/") + "/"
}
