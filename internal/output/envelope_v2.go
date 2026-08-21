package output

import (
	"encoding/json"
	"io"
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

// writeJSONSuccessV2 writes a v2 success envelope to w. Collection results are
// reshaped to {items, total, truncated} and versioned payloads gain their
// `version` field; every other result marshals exactly as it does under v1,
// since a scalar payload has no items/total/truncated to report.
func writeJSONSuccessV2(w io.Writer, data any, message string) {
	if v, ok := data.(Versioned); ok {
		data = v.VersionedPayload()
	}

	if c, ok := data.(Collection); ok {
		items := c.CollectionItems()
		if v, ok := items.(Versioned); ok {
			items = v.VersionedPayload()
		}
		data = collectionEnvelope{
			Items:     items,
			Total:     c.CollectionTotal(),
			Truncated: c.CollectionTruncated(),
		}
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(successEnvelope{
		OK:      true,
		Data:    data,
		Message: message,
	})
}
