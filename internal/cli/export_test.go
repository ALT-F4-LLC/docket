package cli

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

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
	if err != nil {
		t.Fatalf("renderExportCSV: %v", err)
	}

	records, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
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
