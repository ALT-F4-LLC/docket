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
	// DomainPaths declares the path globs this workflow's domain OCCUPIES —
	// "work under these paths is this pipeline's business" (DKT-1182).
	//
	// IT IS ADVISORY AND BINDS NOTHING. `Matches` does not read it, and no
	// amount of scope agreement makes a workflow bind an issue whose labels do
	// not select it: routing stays keyed on `kind` and labels exactly as §11.1
	// specifies. The one consumer is activation's binding lint
	// (engine.lintDomainScopeMismatch), which reports an issue whose declared
	// scope lies ENTIRELY inside this domain while its labels bound it
	// somewhere else — the exactly-one-WRONG-match case that the exactly-one
	// rule structurally cannot see, since a mis-labelled issue matches its
	// wrong workflow exactly once and activation has nothing to refuse.
	//
	// The measured case: an issue scoped entirely to a TUI test file carried
	// `qa` and not `ui`, so it bound the label-less baseline pipeline instead of
	// the UI one and silently lost that pipeline's judge fanout and render
	// gates. Nothing in the binding rule could have noticed; the only check was
	// a conductor diffing every issue's labels against its scope by hand.
	//
	// Declaring it is optional and a workflow that omits it is linted against
	// nothing — the field is dormant until a corpus author states a domain, and
	// `omitempty` keeps the canonical form of every definition that never
	// declares one byte-identical to what it always was.
	DomainPaths []string `toml:"domain_paths" json:"domain_paths,omitempty"`
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

// PassFloor is a step's `pass_floor = { field, at }` table (DKT-870): the
// author's exit bar on a `pass` routing. Both values are OPAQUE TOKENS — a
// payload property name and a value of that property's declared order — read
// by position exactly as `route_at`'s value is, and by nothing else.
type PassFloor struct {
	Field string `toml:"field" json:"field"`
	At    string `toml:"at" json:"at"`
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
	// AfterFired names predecessors this step runs ONLY IF THEY FIRED
	// (DKT-1085). When every instance of a named step ends `skipped` — an
	// interposed gate its threshold routed elsewhere (§11.2), a false `when`,
	// an `on_fail = "skip"` routing, an operator's `--as skip` — this step is
	// terminalized `skipped` in the SAME transaction, and the skip cascades
	// through every step declaring `after_fired` on it in turn.
	//
	// It is a SECOND predecessor list beside `after`, never a replacement:
	// `after` keeps its exact meaning (done OR skipped releases the join, J1),
	// and every entry here must also appear in `after` (V39a), so R3 already
	// orders the step behind the gate and "did it fire?" is settled before the
	// step could ever be ready. The corpus case: a `drain-highs` executor that
	// should run after `security-vote` APPROVED a round, and not on a round
	// where reconcile never routed to the vote at all. `when` cannot say it —
	// it reads issue kind and labels only (V22) — and `threshold` cannot: a
	// second step-name routing beside the vote would fire first and skip it.
	//
	// `omitempty`, so the pinned form of every definition that never declares
	// it is byte-identical to what it was — an idempotent re-register must not
	// read as a CONFLICT (canonical.go).
	AfterFired []string `toml:"after_fired" json:"after_fired,omitempty"`
	// PassFloor refuses a `pass` routing that would exit with declared-floor
	// work still standing (DKT-870): when this step's routing resolves to
	// `pass` but its recorded payload holds an element whose `field` value
	// sits AT OR ABOVE `at`'s position in the step's pinned schema order —
	// and that element is neither `held` nor `operator_resolved` — the step
	// parks `waiting-human` instead of exiting.
	//
	// It exists because a threshold reads whatever field its author chose,
	// and a routing step's self-reported disposition can contradict its own
	// recorded evidence: RUN-58's reconcile routed `pass` and the loop exited
	// with all 16 clusters open, SIX at the order's high position, none held
	// and none operator-resolved — "converged" in the ledger meaning
	// "dispositioned". The floor is the author's declaration of the exit bar,
	// exactly as `route_at` is the author's declaration of the routing floor:
	// `field` and `at` are opaque tokens compared only by position in the
	// declared order (genericity.md), so core still holds no opinion about
	// severities.
	//
	// `held` and `operator_resolved` elements are exempt because both already
	// carry a decision channel: a held cluster gates the step for an operator,
	// and a resolved one records the operator's acceptance. The park names
	// `--as override-pass` (exit as recorded) and `--as fix-round` (buy a
	// round instead) as the ways out. Requires `payload` (V37) — without a
	// pinned order there is no position to compare — and the floor's own
	// coherence against that schema is V37a's, at register time.
	PassFloor *PassFloor `toml:"pass_floor" json:"pass_floor,omitempty"`
	OnFail    string     `toml:"on_fail" json:"on_fail,omitempty"`
	Loop      bool       `toml:"loop" json:"loop"`
	AfterLoop string     `toml:"after_loop" json:"after_loop,omitempty"`
	// Serves scopes a `loop = true` body to the steps whose `fix-loop`
	// routings it answers (§11.3 cluster scoping, DKT-544): on a `fix-loop`
	// routed by a step named here, this body instantiates and its
	// `after_loop` root joins the sweep/re-instantiation set; on one routed
	// by any other step, this body is not part of the entry at all.
	//
	// OMITTED MEANS SERVES EVERY TRIGGER — the backward-compatible reading.
	// A workflow with no `serves` anywhere therefore has exactly one loop
	// cluster spanning every body and every `after_loop` root, which is
	// §11.3's original single-construct behavior, byte for byte.
	//
	// Valid only on `loop = true` steps (V35); every entry must name a step
	// that can actually route `fix-loop` (V35), and every step that can must
	// be served by at least one body (V17c).
	Serves      []string `toml:"serves" json:"serves,omitempty"`
	MaxAttempts *int     `toml:"max_attempts" json:"max_attempts,omitempty"`
	// MaxFixLoops bounds EVERY `fix-loop` routing on the issue -- threshold,
	// gate-failure or attempt-exhaustion `on_fail`, rejected vote, rejected
	// human gate, and quorum miss alike (see engine.EnterLoop; DKT-587). It is
	// ONE workflow-wide bound over ONE issue-level counter: whichever
	// non-cluster step declares it, every routing source moves the same
	// counter, so the bound cannot depend on which step happened to route.
	//
	// HOW THE COUNT IS COMPUTED (§11.3 (1)): each admitted entry increments
	// the issue's counter FIRST and the new value is that entry's 1-indexed
	// loop ordinal; an entry whose new count EXCEEDS the bound is refused --
	// the counter is put back, nothing is instantiated, and the routing step
	// parks `waiting-human`. `max_fix_loops = N` therefore admits exactly N
	// loop entries (ordinals 1..N) and refuses the N+1th, from any mix of
	// routing sources. Zero or absent means unbounded.
	//
	// Declared on a `serves`-scoped loop body it is instead that CLUSTER's
	// round bound (§11.3 cluster scoping): it bounds entries triggered by the
	// steps the body serves — counted as the distinct ordinals holding the
	// cluster's scoped bodies, checked INDEPENDENTLY of and ADDITIVELY under
	// the issue-level ceiling, which stays whatever a non-cluster step
	// declares. A cluster bound never raises or lowers the issue-level one;
	// each refuses on its own arithmetic, and both admit exactly their
	// declared number of rounds.
	//
	// The only way past either bound is a tracked operator grant — `docket
	// step resolve --as fix-round` (DKT-237) — never a skip. Each grant
	// records `loop_grants + 1` on the issue's run row and enters the round in
	// the same transaction: the effective issue-level bound becomes declared +
	// grants, and the authorized entry also skips the cluster bound and the
	// non-convergence refusal, because the operator who granted it has
	// answered the question those refusals ask. A third completed ordinal
	// under `max_fix_loops = 2` is therefore the signature of a recorded
	// grant, not of the counter differing by entry path.
	MaxFixLoops *int `toml:"max_fix_loops" json:"max_fix_loops,omitempty"`
	// MaxStalledRounds parks a `fix-loop` entry when this step — a routing
	// step, i.e. one that can route `fix-loop` — has recorded that many
	// CONSECUTIVE rounds without its routed payload's element count ever
	// falling below the smallest count any earlier round recorded (DKT-870).
	//
	// It is the author's declaration of the non-convergence signal the corpus
	// already reads by hand: a fix loop that is converging shrinks the standing
	// set it routes each round, and one whose volume stays flat is churning.
	// RUN-51 held 8-12 clusters across TEN rounds (~271k + ~251k output tokens
	// spent on the last two alone) and RUN-50 held 7-10 across six; both ended
	// only by operator action, because nothing engine-visible read the plateau.
	// The count is of PAYLOAD ELEMENTS — packaging vocabulary, like `held` —
	// so core never learns what a cluster or a severity is.
	//
	// The refusal takes the non-convergence park's exact shape (engine.EnterLoop,
	// DKT-340/DKT-589): nothing superseded, nothing instantiated, the counter
	// restored, `waiting-human` naming `--as fix-round` as the way out, and an
	// authorized entry skips it. Zero or absent means the check never fires —
	// the same opt-in reading `hold_spread = 0` has.
	MaxStalledRounds *int           `toml:"max_stalled_rounds" json:"max_stalled_rounds,omitempty"`
	ExpectedCost     *float64       `toml:"expected_cost" json:"expected_cost,omitempty"`
	When             string         `toml:"when" json:"when,omitempty"`
	Metadata         map[string]any `toml:"metadata" json:"metadata,omitempty"`
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
