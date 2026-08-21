package cli

import (
	"encoding/csv"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
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

func TestCsvSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"equals", "=HYPERLINK(\"evil\")", "'=HYPERLINK(\"evil\")"},
		{"plus", "+1234", "'+1234"},
		{"minus", "-cmd", "'-cmd"},
		{"at", "@SUM(A1)", "'@SUM(A1)"},
		{"tab", "\t=cmd", "'\t=cmd"},
		{"carriage return", "\r=cmd", "'\r=cmd"},
		{"benign", "normal title", "normal title"},
		{"empty", "", ""},
		{"interior trigger", "a=b", "a=b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := csvSafe(tc.in); got != tc.want {
				t.Errorf("csvSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderExportCSVNeutralizesFormulaInjection(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	issues := []*model.Issue{
		{
			ID:          1,
			Title:       "=HYPERLINK(\"evil\")",
			Description: "benign description",
			Status:      model.StatusTodo,
			Priority:    model.PriorityMedium,
			Kind:        model.IssueKindFeature,
			Assignee:    "@alice",
			Labels:      []string{"bug"},
			Files:       []string{"a.go"},
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}

	out, err := renderExportCSV(issues)
	testsupport.Must(t, err, "renderExportCSV: %v", err)

	records, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	testsupport.Must(t, err, "parse CSV: %v", err)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (header + 1 row)", len(records))
	}

	row := records[1]
	if got := row[2]; got != "'=HYPERLINK(\"evil\")" {
		t.Errorf("title cell = %q, want %q", got, "'=HYPERLINK(\"evil\")")
	}
	if got := row[3]; got != "benign description" {
		t.Errorf("description cell = %q, want untouched %q", got, "benign description")
	}
	if got := row[7]; got != "'@alice" {
		t.Errorf("assignee cell = %q, want %q", got, "'@alice")
	}
}
