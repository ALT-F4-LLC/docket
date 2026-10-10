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
	// SourceStatus is the DISK VERDICT on SourcePath: whether the bytes at
	// that path still hash to SourceSHA256 (DKT-590).
	//
	// It is NOT a column. Nothing loading a workflow row populates it; a
	// reader that chose to go and LOOK sets it (engine.CheckWorkflowSource),
	// and it stays nil everywhere else — so a payload that never checked
	// carries no key at all rather than a default that reads as an answer.
	// That is the same "a field that is not a fact does not appear" rule the
	// optional wire fields follow.
	SourceStatus *WorkflowSourceStatus `json:"-"`
	// Origin is the DISK VERDICT on this row's NAME: whether any file in the
	// instance-config roots still declares it (DKT-609).
	//
	// Like SourceStatus it is NOT a column and is populated by a reader that
	// chose to go and look (engine.WorkflowOriginIndex), staying nil
	// everywhere else so a payload that never scanned carries no key rather
	// than a default that reads as an answer.
	Origin *WorkflowOriginStatus `json:"-"`
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

// WorkflowSourceState is the verdict on a registered workflow's recorded source
// file (DKT-590). The four values are DIFFERENT FACTS and are never collapsed:
// an operator acts on each of them differently.
type WorkflowSourceState string

const (
	// WorkflowSourceMatches: the file at source_path hashes to source_sha256.
	// What is registered is what is on disk.
	WorkflowSourceMatches WorkflowSourceState = "matches"
	// WorkflowSourceDrifted: the file is readable and hashes to something
	// ELSE. The definition an operator edits at that path is not the
	// definition a run binding this name@version would execute — the silent
	// divergence DKT-590 is about.
	WorkflowSourceDrifted WorkflowSourceState = "drifted"
	// WorkflowSourceUnreadable: the file could not be read at all — deleted,
	// replaced by a directory, or permission-denied. NOT the same as drift:
	// the registered bytes are intact and still reproduce, only their
	// provenance no longer resolves.
	WorkflowSourceUnreadable WorkflowSourceState = "unreadable"
	// WorkflowSourceUnchecked: no comparison was attempted, because the
	// recorded path cannot answer the question from here — nothing was
	// recorded (a stdin registration), or what was recorded is RELATIVE and
	// would resolve against whatever directory happens to be current. Saying
	// "unchecked" is the honest answer; resolving a relative provenance path
	// against an unrelated cwd would manufacture drift out of a different
	// file that happens to share a name.
	WorkflowSourceUnchecked WorkflowSourceState = "unchecked"
)

// WorkflowSourceStatus is one verdict on one registered workflow's source file.
type WorkflowSourceStatus struct {
	State WorkflowSourceState `json:"state"`
	// Path is the recorded source_path, verbatim — the value compared
	// against, so a reader can tell WHICH file was (or was not) read.
	Path             string `json:"path,omitempty"`
	RegisteredSHA256 string `json:"registered_sha256"`
	// CurrentSHA256 is the hash of the bytes found at Path, present only when
	// the file was actually read.
	CurrentSHA256 string `json:"current_sha256,omitempty"`
	// Reason explains an `unreadable` or `unchecked` verdict. The two other
	// states are self-explaining and carry none.
	Reason string `json:"reason,omitempty"`
}

// Drifted reports the one state that means the registry and the disk disagree
// about bytes that both exist.
func (s *WorkflowSourceStatus) Drifted() bool {
	return s != nil && s.State == WorkflowSourceDrifted
}

// WorkflowOriginState is the verdict on whether a registered workflow's NAME
// still has a source file in the instance-config roots (DKT-609).
//
// It is a SIBLING of WorkflowSourceState and answers a different question. The
// source check asks "do the bytes at THIS row's recorded path still hash to
// what was registered" — the drift case, where one file changed. This asks "is
// there any file, anywhere in the configured roots, that still declares this
// NAME" — the rename case, where the old file is gone from disk entirely and
// its recorded path answers for nothing.
//
// THE VERDICT IS PER NAME, NOT PER VERSION, and deliberately so. A bumped
// definition leaves every superseded version's recorded path holding the new
// version's bytes, and calling all of those orphans would light up the entire
// version history of every workflow in the corpus. A name whose file was
// renamed away has no file declaring it at ANY version, which is exactly the
// state that wedged RUN-45.
type WorkflowOriginState string

const (
	// WorkflowOriginPresent: some file in some instance-config root declares
	// this workflow's name. A rename did not strand it.
	WorkflowOriginPresent WorkflowOriginState = "present"
	// WorkflowOriginOrphaned: the roots were scanned and NO file in any of
	// them declares this name. The registration outlives its definition — the
	// residue of a rename or a deleted file — and it keeps binding, because a
	// registration is a row and not a file.
	WorkflowOriginOrphaned WorkflowOriginState = "orphaned"
	// WorkflowOriginUnchecked: nothing was scanned, so the question was not
	// answered. No instance-config root exists on this machine, or the scan
	// itself failed. Reporting "orphaned" from an unscanned root would call
	// every registration in the store an orphan on the strength of having
	// looked nowhere.
	WorkflowOriginUnchecked WorkflowOriginState = "unchecked"
)

// WorkflowOriginStatus is one verdict on one registered workflow NAME.
type WorkflowOriginStatus struct {
	State WorkflowOriginState `json:"state"`
	// Roots are the instance-config roots that were scanned, in precedence
	// order, so a reader can tell WHERE the name was looked for.
	Roots []string `json:"roots,omitempty"`
	// Path is the file that declares this name, present only on a `present`
	// verdict. It is the file found by scanning FRESH, which is why it can
	// differ from the row's recorded source_path.
	Path string `json:"path,omitempty"`
	// Reason explains an `unchecked` verdict. The two others are
	// self-explaining and carry none.
	Reason string `json:"reason,omitempty"`
}

// Orphaned reports the one state that means a registered name has outlived
// every file that declared it.
func (s *WorkflowOriginStatus) Orphaned() bool {
	return s != nil && s.State == WorkflowOriginOrphaned
}

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
	// SourceStatus reaches v1 WHEN A READER CHECKED (DKT-590), the same
	// omitempty shape `scope` and `resolution` took onto `issue show`: a
	// payload from a reader that did not look is byte-identical to the frozen
	// v1 output, while `workflow show` stops reporting a source_path and a
	// source_sha256 that silently disagree with the bytes at that path.
	SourceStatus *WorkflowSourceStatus `json:"source_status,omitempty"`
	// Origin reaches v1 on the same terms and for the same reason (DKT-609):
	// omitted entirely by a reader that did not scan the instance-config
	// roots, so the frozen v1 payload is byte-identical everywhere the
	// question was never asked.
	Origin *WorkflowOriginStatus `json:"origin,omitempty"`
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
		SourceStatus: w.SourceStatus,
		Origin:       w.Origin,
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
	// SourceStatus is v1's field carried forward unchanged; see workflowJSON.
	SourceStatus *WorkflowSourceStatus `json:"source_status,omitempty"`
	// Origin is likewise v1's field carried forward unchanged.
	Origin     *WorkflowOriginStatus `json:"origin,omitempty"`
	RowVersion int                   `json:"row_version"`
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
		SourceStatus:   base.SourceStatus,
		Origin:         base.Origin,
		RowVersion:     w.RowVersion,
		DeprecatedAtMS: w.DeprecatedAtMS,
	}
}
