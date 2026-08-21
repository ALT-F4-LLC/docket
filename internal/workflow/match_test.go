package workflow

import (
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// mustParseFixture loads the canonical register-test fixture, parsed AND
// validated. Validation matters here: several tests below assert properties
// that only hold for a definition the validator accepted, and reading an
// invalid fixture would prove them of something no run could ever bind.
func mustParseFixture(t *testing.T) *Definition {
	t.Helper()

	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture %s: %v", fixturePath, err)
	def, err := Parse(src)
	testsupport.Must(t, err, "parsing fixture: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("validating fixture: %v", err)
	}
	if err := Lint(def); err != nil {
		t.Fatalf("linting fixture: %v", err)
	}
	return def
}

// TestMatchMatrix is the §5.7 obligation: the `[match]` predicate matrix —
// kind/labels_any/labels_all/unless_labels in every combination, including
// unless_labels beating labels_any, and absent clauses matching anything.
func TestMatchMatrix(t *testing.T) {
	subject := Subject{Kind: "bug", Labels: []string{"backend", "urgent"}}

	cases := []struct {
		name  string
		match *Match
		subj  Subject
		want  bool
	}{
		// An absent [match] binds anything. Exactly-one-match then does the
		// refusing — a match-less workflow must participate in the count or
		// "zero or multiple matches" means nothing.
		{"absent match binds anything", nil, subject, true},
		{"empty match binds anything", &Match{}, subject, true},

		// kind
		{"kind in list", &Match{Kind: []string{"bug", "task"}}, subject, true},
		{"kind not in list", &Match{Kind: []string{"task", "chore"}}, subject, false},
		{"kind absent matches any", &Match{LabelsAny: []string{"backend"}}, subject, true},
		{"kind single match", &Match{Kind: []string{"bug"}}, subject, true},

		// labels_any — intersection
		{"labels_any intersects", &Match{LabelsAny: []string{"backend", "docs"}}, subject, true},
		{"labels_any disjoint", &Match{LabelsAny: []string{"docs", "ui"}}, subject, false},
		{"labels_any on unlabelled issue",
			&Match{LabelsAny: []string{"backend"}}, Subject{Kind: "bug"}, false},

		// labels_all — subset
		{"labels_all subset", &Match{LabelsAll: []string{"backend", "urgent"}}, subject, true},
		{"labels_all partial is not subset",
			&Match{LabelsAll: []string{"backend", "docs"}}, subject, false},
		{"labels_all single", &Match{LabelsAll: []string{"urgent"}}, subject, true},

		// unless_labels — disjoint
		{"unless_labels disjoint", &Match{UnlessLabels: []string{"docs"}}, subject, true},
		{"unless_labels intersects", &Match{UnlessLabels: []string{"urgent"}}, subject, false},

		// The clause that must be evaluated LAST and WIN. An exclusion the
		// author wrote as "not this one" must not be defeated by an inclusion,
		// or work routes through a pipeline written to refuse it.
		{"unless_labels beats labels_any",
			&Match{LabelsAny: []string{"backend"}, UnlessLabels: []string{"urgent"}},
			subject, false},
		{"unless_labels beats labels_all",
			&Match{LabelsAll: []string{"backend", "urgent"}, UnlessLabels: []string{"urgent"}},
			subject, false},
		{"unless_labels beats kind",
			&Match{Kind: []string{"bug"}, UnlessLabels: []string{"backend"}},
			subject, false},

		// Combinations: every clause must hold.
		{"all four hold",
			&Match{
				Kind: []string{"bug"}, LabelsAny: []string{"backend"},
				LabelsAll: []string{"urgent"}, UnlessLabels: []string{"docs"},
			}, subject, true},
		{"kind fails while the rest hold",
			&Match{
				Kind: []string{"chore"}, LabelsAny: []string{"backend"},
				LabelsAll: []string{"urgent"}, UnlessLabels: []string{"docs"},
			}, subject, false},
		{"labels_all fails while the rest hold",
			&Match{
				Kind: []string{"bug"}, LabelsAny: []string{"backend"},
				LabelsAll: []string{"missing"}, UnlessLabels: []string{"docs"},
			}, subject, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.match.Matches(tc.subj); got != tc.want {
				t.Errorf("Matches(%+v) = %v, want %v", tc.subj, got, tc.want)
			}
		})
	}
}

// TestFixtureMatchBinding pins the committed fixture's own [match] clause
// against the kinds it declares and the labels it excludes, so an edit to the
// fixture that changes what it binds is caught here.
func TestFixtureMatchBinding(t *testing.T) {
	def := mustParseFixture(t)

	for _, kind := range []string{"task", "feature", "bug", "chore"} {
		if !def.Match.Matches(Subject{Kind: kind}) {
			t.Errorf("fixture does not bind kind %q, but its [match] lists it", kind)
		}
	}
	if def.Match.Matches(Subject{Kind: "epic"}) {
		t.Error("fixture binds kind \"epic\", which its [match] does not list")
	}

	// Every excluded label wins over the kind that would otherwise bind.
	for _, label := range []string{
		"security-load-bearing", "ui", "docs-only", "investigation",
	} {
		s := Subject{Kind: "bug", Labels: []string{label}}
		if def.Match.Matches(s) {
			t.Errorf("fixture binds an issue labelled %q, which unless_labels excludes", label)
		}
	}
}

// TestWhenHolds covers the `when` evaluator over the V22 grammar. A false
// `when` does not omit a step — expansion creates it `skipped` — so getting
// this wrong changes a step's status, never the topology.
func TestWhenHolds(t *testing.T) {
	subject := Subject{Kind: "bug", Labels: []string{"backend", "urgent"}}

	cases := []struct {
		when string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"kind == bug", true},
		{`kind == "bug"`, true},
		{"kind == task", false},
		{"kind != task", true},
		{"kind != bug", false},
		{"labels contains backend", true},
		{"labels contains docs", false},
		{`labels contains "urgent"`, true},
		{"labels == backend", true},
		{"labels != docs", true},
		{"labels != backend", false},
		{"kind == bug and labels contains backend", true},
		{"kind == bug and labels contains docs", false},
		{"kind == task and labels contains backend", false},
		// A clause V22 would have rejected evaluates FALSE rather than
		// erroring: reaching it means a stored definition predates the current
		// grammar, and skipping the step is the conservative reading.
		{"status == done", false},
	}

	for _, tc := range cases {
		t.Run(tc.when, func(t *testing.T) {
			if got := WhenHolds(tc.when, subject); got != tc.want {
				t.Errorf("WhenHolds(%q) = %v, want %v", tc.when, got, tc.want)
			}
		})
	}
}

// TestFenceTags asserts a definition reports exactly the tags its gates
// declare — the set activation harvests against.
//
// The committed fixture declares NO fenced gate: its `verify` pre-gate is
// `{name = "ac-commands", pre = true}`, a trusted gate name with no `source`.
// That distinction is the point of the first case — a `pre` gate and a `fence:`
// gate are different things, and a FenceTags that confused them would harvest
// against a tag nobody declared. The shipped templates carry the fenced form.
func TestFenceTags(t *testing.T) {
	t.Run("fixture declares no fence tag", func(t *testing.T) {
		if tags := mustParseFixture(t).FenceTags(); len(tags) != 0 {
			t.Errorf("FenceTags() = %v; the fixture's gates declare no `source = \"fence:...\"`", tags)
		}
	})

	t.Run("declared tags, sorted and deduplicated", func(t *testing.T) {
		def := &Definition{Steps: []*Step{
			{Name: "a", Gates: []Gate{
				{Name: "checks", Source: "fence:checks"},
				{Name: "trusted"}, // a bare gate name contributes no tag
			}},
			{Name: "b", Gates: []Gate{
				{Name: "acceptance", Source: "fence:acceptance", Pre: true},
				{Name: "again", Source: "fence:checks"}, // the same tag twice
			}},
		}}

		got := def.FenceTags()
		want := []string{"acceptance", "checks"}
		if len(got) != len(want) {
			t.Fatalf("FenceTags() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("FenceTags() = %v, want %v", got, want)
				break
			}
		}
	})
}

// TestTemplateFenceTags closes the loop the fixture cannot: the shipped
// templates DO declare fenced gates, and activation must see them. A template
// whose fence tag went unread would harvest nothing and stub every gate — which
// looks exactly like a passing run.
func TestTemplateFenceTags(t *testing.T) {
	for name, want := range map[string]string{
		"standard-dev":   "checks",
		"parallel-check": "acceptance",
	} {
		t.Run(name, func(t *testing.T) {
			src, err := Template(name)
			testsupport.Must(t, err, "Template(%q): %v", name, err)
			def, err := Parse(src)
			testsupport.Must(t, err, "parsing template %q: %v", name, err)
			tags := def.FenceTags()
			if len(tags) != 1 || tags[0] != want {
				t.Errorf("template %q FenceTags() = %v, want [%s]", name, tags, want)
			}
		})
	}
}

// TestHarvestFencesIsLiteralAndTagScoped is the §5.7 fence obligation:
// multiple blocks, multiple tags, stable ordinals, and — the load-bearing half
// — a block whose tag no workflow declares is NOT harvested. Harvesting every
// fenced block would make any code sample in an issue body a candidate
// command, which is exactly what declaring the tag prevents.
func TestHarvestFencesIsLiteralAndTagScoped(t *testing.T) {
	body := strings.Join([]string{
		"Some prose describing the work.",
		"",
		"```ac",
		"make build",
		"make test",
		"```",
		"",
		"More prose.",
		"",
		"```sh",
		"rm -rf /",
		"```",
		"",
		"```lint",
		"golangci-lint run",
		"```",
		"",
		"```ac",
		"make vet",
		"```",
	}, "\n")

	got := HarvestFences(body, []string{"ac", "lint"})

	want := []Fence{
		{Tag: "ac", Ordinal: 0, Command: "make build"},
		{Tag: "ac", Ordinal: 1, Command: "make test"},
		{Tag: "lint", Ordinal: 0, Command: "golangci-lint run"},
		// The second `ac` block CONTINUES the tag's ordinal sequence rather
		// than restarting it: the ordinal is part of the run_fences key and
		// must be unique per (issue, tag).
		{Tag: "ac", Ordinal: 2, Command: "make vet"},
	}

	if len(got) != len(want) {
		t.Fatalf("harvested %d fences, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fence %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The undeclared tag's contents appear nowhere — asserted directly rather
	// than inferred from the count, so a future off-by-one cannot hide it.
	for _, f := range got {
		if strings.Contains(f.Command, "rm -rf") {
			t.Errorf("harvested %q from an UNDECLARED `sh` block", f.Command)
		}
		if f.Tag == "sh" {
			t.Errorf("harvested a fence tagged %q, which no workflow declared", f.Tag)
		}
	}

	// No declared tags means no harvest at all, not "harvest everything".
	if n := len(HarvestFences(body, nil)); n != 0 {
		t.Errorf("HarvestFences with no declared tags returned %d rows, want 0", n)
	}
}

// TestHarvestFencesIsVerbatim pins the "no prose parsing" rule (engine-core §6):
// the block's lines are taken byte for byte. A command an operator approved at
// plan time is the command that runs — anything trimmed, reflowed, or
// interpreted makes the approval a summary rather than the thing itself.
func TestHarvestFencesIsVerbatim(t *testing.T) {
	body := "```ac\n  indented --flag \"quoted arg\"\t\n```"

	got := HarvestFences(body, []string{"ac"})
	if len(got) != 1 {
		t.Fatalf("harvested %d fences, want 1: %+v", len(got), got)
	}
	if want := "  indented --flag \"quoted arg\"\t"; got[0].Command != want {
		t.Errorf("harvested %q, want %q verbatim", got[0].Command, want)
	}
}

// TestHarvestFencesInfoStringTag asserts the tag is the info string's FIRST
// TOKEN, so ```ac and ```ac title=x both declare the `ac` fence.
func TestHarvestFencesInfoStringTag(t *testing.T) {
	body := "```ac title=setup\nmake build\n```"

	got := HarvestFences(body, []string{"ac"})
	if len(got) != 1 || got[0].Tag != "ac" || got[0].Command != "make build" {
		t.Errorf("HarvestFences over an info string with attributes = %+v", got)
	}
}
