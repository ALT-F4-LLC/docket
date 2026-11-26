package cli

import (
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// TestExportCollectionsCoverEveryExportField ties the export document's shape
// to the table that carries it. Every slice on the document has to belong to
// exactly one collection, so a field added to the document and to nothing else
// fails here — where the failure names the field — instead of shipping as an
// export that writes rows no import will ever read back.
//
// Which field a collection binds is read out of the collection itself rather
// than declared beside it: normalize is the one half whose effect is visible on
// an otherwise-empty document, since it turns exactly one nil slice into an
// empty one. A name repeated here would be one more thing to keep in sync, and
// keeping things in sync by hand is what this table exists to stop.
func TestExportCollectionsCoverEveryExportField(t *testing.T) {
	document := reflect.TypeOf(model.ExportData{})

	boundBy := make(map[string]string, len(exportCollections))
	for _, c := range exportCollections {
		var probe model.ExportData
		c.normalize(&probe)

		var bound []string
		value := reflect.ValueOf(probe)
		for i := 0; i < document.NumField(); i++ {
			if value.Field(i).Kind() == reflect.Slice && !value.Field(i).IsNil() {
				bound = append(bound, document.Field(i).Name)
			}
		}
		if len(bound) != 1 {
			t.Fatalf("collection %q touches %v, want exactly one field", c.name, bound)
		}
		if other, taken := boundBy[bound[0]]; taken {
			t.Errorf("field %s is claimed by both %q and %q", bound[0], other, c.name)
		}
		boundBy[bound[0]] = c.name

		if c.fetch == nil || c.restore == nil || c.count == nil {
			t.Errorf("collection %q is missing one of its halves", c.name)
		}
	}

	for i := 0; i < document.NumField(); i++ {
		field := document.Field(i)
		if field.Type.Kind() != reflect.Slice {
			continue
		}
		if _, covered := boundBy[field.Name]; !covered {
			t.Errorf("field %s belongs to no collection: an export would carry it and an import would drop it", field.Name)
		}
	}
}
