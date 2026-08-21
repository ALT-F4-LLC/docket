package model

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Schema is a registered payload schema — one `schemas` row
// (docs/tdd/payloads-thresholds.md §4.4).
//
// The shape is `Workflow`'s, deliberately and line for line: name, version,
// source_path, source_sha256, body, a derived column, created_at_ms,
// row_version. Two registries with the same immutability contract that looked
// different would invite two different implementations of that contract.
//
// Version and RowVersion are two different numbers for the reason Workflow
// states: Version is the registration's own — the number a run pins — and
// RowVersion is the CAS column every mutable entity carries.
type Schema struct {
	ID int
	// ProjectID is the v12 tenancy dimension; zero normalizes to the default
	// project on write. The builtin row stays visible to every project via
	// the Builtin flag, whatever project row it happens to sit on.
	ProjectID int
	Name      string
	Version   int
	// SourcePath is provenance only: where the bytes came from. It is never
	// re-read, and it is empty for the builtin, which came from the binary.
	SourcePath   string
	SourceSHA256 string
	// Body is the registered document, VERBATIM. It is what a run validates
	// against (§4.7 P4) and what `schema show --body` emits, so it is retained
	// rather than reconstructed from a parsed form.
	Body string
	// Ordered is the §4.3 derived index as stored JSON. It is a CACHE of a pure
	// function of Body (I2), never a second source of truth.
	Ordered string
	// Builtin is 1 on `aggregate@1` and on nothing else. It records where the
	// bytes came from, not whether a run may use them.
	Builtin     bool
	CreatedAtMS int64
	RowVersion  int
}

// Ref renders the `name@version` identity a run pins.
func (s *Schema) Ref() string {
	return fmt.Sprintf("%s@%d", s.Name, s.Version)
}

// OrderedFields lists the fields the schema declares an order for, sorted.
//
// It reads the STORED index rather than re-walking the document: a read verb
// must not depend on re-parsing bytes, and I2's round-trip check is what keeps
// the two honest.
func (s *Schema) OrderedFields() []string {
	var index map[string][]string
	if err := json.Unmarshal([]byte(s.Ordered), &index); err != nil {
		return nil
	}
	out := make([]string, 0, len(index))
	for name := range index {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// schemaJSON is the wire shape of a registered schema.
//
// `source_sha256` rather than a bare `sha256`, matching `workflow`'s wire shape:
// §4.4's argument is that the two registries must not LOOK different, and a
// consumer reading both would otherwise have to remember which one spells it
// which way. `body` is not included — `schema show --body` emits the registered
// bytes verbatim rather than as a JSON-escaped string, exactly as
// `workflow show --source` does.
type schemaJSON struct {
	Name          string   `json:"name"`
	Version       int      `json:"version"`
	SourcePath    string   `json:"source_path,omitempty"`
	SourceSHA256  string   `json:"source_sha256"`
	OrderedFields []string `json:"ordered_fields"`
	Builtin       bool     `json:"builtin"`
	CreatedAtMS   int64    `json:"created_at_ms"`
}

// MarshalJSON renders the v1 shape.
func (s *Schema) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.wire())
}

func (s *Schema) wire() schemaJSON {
	fields := s.OrderedFields()
	if fields == nil {
		// An empty list rather than null: a consumer iterating `ordered_fields`
		// should not have to special-case a schema that declares no order.
		fields = []string{}
	}
	return schemaJSON{
		Name:          s.Name,
		Version:       s.Version,
		SourcePath:    s.SourcePath,
		SourceSHA256:  s.SourceSHA256,
		OrderedFields: fields,
		Builtin:       s.Builtin,
		CreatedAtMS:   s.CreatedAtMS,
	}
}

// schemaVersionedJSON adds the CAS row version, which surfaces under --json=v2
// only. The fields are restated rather than embedded, for the reason
// workflowVersionedJSON gives: `version` is already the REGISTRATION's version,
// and an embedded struct plus a second `version` field would silently drop one.
type schemaVersionedJSON struct {
	Name          string   `json:"name"`
	Version       int      `json:"version"`
	SourcePath    string   `json:"source_path,omitempty"`
	SourceSHA256  string   `json:"source_sha256"`
	OrderedFields []string `json:"ordered_fields"`
	Builtin       bool     `json:"builtin"`
	CreatedAtMS   int64    `json:"created_at_ms"`
	RowVersion    int      `json:"row_version"`
}

// VersionedPayload implements output.Versioned.
func (s *Schema) VersionedPayload() any {
	base := s.wire()
	return schemaVersionedJSON{
		Name:          base.Name,
		Version:       base.Version,
		SourcePath:    base.SourcePath,
		SourceSHA256:  base.SourceSHA256,
		OrderedFields: base.OrderedFields,
		Builtin:       base.Builtin,
		CreatedAtMS:   base.CreatedAtMS,
		RowVersion:    s.RowVersion,
	}
}

// VersionedSchema renders a schema under --json=v2. It exists so a LIST's items
// each carry the row version: the v2 collection envelope consults
// output.Versioned on the items CONTAINER, not on every element.
type VersionedSchema struct{ Schema }

// MarshalJSON emits the v2 shape for one schema.
func (v VersionedSchema) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.Schema.VersionedPayload())
}

// SchemasWithVersion wraps schemas for v2 marshaling.
func SchemasWithVersion(schemas []*Schema) []VersionedSchema {
	return withVersionSlice(schemas, func(s *Schema) VersionedSchema {
		return VersionedSchema{Schema: *s}
	})
}
