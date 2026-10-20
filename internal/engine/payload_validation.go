package engine

import (
	"database/sql"
	"errors"

	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// validateDeclaredPayload validates `--payload-file` against the step's
// declared `payload = schema@ver`, inside stage 0, BEFORE the artifact records.
//
// C1: a step that declares NO `payload` is unchanged. Shape-only validation,
// and no new refusal exists for it. That is the dormancy claim
// at the verb: a repo whose definitions declare no payload cannot tell v9 from
// v8 here.
func validateDeclaredPayload(
	conn *sql.DB,
	step *store.Step,
	spec *workflow.Step,
	payload []byte,
) error {
	if spec.Payload == "" {
		return nil // C1.
	}

	// An `action` step PRODUCES its payload internally, and that payload is
	// validated where it is produced — against the step's declared schema AND
	// `aggregate@1` — rather than here.
	//
	// The distinction is who authored the bytes. Stage 0 validates what a WORKER
	// submitted, and an action step has no worker: whatever artifact a caller
	// happens to complete it with is not the result the threshold routes on.
	// Refusing here would make the fixture's `reconcile` — an aggregate step
	// that V29 REQUIRES to declare a payload — impossible to complete.
	if spec.StepClass() == workflow.ClassAction {
		return nil
	}

	// C4: an ABSENT payload on a step that declares one is a refusal naming the
	// schema. A declared payload is a contract; silently recording no payload
	// would make every threshold over it evaluate against the empty set and
	// route `pass` — a silent misroute produced by an omission.
	if len(payload) == 0 {
		return validationErr(
			"step %s declares `payload = %q` and no --payload-file was given; "+
				"a declared payload is a contract, and recording none would make "+
				"every threshold over it evaluate against the empty set",
			step.Instance, spec.Payload)
	}

	registered, err := pinnedSchema(conn, step.RunID, spec.Payload)
	if err != nil {
		return err
	}

	// C2/C3: the refusal is path-precise, rendered from the validator's own
	// instance location, and capped at five lines.
	if err := registered.ValidatePayload(payload); err != nil {
		var perr *schema.PayloadError
		if errors.As(err, &perr) {
			return validationErr("step %s: %s", step.Instance, perr.Error())
		}
		return validationErr("step %s: %v", step.Instance, err)
	}
	return nil
}

// pinnedSchema compiles the schema a run PINNED, not the one the registry holds
// today.
//
// This is the whole reason schemas pin. Reading the live table would make a
// verdict depend on when `complete` happened to run, and determinism requires
// the opposite: the same run at the same pins reaches the same verdict on the same
// payload.
//
// The pin records `ref` and `sha256`; the BYTES come from the registry row that
// still carries that hash. A row whose hash no longer matches the pin is a
// registry someone edited underneath a run, which cannot happen through any verb
// — `name@version` is frozen — and the refusal says so rather than validating
// against bytes the run never agreed to.
func pinnedSchema(conn *sql.DB, runID int, ref string) (*schema.Registered, error) {
	st := store.StoreOn(conn)
	pins, err := st.ListPins(runID)
	if err != nil {
		return nil, err
	}
	// The RUN's project scopes the registry read — a pinned ref resolves in
	// the project that pinned it, never the invoking process's.
	projectID, err := st.RunProjectID(runID)
	if err != nil {
		return nil, err
	}

	var pinnedHash string
	for _, p := range pins {
		if p.Kind == store.PinKindSchema && p.Ref == ref {
			pinnedHash = p.SHA256
			break
		}
	}
	if pinnedHash == "" {
		return nil, validationErr(
			"schema %s is declared by this step but was not pinned by the run; "+
				"re-activation inherits its original pin set, so a schema "+
				"registered mid-run does not enter a live run", ref)
	}

	name, version, err := workflow.ParsePayloadRef(ref)
	if err != nil {
		return nil, validationErr("schema reference %q: %v", ref, err)
	}

	row, err := st.GetSchema(projectID, name, version)
	if errors.Is(err, store.ErrSchemaNotFound) {
		return nil, validationErr(
			"schema %s is pinned by this run but is no longer registered", ref)
	}
	if err != nil {
		return nil, err
	}
	if row.SourceSHA256 != pinnedHash {
		return nil, validationErr(
			"schema %s is registered as %s but this run pinned %s; "+
				"a registered name@version is frozen, so validating against the "+
				"current bytes would apply criteria the run never agreed to",
			ref, row.SourceSHA256, pinnedHash)
	}

	registered, err := schema.Compile(name, version, []byte(row.Body))
	if err != nil {
		return nil, validationErr("pinned schema %s no longer compiles: %v", ref, err)
	}
	return registered, nil
}
