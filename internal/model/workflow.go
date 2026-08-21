package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Workflow is a registered workflow definition — one `workflows` row
// (docs/tdd/engine-spine.md §4.1).
//
// Version and RowVersion are two different numbers and are named apart on
// purpose. Version is the author's `[pipeline].version` from engine-spec §11.1
// — the number runs pin. RowVersion is the CAS column every mutable entity
// carries (reliability-delta §6.1). Collapsing them would make `--if-version`
// mean "the workflow definition version", which a re-register must never
// silently bump.
type Workflow struct {
	ID int
	// ProjectID is the v12 tenancy dimension; zero normalizes to the default
	// project on write.
	ProjectID    int
	Name         string
	Version      int
	Description  string
	SourcePath   string
	SourceSHA256 string
	// Body is the registered TOML, verbatim. It is retained so
	// `workflow show --source` and the content hash mean something auditable.
	Body string
	// Parsed is the canonical JSON of the parsed definition — the PINNED
	// interpretation. Activation and `next` read it on a hot path, so
	// re-parsing the TOML per read would be both slower and a correctness
	// hazard: a parser change would silently re-interpret a pinned definition.
	Parsed      string
	CreatedAtMS int64
	RowVersion  int
	// DeprecatedAtMS is when this version was RETIRED FROM BINDING, or 0 when
	// it still binds.
	//
	// Retirement is a binding-time filter, not a retraction: a retired row is
	// never deleted, stays readable by explicit `@version` and by
	// definitionByID, and a run that already pinned it keeps resolving it. The
	// only thing it stops doing is participating in `[match]` candidacy.
	DeprecatedAtMS int64
}

// Deprecated reports whether this version has been retired from binding.
func (w *Workflow) Deprecated() bool { return w.DeprecatedAtMS > 0 }

// Ref renders the `name@version` identity a run pins.
func (w *Workflow) Ref() string {
	return fmt.Sprintf("%s@%d", w.Name, w.Version)
}

// ParseWorkflowRef splits a `name[@version]` argument. A missing version
// returns 0, which every reader takes to mean "the highest registered".
func ParseWorkflowRef(ref string) (name string, version int, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", 0, fmt.Errorf("empty workflow reference")
	}

	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return ref, 0, nil
	}

	name = ref[:at]
	rest := ref[at+1:]
	if name == "" {
		return "", 0, fmt.Errorf("invalid workflow reference %q: no name before @", ref)
	}
	version, err = strconv.Atoi(rest)
	if err != nil || version < 1 {
		return "", 0, fmt.Errorf(
			"invalid workflow reference %q: version must be an integer >= 1", ref)
	}
	return name, version, nil
}

// workflowJSON is the wire shape of a registered workflow.
//
// `parsed` is emitted as embedded JSON rather than as a string, so a consumer
// reads one document instead of decoding a string that contains another.
type workflowJSON struct {
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	Description  string          `json:"description,omitempty"`
	SourcePath   string          `json:"source_path,omitempty"`
	SourceSHA256 string          `json:"source_sha256"`
	CreatedAtMS  int64           `json:"created_at_ms"`
	Definition   json.RawMessage `json:"definition,omitempty"`
}

// MarshalJSON renders the v1 shape: the registration's identity and provenance
// plus the parsed definition. `body` is NOT included — it is served by
// `workflow show --source`, which emits the stored TOML verbatim rather than
// as a JSON-escaped string.
func (w *Workflow) MarshalJSON() ([]byte, error) {
	return json.Marshal(w.wire())
}

func (w *Workflow) wire() workflowJSON {
	out := workflowJSON{
		Name:         w.Name,
		Version:      w.Version,
		Description:  w.Description,
		SourcePath:   w.SourcePath,
		SourceSHA256: w.SourceSHA256,
		CreatedAtMS:  w.CreatedAtMS,
	}
	if json.Valid([]byte(w.Parsed)) {
		out.Definition = json.RawMessage(w.Parsed)
	}
	return out
}

// workflowVersionedJSON adds the CAS row version, which surfaces under
// --json=v2 only (engine-spec §5) — v1 never consults the Versioned interface,
// so the row version stays invisible there.
//
// The fields are restated rather than embedded: `version` is already taken by
// the DEFINITION's version, and an embedded struct plus a second `version`
// field would silently drop one of them at marshal time. Naming them apart in
// the payload is the same discipline the columns follow — a consumer that
// conflated the two would pin the wrong number.
type workflowVersionedJSON struct {
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	Description  string          `json:"description,omitempty"`
	SourcePath   string          `json:"source_path,omitempty"`
	SourceSHA256 string          `json:"source_sha256"`
	CreatedAtMS  int64           `json:"created_at_ms"`
	Definition   json.RawMessage `json:"definition,omitempty"`
	RowVersion   int             `json:"row_version"`
	// DeprecatedAtMS marks a version retired from binding (DKT-82): without
	// it, binding eligibility was unauditable from list output — all
	// registered versions rendered alike. Omitted while the version still
	// binds, per the "a field that is not a fact does not appear" rule. v2
	// only: v1 is frozen and never gains fields.
	DeprecatedAtMS int64 `json:"deprecated_at_ms,omitempty"`
}

// VersionedWorkflow renders a workflow under --json=v2, carrying the CAS row
// version. It exists so a LIST's items each carry it too: the v2 collection
// envelope consults output.Versioned on the items container, not on every
// element, so a bare []*Workflow would render v1 items inside a v2 envelope.
type VersionedWorkflow struct{ Workflow }

// MarshalJSON emits the v2 shape for one workflow.
func (v VersionedWorkflow) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.Workflow.VersionedPayload())
}

// WorkflowsWithVersion wraps workflows for v2 marshaling. Callers pass the result as
// the v2 payload; the v1 path keeps using the bare Workflow values, which is
// the dormancy guarantee at the type level.
func WorkflowsWithVersion(workflows []*Workflow) []VersionedWorkflow {
	return withVersionSlice(workflows, func(wf *Workflow) VersionedWorkflow {
		return VersionedWorkflow{Workflow: *wf}
	})
}

// VersionedPayload implements output.Versioned.
func (w *Workflow) VersionedPayload() any {
	base := w.wire()
	return workflowVersionedJSON{
		Name:           base.Name,
		Version:        base.Version,
		Description:    base.Description,
		SourcePath:     base.SourcePath,
		SourceSHA256:   base.SourceSHA256,
		CreatedAtMS:    base.CreatedAtMS,
		Definition:     base.Definition,
		RowVersion:     w.RowVersion,
		DeprecatedAtMS: w.DeprecatedAtMS,
	}
}
