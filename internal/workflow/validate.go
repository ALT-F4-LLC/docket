package workflow

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RuleIDs is the register-time validation table, by ID, in documented order —
// engine-spine §4.3 as payloads-thresholds §4.9.1 extends it. It is the
// authority TestValidationTableIsComplete asserts set equality against — never a
// count, which is exactly the assertion that breaks when a rule is split
// (V13/V13a are one check and two author-facing errors; V21 is now one grammar
// rule and four cross-validation rules).
//
// V21a-V21d, V25a, V29, and V30 are the SCHEMA-AWARE half and live in
// ValidateSchemas rather than in Validate: they are the only rules that ask a
// question about the environment, and Validate stays a pure function of bytes
// (§4.9.2). V27, V28, and V31 are decisions about bytes and stay here.
var RuleIDs = []string{
	"V1", "V2", "V3", "V4", "V5", "V6", "V7", "V8", "V9", "V10",
	"V11", "V12", "V13", "V13a", "V14", "V15", "V16", "V17", "V17b", "V18", "V19",
	"V20", "V21", "V21a", "V21b", "V21c", "V21d",
	"V22", "V23", "V24", "V25", "V25a", "V26",
	"V27", "V28", "V29", "V30", "V31",
	"V32", "V33", "V34",
}

// VoteRuleResolver reports whether a named vote rule is registered, and lists
// the registered ones so a refusal can name the alternatives.
//
// It is an INTERFACE rather than a database handle because Validate is pure and
// must stay so: V1-V25 are decisions about bytes, and coupling the validator to
// a connection would make every one of them need one. V26 is the single rule
// that asks a question about the environment, so it takes the environment as a
// parameter.
type VoteRuleResolver interface {
	VoteRuleExists(rule string) (bool, error)
	RegisteredVoteRules() ([]string, error)
	// RuleSetElsewhere reports how many OTHER projects in this store have the
	// rule configured, and one value they use (DKT-264).
	//
	// It exists because "not registered" was true and useless in the one case
	// that actually happens: a corpus workflow is shared across every project
	// and its rules are not, so a fresh project refuses a definition that works
	// everywhere else in the store. The remedy is one `--global` set, and
	// nothing said so — which is how thirteen projects came to hold thirteen
	// per-project copies of the same three thresholds while the store-wide
	// fallback that already exists sat empty.
	//
	// A zero count is the ordinary case for a genuinely new rule, and the
	// refusal then reads exactly as it did before.
	RuleSetElsewhere(rule string) (projects int, value string, err error)
}

// ValidateVoteRules is V26 (gates-trust §8.2): a `vote_rule` naming an
// unregistered threshold configuration is a VALIDATION_ERROR at
// `workflow register`, naming the rule and listing the registered ones.
//
// Catching it at REGISTER rather than at run time is the discipline every other
// V-rule follows: a workflow that cannot possibly run should not register. The
// failure mode it prevents is a run that reaches its vote step — potentially
// hours in — and discovers there is no rule to tally by.
//
// It is separate from Validate because it needs the config registry; V14
// (unchanged) still requires `vote_rule` to be PRESENT, and this checks that
// what it names EXISTS.
func ValidateVoteRules(def *Definition, resolver VoteRuleResolver) error {
	if def == nil || resolver == nil {
		return nil
	}
	for _, step := range def.Steps {
		if step.Type != TypeVote || step.VoteRule == "" {
			continue
		}
		exists, err := resolver.VoteRuleExists(step.VoteRule)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		registered, err := resolver.RegisteredVoteRules()
		if err != nil {
			return err
		}
		known := "none are registered"
		if len(registered) > 0 {
			known = "registered rules: " + strings.Join(registered, ", ")
		}

		// THE REMEDY THAT FITS THE CASE (DKT-264). A rule this project lacks
		// and thirteen others have is not a new rule — it is a shared corpus
		// workflow meeting an unshared threshold, and the fix is one
		// store-wide set rather than a per-project one that leaves the next
		// project in the same place.
		remedy := fmt.Sprintf(
			"Register one with `docket config set vote.rule.%s.threshold <0-1>`",
			step.VoteRule)
		if n, value, err := resolver.RuleSetElsewhere(step.VoteRule); err == nil && n > 0 {
			remedy = fmt.Sprintf(
				"%d other project(s) in this store already configure it (e.g. %s), "+
					"so this is a shared workflow meeting an unshared threshold. "+
					"Set it once for every project with "+
					"`docket config set --global vote.rule.%s.threshold %s`",
				n, value, step.VoteRule, value)
		}

		return &Error{
			Rule: "V26", Step: step.Name, Field: "vote_rule",
			Message: fmt.Sprintf(
				"step %q: `vote_rule` %q is not registered here; %s. %s",
				step.Name, step.VoteRule, known, remedy),
		}
	}
	return nil
}

// LintIDs are the register-time DAG lints of TDD §4.3.3.
var LintIDs = []string{"L1", "L2", "L3", "L4"}

// Validate applies the §4.3 table to a parsed definition, in rule order, and
// returns the first violation. Every error names the workflow, the step, and
// the offending field.
//
// Defaults from the §11.1 default column are applied here, not at parse: a
// default is a semantic decision (V23's `class` = `executor`, `min_siblings` =
// len(fanout)) and applying it during decoding would erase the difference
// between "the author wrote this" and "the grammar supplied it" — which V8 and
// V10 depend on.
func Validate(def *Definition) error {
	name := def.Pipeline.Name

	if err := validatePipeline(def); err != nil {
		return withWorkflow(err, name)
	}
	if err := validateSteps(def); err != nil {
		return withWorkflow(err, name)
	}
	if err := validateLimits(def); err != nil {
		return withWorkflow(err, name)
	}

	applyDefaults(def)
	return nil
}

// validatePipeline covers V1-V3: the `[pipeline]` table and the existence of
// at least one step.
func validatePipeline(def *Definition) error {
	// V1: name present, non-empty. Uniqueness *in file* is structural — TOML
	// cannot express two `[pipeline]` tables — so what this rule can check at
	// parse time is presence.
	if strings.TrimSpace(def.Pipeline.Name) == "" {
		return &Error{
			Rule: "V1", Field: "pipeline.name",
			Message: "[pipeline].name is required and must be non-empty",
		}
	}

	// V2: version present, integer >= 1.
	if def.Pipeline.Version < 1 {
		return &Error{
			Rule: "V2", Field: "pipeline.version",
			Message: fmt.Sprintf(
				"[pipeline].version must be an integer >= 1, got %d", def.Pipeline.Version),
		}
	}

	// V3: at least one [[step]] — a zero-step workflow can never activate.
	if len(def.Steps) == 0 {
		return &Error{
			Rule: "V3", Field: "step",
			Message: "a workflow must declare at least one [[step]]",
		}
	}

	return nil
}

// validateSteps covers V4-V23, V25 — every per-step rule, in table order
// within each step so an author fixing errors sees them in a predictable
// sequence.
func validateSteps(def *Definition) error {
	byName := make(map[string]*Step, len(def.Steps))
	for i, step := range def.Steps {
		// V4: name present and unique within the workflow.
		if strings.TrimSpace(step.Name) == "" {
			return &Error{
				Rule: "V4", Field: "name",
				Message: fmt.Sprintf("[[step]] #%d has no `name`", i+1),
			}
		}
		// V27: the `-held` suffix is RESERVED. It is checked here, with V4,
		// because it is a rule about the name itself: a declared step named
		// `reconcile-held` would collide with the identity a tripped
		// `hold_spread` mints for `reconcile` (§7.7.1 H1), and the collision
		// would surface as a UNIQUE violation mid-run rather than as a sentence
		// at register.
		if strings.HasSuffix(step.Name, HeldSuffix) {
			return &Error{
				Rule: "V27", Step: step.Name, Field: "name",
				Message: fmt.Sprintf(
					"step name %q ends in %q, which is reserved: the engine mints "+
						"`<step>%s` when an `aggregate` step's `hold_spread` trips, "+
						"and two rows cannot claim one identity",
					step.Name, HeldSuffix, HeldSuffix),
			}
		}
		// V34: the `issue.latest` name and everything under it are RESERVED
		// (DKT-492) — V27's reasoning applied to the input namespace:
		// `issue.latest.<kind>` is the engine-served latest-of-kind input
		// form, resolved before any step lookup, so a step under that name
		// could never be addressed as an input — the reserved form would
		// shadow it silently on every consumer.
		if step.Name == "issue.latest" || strings.HasPrefix(step.Name, InputIssueLatestPrefix) {
			return &Error{
				Rule: "V34", Step: step.Name, Field: "name",
				Message: fmt.Sprintf(
					"step name %q is reserved: `issue.latest.<kind>` is the "+
						"engine-served latest-of-kind input form, and a step under "+
						"that name could never be addressed as an input",
					step.Name),
			}
		}
		if _, dup := byName[step.Name]; dup {
			return &Error{
				Rule: "V4", Step: step.Name, Field: "name",
				Message: fmt.Sprintf("step name %q is declared more than once", step.Name),
			}
		}
		byName[step.Name] = step
	}

	for i, step := range def.Steps {
		if err := validateStep(def, step, i, byName); err != nil {
			return err
		}
	}

	return nil
}

func validateStep(def *Definition, step *Step, index int, byName map[string]*Step) error {
	// V15: fanout non-empty when present. Checked BEFORE V5 because an
	// explicit `fanout = []` declares the alternative without satisfying it:
	// StepClass sees no fanout and V5 would report the less useful "declares
	// none" for a step whose actual problem is an empty list.
	if step.Fanout != nil && len(step.Fanout) == 0 {
		return &Error{
			Rule: "V15", Step: step.Name, Field: "fanout",
			Message: fmt.Sprintf("step %q: `fanout` must be non-empty when present", step.Name),
		}
	}
	for _, hint := range step.Fanout {
		if strings.TrimSpace(hint) == "" {
			return &Error{
				Rule: "V15", Step: step.Name, Field: "fanout",
				Message: fmt.Sprintf("step %q: `fanout` contains an empty hint", step.Name),
			}
		}
	}

	class := step.StepClass()

	// V5: exactly one of executor / action / type / fanout.
	if class == "" {
		return &Error{
			Rule: "V5", Step: step.Name,
			Message: fmt.Sprintf(
				"step %q must declare exactly one of `executor`, `action`, `type`, `fanout`; it declares %s",
				step.Name, describeDeclared(step)),
		}
	}

	// V6: type in {human, vote}.
	if step.Type != "" && step.Type != TypeHuman && step.Type != TypeVote {
		return &Error{
			Rule: "V6", Step: step.Name, Field: "type",
			Message: fmt.Sprintf(
				"step %q: `type` must be %q or %q, got %q",
				step.Name, TypeHuman, TypeVote, step.Type),
		}
	}

	// V7: emits required on executor steps. §4.3.1: required for executors,
	// optional on fanout steps that declare it, absent on action/human/vote —
	// the spec makes it required only for executors and this says exactly that
	// and no more.
	if class == ClassExecutor && step.Emits == "" {
		return &Error{
			Rule: "V7", Step: step.Name, Field: "emits",
			Message: fmt.Sprintf("step %q: `emits` is required on executor steps", step.Name),
		}
	}

	// V11a: `gate-results` is a RESERVED kind (DKT-77), V27's reasoning for a
	// kind rather than a name: `<step>.gate-results` resolves to the engine's
	// recorded results, so a step emitting an artifact of that kind could
	// never be addressed as an input — the reserved form would shadow it.
	if produced, _ := producedKind(step); produced == GateResultsKind {
		return &Error{
			Rule: "V11a", Step: step.Name, Field: "emits",
			Message: fmt.Sprintf(
				"step %q produces kind %q, which is reserved: "+
					"`<step>.%s` is the engine-served input form for recorded "+
					"gate results, and an artifact of that kind could never be "+
					"addressed", step.Name, GateResultsKind, GateResultsKind),
		}
	}

	// V8: after required except on the first step and on loop = true steps.
	// V10: `after = []` is legal and means root; a MISSING `after` on a
	// non-exempt step is the error. The two are one check over hasAfter, and
	// two rows because they are two author-facing statements.
	exempt := index == 0 || step.Loop
	if !step.HasAfter() && !exempt {
		return &Error{
			Rule: "V8", Step: step.Name, Field: "after",
			Message: fmt.Sprintf(
				"step %q: `after` is required (write `after = []` for a root step); "+
					"only the first step and `loop = true` steps may omit it", step.Name),
		}
	}
	if step.HasAfter() && len(step.After) == 0 && step.Loop {
		// V10's other half: an explicit empty `after` is a root declaration,
		// which a loop step cannot make — its ordering comes from loop entry.
		return &Error{
			Rule: "V10", Step: step.Name, Field: "after",
			Message: fmt.Sprintf(
				"step %q: a `loop = true` step may not declare `after`, not even `[]`; "+
					"its ordering comes from loop entry", step.Name),
		}
	}

	// V18: loop = true steps have no `after`.
	if step.Loop && step.HasAfter() && len(step.After) > 0 {
		return &Error{
			Rule: "V18", Step: step.Name, Field: "after",
			Message: fmt.Sprintf(
				"step %q: `loop = true` steps have no `after` — their ordering comes from loop entry",
				step.Name),
		}
	}

	// V9: every after entry names a step in this workflow.
	for _, pred := range step.After {
		if _, ok := byName[pred]; !ok {
			return &Error{
				Rule: "V9", Step: step.Name, Field: "after",
				Message: fmt.Sprintf(
					"step %q: `after` names %q, which is not a step in this workflow",
					step.Name, pred),
			}
		}
	}

	// V11: inputs shape, existence, and artifact-kind resolution.
	if err := validateInputs(step, byName); err != nil {
		return err
	}

	// V32 and V33: the declared packet files' shape, and the executor token's
	// precondition (docs/tdd/packet-composition.md §1.6). Both are
	// decisions about BYTES, so they live here rather than in ValidateSchemas.
	if err := validatePacket(step); err != nil {
		return err
	}

	// V27's action clause and V28: the builtin's name and its params.
	if err := validateAction(step); err != nil {
		return err
	}

	// V12: on_fail in the closed vocabulary.
	if step.OnFail != "" && !slices.Contains(onFailValues, step.OnFail) {
		return &Error{
			Rule: "V12", Step: step.Name, Field: "on_fail",
			Message: fmt.Sprintf(
				"step %q: `on_fail` must be one of %s, got %q",
				step.Name, quotedList(onFailValues), step.OnFail),
		}
	}

	// V14: voters and vote_rule required on type="vote", forbidden elsewhere.
	//
	// It is checked BEFORE V13a/V13 so an author who wrote `type = "vote"` and
	// nothing else is told what a vote step IS before being told how its
	// failures must route. The shape of the step comes before the routing of
	// its failures; reversing the two answers a question the author has not
	// reached yet.
	if step.Type == TypeVote {
		if len(step.Voters) == 0 {
			return &Error{
				Rule: "V14", Step: step.Name, Field: "voters",
				Message: fmt.Sprintf(
					"step %q: `voters` is required on `type=\"vote\"` steps", step.Name),
			}
		}
		if step.VoteRule == "" {
			return &Error{
				Rule: "V14", Step: step.Name, Field: "vote_rule",
				Message: fmt.Sprintf(
					"step %q: `vote_rule` is required on `type=\"vote\"` steps", step.Name),
			}
		}
	} else {
		if len(step.Voters) > 0 {
			return &Error{
				Rule: "V14", Step: step.Name, Field: "voters",
				Message: fmt.Sprintf(
					"step %q: `voters` is only valid on `type=\"vote\"` steps", step.Name),
			}
		}
		if step.VoteRule != "" {
			return &Error{
				Rule: "V14", Step: step.Name, Field: "vote_rule",
				Message: fmt.Sprintf(
					"step %q: `vote_rule` is only valid on `type=\"vote\"` steps", step.Name),
			}
		}
	}

	// V13a: on_fail is required, explicitly, on EVERY GATE STEP — `type="human"`
	// and `type="vote"` alike — the corollary of V13 over the EFFECTIVE value
	// (§4.3.2).
	//
	// Silence is not neutral, because §11.1's default IS `waiting-human`: a gate
	// that declares nothing HAS a routing its author never chose. That was
	// already stated for human gates; it is stated for vote gates now because a
	// declared vote step omitting `on_fail` parked on a failed tally, and an
	// author who meant `fix-loop` could not tell by reading the step. The
	// grammar refuses at register rather than letting the run discover it.
	//
	// It is checked before V13 so a step that declares nothing gets the specific
	// message ("must declare on_fail") rather than the confusing "your reject
	// routing is waiting-human" for a field the author never wrote.
	//
	// THE LEGAL SETS DIFFER, which is exactly why V13's prohibition below is NOT
	// extended alongside this. `waiting-human` is forbidden on a human gate — it
	// would park on the resolution of the thing that just rejected — and LEGAL
	// on a vote gate, where it is the ESCALATION: a tally that did not reach its
	// threshold decided nothing, so the question passes to an operator who has
	// not been asked yet. Requiring the author to say which they mean is the
	// whole of this rule; deciding for them is what it refuses to do.
	if (step.Type == TypeHuman || step.Type == TypeVote) && step.OnFail == "" {
		legal := onFailValues
		if step.Type == TypeHuman {
			legal = humanOnFailValues()
		}
		return &Error{
			Rule: "V13a", Step: step.Name, Field: "on_fail",
			Message: fmt.Sprintf(
				"step %q: `type=%q` step must declare `on_fail`; legal values here: %s",
				step.Name, step.Type, quotedList(legal)),
		}
	}

	// V13: a type="human" step's reject routing may not be waiting-human,
	// evaluated against the EFFECTIVE routing so the default cannot smuggle
	// the deadlock back in. A human gate that routes rejects to waiting-human
	// parks forever on the resolution of the thing that just rejected.
	//
	// HUMAN ONLY, deliberately. See V13a above: the same routing on a vote gate
	// is the escalation to an operator, not a wait on the decider that just
	// declined, so the two kinds do not share this prohibition.
	if step.Type == TypeHuman && step.EffectiveOnFail() == OnFailWaitingHuman {
		return &Error{
			Rule: "V13", Step: step.Name, Field: "on_fail",
			Message: fmt.Sprintf(
				"step %q: `on_fail` is %q, but a `type=\"human\"` step may not route rejects there — "+
					"it would park the issue on the resolution of the thing that just rejected; "+
					"legal values here: %s",
				step.Name, OnFailWaitingHuman, quotedList(humanOnFailValues())),
		}
	}

	// V16: min_siblings >= 1 and <= len(fanout); only on fanout steps.
	if step.MinSiblings != nil {
		if class != ClassFanout {
			return &Error{
				Rule: "V16", Step: step.Name, Field: "min_siblings",
				Message: fmt.Sprintf(
					"step %q: `min_siblings` is only valid on fanout steps", step.Name),
			}
		}
		if *step.MinSiblings < 1 {
			return &Error{
				Rule: "V16", Step: step.Name, Field: "min_siblings",
				Message: fmt.Sprintf(
					"step %q: `min_siblings` must be >= 1, got %d", step.Name, *step.MinSiblings),
			}
		}
		if *step.MinSiblings > len(step.Fanout) {
			return &Error{
				Rule: "V16", Step: step.Name, Field: "min_siblings",
				Message: fmt.Sprintf(
					"step %q: `min_siblings` is %d but `fanout` declares %d hints",
					step.Name, *step.MinSiblings, len(step.Fanout)),
			}
		}
	}

	// V17: after_loop names an existing step; only meaningful with loop = true
	// in the workflow.
	if step.AfterLoop != "" {
		if _, ok := byName[step.AfterLoop]; !ok {
			return &Error{
				Rule: "V17", Step: step.Name, Field: "after_loop",
				Message: fmt.Sprintf(
					"step %q: `after_loop` names %q, which is not a step in this workflow",
					step.Name, step.AfterLoop),
			}
		}
		if !hasLoopStep(def) {
			return &Error{
				Rule: "V17", Step: step.Name, Field: "after_loop",
				Message: fmt.Sprintf(
					"step %q: `after_loop` is only meaningful in a workflow that declares a `loop = true` step",
					step.Name),
			}
		}
	}

	// V17b — V17's mirror (DKT-196, surfaced by DKT-168): a step that CAN
	// route `fix-loop` requires a `loop = true` step to instantiate. Without
	// one, every loop entry bumps the counter and supersedes the downstream,
	// and instantiateOrdinal emits zero rows — work is superseded and nothing
	// replaces it, a recorded no-op in exactly the silent-fix-that-never-ran
	// shape the fix-loop machinery exists to prevent.
	if !hasLoopStep(def) {
		field := ""
		if step.OnFail == OnFailFixLoop {
			field = "on_fail"
		} else if _, ok := step.Threshold[OnFailFixLoop]; ok {
			field = "threshold"
		}
		if field != "" {
			return &Error{
				Rule: "V17b", Step: step.Name, Field: field,
				Message: fmt.Sprintf(
					"step %q can route `fix-loop`, but the workflow declares no "+
						"`loop = true` step — the loop entry would supersede downstream "+
						"work and instantiate nothing in its place", step.Name),
			}
		}
	}

	// V19: max_attempts >= 1; max_fix_loops >= 0; expected_cost >= 0.
	if step.MaxAttempts != nil && *step.MaxAttempts < 1 {
		return &Error{
			Rule: "V19", Step: step.Name, Field: "max_attempts",
			Message: fmt.Sprintf(
				"step %q: `max_attempts` must be >= 1, got %d", step.Name, *step.MaxAttempts),
		}
	}
	if step.MaxFixLoops != nil && *step.MaxFixLoops < 0 {
		return &Error{
			Rule: "V19", Step: step.Name, Field: "max_fix_loops",
			Message: fmt.Sprintf(
				"step %q: `max_fix_loops` must be >= 0, got %d", step.Name, *step.MaxFixLoops),
		}
	}
	if step.ExpectedCost != nil && *step.ExpectedCost < 0 {
		return &Error{
			Rule: "V19", Step: step.Name, Field: "expected_cost",
			Message: fmt.Sprintf(
				"step %q: `expected_cost` must be >= 0, got %v", step.Name, *step.ExpectedCost),
		}
	}

	// V20/V21: threshold routings and predicates.
	if err := validateThreshold(step, byName); err != nil {
		return err
	}

	// V22: `when` parses as a predicate over kind/labels only.
	if step.When != "" {
		if err := validateWhen(step); err != nil {
			return err
		}
	}

	// V25: payload matches name@version shape. SHAPE ONLY at this stage — the
	// schema register does not exist until S5, so there is nothing to resolve
	// the name against yet (§6.14).
	if step.Payload != "" && !PayloadShape.MatchString(step.Payload) {
		return &Error{
			Rule: "V25", Step: step.Name, Field: "payload",
			Message: fmt.Sprintf(
				"step %q: `payload` must have the shape `name@version`, got %q",
				step.Name, step.Payload),
		}
	}

	return nil
}

// producedKind resolves the artifact kind a step actually produces, per step
// class (TDD §4.3.1). The kind itself is ArtifactKind's — expand.go and this
// package must agree on which field the kind comes from, and two readers
// deriving it independently is how they drift.
//
// The second return reports whether the step produces an artifact at all:
// human and vote steps produce none — a gate records a decision, not an
// artifact. ClassType is exactly the class ArtifactKind answers "" for
// unconditionally, so `!= ClassType` is that same table read from the other
// side.
func producedKind(step *Step) (kind string, produces bool) {
	return ArtifactKind(step), step.StepClass() != ClassType
}

// inputShape matches the §11.1 `inputs` grammar: `<step>.<kind>`, `<step>.*`,
// `issue.body`, or `issue.diff`.
var inputShape = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\.([A-Za-z0-9_-]+|\*)$`)

// InputIssueLatestPrefix is the `issue.latest.<kind>` engine form's prefix
// (DKT-492): the issue's latest recorded round of artifacts of one kind,
// whoever produced them. Exported because the engine's resolver consumes the
// same form the validator admits.
const InputIssueLatestPrefix = "issue.latest."

// latestKindShape is the `<kind>` half of `issue.latest.<kind>` — inputShape's
// kind grammar, deliberately without the `*` alternative: "the latest artifact
// of every kind" answers no question a consumer can ask, and admitting it
// would make one input's resolution span the whole artifact table.
var latestKindShape = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// LatestKind reports whether a declared input is the `issue.latest.<kind>`
// form, returning its kind half. ONE parser for the form, shared by V11, L4,
// and the engine's resolver — PayloadShape's reasoning applied again: two
// readings of one grammar in two packages are exactly how they would drift.
func LatestKind(input string) (kind string, ok bool) {
	rest, found := strings.CutPrefix(input, InputIssueLatestPrefix)
	if !found || !latestKindShape.MatchString(rest) {
		return "", false
	}
	return rest, true
}

// PayloadShape matches §11.1's `schema@ver`.
//
// It is EXPORTED and shared rather than restated, because `docket schema
// register` accepts the same grammar (TDD §4.5): the shape a workflow
// REFERENCES and the shape a registry ACCEPTS cannot be allowed to drift, and
// two regexps in two packages are exactly how they would.
var PayloadShape = regexp.MustCompile(`^[A-Za-z0-9_-]+@[0-9]+$`)

// ParsePayloadRef splits a `name@version` reference.
//
// Unlike model.ParseWorkflowRef, the version is REQUIRED and there is no
// "highest registered" fallback: a `payload` declaration names an exact version
// because that is what a run pins, and a registration that guessed a version
// would create a row nobody asked for.
func ParsePayloadRef(ref string) (name string, version int, err error) {
	if !PayloadShape.MatchString(ref) {
		return "", 0, fmt.Errorf(
			"%q must have the shape `name@version`, with a name of letters, "+
				"digits, `_` or `-` and a version that is an integer >= 1", ref)
	}
	at := strings.LastIndex(ref, "@")
	version, err = strconv.Atoi(ref[at+1:])
	if err != nil || version < 1 {
		return "", 0, fmt.Errorf("%q: version must be an integer >= 1", ref)
	}
	return ref[:at], version, nil
}

// validateInputs is V11: entries match the declared shapes, the named step
// exists, and a `<step>.<kind>` names a kind that step actually produces.
//
// It also enforces the PRODUCER side of the same table (TDD §6.13): an `action`
// step must declare `params.output`. That is V5's neighbourhood but a distinct
// check, and it rides here because V11 is where the produced-kind table is
// enforced — a step whose produced kind is empty can never satisfy V11 for any
// downstream consumer, so the two rules are one rule seen from two ends.
func validateInputs(step *Step, byName map[string]*Step) error {
	if step.StepClass() == ClassAction && ArtifactKind(step) == "" {
		return &Error{
			Rule: "V11", Step: step.Name, Field: "params",
			Message: fmt.Sprintf(
				"step %q: an `action` step must declare `params.output` — it is the "+
					"kind the step produces, so without it no downstream `inputs` "+
					"entry could ever resolve against this step",
				step.Name),
		}
	}

	for _, input := range step.Inputs {
		if input == "issue.body" || input == "issue.diff" {
			continue
		}

		// `issue.latest.<kind>` (DKT-492) is engine-produced like the issue
		// forms: it resolves to the issue's latest recorded round of that
		// kind, whoever produced it, so it names no producer step. It exists
		// for the input the named forms cannot express — a loop BODY reading
		// the live version of the thing it iterates: the body is structurally
		// outside `after_loop`'s downstream set (V10/V18: it cannot declare
		// `after`), so the loop-producer redirect never rebinds its inputs,
		// and a declared `<author>.<kind>` falls back per §7.4 to the
		// ordinal-0 draft forever. The kind must be one some step produces:
		// an issue can hold artifacts of no other kind, so anything else is a
		// typo caught now rather than an input resolving to nothing on every
		// run — the same produced-kind table `<step>.<kind>` is held to.
		if input == "issue.latest" || strings.HasPrefix(input, InputIssueLatestPrefix) {
			kind, ok := LatestKind(input)
			if !ok {
				return &Error{
					Rule: "V11", Step: step.Name, Field: "inputs",
					Message: fmt.Sprintf(
						"step %q: `inputs` entry %q must be `issue.latest.<kind>`, "+
							"with a kind of letters, digits, `_` or `-`",
						step.Name, input),
				}
			}
			if !anyStepProduces(byName, kind) {
				return &Error{
					Rule: "V11", Step: step.Name, Field: "inputs",
					Message: fmt.Sprintf(
						"step %q: `inputs` entry %q names kind %q, which no step in "+
							"this workflow produces",
						step.Name, input, kind),
				}
			}
			continue
		}

		m := inputShape.FindStringSubmatch(input)
		if m == nil {
			return &Error{
				Rule: "V11", Step: step.Name, Field: "inputs",
				Message: fmt.Sprintf(
					"step %q: `inputs` entry %q must be `<step>.<kind>`, `<step>.*`, `issue.body`, `issue.diff`, or `issue.latest.<kind>`",
					step.Name, input),
			}
		}
		producerName, kind := m[1], m[2]

		producer, ok := byName[producerName]
		if !ok {
			return &Error{
				Rule: "V11", Step: step.Name, Field: "inputs",
				Message: fmt.Sprintf(
					"step %q: `inputs` entry %q names step %q, which is not a step in this workflow",
					step.Name, input, producerName),
			}
		}

		// `<step>.gate-results` (DKT-77) is ENGINE-PRODUCED, like the issue
		// forms: it resolves to what the engine recorded for the named step,
		// so it needs the step to exist and nothing about what the step emits.
		// A producer with no gates resolves to an empty array at runtime — a
		// legal answer, not an authoring error, because gates can also arrive
		// from a fence source the definition does not enumerate.
		if kind == GateResultsKind {
			continue
		}

		produced, produces := producedKind(producer)
		if !produces {
			return &Error{
				Rule: "V11", Step: step.Name, Field: "inputs",
				Message: fmt.Sprintf(
					"step %q: `inputs` entry %q names step %q, which is a `type=%q` step and produces no artifact",
					step.Name, input, producerName, producer.Type),
			}
		}

		if kind == "*" {
			continue
		}
		if produced != kind {
			return &Error{
				Rule: "V11", Step: step.Name, Field: "inputs",
				Message: fmt.Sprintf(
					"step %q: `inputs` entry %q names kind %q, but step %q produces %q",
					step.Name, input, kind, producerName, produced),
			}
		}
	}
	return nil
}

// anyStepProduces reports whether any step of the workflow produces artifacts
// of this kind — the produced-kind table validateInputs holds `<step>.<kind>`
// to, read across every producer for the `issue.latest.<kind>` form.
func anyStepProduces(byName map[string]*Step, kind string) bool {
	for _, step := range byName {
		if produced, produces := producedKind(step); produces && produced == kind {
			return true
		}
	}
	return false
}

// thresholdRoutings are the §11.2 non-step routings.
var thresholdRoutings = []string{OnFailFixLoop, OnFailWaitingHuman, "pass"}

// predicateShape matches the §11.2 grammar `agg(field op literal)`.
var predicateShape = regexp.MustCompile(
	`^\s*(any|all|count>=\d+)\s*\(\s*([A-Za-z0-9_.-]+)\s*(==|!=|>=|>|<=|<)\s*(\S+?)\s*\)\s*$`)

// validateThreshold is V20 and V21.
func validateThreshold(step *Step, byName map[string]*Step) error {
	// Iterate deterministically: a map's range order varies per run, and an
	// error message that varies is one nobody can test against.
	routings := make([]string, 0, len(step.Threshold))
	for routing := range step.Threshold {
		routings = append(routings, routing)
	}
	sort.Strings(routings)

	for _, routing := range routings {
		// V20: keys in {fix-loop, waiting-human, pass} union step names.
		if !slices.Contains(thresholdRoutings, routing) {
			if _, ok := byName[routing]; !ok {
				return &Error{
					Rule: "V20", Step: step.Name, Field: "threshold",
					Message: fmt.Sprintf(
						"step %q: `threshold` routing %q must be one of %s or a step name in this workflow",
						step.Name, routing, quotedList(thresholdRoutings)),
				}
			}
		}

		// V21: the predicate parses as agg(field op literal). At this stage
		// predicates parse GRAMMATICALLY only — the field and the literal are
		// opaque tokens until their payload schema registers at S5. The
		// grammar knows `agg(field op literal)`, never what a field means,
		// which is the genericity line holding exactly where §11.2 draws it.
		if !predicateShape.MatchString(step.Threshold[routing]) {
			return &Error{
				Rule: "V21", Step: step.Name, Field: "threshold",
				Message: fmt.Sprintf(
					"step %q: `threshold` predicate %q must have the shape "+
						"`agg(field op literal)` with agg in {any, all, count>=n} "+
						"and op in {==, !=, >=, >, <=, <}",
					step.Name, step.Threshold[routing]),
			}
		}
	}
	return nil
}

// whenShape matches the §11.1 `when` grammar: a predicate over the issue's
// `kind` or `labels` only (engine-core §4: "conditions (predicates over issue
// kind/labels only)"). Nothing else is addressable — a `when` over a status or
// an assignee is a validation error, not a silently-false predicate.
var whenShape = regexp.MustCompile(
	`^\s*(kind|labels)\s*(==|!=|contains)\s*(\S+)\s*$`)

func validateWhen(step *Step) error {
	for _, clause := range splitWhen(step.When) {
		if !whenShape.MatchString(clause) {
			return &Error{
				Rule: "V22", Step: step.Name, Field: "when",
				Message: fmt.Sprintf(
					"step %q: `when` clause %q must be a predicate over `kind` or `labels` only, "+
						"as `<kind|labels> <==|!=|contains> <value>`",
					step.Name, strings.TrimSpace(clause)),
			}
		}
	}
	return nil
}

// splitWhen breaks a `when` expression into its conjuncts. `and` is the only
// connective: a disjunction over kind/labels is expressible as two steps, and
// admitting one operator keeps the predicate language small enough to stay
// obviously decidable.
func splitWhen(expr string) []string {
	parts := strings.Split(expr, " and ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// validateLimits is V24: `max` >= 1 and the durations parse.
func validateLimits(def *Definition) error {
	classes := make([]string, 0, len(def.Limits))
	for class := range def.Limits {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	for _, class := range classes {
		limit := def.Limits[class]
		if limit.Max < 1 {
			return &Error{
				Rule: "V24", Field: "limits",
				Message: fmt.Sprintf(
					"[limits] %q: `max` must be >= 1, got %d", class, limit.Max),
			}
		}

		// lease_ttl and max_step_duration are the same check — a named field
		// that, when present, must parse as a Go duration — over two fields,
		// so one loop runs it rather than writing the block out twice.
		durationFields := []struct{ name, value string }{
			{"lease_ttl", limit.LeaseTTL},
			{"max_step_duration", limit.MaxStepDuration},
		}
		for _, field := range durationFields {
			if field.value == "" {
				continue
			}
			if _, err := time.ParseDuration(field.value); err != nil {
				return &Error{
					Rule: "V24", Field: "limits",
					Message: fmt.Sprintf(
						"[limits] %q: `%s` %q is not a duration", class, field.name, field.value),
				}
			}
		}
	}
	return nil
}

// applyDefaults materializes the §11.1 default column into the parsed form, so
// the stored `parsed` JSON is the pinned INTERPRETATION rather than a
// re-derivation every reader has to repeat identically.
//
// V23 lives here: `class` defaults to the `executor` value.
func applyDefaults(def *Definition) {
	for _, step := range def.Steps {
		// V23: class default = executor value.
		if step.Class == "" && step.Executor != "" {
			step.Class = step.Executor
		}

		// on_fail default = waiting-human (§11.1). V13/V13a guarantee this
		// default is never reached on a type="human" step.
		if step.OnFail == "" {
			step.OnFail = OnFailWaitingHuman
		}

		// min_siblings default = all (§11.1 "default = all").
		if step.MinSiblings == nil && len(step.Fanout) > 0 {
			n := len(step.Fanout)
			step.MinSiblings = &n
		}

		// expected_cost default = 0 (§11.1). Parsed, validated, stored, and
		// emitted on `next` rows at this stage, but enforcing nothing — S6
		// owns the budget floor. Materializing the default now keeps the wire
		// shape whole.
		if step.ExpectedCost == nil {
			zero := 0.0
			step.ExpectedCost = &zero
		}

		// holds_tree default = true. Materialized rather than left nil because
		// THE DEFAULT IS THE UNSAFE ONE TO GUESS: a reader seeing nil must not
		// have to remember which way it falls, and the pinned `parsed` JSON is
		// where that interpretation is settled once for every reader.
		if step.HoldsTree == nil {
			holds := true
			step.HoldsTree = &holds
		}

		// `loop` defaults to false, which is the Go zero value already.
	}
}

func hasLoopStep(def *Definition) bool {
	for _, step := range def.Steps {
		if step.Loop {
			return true
		}
	}
	return false
}

// humanOnFailValues is the closed vocabulary minus waiting-human — the legal
// routings for a type="human" step's reject, per V13.
func humanOnFailValues() []string {
	out := make([]string, 0, len(onFailValues)-1)
	for _, v := range onFailValues {
		if v != OnFailWaitingHuman {
			out = append(out, v)
		}
	}
	return out
}

// describeDeclared renders which of the four alternatives a step declared, for
// V5's message. "none" and a list both name the actual problem, which "exactly
// one of ..." alone does not.
func describeDeclared(step *Step) string {
	var declared []string
	if step.Executor != "" {
		declared = append(declared, "`executor`")
	}
	if step.Action != "" {
		declared = append(declared, "`action`")
	}
	if step.Type != "" {
		declared = append(declared, "`type`")
	}
	if len(step.Fanout) > 0 {
		declared = append(declared, "`fanout`")
	}
	if len(declared) == 0 {
		return "none"
	}
	return strings.Join(declared, ", ")
}

func quotedList(values []string) string {
	return strings.Join(quotedValues(values), ", ")
}
