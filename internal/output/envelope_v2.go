package output

import (
	"encoding/json"
	"io"
	"maps"
)

// Collection is implemented by result types that are a list of items, so the
// v2 envelope can render them uniformly as {items, total, truncated}
// (engine-spec.md §5).
//
// Total is the number of matching records BEFORE any limit is applied — that
// is the whole point of reporting it separately from len(items). A result whose
// total is only knowable post-limit must compute the true total before
// truncating rather than implementing this interface dishonestly.
//
// The v1 path never consults this interface, which is the dormancy guarantee at
// the type level: adding an implementation cannot change v1 output.
// Method names are suffixed to avoid colliding with the Total/Items JSON
// fields that result structs already carry — a struct cannot have a field and
// a method of the same name.
type Collection interface {
	// CollectionItems returns the (possibly truncated) list of records.
	CollectionItems() any
	// CollectionTotal returns the count of matching records before limiting.
	CollectionTotal() int
	// CollectionTruncated reports whether records were dropped by a limit.
	CollectionTruncated() bool
}

// CollectionSiblings is implemented by a Collection that reports fields beside
// items, total and truncated in its v2 envelope. Each returned key becomes a
// sibling of those three; the three are reserved and a sibling cannot replace
// them. A Collection without this interface keeps exactly the three keys.
type CollectionSiblings interface {
	CollectionSiblings() map[string]any
}

// collectionEnvelope is the v2 data shape for list results.
type collectionEnvelope struct {
	Items     any  `json:"items"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

// IsTruncated is the standard truncation computation: records were dropped iff
// a positive limit was in effect and the true total exceeds what was returned.
func IsTruncated(limit, total, returned int) bool {
	return limit > 0 && total > returned
}

// Versioned is implemented by payloads that carry an optimistic-concurrency
// version. Under v2 the value is surfaced as `version` in .data
// (engine-spec.md §5); v1 never consults this interface, so the version stays
// invisible there.
type Versioned interface {
	// VersionedPayload returns the value to marshal in place of the receiver,
	// carrying its version field.
	VersionedPayload() any
}

// shapeV2Data returns the v2 form of a payload. Collection results are
// reshaped to {items, total, truncated} and versioned payloads gain their
// `version` field; every other result is returned unchanged, since a scalar
// payload has no items/total/truncated to report.
func shapeV2Data(data any) any {
	if v, ok := data.(Versioned); ok {
		data = v.VersionedPayload()
	}

	if c, ok := data.(Collection); ok {
		items := c.CollectionItems()
		if v, ok := items.(Versioned); ok {
			items = v.VersionedPayload()
		}
		envelope := collectionEnvelope{
			Items:     items,
			Total:     c.CollectionTotal(),
			Truncated: c.CollectionTruncated(),
		}
		data = envelope
		if s, ok := c.(CollectionSiblings); ok {
			data = withSiblings(envelope, s.CollectionSiblings())
		}
	}
	return data
}

// withSiblings flattens envelope and siblings into one object, writing the
// three envelope keys last so no sibling can shadow them.
func withSiblings(envelope collectionEnvelope, siblings map[string]any) map[string]any {
	merged := make(map[string]any, len(siblings)+3)
	maps.Copy(merged, siblings)
	merged["items"] = envelope.Items
	merged["total"] = envelope.Total
	merged["truncated"] = envelope.Truncated
	return merged
}

// writeJSONSuccessV2 writes a v2 success envelope to w, with data in its v2
// form (see shapeV2Data).
func writeJSONSuccessV2(w io.Writer, data any, message string) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(successEnvelope{
		OK:      true,
		Data:    shapeV2Data(data),
		Message: message,
	})
}
