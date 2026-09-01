package workflow

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// RuleIDs is the register-time validation table, by ID, in documented order —
// engine-spine §4.3 as payloads-thresholds §4.9.1 extends it. It is the
// authority TestValidationTableIsComplete asserts set equality against — never a
// count, which is exactly the assertion that breaks when a rule is split
// (V13/V13a are one check and two author-facing errors; V21 is now one grammar
// rule and four cross-validation rules).
//
// V21a-V21d, V25a, V28a, V29, V30, and V37a are the SCHEMA-AWARE half and live
// in ValidateSchemas rather than in Validate: they are the only rules that ask
// a question about the environment, and Validate stays a pure function of bytes
// (§4.9.2). V27, V28, and V31 are decisions about bytes and stay here.
var RuleIDs = []string{
	"V1", "V2", "V3", "V4", "V5", "V6", "V7", "V8", "V9", "V10",
	"V11", "V11a", "V11b", "V12", "V13", "V13a", "V14", "V15", "V16", "V17", "V17b", "V17c", "V18", "V19",
	"V20", "V21", "V21a", "V21b", "V21c", "V21d",
	"V22", "V23", "V24", "V25", "V25a", "V26",
	"V27", "V28", "V28a", "V29", "V30", "V31",
	"V32", "V33", "V34", "V35", "V36", "V37", "V37a", "V38",
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
		// The same reservation for `issue.linked` (DKT-547): the
		// cross-issue linked-artifact form is resolved before any step
		// lookup, so a step under that name could never be addressed.
		if step.Name == "issue.linked" || strings.HasPrefix(step.Name, InputIssueLinkedPrefix) {
			return &Error{
				Rule: "V34", Step: step.Name, Field: "name",
				Message: fmt.Sprintf(
					"step name %q is reserved: `issue.linked.<relation>.<kind>` is "+
						"the engine-served cross-issue input form, and a step under "+
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

	// V11b: `vote-record` is a RESERVED kind (DKT-545) — V11a's reasoning for
	// the vote step's record: `<step>.vote-record` resolves to the proposal
	// the engine recorded for the named vote step, so a step emitting an
	// artifact of that kind could never be addressed as an input.
	if produced, _ := producedKind(step); produced == VoteRecordKind {
		return &Error{
			Rule: "V11b", Step: step.Name, Field: "emits",
			Message: fmt.Sprintf(
				"step %q produces kind %q, which is reserved: "+
					"`<step>.%s` is the engine-served input form for a vote "+
					"step's recorded proposal, and an artifact of that kind "+
					"could never be addressed", step.Name, VoteRecordKind,
				VoteRecordKind),
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

	// V35: `serves` declares a loop CLUSTER (§11.3 cluster scoping, DKT-544)
	// and every part of the declaration must be coherent at register time:
	// only a `loop = true` body has fix-loop routings to serve, an entry must
	// name a step of this workflow, and the named step must be able to route
	// `fix-loop` at all — a body serving a step that never routes there is a
	// cluster that can never be entered, which is a misdeclaration and not a
	// choice.
	if len(step.Serves) > 0 && !step.Loop {
		return &Error{
			Rule: "V35", Step: step.Name, Field: "serves",
			Message: fmt.Sprintf(
				"step %q: `serves` is only valid on `loop = true` steps — it names "+
					"the steps whose `fix-loop` routings this loop body answers",
				step.Name),
		}
	}
	for _, target := range step.Serves {
		if strings.TrimSpace(target) == "" {
			return &Error{
				Rule: "V35", Step: step.Name, Field: "serves",
				Message: fmt.Sprintf("step %q: `serves` contains an empty entry", step.Name),
			}
		}
		named, ok := byName[target]
		if !ok {
			return &Error{
				Rule: "V35", Step: step.Name, Field: "serves",
				Message: fmt.Sprintf(
					"step %q: `serves` names %q, which is not a step in this workflow",
					step.Name, target),
			}
		}
		if !canRouteFixLoop(named) {
			return &Error{
				Rule: "V35", Step: step.Name, Field: "serves",
				Message: fmt.Sprintf(
					"step %q: `serves` names %q, which never routes `fix-loop` — "+
						"neither its `on_fail` nor any `threshold` key routes there, "+
						"so this loop body would serve a trigger that cannot fire",
					step.Name, target),
			}
		}
	}

	// V17c — V17b scoped per trigger (DKT-544): once any body declares
	// `serves`, a step that can route `fix-loop` must still be served by AT
	// LEAST ONE `loop = true` body (a body with no `serves` serves every
	// trigger, so this only bites when every body is scoped and one trigger is
	// in none of their lists). An unserved trigger's loop entry would bump the
	// counter and instantiate nothing — V17b's exact silent-no-op shape,
	// reintroduced per cluster.
	if hasLoopStep(def) && canRouteFixLoop(step) && !anyBodyServes(def, step.Name) {
		field := "on_fail"
		if step.OnFail != OnFailFixLoop {
			field = "threshold"
		}
		return &Error{
			Rule: "V17c", Step: step.Name, Field: field,
			Message: fmt.Sprintf(
				"step %q can route `fix-loop`, but no `loop = true` step serves it — "+
					"every loop body's `serves` names other steps, so this trigger's "+
					"loop entry would supersede downstream work and instantiate "+
					"nothing in its place; add %q to a body's `serves` (or declare "+
					"a body without `serves`, which serves every trigger)",
				step.Name, step.Name),
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
	if step.MaxStalledRounds != nil && *step.MaxStalledRounds < 0 {
		return &Error{
			Rule: "V19", Step: step.Name, Field: "max_stalled_rounds",
			Message: fmt.Sprintf(
				"step %q: `max_stalled_rounds` must be >= 0, got %d",
				step.Name, *step.MaxStalledRounds),
		}
	}
	if step.ExpectedCost != nil && *step.ExpectedCost < 0 {
		return &Error{
			Rule: "V19", Step: step.Name, Field: "expected_cost",
			Message: fmt.Sprintf(
				"step %q: `expected_cost` must be >= 0, got %v", step.Name, *step.ExpectedCost),
		}
	}

	// V38: `max_stalled_rounds` is a non-convergence bound over THIS step's
	// per-round routed volume (DKT-870), so it is coherent only on a step
	// that (a) can actually route `fix-loop` — the check runs at loop entry,
	// triggered by this step's own routing — and (b) records an artifact
	// whose payload the volume can be counted from. Declared anywhere else it
	// is inert, and an inert declaration is a misdeclaration, not a choice
	// (V17/V35's discipline).
	if step.MaxStalledRounds != nil && *step.MaxStalledRounds > 0 {
		if !canRouteFixLoop(step) {
			return &Error{
				Rule: "V38", Step: step.Name, Field: "max_stalled_rounds",
				Message: fmt.Sprintf(
					"step %q: `max_stalled_rounds` bounds this step's own "+
						"`fix-loop` rounds, but neither its `on_fail` nor any "+
						"`threshold` key routes there, so the bound could never "+
						"fire", step.Name),
			}
		}
		if ArtifactKind(step) == "" {
			return &Error{
				Rule: "V38", Step: step.Name, Field: "max_stalled_rounds",
				Message: fmt.Sprintf(
					"step %q: `max_stalled_rounds` counts the elements of this "+
						"step's recorded payload per round, and a `type` step "+
						"records no artifact to count, so the bound could never "+
						"fire", step.Name),
			}
		}
	}

	// V37: `pass_floor` compares a payload value's POSITION against the
	// declared `at`'s (DKT-870), and a position exists only in the order a
	// pinned `payload` schema declares — a floor with no schema can never
	// position anything and is an inert declaration. Whether the field IS
	// ordered and `at` IS in its order is V37a's, beside the schema (§4.9.1).
	if step.PassFloor != nil {
		if step.PassFloor.Field == "" || step.PassFloor.At == "" {
			return &Error{
				Rule: "V37", Step: step.Name, Field: "pass_floor",
				Message: fmt.Sprintf(
					"step %q: `pass_floor` requires both `field` (the payload "+
						"property to compare) and `at` (a value of that "+
						"property's declared order)", step.Name),
			}
		}
		if step.Payload == "" {
			return &Error{
				Rule: "V37", Step: step.Name, Field: "pass_floor",
				Message: fmt.Sprintf(
					"step %q: `pass_floor` compares positions in a declared "+
						"order, so the step must declare `payload = "+
						"\"name@version\"` naming a schema that orders %q",
					step.Name, step.PassFloor.Field),
			}
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

// InputIssueLinkedPrefix is the `issue.linked.<relation>.<kind>` engine form's
// prefix (DKT-547): the latest recorded artifact of one kind held by the
// issue(s) this issue is LINKED to by a relation, resolved and pinned at
// activation. Exported because the engine's resolver consumes the same form
// the validator admits.
const InputIssueLinkedPrefix = "issue.linked."

// linkedRelationShape is the `<relation>` half of the form — a relation token
// (canonical or inverse, hyphenated or underscored); model.ParseRelationDirection
// is the authority on which tokens mean anything, this only bounds the shape.
var linkedRelationShape = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// LinkedInput reports whether a declared input is the
// `issue.linked.<relation>.<kind>` form, returning its two halves. ONE parser
// for the form, shared by V11, L4, and the engine's activation-time resolver —
// LatestKind's reasoning again: two readings of one grammar in two packages
// are exactly how they would drift.
//
// The split is on the FIRST dot after the prefix: relation tokens contain no
// dot, and the kind half reuses latestKindShape (one kind, never `*` — the
// wildcard answers no question a cross-issue consumer can ask).
func LinkedInput(input string) (relation, kind string, ok bool) {
	rest, found := strings.CutPrefix(input, InputIssueLinkedPrefix)
	if !found {
		return "", "", false
	}
	i := strings.Index(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	relation, kind = rest[:i], rest[i+1:]
	if !linkedRelationShape.MatchString(relation) || !latestKindShape.MatchString(kind) {
		return "", "", false
	}
	return relation, kind, true
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

		// `issue.linked.<relation>.<kind>` (DKT-547) is engine-produced like
		// the forms above, but its PRODUCER IS ANOTHER ISSUE'S RUN: activation
		// resolves this issue's linked issue(s) by the relation, pins each
		// one's latest recorded artifact of the kind, and fails loudly when
		// the relation or the artifact is missing. V11's produced-kind table
		// therefore deliberately does NOT apply — no step of THIS workflow
		// need produce the kind, because none does; the binding it enforces
		// elsewhere is enforced here by activation instead. What register CAN
		// refuse it does: a malformed shape, a relation outside the
		// vocabulary, and the two engine-reserved kinds, which no step
		// anywhere may emit (V11a/V11b) and which the form could therefore
		// never resolve.
		if input == "issue.linked" || strings.HasPrefix(input, InputIssueLinkedPrefix) {
			relation, kind, ok := LinkedInput(input)
			if !ok {
				return &Error{
					Rule: "V11", Step: step.Name, Field: "inputs",
					Message: fmt.Sprintf(
						"step %q: `inputs` entry %q must be "+
							"`issue.linked.<relation>.<kind>`, with a relation token "+
							"and one kind of letters, digits, `_` or `-` (never `*`)",
						step.Name, input),
				}
			}
			if _, _, err := model.ParseRelationDirection(relation); err != nil {
				return &Error{
					Rule: "V11", Step: step.Name, Field: "inputs",
					Message: fmt.Sprintf(
						"step %q: `inputs` entry %q names relation %q, which is not "+
							"a relation type; must be one of %v",
						step.Name, input, relation, model.RelationDirectionTokens()),
				}
			}
			if kind == GateResultsKind || kind == VoteRecordKind {
				return &Error{
					Rule: "V11", Step: step.Name, Field: "inputs",
					Message: fmt.Sprintf(
						"step %q: `inputs` entry %q names kind %q, which is "+
							"engine-reserved — no step may emit it, so no linked "+
							"issue could ever hold an artifact of it",
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
					"step %q: `inputs` entry %q must be `<step>.<kind>`, `<step>.*`, `issue.body`, `issue.diff`, `issue.latest.<kind>`, or `issue.linked.<relation>.<kind>`",
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

		// `<step>.vote-record` (DKT-545) is ENGINE-PRODUCED like gate-results:
		// it resolves to the proposal record the named vote step's tally left
		// — outcome, casts, and rationales — so it needs the producer to exist
		// and to BE a vote step. Any other step opens no proposal, so the
		// input could never resolve to anything on any run: a typo caught now
		// rather than an input silently absent forever.
		if kind == VoteRecordKind {
			if producer.Type != TypeVote {
				return &Error{
					Rule: "V11", Step: step.Name, Field: "inputs",
					Message: fmt.Sprintf(
						"step %q: `inputs` entry %q names step %q, which is not a "+
							"`type=\"vote\"` step — `%s` resolves only against vote "+
							"steps, whose tally leaves the record it serves",
						step.Name, input, producerName, VoteRecordKind),
				}
			}
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

// VoteCastField* are the addressable fields of one CAST in a vote step's
// threshold evaluation (DKT-545). The engine builds each cast's payload from
// exactly these keys, so the vocabulary is engine-defined the same way `when`'s
// kind/labels vocabulary is (V22) — a field outside it would silently never
// match, which is why V36 refuses it at register instead.
//
// `vote` and `verdict` are ALIASES for the same value: the model's word is
// `verdict` (model.Verdict, `vote cast --verdict`), and `vote` is the word a
// threshold author reaches for (`count>=2(vote == approve-with-concerns)`).
// Admitting both costs one map key; refusing one of them would refuse the
// spelling half of authors would try first.
const (
	VoteCastFieldVote    = "vote"
	VoteCastFieldVerdict = "verdict"
	VoteCastFieldVoter   = "voter"
)

// VoteCastFields is the closed field vocabulary of a vote step's threshold
// predicates — V36's authority, exported because the engine's cast-payload
// builder must produce exactly these keys and no reader may drift from it.
var VoteCastFields = []string{VoteCastFieldVote, VoteCastFieldVerdict, VoteCastFieldVoter}

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

		// V36 (DKT-545): a `type="vote"` step's threshold is evaluated over
		// the tally's CAST SET after an APPROVED tally, not over recorded
		// payloads, and each constraint below refuses a declaration that
		// could never route:
		//
		//   - the routing vocabulary is the three non-step routings only.
		//     Step-name interposition is the saga's machinery (the same-
		//     transaction skip of the unrouted gate, DKT-38's latch), and the
		//     vote routing path has none of it — a step-name key would record
		//     a routing nothing downstream consumes, RUN-25's exact shape.
		//   - the field vocabulary is the cast's (VoteCastFields). Casts are
		//     engine-produced like `when`'s kind/labels (V22), so any other
		//     field would silently never match.
		//   - ordered operators are refused. Casts have no registered schema,
		//     and §11.2 defines ordered comparisons only over `ordered_enum`
		//     fields — the comparison would be a guaranteed T3 park on every
		//     evaluation, which is a misdeclaration and not a choice.
		if step.Type == TypeVote {
			if !slices.Contains(thresholdRoutings, routing) {
				return &Error{
					Rule: "V36", Step: step.Name, Field: "threshold",
					Message: fmt.Sprintf(
						"step %q: a `type=\"vote\"` step's `threshold` routing %q "+
							"must be one of %s — step-name interposition is not "+
							"available on vote steps",
						step.Name, routing, quotedList(thresholdRoutings)),
				}
			}
			pred, err := ParsePredicate(step.Threshold[routing])
			if err != nil {
				// Unreachable: V21's shape check above admits exactly what
				// ParsePredicate parses. Kept so a grammar drift fails loudly.
				return &Error{
					Rule: "V36", Step: step.Name, Field: "threshold",
					Message: fmt.Sprintf("step %q: %v", step.Name, err),
				}
			}
			if pred.Ordered() {
				return &Error{
					Rule: "V36", Step: step.Name, Field: "threshold",
					Message: fmt.Sprintf(
						"step %q: `threshold` predicate %q uses the ordered "+
							"operator %q, but a vote step's threshold evaluates "+
							"over casts, which have no registered schema — "+
							"ordered comparisons are defined only over "+
							"`ordered_enum` fields (engine-spec §11.2); use == or !=",
						step.Name, step.Threshold[routing], pred.Op),
				}
			}
			if !slices.Contains(VoteCastFields, pred.Field) {
				return &Error{
					Rule: "V36", Step: step.Name, Field: "threshold",
					Message: fmt.Sprintf(
						"step %q: `threshold` predicate %q addresses field %q, "+
							"but a vote step's threshold evaluates over the cast "+
							"set, whose addressable fields are %s",
						step.Name, step.Threshold[routing], pred.Field,
						quotedList(VoteCastFields)),
				}
			}
		}
	}
	return nil
}

// whenShape matches the §11.1 `when` grammar: a predicate over the issue's
// `kind` or `labels` only (engine-core §4: "conditions (predicates over issue
// kind/labels only)"). Nothing else is addressable — a `when` over a status or
// an assignee is a validation error, not a silently-false predicate.
//
// Two clause forms:
//
//   - `<kind|labels> <==|!=|contains> <value>` — one value
//   - `labels contains-any (a, b, c)` — the lists intersect (DKT-550), also
//     spelled `labels contains_any [a, b, c]` (DKT-1000)
//
// `contains-any` is the step-level spelling of the workflow-level `labels_any`
// [match] clause, and it is evaluated by the same intersection test (Matches).
// It exists so "kind X AND any of these labels" is ONE homogeneous-`and`
// predicate: without it that need can only be written by mixing `and` with
// `or`, which V22 refuses. A set-membership operator answers it without a
// connective at all, so the homogeneity rule never enters into it.
//
// The operator has two spellings and the list has two delimiters — `contains-any`
// or `contains_any`, `(…)` or `[…]` — and all four combinations mean the same
// thing (DKT-1000). Accepting both is not decoration: `contains-any (…)` is what
// already-registered definitions carry, and `contains_any [a, b]` is the
// spelling authors reach for because it is how a list is written everywhere else
// in a workflow TOML. Refusing either would make an operator's first correct
// guess a validation error. The delimiters must PAIR — `(a, b]` matches neither
// branch — because RE2 has no backreference to enforce it in one branch, so the
// two spellings are written out as two.
//
// The list branch is FIRST in the alternation deliberately. Go's regexp is
// leftmost-first, so ordering it after the one-value branch would let
// `labels contains-any(a,b)` match as `labels contains "-any(a,b)"` — a clause
// that registers and then quietly evaluates as something the author never
// wrote.
//
// A list element may not contain whitespace, which is what keeps the new form
// clear of whenConnective: that splitter runs over the whole predicate before
// any clause is shape-checked, and it only separates on a connective with
// whitespace on BOTH sides. A pathological `(a , and , b)` therefore splits
// into two clauses that both fail this regex — V22 refuses it, and WhenHolds
// reads it as false, because both go through the same splitter and the same
// shape. Fail-closed, never divergent.
var whenShape = regexp.MustCompile(
	`^\s*(?:` +
		`(labels)\s*(contains-any|contains_any)\s*(?:` +
		`\(\s*(` + whenListElements + `)\s*\)` +
		`|\[\s*(` + whenListElements + `)\s*\]` +
		`)` +
		`|(kind|labels)\s*(==|!=|contains)\s*(\S+)` +
		`)\s*$`)

// whenListElements is a non-empty comma-separated list of whitespace-free
// values, quoted or bare. It is spelled ONCE and shared by both delimiter
// branches of whenShape so `(…)` and `[…]` cannot drift into accepting
// different element vocabularies — a list that registers under one delimiter
// and is refused under the other would make the two spellings different
// grammars wearing the same name.
//
// Brackets are excluded from an element for the same reason parens are: an
// element that could contain the closing delimiter would let `[a, b` match by
// swallowing it.
const whenListElements = `[^\s,()\[\]]+(?:\s*,\s*[^\s,()\[\]]+)*`

func validateWhen(step *Step) error {
	clauses, _, mixed := splitWhen(step.When)
	// A mixed predicate is refused BEFORE its clauses are shape-checked: the
	// operator wrote something whose meaning depends on a precedence rule the
	// grammar does not have, and reporting a clause typo first would tell them
	// to fix the wrong thing.
	if mixed {
		return &Error{
			Rule: "V22", Step: step.Name, Field: "when",
			Message: fmt.Sprintf(
				"step %q: `when` %q mixes `and` and `or`; a predicate must join its "+
					"clauses with one connective throughout, because the grammar has "+
					"no precedence rule and no parentheses to disambiguate the mix",
				step.Name, strings.TrimSpace(step.When)),
		}
	}
	for _, clause := range clauses {
		if !whenShape.MatchString(clause) {
			return &Error{
				Rule: "V22", Step: step.Name, Field: "when",
				Message: fmt.Sprintf(
					"step %q: `when` clause %q must be a predicate over `kind` or `labels` only, "+
						"as `<kind|labels> <==|!=|contains> <value>` or "+
						"`labels contains-any (a, b, c)` (equivalently "+
						"`labels contains_any [a, b, c]`), with clauses joined by "+
						"`and` throughout or `or` throughout",
					step.Name, strings.TrimSpace(clause)),
			}
		}
	}
	return nil
}

// The two connectives of the §11.1 `when` grammar.
const (
	WhenAnd = "and"
	WhenOr  = "or"
)

// whenConnective matches a connective token BETWEEN two clauses — whitespace on
// both sides is part of the match, so a label or kind literal that merely
// contains the letters (`labels contains and-then`) is not a separator. It is
// the one place the connective vocabulary is spelled, for the same reason
// whenShape is the one place the clause shape is: validate and evaluate must
// split a predicate identically or a definition that registered could evaluate
// as something else (predicate.go, on §11.2's single-regex discipline).
var whenConnective = regexp.MustCompile(`\s+(and|or)\s+`)

// splitWhen breaks a `when` expression into its clauses and reports which
// connective joins them.
//
// `and` and `or` may each join any number of clauses, but a single predicate
// must use ONE of them throughout — `mixed` is true otherwise, and V22 refuses
// it (DKT-548). That is the whole of the precedence question: `a and b or c`
// has two readings, the grammar has no parentheses to pick one, and picking a
// default silently would route work through a lane the author did not write.
// Requiring homogeneity costs an author one extra step in the rare mixed case
// and costs nothing in the common one.
//
// A single-clause predicate reports WhenAnd: with nothing to join, the two
// connectives agree, and `and` is the identity the old grammar already had.
func splitWhen(expr string) (clauses []string, connective string, mixed bool) {
	seps := whenConnective.FindAllStringSubmatchIndex(expr, -1)

	clauses = make([]string, 0, len(seps)+1)
	prev := 0
	for _, sep := range seps {
		clauses = append(clauses, strings.TrimSpace(expr[prev:sep[0]]))
		switch conn := expr[sep[2]:sep[3]]; {
		case connective == "":
			connective = conn
		case connective != conn:
			mixed = true
		}
		prev = sep[1]
	}
	clauses = append(clauses, strings.TrimSpace(expr[prev:]))

	if connective == "" {
		connective = WhenAnd
	}
	return clauses, connective, mixed
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

// canRouteFixLoop reports whether a step has any routing that can resolve to
// `fix-loop`: an explicit `on_fail`, or a `threshold` key. The DECLARED values
// only — the `on_fail` default is `waiting-human`, so silence never routes
// there.
func canRouteFixLoop(step *Step) bool {
	if step.OnFail == OnFailFixLoop {
		return true
	}
	_, ok := step.Threshold[OnFailFixLoop]
	return ok
}

// anyBodyServes reports whether at least one `loop = true` step serves a
// trigger — V17c's question, asked with the same ServesTrigger reading the
// engine's loop entry uses, so "who answers this trigger" has one definition.
func anyBodyServes(def *Definition, trigger string) bool {
	for _, step := range def.Steps {
		if step.Loop && ServesTrigger(step, trigger) {
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
