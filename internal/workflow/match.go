package workflow

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Subject is the issue state a `[match]` clause and a `when` predicate are
// evaluated against. It is deliberately NOT model.Issue: the two predicate
// languages address `kind` and `labels` and nothing else (engine-spec §11.1;
// engine-core §4, "conditions (predicates over issue kind/labels only)"), so
// passing a whole issue would let a future clause reach a field the grammar
// has no way to name.
type Subject struct {
	Kind   string
	Labels []string
}

// Matches reports whether an issue satisfies this workflow's `[match]` clause
// — the binding rule of engine-spec §11.1, evaluated at activation.
//
// An ABSENT workflow-level `[match]` matches anything: a workflow that
// declares no binding rule is a candidate for every issue, and exactly-one-match
// (TDD §5.3 stage 1) then does the refusing. That is the spec's shape, not a
// convenience — "zero or multiple matches is a VALIDATION_ERROR naming the
// issue and the candidate workflows" only means something if a match-less
// workflow participates in the count.
//
// The four clauses, per §11.1 and TDD §5.3 stage 1:
//
//   - `kind`          — the issue's kind is in the list; an absent list matches any
//   - `labels_any`    — the lists intersect
//   - `labels_all`    — the clause's labels are a subset of the issue's
//   - `unless_labels` — the lists are disjoint
//
// `unless_labels` is evaluated LAST and WINS, so an exclusion cannot be
// defeated by an inclusion. A workflow that excludes `security-load-bearing`
// must not bind a security-load-bearing issue merely because that issue also
// carries a label the workflow includes — the exclusion is the author saying
// "not this one", and an inclusion overriding it would silently route work
// through a pipeline written to refuse it.
func (m *Match) Matches(s Subject) bool {
	if m == nil {
		return true
	}

	if len(m.Kind) > 0 && !slices.Contains(m.Kind, s.Kind) {
		return false
	}
	if len(m.LabelsAny) > 0 && !intersects(m.LabelsAny, s.Labels) {
		return false
	}
	if len(m.LabelsAll) > 0 && !subset(m.LabelsAll, s.Labels) {
		return false
	}
	// Last and wins.
	if len(m.UnlessLabels) > 0 && intersects(m.UnlessLabels, s.Labels) {
		return false
	}

	return true
}

// WhenHolds evaluates a step's `when` predicate against an issue.
//
// An empty `when` holds: a step that declares no condition is unconditional.
// The grammar is §11.1's, validated at register time by V22 and re-parsed here
// against the SAME regexp, so a predicate that registered cannot fail to
// evaluate. Conjuncts are joined by `and` — the only connective (splitWhen) —
// and every one must hold.
//
// A false `when` does not omit the step: expansion creates it with status
// `skipped` (§5.3.1), which is what keeps a downstream `after` resolvable and
// the topology identical regardless of the predicate's value.
func WhenHolds(when string, s Subject) bool {
	if strings.TrimSpace(when) == "" {
		return true
	}
	for _, clause := range splitWhen(when) {
		if !whenClauseHolds(clause, s) {
			return false
		}
	}
	return true
}

// whenClauseHolds evaluates one `<kind|labels> <==|!=|contains> <value>`
// clause. An unparseable clause is FALSE rather than an error: V22 rejected it
// at register time, so reaching this branch means a stored definition predates
// the current grammar, and skipping the step is the conservative reading —
// creating it ready would run work whose condition nobody could evaluate.
func whenClauseHolds(clause string, s Subject) bool {
	m := whenShape.FindStringSubmatch(clause)
	if m == nil {
		return false
	}
	field, op, value := m[1], m[2], unquote(m[3])

	switch field {
	case "kind":
		switch op {
		case "==":
			return s.Kind == value
		case "!=":
			return s.Kind != value
		case "contains":
			return strings.Contains(s.Kind, value)
		}
	case "labels":
		switch op {
		case "contains", "==":
			// `labels == x` and `labels contains x` are the same question over
			// a set: the grammar admits both spellings and neither can mean
			// "the label set equals the one-element list", which nothing needs
			// and which would make `==` a trap next to `contains`.
			return slices.Contains(s.Labels, value)
		case "!=":
			return !slices.Contains(s.Labels, value)
		}
	}
	return false
}

// unquote strips the optional quotes around a predicate literal, so
// `kind == "bug"` and `kind == bug` mean the same thing. §11.1's examples use
// both spellings.
func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func intersects(a, b []string) bool {
	for _, v := range a {
		if slices.Contains(b, v) {
			return true
		}
	}
	return false
}

func subset(want, have []string) bool {
	for _, v := range want {
		if !slices.Contains(have, v) {
			return false
		}
	}
	return true
}

// fenceTags returns every fence tag this definition's gates declare, sorted and
// deduplicated — the `<tag>` of a `source = "fence:<tag>"` gate (§11.1).
//
// Activation harvests ONLY declared tags (TDD §5.3 stage 5): a fenced block
// whose tag no bound workflow declares is not harvested at all. That is the
// trust boundary doing its job — harvesting every fenced block would make any
// code sample in an issue body a candidate command, which is precisely what
// declaring the tag exists to prevent.
func (d *Definition) FenceTags() []string {
	seen := make(map[string]struct{})
	for _, step := range d.Steps {
		for _, gate := range step.Gates {
			if tag, ok := strings.CutPrefix(gate.Source, "fence:"); ok && tag != "" {
				seen[tag] = struct{}{}
			}
		}
	}

	tags := make([]string, 0, len(seen))
	for tag := range seen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// fenceOpen matches an opening code fence and captures its info string. The
// info string's FIRST word is the tag, so ```ac and ```ac title=x both declare
// the `ac` fence — a fence's tag is a token, not the whole line.
var fenceOpen = regexp.MustCompile("^\\s*(```+|~~~+)\\s*(\\S*)")

// Fence is one harvested fenced block: the tag it declared, its position within
// the issue body, and its lines verbatim.
type Fence struct {
	Tag     string
	Ordinal int
	Command string
}

// HarvestFences extracts fenced code blocks from an issue body whose info-string
// tag is in `tags` (engine-core §6; TDD §5.3 stage 5).
//
// Extraction is LITERAL — "no prose parsing". The block's lines are taken
// verbatim, one Fence per line, and nothing interprets, trims, or reflows them.
// A command an operator approved at plan time is the command that runs, byte
// for byte; anything else would make the approval a summary of what runs rather
// than the thing itself.
//
// Ordinals are per (issue, tag) and count LINES across the issue's blocks, in
// body order, so two `ac` blocks continue one ordinal sequence. The ordinal is
// part of the run_fences key, so it must be a pure function of the snapshot —
// which it is: the body is walked once, top to bottom.
func HarvestFences(body string, tags []string) []Fence {
	if len(tags) == 0 || body == "" {
		return nil
	}

	var (
		out      []Fence
		ordinals = make(map[string]int)
		inBlock  bool
		tag      string
		closer   string
	)

	for _, line := range strings.Split(body, "\n") {
		if !inBlock {
			m := fenceOpen.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// An undeclared tag opens a block that is SKIPPED rather than
			// ignored: its lines must not be read as top-level text, or a
			// declared fence nested in a prose example would be harvested.
			inBlock, closer, tag = true, m[1], firstWord(m[2])
			continue
		}

		// A closing fence is a run of the same character at least as long as
		// the opener, and carries no info string.
		if isFenceClose(line, closer) {
			inBlock, tag, closer = false, "", ""
			continue
		}

		if !slices.Contains(tags, tag) {
			continue
		}
		out = append(out, Fence{Tag: tag, Ordinal: ordinals[tag], Command: line})
		ordinals[tag]++
	}

	return out
}

// isFenceClose reports whether a line closes a block opened with `opener`.
func isFenceClose(line, opener string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || len(trimmed) < len(opener) {
		return false
	}
	marker := opener[:1]
	return strings.Trim(trimmed, marker) == "" && strings.HasPrefix(trimmed, opener)
}

// firstWord returns the leading token of a fence info string, which is the tag.
func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}
