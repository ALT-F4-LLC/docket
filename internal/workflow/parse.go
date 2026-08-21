// Package workflow implements the workflow-definition grammar of
// docs/design/engine-spec.md §11.1, its register-time validation table, and the
// DAG lints over a definition's `after` topology (TDD docs/tdd/engine-spine.md
// §4.2-§4.3.3).
//
// The package carries no agent, model, or LLM vocabulary of any kind
// (docs/design/genericity.md). `executor` is an opaque string the engine uses
// only as a map key for `class` lookup; `params` and `metadata` are opaque KV
// bags stored as JSON and returned verbatim. Nothing here reads a key inside
// them.
package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Definition is a parsed workflow definition — the §11.1 grammar, field for
// field.
type Definition struct {
	Pipeline Pipeline         `toml:"pipeline" json:"pipeline"`
	Match    *Match           `toml:"match" json:"match,omitempty"`
	Limits   map[string]Limit `toml:"-" json:"limits,omitempty"`
	Steps    []*Step          `toml:"step" json:"steps"`
}

// Pipeline is the §11.1 `[pipeline]` table.
type Pipeline struct {
	Name        string `toml:"name" json:"name"`
	Version     int    `toml:"version" json:"version"`
	Description string `toml:"description" json:"description,omitempty"`
}

// Match is the §11.1 `[match]` table — the binding rule, evaluated at run
// activation (stage 2 owns the evaluation; this stage parses and validates it).
type Match struct {
	Kind         []string `toml:"kind" json:"kind,omitempty"`
	LabelsAny    []string `toml:"labels_any" json:"labels_any,omitempty"`
	LabelsAll    []string `toml:"labels_all" json:"labels_all,omitempty"`
	UnlessLabels []string `toml:"unless_labels" json:"unless_labels,omitempty"`
}

// Limit is one `[limits]` entry: an executor class's concurrency and timing
// bounds. §11.1 allows a bare int as shorthand for `max`, so the TOML form is
// decoded by hand (see decodeLimits) and this struct is the normalized shape.
type Limit struct {
	Max             int    `json:"max,omitempty"`
	LeaseTTL        string `json:"lease_ttl,omitempty"`
	MaxStepDuration string `json:"max_step_duration,omitempty"`
}

// Gate is one entry of a step's `gates` list. §11.1 admits two spellings — a
// bare string naming a trusted gate, or an inline table — and both normalize to
// this one shape in the parsed form so downstream code never branches on which
// the author wrote.
type Gate struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	Pre    bool   `json:"pre"`
}

// Step is one §11.1 `[[step]]`, carrying every row of that table.
type Step struct {
	Name        string            `toml:"name" json:"name"`
	Executor    string            `toml:"executor" json:"executor,omitempty"`
	Action      string            `toml:"action" json:"action,omitempty"`
	Type        string            `toml:"type" json:"type,omitempty"`
	Fanout      []string          `toml:"fanout" json:"fanout,omitempty"`
	Class       string            `toml:"class" json:"class,omitempty"`
	Emits       string            `toml:"emits" json:"emits,omitempty"`
	Payload     string            `toml:"payload" json:"payload,omitempty"`
	Voters      []string          `toml:"voters" json:"voters,omitempty"`
	VoteRule    string            `toml:"vote_rule" json:"vote_rule,omitempty"`
	After       []string          `toml:"after" json:"after"`
	Inputs      []string          `toml:"inputs" json:"inputs,omitempty"`
	Gates       []Gate            `toml:"-" json:"gates,omitempty"`
	Params      map[string]any    `toml:"params" json:"params,omitempty"`
	MinSiblings *int              `toml:"min_siblings" json:"min_siblings,omitempty"`
	Threshold   map[string]string `toml:"threshold" json:"threshold,omitempty"`
	OnFail      string            `toml:"on_fail" json:"on_fail,omitempty"`
	Loop        bool              `toml:"loop" json:"loop"`
	AfterLoop   string            `toml:"after_loop" json:"after_loop,omitempty"`
	MaxAttempts *int              `toml:"max_attempts" json:"max_attempts,omitempty"`
	// MaxFixLoops bounds EVERY `fix-loop` routing on the issue -- threshold,
	// rejected vote, or rejected human gate alike (see engine.EnterLoop). The
	// only way past it is a tracked operator grant (`fix-round`), never a skip.
	MaxFixLoops  *int           `toml:"max_fix_loops" json:"max_fix_loops,omitempty"`
	ExpectedCost *float64       `toml:"expected_cost" json:"expected_cost,omitempty"`
	When         string         `toml:"when" json:"when,omitempty"`
	Metadata     map[string]any `toml:"metadata" json:"metadata,omitempty"`
	// HoldsTree reports whether this step OCCUPIES its issue's scope while it
	// runs — the question scope exclusion (R4) is actually asking.
	//
	// DEFAULT TRUE, which is the conservative answer and the behavior every
	// workflow had before this field existed: a step is assumed to touch the
	// tree unless its author says otherwise, because the error directions are
	// not symmetric. A wrong `false` lets two writers race one working tree; a
	// wrong `true` only delays a step.
	//
	// CORE ATTACHES NO MEANING TO A CLASS NAME. It does not know that `write`
	// writes or that a judge reads — classes are opaque strings (§6.5's
	// genericity rule), so the exemption must be DECLARED, never inferred.
	// That is the whole reason this is a field: the instance knows its judges
	// only read the tree, and that its `synthesize` step reads recorded
	// payloads and never opens a file at all, and it says so here.
	//
	// A `*bool` rather than a `bool` because the zero value is the DANGEROUS
	// answer: an unset field must mean "holds", not "does not".
	HoldsTree *bool `toml:"holds_tree" json:"holds_tree,omitempty"`
	// Packet is the files this step declares it needs in its rendered work
	// packet, in DECLARED ORDER — which is the order the assembler inlines
	// them in (docs/tdd/packet-composition.md §1.1).
	//
	// CORE ATTACHES NO MEANING TO ANY ENTRY. They are paths relative to the
	// instance-config directory; the engine reads their bytes, verifies them
	// against the hash activation pinned, and hands them to the template. It
	// never interprets what a file says, never treats a directory as special,
	// and never derives a path it was not given.
	//
	// An entry may carry the `{executor}` token, which substitutes that
	// SIBLING's resolved executor hint (§1.1.1). That is the whole of the
	// mechanism: a fanout step is one step whose siblings carry different
	// hints, so a literal entry serves them all identically and only the token
	// lets one step declare per-sibling files. The instance authors the mapping
	// rule as syntax; core performs a string replacement.
	Packet []string `toml:"packet" json:"packet,omitempty"`

	// hasAfter records whether the author wrote `after` at all. V8 and V10
	// turn on the difference between a MISSING `after` (an error on a
	// non-exempt step) and an explicit `after = []` (a legal root), and an
	// empty slice cannot tell those apart.
	hasAfter bool
}

// HasAfter reports whether the definition declared `after` on this step, as
// opposed to omitting it. See the field comment for why the difference matters.
func (s *Step) HasAfter() bool { return s.hasAfter }

// The four step classes of §11.1's "exactly one of" rule. `class` here means
// the kind of step, which is distinct from the `class` FIELD (the
// concurrency-accounting key) — §11.1 uses the word for both and this package
// keeps them apart by name.
const (
	ClassExecutor = "executor"
	ClassAction   = "action"
	ClassType     = "type"
	ClassFanout   = "fanout"
)

// StepClass reports which of the four §11.1 alternatives this step declares, or
// "" when it declares none or more than one (V5 reports both cases).
func (s *Step) StepClass() string {
	var classes []string
	if s.Executor != "" {
		classes = append(classes, ClassExecutor)
	}
	if s.Action != "" {
		classes = append(classes, ClassAction)
	}
	if s.Type != "" {
		classes = append(classes, ClassType)
	}
	if len(s.Fanout) > 0 {
		classes = append(classes, ClassFanout)
	}
	if len(classes) != 1 {
		return ""
	}
	return classes[0]
}

// EffectiveOnFail is the routing a failure actually takes: the declared
// `on_fail`, else §11.1's default. V13 evaluates this rather than the declared
// value, which is what makes explicit `on_fail` mandatory on `type="human"`
// steps (V13a, TDD §4.3.2) — a human step that declares nothing HAS the
// forbidden `waiting-human` routing, silently.
//
// V13a extends the same requirement to `type="vote"` steps, where the default
// is not forbidden but is a DECISION: a vote step that declares nothing parks
// on a failed tally, which is either the intended escalation to an operator or
// an author who meant `fix-loop`, and the step itself cannot say which. Only
// the prohibition is human-only; the obligation to state the routing is every
// gate's.
func (s *Step) EffectiveOnFail() string {
	if s.OnFail != "" {
		return s.OnFail
	}
	return OnFailWaitingHuman
}

// The §11.1 `on_fail` vocabulary — closed (engine-core §4).
const (
	OnFailFixLoop      = "fix-loop"
	OnFailWaitingHuman = "waiting-human"
	OnFailSkip         = "skip"
	OnFailAbandonIssue = "abandon-issue"
)

// onFailValues is the closed vocabulary in declaration order, used by V12 and
// by the V13a error message.
var onFailValues = []string{
	OnFailFixLoop, OnFailWaitingHuman, OnFailSkip, OnFailAbandonIssue,
}

// The §11.1 `type` vocabulary.
const (
	TypeHuman = "human"
	TypeVote  = "vote"
)

// Parse decodes TOML source into a Definition. Decoding is STRICT: an unknown
// key anywhere is a validation error naming the key and, for step-level keys,
// the step. A typo'd `max_attempt` silently defaulting to the config value is
// exactly the class of bug that makes a workflow behave differently from what
// its author read, and it stays invisible until a run misroutes.
//
// Parse performs no semantic validation — see Validate.
func Parse(src []byte) (*Definition, error) {
	var raw rawDefinition
	md, err := toml.Decode(string(src), &raw)
	if err != nil {
		return nil, &Error{Rule: "PARSE", Message: fmt.Sprintf("invalid TOML: %v", err)}
	}

	if err := rejectUndecoded(md, raw); err != nil {
		return nil, err
	}

	def := &Definition{
		Pipeline: raw.Pipeline,
		Match:    raw.Match,
		Steps:    make([]*Step, 0, len(raw.Steps)),
	}

	limits, err := decodeLimits(raw.Limits)
	if err != nil {
		return nil, err
	}
	def.Limits = limits

	for i := range raw.Steps {
		step := raw.Steps[i].Step
		step.hasAfter = raw.Steps[i].After != nil
		if step.hasAfter {
			step.After = *raw.Steps[i].After
		} else {
			step.After = nil
		}
		gates, err := normalizeGates(step.Name, raw.Steps[i].Gates)
		if err != nil {
			return nil, err
		}
		step.Gates = gates

		def.Steps = append(def.Steps, &step)
	}

	return def, nil
}

// rawDefinition mirrors Definition for decoding. `after`, `gates`, and
// `limits` need shapes the public struct cannot carry: a pointer to distinguish
// missing from empty, `any` for the two gate spellings, and `any` for the
// bare-int limits shorthand.
type rawDefinition struct {
	Pipeline Pipeline       `toml:"pipeline"`
	Match    *Match         `toml:"match"`
	Limits   map[string]any `toml:"limits"`
	Steps    []rawStep      `toml:"step"`
}

type rawStep struct {
	Step
	After *[]string `toml:"after"`
	Gates []any     `toml:"gates"`
}

// rejectUndecoded turns BurntSushi's undecoded-key report into the strict-mode
// error §4.2 requires. Keys under `[[step]]` are attributed to their step by
// name, so the author is told where to look rather than just what to look for.
func rejectUndecoded(md toml.MetaData, raw rawDefinition) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}

	// Report deterministically: TOML key order is not stable across runs, and
	// an error message that reorders itself is one nobody can test against.
	keys := make([]string, 0, len(undecoded))
	for _, k := range undecoded {
		key := k.String()
		if isHandDecodedKey(key) {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)

	key := keys[0]
	if step, field, ok := attributeStepKey(md, key, raw.Steps); ok {
		return &Error{
			Rule: "PARSE", Step: step,
			Message: fmt.Sprintf("unknown key %q on step %q", field, step),
		}
	}
	return &Error{Rule: "PARSE", Message: fmt.Sprintf("unknown key %q", key)}
}

// handDecodedPrefixes are the values this package decodes by hand rather than
// through struct tags, because their §11.1 shapes do not map to a single Go
// type: `gates` admits a string or a table, `limits` admits an int or a table,
// and `params`/`metadata` are opaque KV bags the engine never reads into.
//
// The TOML decoder reports the keys INSIDE these as undecoded, since no struct
// field consumed them. Strictness for them is not waived, only relocated:
// normalizeGates and limitFromTable reject an unknown key in their own tables
// with the same VALIDATION_ERROR, while `params` and `metadata` are opaque by
// specification and have no key set to be strict about.
var handDecodedPrefixes = []string{"gates", "limits", "params", "metadata"}

// isHandDecodedKey reports whether an undecoded key names something inside a
// hand-decoded value, rather than an unknown key the author typo'd.
func isHandDecodedKey(key string) bool {
	parts := strings.Split(key, ".")
	for i, part := range parts {
		// Only an INTERIOR match counts: `step.0.gates.name` is inside a gate
		// table, while a trailing `gates` is the field itself and is decoded.
		if i == len(parts)-1 {
			break
		}
		for _, prefix := range handDecodedPrefixes {
			if part == prefix {
				return true
			}
		}
	}
	return false
}

// attributeStepKey maps an undecoded step-level key back to the step that
// declared it, so the author is told where to look rather than only what to
// look for.
//
// The decoder reports keys under an array of tables WITHOUT the element index —
// `step.max_attempt`, not `step.2.max_attempt`. What it does give is
// md.Keys() in document order, where a bare `step` key marks each element's
// start, so the index is recovered by counting those boundaries up to the first
// element that carries the offending key.
//
// When several elements carry it, the first is named: fixing that one surfaces
// the next on the following run, which is the same one-error-at-a-time flow the
// validation table follows.
func attributeStepKey(md toml.MetaData, key string, steps []rawStep) (step, field string, ok bool) {
	parts := strings.Split(key, ".")
	if len(parts) < 2 || parts[0] != "step" {
		return "", "", false
	}
	field = strings.Join(parts[1:], ".")

	idx := -1
	for _, k := range md.Keys() {
		if len(k) == 1 && k[0] == "step" {
			idx++
			continue
		}
		if idx >= 0 && k.String() == key {
			break
		}
	}
	if idx < 0 || idx >= len(steps) {
		return "", "", false
	}

	name := steps[idx].Name
	if name == "" {
		name = fmt.Sprintf("#%d", idx+1)
	}
	return name, field, true
}

// normalizeGates collapses §11.1's two gate spellings into one shape: a bare
// string becomes {name, source: "", pre: false}.
func normalizeGates(step string, raw []any) ([]Gate, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	gates := make([]Gate, 0, len(raw))
	for _, entry := range raw {
		switch v := entry.(type) {
		case string:
			if v == "" {
				return nil, &Error{
					Rule: "PARSE", Step: step, Field: "gates",
					Message: fmt.Sprintf("step %q: gates entry is empty", step),
				}
			}
			gates = append(gates, Gate{Name: v})
		case map[string]any:
			gate, err := gateFromTable(step, v)
			if err != nil {
				return nil, err
			}
			gates = append(gates, gate)
		default:
			return nil, &Error{
				Rule: "PARSE", Step: step, Field: "gates",
				Message: fmt.Sprintf(
					"step %q: gates entry must be a string or a table, got %T", step, entry),
			}
		}
	}
	return gates, nil
}

func gateFromTable(step string, t map[string]any) (Gate, error) {
	var gate Gate
	// Strict here too: a gate table is core surface and a typo in it is the
	// same class of silent misbehavior strict decoding exists to prevent.
	for k, v := range t {
		switch k {
		case "name":
			s, ok := v.(string)
			if !ok {
				return gate, &Error{
					Rule: "PARSE", Step: step, Field: "gates",
					Message: fmt.Sprintf("step %q: gates entry `name` must be a string", step),
				}
			}
			gate.Name = s
		case "source":
			s, ok := v.(string)
			if !ok {
				return gate, &Error{
					Rule: "PARSE", Step: step, Field: "gates",
					Message: fmt.Sprintf("step %q: gates entry `source` must be a string", step),
				}
			}
			gate.Source = s
		case "pre":
			b, ok := v.(bool)
			if !ok {
				return gate, &Error{
					Rule: "PARSE", Step: step, Field: "gates",
					Message: fmt.Sprintf("step %q: gates entry `pre` must be a boolean", step),
				}
			}
			gate.Pre = b
		default:
			return gate, &Error{
				Rule: "PARSE", Step: step, Field: "gates",
				Message: fmt.Sprintf("step %q: unknown key %q in gates entry", step, k),
			}
		}
	}
	if gate.Name == "" {
		return gate, &Error{
			Rule: "PARSE", Step: step, Field: "gates",
			Message: fmt.Sprintf("step %q: gates entry has no `name`", step),
		}
	}
	return gate, nil
}

// decodeLimits normalizes §11.1's `[limits]` shorthand: a bare int is `max`.
func decodeLimits(raw map[string]any) (map[string]Limit, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	limits := make(map[string]Limit, len(raw))
	for class, entry := range raw {
		switch v := entry.(type) {
		case int64:
			limits[class] = Limit{Max: int(v)}
		case map[string]any:
			limit, err := limitFromTable(class, v)
			if err != nil {
				return nil, err
			}
			limits[class] = limit
		default:
			return nil, &Error{
				Rule: "PARSE", Field: "limits",
				Message: fmt.Sprintf(
					"[limits] %q must be an integer or a table, got %T", class, entry),
			}
		}
	}
	return limits, nil
}

func limitFromTable(class string, t map[string]any) (Limit, error) {
	var limit Limit
	for k, v := range t {
		switch k {
		case "max":
			n, ok := v.(int64)
			if !ok {
				return limit, &Error{
					Rule: "PARSE", Field: "limits",
					Message: fmt.Sprintf("[limits] %q: `max` must be an integer", class),
				}
			}
			limit.Max = int(n)
		case "lease_ttl":
			s, ok := v.(string)
			if !ok {
				return limit, &Error{
					Rule: "PARSE", Field: "limits",
					Message: fmt.Sprintf("[limits] %q: `lease_ttl` must be a string", class),
				}
			}
			limit.LeaseTTL = s
		case "max_step_duration":
			s, ok := v.(string)
			if !ok {
				return limit, &Error{
					Rule: "PARSE", Field: "limits",
					Message: fmt.Sprintf("[limits] %q: `max_step_duration` must be a string", class),
				}
			}
			limit.MaxStepDuration = s
		default:
			return limit, &Error{
				Rule: "PARSE", Field: "limits",
				Message: fmt.Sprintf("[limits] %q: unknown key %q", class, k),
			}
		}
	}
	return limit, nil
}
