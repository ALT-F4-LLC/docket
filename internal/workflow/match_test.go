package workflow

import (
	"os"
	"slices"
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

// TestLabelGapFor covers DKT-1182's grammar half: what a subject would have to
// be labelled for a `[match]` to admit it, and when no labelling could.
//
// The `reachable=false` rows are the ones that keep the binding lint quiet where
// it should be. A kind the workflow does not list is not a mis-label, and an
// `unless_labels` entry that fires is the author saying "not this one" about
// this very issue — advising an operator to defeat either would be advice to
// break a decision somebody made on purpose.
func TestLabelGapFor(t *testing.T) {
	subject := Subject{Kind: "bug", Labels: []string{"qa"}}

	cases := []struct {
		name          string
		match         *Match
		subj          Subject
		wantReachable bool
		wantAll       []string
		wantAny       []string
	}{
		{"absent match has no gap", nil, subject, true, nil, nil},
		{"already matching has no gap",
			&Match{LabelsAny: []string{"qa", "ui"}}, subject, true, nil, nil},

		// The HRN-1118 shape: one `labels_any` entry short.
		{"labels_any missing entirely",
			&Match{LabelsAny: []string{"ui"}}, subject, true, nil, []string{"ui"}},
		{"labels_any reports every alternative",
			&Match{LabelsAny: []string{"ui", "frontend"}}, subject, true,
			nil, []string{"ui", "frontend"}},

		{"labels_all reports only what is missing",
			&Match{LabelsAll: []string{"qa", "ui", "reviewed"}}, subject, true,
			[]string{"ui", "reviewed"}, nil},
		{"both clauses report separately",
			&Match{LabelsAll: []string{"ui"}, LabelsAny: []string{"frontend"}},
			subject, true, []string{"ui"}, []string{"frontend"}},

		// Unreachable: no label could close these.
		{"kind excludes",
			&Match{Kind: []string{"chore"}, LabelsAny: []string{"ui"}},
			subject, false, nil, nil},
		{"unless_labels fires",
			&Match{LabelsAny: []string{"ui"}, UnlessLabels: []string{"qa"}},
			subject, false, nil, nil},
		{"kind admits, gap stands",
			&Match{Kind: []string{"bug"}, LabelsAny: []string{"ui"}},
			subject, true, nil, []string{"ui"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gap, reachable := tc.match.LabelGapFor(tc.subj)
			if reachable != tc.wantReachable {
				t.Fatalf("reachable = %v, want %v", reachable, tc.wantReachable)
			}
			if !slices.Equal(gap.MissingAll, tc.wantAll) {
				t.Errorf("MissingAll = %v, want %v", gap.MissingAll, tc.wantAll)
			}
			if !slices.Equal(gap.MissingAny, tc.wantAny) {
				t.Errorf("MissingAny = %v, want %v", gap.MissingAny, tc.wantAny)
			}
			// Empty() and Matches() must agree on a reachable subject: a gap of
			// nothing is exactly the state that already binds. Disagreement
			// would make the lint either silent on real mis-bindings or noisy
			// about issues that bound correctly.
			if reachable && gap.Empty() != tc.match.Matches(tc.subj) {
				t.Errorf("gap.Empty() = %v but Matches() = %v",
					gap.Empty(), tc.match.Matches(tc.subj))
			}
		})
	}
}

// TestDomainPathsParseAndBindNothing pins both halves of the advisory field: it
// decodes under `[match]` (strict decoding would refuse an unknown key), and it
// changes no binding — the same subject binds the same way with the paths there
// and with them absent.
func TestDomainPathsParseAndBindNothing(t *testing.T) {
	const src = `
[pipeline]
name = "ui-ish"
version = 1
[match]
labels_any = ["ui"]
domain_paths = ["internal/tui/**", "web/"]
[[step]]
name = "implement"
executor = "worker"
emits = "change-summary"
after = []
`
	def, err := Parse([]byte(src))
	testsupport.Must(t, err, "parsing a definition with domain_paths: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("validating: %v", err)
	}

	want := []string{"internal/tui/**", "web/"}
	if !slices.Equal(def.Match.DomainPaths, want) {
		t.Errorf("domain_paths = %v, want %v", def.Match.DomainPaths, want)
	}

	// Binding is unchanged: the paths select nothing.
	inDomainWrongLabel := Subject{Kind: "bug", Labels: []string{"qa"}}
	if def.Match.Matches(inDomainWrongLabel) {
		t.Error("domain_paths admitted an issue its labels_any excludes — " +
			"the field must bind nothing")
	}
	if !def.Match.Matches(Subject{Kind: "bug", Labels: []string{"ui"}}) {
		t.Error("domain_paths changed what the label clause admits")
	}

	// And it round-trips through the canonical form runs actually read.
	canonical, err := Canonical(def)
	testsupport.Must(t, err, "canonicalizing: %v", err)
	restored, err := FromCanonical(canonical)
	testsupport.Must(t, err, "restoring: %v", err)
	if !slices.Equal(restored.Match.DomainPaths, want) {
		t.Errorf("domain_paths after round trip = %v, want %v",
			restored.Match.DomainPaths, want)
	}
}

// TestCanonicalFormUnchangedWithoutDomainPaths is the dormancy assertion: a
// definition that declares no domain must serialize byte-identically to what it
// always did, or every already-registered workflow would look like a CONFLICT on
// an idempotent re-register.
func TestCanonicalFormUnchangedWithoutDomainPaths(t *testing.T) {
	def := mustParseFixture(t)
	canonical, err := Canonical(def)
	testsupport.Must(t, err, "canonicalizing the fixture: %v", err)
	if strings.Contains(string(canonical), "domain_paths") {
		t.Errorf("canonical form of a domain-less definition mentions "+
			"domain_paths: %s", canonical)
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
		// `contains-any` (DKT-550): the clause holds when the list and the
		// issue's labels intersect. The empty-intersection row is the one that
		// matters — a membership test that held vacuously would route every
		// issue through the specialized lane it was written to select.
		{"labels contains-any (backend)", true},
		{"labels contains-any (docs)", false},
		{"labels contains-any (docs, backend)", true},
		{"labels contains-any (backend, docs)", true},
		{"labels contains-any (docs, frontend)", false},
		{"labels contains-any (backend, urgent)", true},
		{`labels contains-any ("docs", "urgent")`, true},
		{"labels contains-any(backend,docs)", true},
		{"labels contains-any   (  docs ,  urgent  )", true},
		// One homogeneous-`and` predicate carrying a set test — DKT-550's whole
		// point: this needs no `or`, so the mixing rule never applies to it.
		{"kind == bug and labels contains-any (docs, backend)", true},
		{"kind == bug and labels contains-any (docs, frontend)", false},
		{"kind == task and labels contains-any (docs, backend)", false},
		{"labels contains-any (docs, backend) and labels != docs-only", true},
		// The set form composes with `or` too — it is a clause, not a
		// connective, so it carries no opinion about which one joins it.
		{"kind == task or labels contains-any (docs, urgent)", true},
		{"kind == task or labels contains-any (docs, frontend)", false},
		// `contains_any [...]` (DKT-1000): the same clause under the spelling
		// and delimiters authors reach for, so every row above has a twin here.
		// The two spellings are ONE operator — a subject they answered
		// differently would mean the grammar has two set tests, not two ways to
		// write one.
		{"labels contains_any [backend]", true},
		{"labels contains_any [docs]", false},
		{"labels contains_any [docs, backend]", true},
		{"labels contains_any [backend, docs]", true},
		{"labels contains_any [docs, frontend]", false},
		{`labels contains_any ["docs", "urgent"]`, true},
		{`labels contains_any ["docs", "frontend"]`, false},
		{"labels contains_any[backend,docs]", true},
		{"labels contains_any   [  docs ,  urgent  ]", true},
		// The spelling and the delimiter are independent choices: all four
		// combinations are the same clause.
		{"labels contains_any (docs, backend)", true},
		{"labels contains-any [docs, backend]", true},
		{"labels contains_any (docs, frontend)", false},
		{"labels contains-any [docs, frontend]", false},
		// A list must be closed by the delimiter that opened it. RE2 cannot
		// backreference, so this is the assertion that the two branches were
		// written out rather than a delimiter class that pairs anything.
		{"labels contains_any [docs, backend)", false},
		{"labels contains_any (docs, backend]", false},
		// DKT-1000's own example, verbatim.
		{"labels contains_any [security-change, security-load-bearing, security]", false},
		// Composition with `and` and `or` is the point of the clause form —
		// it must carry no opinion about which connective joins it.
		{"kind == bug and labels contains_any [docs, backend]", true},
		{"kind == bug and labels contains_any [docs, frontend]", false},
		{"kind == task and labels contains_any [docs, backend]", false},
		{"labels contains_any [docs, backend] and labels != docs-only", true},
		{"labels contains_any [docs, backend] and labels contains urgent", true},
		{"kind == task or labels contains_any [docs, urgent]", true},
		{"kind == task or labels contains_any [docs, frontend]", false},
		// `kind` has no set form: it is one value, so membership is `==`.
		// Unregisterable, therefore false rather than an error.
		{"kind contains-any (bug, task)", false},
		{"kind contains_any [bug, task]", false},
		// `or` (DKT-548): at least one clause holds. The false-false row is the
		// one that matters — a disjunction that held vacuously would run every
		// lane it was written to select between.
		{"kind == bug or labels contains docs", true},
		{"kind == task or labels contains backend", true},
		{"kind == task or labels contains docs", false},
		{"kind == bug or labels contains urgent", true},
		{"kind == task or kind == chore or labels contains urgent", true},
		{"kind == task or kind == chore or labels contains docs", false},
		// A clause V22 would have rejected evaluates FALSE rather than
		// erroring: reaching it means a stored definition predates the current
		// grammar, and skipping the step is the conservative reading.
		{"status == done", false},
		// The same conservatism over the disjunction: an unparseable clause is
		// false, so it cannot carry the `or`, and it cannot poison a sibling
		// clause that genuinely holds.
		{"status == done or labels contains backend", true},
		{"status == done or labels contains docs", false},
		// A predicate mixing connectives is not in the grammar (V22 refuses it),
		// so it is false whatever its clauses say — including when every clause
		// holds, which is the row that would pass under any accidental
		// precedence default.
		{"kind == bug and labels contains backend or labels contains urgent", false},
		{"labels contains urgent or kind == bug and labels contains backend", false},
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

// TestSplitWhenReportsItsConnective covers the parser V22 and WhenHolds share.
// Both read the connective from here, so a predicate that splits one way for
// the validator and another for the evaluator is impossible by construction —
// which is the property DKT-548 required be preserved, not just the new `or`.
func TestSplitWhenReportsItsConnective(t *testing.T) {
	cases := []struct {
		expr       string
		clauses    []string
		connective string
		mixed      bool
	}{
		// Nothing to join: `and` is the identity the pre-DKT-548 grammar had.
		{expr: "kind == bug", clauses: []string{"kind == bug"}, connective: WhenAnd},
		{
			expr:       "kind == bug and labels contains x",
			clauses:    []string{"kind == bug", "labels contains x"},
			connective: WhenAnd,
		},
		{
			expr:       "labels contains security-load-bearing or labels contains security",
			clauses:    []string{"labels contains security-load-bearing", "labels contains security"},
			connective: WhenOr,
		},
		{
			expr:       "kind == bug  or   labels contains x",
			clauses:    []string{"kind == bug", "labels contains x"},
			connective: WhenOr,
		},
		{
			expr:       "a and b or c",
			clauses:    []string{"a", "b", "c"},
			connective: WhenAnd, // the FIRST connective; `mixed` is what refuses it
			mixed:      true,
		},
		// The connective is a whitespace-delimited token, so a literal that
		// merely embeds one is not a split point.
		{
			expr:       "labels contains and-then",
			clauses:    []string{"labels contains and-then"},
			connective: WhenAnd,
		},
		{
			expr:       "labels contains x-or-y",
			clauses:    []string{"labels contains x-or-y"},
			connective: WhenAnd,
		},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			clauses, connective, mixed := splitWhen(tc.expr)
			if !slices.Equal(clauses, tc.clauses) {
				t.Errorf("clauses = %q, want %q", clauses, tc.clauses)
			}
			if connective != tc.connective {
				t.Errorf("connective = %q, want %q", connective, tc.connective)
			}
			if mixed != tc.mixed {
				t.Errorf("mixed = %v, want %v", mixed, tc.mixed)
			}
		})
	}
}

// TestDisjunctionCollapsesTwoIdenticalLanes is DKT-548's motivating case as an
// assertion: the workflow that routed a TDD-security author lane on
// "security-load-bearing OR security" carried two byte-identical steps whose
// only difference was the predicate, because the grammar had no `or`.
//
// The test asserts the collapse is EXACT — the single predicate holds on
// precisely the subjects on which at least one of the two old lanes' predicates
// held, and on no others. An `or` that merely covered the motivating labels
// while also holding on unrelated issues would collapse the duplication and
// widen the routing, which is a worse defect than the duplication.
func TestDisjunctionCollapsesTwoIdenticalLanes(t *testing.T) {
	const (
		lane    = "labels contains security-load-bearing"
		laneAlt = "labels contains security"
		merged  = "labels contains security-load-bearing or labels contains security"
	)

	subjects := []Subject{
		{Kind: "bug"},
		{Kind: "bug", Labels: []string{"security-load-bearing"}},
		{Kind: "bug", Labels: []string{"security"}},
		{Kind: "bug", Labels: []string{"security-load-bearing", "security"}},
		{Kind: "task", Labels: []string{"security"}},
		{Kind: "chore", Labels: []string{"backend", "urgent"}},
		{Kind: "feature", Labels: []string{"securityish"}},
	}

	for _, s := range subjects {
		want := WhenHolds(lane, s) || WhenHolds(laneAlt, s)
		if got := WhenHolds(merged, s); got != want {
			t.Errorf("WhenHolds(%q, %+v) = %v; the two lanes it replaces give %v",
				merged, s, got, want)
		}
	}
}

// TestContainsAnyMirrorsLabelsAny is DKT-550's semantic requirement as an
// assertion: the step-level `labels contains-any (…)` clause must answer a
// subject exactly as the workflow-level `labels_any` [match] clause does.
//
// Two spellings of "any of these labels" that disagreed on any subject would
// mean a step's condition and its workflow's binding rule read the same issue
// differently — which is the failure the mirror exists to rule out, not a
// stylistic preference.
func TestContainsAnyMirrorsLabelsAny(t *testing.T) {
	list := []string{"security-change", "security"}
	m := &Match{LabelsAny: list}

	subjects := []Subject{
		{Kind: "doc"},
		{Kind: "doc", Labels: []string{"security"}},
		{Kind: "doc", Labels: []string{"security-change"}},
		{Kind: "doc", Labels: []string{"security-change", "security"}},
		{Kind: "doc", Labels: []string{"security-load-bearing"}},
		{Kind: "doc", Labels: []string{"doc:tdd"}},
		{Kind: "doc", Labels: []string{"doc:tdd", "security"}},
		{Kind: "bug", Labels: []string{"securityish"}},
	}

	// Every accepted spelling of the clause is held to the same mirror
	// (DKT-1000): a delimiter or an underscore must not be able to change what
	// the predicate asks.
	spellings := []string{
		"labels contains-any (security-change, security)",
		"labels contains_any [security-change, security]",
		"labels contains_any (security-change, security)",
		"labels contains-any [security-change, security]",
		`labels contains_any ["security-change", "security"]`,
	}
	for _, when := range spellings {
		for _, s := range subjects {
			want := m.Matches(s)
			if got := WhenHolds(when, s); got != want {
				t.Errorf("WhenHolds(%q, %+v) = %v; labels_any %q gives %v",
					when, s, got, list, want)
			}
		}
	}
}

// TestContainsAnySpellingsAgree is DKT-1000's compatibility requirement: adding
// `contains_any` and the bracketed list changed no existing operator's meaning.
//
// The registered corpus carries `contains-any (…)`. If the new spelling parsed
// through a second code path — or if adding the bracket branch shifted a
// capture group the evaluator reads by index — a stored definition would keep
// registering and start evaluating differently, which is the one failure a new
// operator must not be able to cause. The sweep runs the four spellings over
// every labels subset so a divergence anywhere is caught, not just on the
// intersecting cases.
func TestContainsAnySpellingsAgree(t *testing.T) {
	spellings := []string{
		"labels contains-any (a, b)",
		"labels contains_any (a, b)",
		"labels contains-any [a, b]",
		"labels contains_any [a, b]",
	}

	subjects := []Subject{
		{Kind: "bug"},
		{Kind: "bug", Labels: []string{"a"}},
		{Kind: "bug", Labels: []string{"b"}},
		{Kind: "bug", Labels: []string{"a", "b"}},
		{Kind: "bug", Labels: []string{"c"}},
		{Kind: "bug", Labels: []string{"c", "b"}},
		{Kind: "bug", Labels: []string{"ab"}},
		{Kind: "bug", Labels: []string{""}},
	}

	for _, s := range subjects {
		want := WhenHolds(spellings[0], s)
		for _, when := range spellings[1:] {
			if got := WhenHolds(when, s); got != want {
				t.Errorf("WhenHolds(%q, %+v) = %v; %q gives %v",
					when, s, got, spellings[0], want)
			}
		}
	}
}

// TestContainsAnyNeedsNoConnectiveMix is DKT-550's motivating case: routing TDD
// authoring to a security-specialized executor when the issue carries ANY of
// the security label spellings.
//
// The natural predicate — `labels contains doc:tdd and (labels contains
// security-change or labels contains security)` — MIXES connectives, which V22
// refuses and WhenHolds therefore reads as false. The test asserts the set form
// expresses the same routing as a single homogeneous-`and` predicate, and that
// it agrees on every subject with the mixed reading the author meant. The
// mixing rule itself is untouched: the row asserting the mixed spelling is
// still false is the guard on that.
func TestContainsAnyNeedsNoConnectiveMix(t *testing.T) {
	const (
		mixed  = "labels contains doc:tdd and (labels contains security-change or labels contains security)"
		single = "kind == doc:tdd and labels contains-any (security-change, security)"
	)

	subjects := []Subject{
		{Kind: "doc:tdd"},
		{Kind: "doc:tdd", Labels: []string{"security"}},
		{Kind: "doc:tdd", Labels: []string{"security-change"}},
		{Kind: "doc:tdd", Labels: []string{"security-change", "security"}},
		{Kind: "doc:tdd", Labels: []string{"security-load-bearing"}},
		{Kind: "doc:tdd", Labels: []string{"backend"}},
		{Kind: "bug", Labels: []string{"security"}},
		{Kind: "bug"},
	}

	for _, s := range subjects {
		// The intent the author could not spell: kind is doc:tdd AND at least
		// one security spelling is present.
		want := s.Kind == "doc:tdd" &&
			(slices.Contains(s.Labels, "security-change") || slices.Contains(s.Labels, "security"))
		if got := WhenHolds(single, s); got != want {
			t.Errorf("WhenHolds(%q, %+v) = %v, want %v", single, s, got, want)
		}
		// The mixed spelling remains outside the grammar, on every subject.
		if WhenHolds(mixed, s) {
			t.Errorf("WhenHolds(%q, %+v) = true; a mixed predicate must not hold", mixed, s)
		}
	}
}

// TestContainsAnyRoutesSecurityTDD is DKT-1000's requested predicate, verbatim:
// spec-doc routes a `doc:tdd` issue to the security TDD author when it carries
// ANY of policy.toml's three [security].labels.
//
// It asserts the whole ask in one string — a labels clause and a set clause
// joined by the single `and` connective — rather than the one duplicated
// [[step]] per label the grammar used to force. The false rows are the load-
// bearing ones: a `doc:tdd` issue with an unrelated label, and a security issue
// that is not a TDD, must both stay out of the specialized lane.
func TestContainsAnyRoutesSecurityTDD(t *testing.T) {
	const when = "labels contains doc:tdd and labels contains_any " +
		"[security-change, security-load-bearing, security]"

	cases := []struct {
		labels []string
		want   bool
	}{
		{nil, false},
		{[]string{"doc:tdd"}, false},
		{[]string{"security"}, false},
		{[]string{"security-change"}, false},
		{[]string{"doc:tdd", "security"}, true},
		{[]string{"doc:tdd", "security-change"}, true},
		{[]string{"doc:tdd", "security-load-bearing"}, true},
		{[]string{"doc:tdd", "security-change", "security"}, true},
		{[]string{"doc:tdd", "backend"}, false},
		{[]string{"doc:tdd", "securityish"}, false},
		{[]string{"doc:adr", "security"}, false},
	}

	for _, tc := range cases {
		s := Subject{Kind: "doc", Labels: tc.labels}
		if got := WhenHolds(when, s); got != tc.want {
			t.Errorf("WhenHolds(%q, labels=%v) = %v, want %v", when, tc.labels, got, tc.want)
		}
	}
}

// TestContainsAnySplitsAsOneClause pins the interaction DKT-550 has with the
// DKT-548 splitter: a `contains-any` list is ONE clause, and the commas inside
// it are not connectives. If splitWhen ever cut inside the parens, V22 and
// WhenHolds would still agree (both use it), but the clause would stop being
// evaluable — so this is the assertion that the new form needed no change to
// the connective logic at all.
func TestContainsAnySplitsAsOneClause(t *testing.T) {
	cases := []struct {
		expr       string
		clauses    []string
		connective string
	}{
		{
			expr:       "labels contains-any (a, b, c)",
			clauses:    []string{"labels contains-any (a, b, c)"},
			connective: WhenAnd,
		},
		{
			expr: "kind == doc:tdd and labels contains-any (a, b)",
			clauses: []string{
				"kind == doc:tdd", "labels contains-any (a, b)",
			},
			connective: WhenAnd,
		},
		{
			expr: "labels contains-any (a, b) or kind == bug",
			clauses: []string{
				"labels contains-any (a, b)", "kind == bug",
			},
			connective: WhenOr,
		},
		// The bracketed spelling is subject to the identical claim (DKT-1000):
		// the commas inside `[…]` are not connectives either.
		{
			expr:       "labels contains_any [a, b, c]",
			clauses:    []string{"labels contains_any [a, b, c]"},
			connective: WhenAnd,
		},
		{
			expr: "kind == doc:tdd and labels contains_any [security-change, security-load-bearing, security]",
			clauses: []string{
				"kind == doc:tdd",
				"labels contains_any [security-change, security-load-bearing, security]",
			},
			connective: WhenAnd,
		},
		{
			expr: "labels contains_any [a, b] or kind == bug",
			clauses: []string{
				"labels contains_any [a, b]", "kind == bug",
			},
			connective: WhenOr,
		},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			clauses, connective, mixed := splitWhen(tc.expr)
			if mixed {
				t.Errorf("splitWhen(%q) reports mixed", tc.expr)
			}
			if !slices.Equal(clauses, tc.clauses) {
				t.Errorf("clauses = %q, want %q", clauses, tc.clauses)
			}
			if connective != tc.connective {
				t.Errorf("connective = %q, want %q", connective, tc.connective)
			}
			for _, clause := range clauses {
				if !whenShape.MatchString(clause) {
					t.Errorf("clause %q does not match whenShape", clause)
				}
			}
		})
	}
}
