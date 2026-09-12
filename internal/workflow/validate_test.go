package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// validationCase is one row of the §4.3 validation table, as a test.
//
// `rule` is the documented rule ID and is the authority
// TestValidationTableIsComplete checks set equality against. `wants` are
// substrings the error must contain: every register-time error names the
// offending step and field, so a rule whose message drops one of them fails
// here rather than in an operator's terminal.
type validationCase struct {
	rule  string
	name  string
	src   string
	wants []string
	// resolver is set on V26's rows only. V1-V25 are decisions about BYTES and
	// Validate is pure; V26 is the one rule that asks a question about the
	// environment, so its cases supply that environment here rather than
	// forcing every other row to carry a database.
	resolver VoteRuleResolver
	// schemas is set on V21a-V21d and V25a's rows — §4.9.1's cross-validation
	// table, which asks the same kind of environment question V26 does and gets
	// the same treatment: a fake resolver in the row, and Validate left pure.
	schemas SchemaResolver
}

// fakeSchemas is the cross-validation rules' environment: the documents a
// resolver would return, keyed `name@version`.
//
// It compiles real documents rather than hand-building field tables, so the
// rules are tested against what a registration actually produces — a fake that
// answered from a hand-written map could disagree with the registry about what
// a schema declares, which is the one thing these rules must not do.
type fakeSchemas struct{ documents map[string]string }

func (f fakeSchemas) Schema(name string, version int) (*Registered, error) {
	ref := fmt.Sprintf("%s@%d", name, version)
	body, ok := f.documents[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotRegistered, ref)
	}
	return schema.Compile(name, version, []byte(body))
}

// riskSchema declares one ordered field, one enum field with NO order, and one
// typed field, so V21a-V21c each have something to refuse.
const riskSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "risk":  {"type": "string", "enum": ["low", "medium", "high"], "ordered_enum": true},
      "stage": {"type": "string", "enum": ["draft", "final"]},
      "count": {"type": "integer"}
    }
  }
}`

// registeredRisk is the environment every cross-validation row shares.
func registeredRisk() SchemaResolver {
	return fakeSchemas{documents: map[string]string{"risk-report@1": riskSchema}}
}

// fakeVoteRules is V26's environment: the set of registered rule names.
type fakeVoteRules struct {
	registered []string
	// elsewhere is the store-wide picture V26's remedy consults (DKT-264):
	// rule -> how many OTHER projects configure it, and one value they use.
	// Empty for every case but the one about the remedy, which is the point —
	// a rule genuinely nobody has must keep the original refusal.
	elsewhere map[string]struct {
		projects int
		value    string
	}
}

func (f fakeVoteRules) VoteRuleExists(rule string) (bool, error) {
	return slices.Contains(f.registered, rule), nil
}

func (f fakeVoteRules) RegisteredVoteRules() ([]string, error) {
	return f.registered, nil
}

func (f fakeVoteRules) RuleSetElsewhere(rule string) (int, string, error) {
	got := f.elsewhere[rule]
	return got.projects, got.value, nil
}

// twoStepPipeline is the fixture six V8-V18 rows share: a first step "a" that
// is a plain executor emitting "k", followed by step "b" with the tail the
// row supplies. Extracted because the six rows repeated this exact 9-line
// preamble verbatim; the varying tail is still literal TOML in the caller, so
// each row's distinguishing shape stays visible at the call site.
func twoStepPipeline(bTail string) string {
	return "\n[pipeline]\nname = \"w\"\nversion = 1\n" +
		"[[step]]\nname = \"a\"\nexecutor = \"x\"\nemits = \"k\"\n" +
		"[[step]]\nname = \"b\"\n" + bTail
}

// validationCases covers V1-V25 including V13a — 26 rules across 25 numbered
// IDs. V13 and V13a are separate cases with distinct messages: V13 feeds a
// human step an explicit `on_fail = "waiting-human"`, V13a feeds one that
// declares no `on_fail` at all (§4.3.2).
var validationCases = []validationCase{
	{
		rule: "V1", name: "pipeline name missing",
		src: `
[pipeline]
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
`,
		wants: []string{"[pipeline].name"},
	},
	{
		rule: "V2", name: "pipeline version below one",
		src: `
[pipeline]
name = "w"
version = 0
[[step]]
name = "a"
executor = "x"
emits = "k"
`,
		wants: []string{"[pipeline].version", ">= 1"},
	},
	{
		rule: "V3", name: "no steps",
		src: `
[pipeline]
name = "w"
version = 1
`,
		wants: []string{"at least one [[step]]"},
	},
	{
		rule: "V4", name: "duplicate step name",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
[[step]]
name = "a"
after = ["a"]
executor = "y"
emits = "k"
`,
		wants: []string{`"a"`, "more than once"},
	},
	{
		rule: "V5", name: "two of executor/action/type/fanout",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
action = "aggregate"
emits = "k"
`,
		wants: []string{`"a"`, "exactly one of", "`executor`", "`action`"},
	},
	{
		rule: "V6", name: "unknown type",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
type = "robot"
on_fail = "skip"
`,
		wants: []string{`"a"`, "`type`", "human", "vote"},
	},
	{
		rule: "V7", name: "executor step without emits",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
`,
		wants: []string{`"a"`, "`emits`", "executor steps"},
	},
	{
		rule: "V8", name: "missing after on a non-first step",
		src:   twoStepPipeline("executor = \"y\"\nemits = \"k\"\n"),
		wants: []string{`"b"`, "`after` is required", "after = []"},
	},
	{
		rule: "V9", name: "after names an unknown step",
		src:   twoStepPipeline("after = [\"ghost\"]\nexecutor = \"y\"\nemits = \"k\"\n"),
		wants: []string{`"b"`, "`after`", `"ghost"`, "not a step in this workflow"},
	},
	{
		rule: "V39", name: "after_fired names an unknown step",
		src:   twoStepPipeline("after = [\"a\"]\nafter_fired = [\"ghost\"]\nexecutor = \"y\"\nemits = \"k\"\n"),
		wants: []string{`"b"`, "`after_fired`", `"ghost"`, "not a step in this workflow"},
	},
	{
		rule: "V39a", name: "after_fired names a step not in after",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
[[step]]
name = "gate"
after = ["a"]
type = "human"
on_fail = "skip"
[[step]]
name = "b"
after = ["a"]
after_fired = ["gate"]
executor = "y"
emits = "k"
`,
		wants: []string{`"b"`, "`after_fired`", `"gate"`, "not in its `after`"},
	},
	{
		rule: "V39a", name: "after_fired on a loop step, which has no after",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
[[step]]
name = "check"
after = ["a"]
executor = "y"
emits = "report"
threshold = { "fix-loop" = "any(status == unmet)" }
[[step]]
name = "fix"
executor = "z"
emits = "k"
loop = true
after_loop = "a"
after_fired = ["check"]
`,
		wants: []string{`"fix"`, "`after_fired`", `"check"`, "not in its `after`"},
	},
	{
		rule: "V39", name: "after_fired beside after registers clean",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "reconcile"
executor = "x"
emits = "report"
threshold = { "security-vote" = "any(status == blocked)" }
[[step]]
name = "security-vote"
after = ["reconcile"]
type = "human"
on_fail = "skip"
[[step]]
name = "drain-highs"
after = ["security-vote"]
after_fired = ["security-vote"]
executor = "y"
emits = "k"
`,
	},
	{
		rule: "V10", name: "loop step declaring an empty after",
		src:   twoStepPipeline("after = []\nloop = true\nexecutor = \"y\"\nemits = \"k\"\nafter_loop = \"a\"\n"),
		wants: []string{`"b"`, "`after`", "loop entry"},
	},
	{
		rule: "V11", name: "inputs naming a kind the producer does not emit",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "change-summary"
[[step]]
name = "b"
after = ["a"]
executor = "y"
emits = "k"
inputs = ["a.findings"]
`,
		wants: []string{`"b"`, "`inputs`", `"a.findings"`, "change-summary"},
	},
	{
		// V11's vote-record clause (DKT-545): `<step>.vote-record` resolves
		// only against `type="vote"` steps — any other step opens no proposal,
		// so the input could never resolve to anything on any run.
		rule: "V11", name: "vote-record input naming a non-vote step",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
[[step]]
name = "b"
after = ["a"]
executor = "y"
emits = "j"
inputs = ["a.vote-record"]
`,
		wants: []string{`"b"`, "`inputs`", `"a.vote-record"`, "vote"},
	},
	{
		// V11a (DKT-77): `gate-results` is a reserved kind — the engine-served
		// input form would shadow an artifact of that kind.
		rule: "V11a", name: "a step may not emit the reserved gate-results kind",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "gate-results"
`,
		wants: []string{`"a"`, `"gate-results"`, "reserved"},
	},
	{
		// V11b (DKT-545): `vote-record` is reserved for the same reason.
		rule: "V11b", name: "a step may not emit the reserved vote-record kind",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "vote-record"
`,
		wants: []string{`"a"`, `"vote-record"`, "reserved"},
	},
	{
		rule: "V12", name: "on_fail outside the closed vocabulary",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
on_fail = "retry-forever"
`,
		wants: []string{`"a"`, "`on_fail`", "retry-forever", "fix-loop"},
	},
	{
		rule: "V13", name: "human step routing rejects to waiting-human",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "gate"
type = "human"
on_fail = "waiting-human"
`,
		wants: []string{
			`"gate"`, "`on_fail`", "may not route rejects",
			"the thing that just rejected",
		},
	},
	{
		rule: "V13a", name: "human step declaring no on_fail",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "gate"
type = "human"
`,
		wants: []string{
			`"gate"`, "must declare `on_fail`", `"fix-loop"`, `"skip"`, `"abandon-issue"`,
		},
	},
	{
		// V13a's other half: a DECLARED vote step must state its `on_fail` too.
		// Silence inherits §11.1's `waiting-human`, so the step parks on a
		// failed tally — which may be exactly right (escalate to an operator) or
		// exactly wrong (the author meant `fix-loop`), and nothing in the step
		// says which.
		//
		// The legal set here INCLUDES `waiting-human`, unlike the human case,
		// because on a vote gate that routing is the escalation rather than a
		// wait on the decider that just declined. V13's prohibition is human-
		// only for the same reason.
		rule: "V13a", name: "vote step declaring no on_fail",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "panel"
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
`,
		wants: []string{
			`"panel"`, "must declare `on_fail`", `"waiting-human"`, `"fix-loop"`,
		},
	},
	{
		rule: "V14", name: "vote step without voters",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "v"
type = "vote"
vote_rule = "majority"
`,
		wants: []string{`"v"`, "`voters`", "required"},
	},
	{
		// V26 (gates-trust §8.2): the rule NAMED must exist. V14 already
		// requires `vote_rule` to be present; this is the other half — a
		// workflow that cannot possibly tally should not register, which is
		// the discipline every V-rule follows.
		rule: "V26", name: "vote_rule names no registered configuration",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "v"
type = "vote"
voters = ["a", "b"]
vote_rule = "unanimous"
on_fail = "skip"
`,
		resolver: fakeVoteRules{registered: []string{"majority"}},
		// The refusal names the rule, lists the alternatives, and states the
		// remedy — an operator who cannot tell which of those they need is an
		// operator who has to guess.
		wants: []string{`"unanimous"`, "not registered", "majority", "config set"},
	},
	{
		rule: "V15", name: "empty fanout",
		src:   twoStepPipeline("after = [\"a\"]\nfanout = []\nemits = \"k\"\n"),
		wants: []string{`"b"`, "`fanout`"},
	},
	{
		rule: "V16", name: "min_siblings above the fanout width",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
fanout = ["p", "q"]
emits = "k"
min_siblings = 3
`,
		wants: []string{`"a"`, "`min_siblings`", "3", "2"},
	},
	{
		rule: "V17", name: "after_loop naming an unknown step",
		src:   twoStepPipeline("loop = true\nexecutor = \"y\"\nemits = \"k\"\nafter_loop = \"ghost\"\n"),
		wants: []string{`"b"`, "`after_loop`", `"ghost"`},
	},
	{
		// V17b is V17's mirror (DKT-196): a step that can route `fix-loop`
		// needs a `loop = true` step to instantiate, or the loop entry
		// supersedes downstream work and replaces it with nothing.
		rule: "V17b", name: "fix-loop routing with no loop step",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
on_fail = "fix-loop"
`,
		wants: []string{`"a"`, "fix-loop", "loop = true"},
	},
	{
		// V17c is V17b scoped per trigger (DKT-544): once every body declares
		// `serves`, a fix-loop-capable step named by none of them has no body
		// to instantiate — the same silent no-op, reintroduced per cluster.
		rule: "V17c", name: "fix-loop trigger no scoped body serves",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
on_fail = "fix-loop"
[[step]]
name = "b"
after = ["a"]
executor = "x"
emits = "k"
threshold = { "fix-loop" = "any(status == unmet)" }
[[step]]
name = "fix"
executor = "y"
emits = "k"
loop = true
serves = ["b"]
after_loop = "b"
`,
		wants: []string{`"a"`, "fix-loop", "serves"},
	},
	{
		rule: "V18", name: "loop step with after",
		src:   twoStepPipeline("after = [\"a\"]\nloop = true\nexecutor = \"y\"\nemits = \"k\"\nafter_loop = \"a\"\n"),
		wants: []string{`"b"`, "loop entry"},
	},
	{
		// V35 (DKT-544): `serves` is a loop-cluster declaration and only a
		// `loop = true` body has fix-loop routings to serve.
		rule: "V35", name: "serves on a non-loop step",
		src:   twoStepPipeline("after = [\"a\"]\nexecutor = \"y\"\nemits = \"k\"\nserves = [\"a\"]\n"),
		wants: []string{`"b"`, "`serves`", "loop = true"},
	},
	{
		rule: "V35", name: "serves naming an unknown step",
		src: twoStepPipeline("loop = true\nexecutor = \"y\"\nemits = \"k\"\n" +
			"serves = [\"ghost\"]\nafter_loop = \"a\"\n"),
		wants: []string{`"b"`, "`serves`", `"ghost"`},
	},
	{
		// A serves entry naming a step that never routes `fix-loop` declares a
		// cluster that can never be entered — a misdeclaration, not a choice.
		rule: "V35", name: "serves naming a step that never routes fix-loop",
		src: twoStepPipeline("loop = true\nexecutor = \"y\"\nemits = \"k\"\n" +
			"serves = [\"a\"]\nafter_loop = \"a\"\n"),
		wants: []string{`"b"`, "`serves`", `"a"`, "fix-loop"},
	},
	{
		rule: "V19", name: "max_attempts below one",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
max_attempts = 0
`,
		wants: []string{`"a"`, "`max_attempts`", ">= 1"},
	},
	{
		rule: "V20", name: "threshold routing that is neither a routing nor a step",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
threshold = { "elsewhere" = "any(f == v)" }
`,
		wants: []string{`"a"`, "`threshold`", `"elsewhere"`},
	},
	{
		rule: "V21", name: "threshold predicate that does not parse",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
threshold = { "fix-loop" = "sometimes maybe" }
[[step]]
name = "fixer"
executor = "x"
emits = "k"
loop = true
`,
		wants: []string{`"a"`, "`threshold`", "agg(field op literal)"},
	},
	{
		rule: "V22", name: "when over a field that is not kind or labels",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
when = "assignee == someone"
`,
		wants: []string{`"a"`, "`when`", "`kind`", "`labels`"},
	},
	{
		rule: "V22", name: "when mixes and with or",
		// The grammar has `or` (DKT-548) but no parentheses, so `a and b or c`
		// has two readings and V22 refuses to pick one. The message must name
		// both connectives — an author who reads only "invalid `when`" will
		// re-check the clause spellings, which are fine.
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
when = "kind == bug and labels contains security or labels contains security-load-bearing"
`,
		wants: []string{`"a"`, "`when`", "`and`", "`or`"},
	},
	{
		rule: "V23", name: "class defaults to the executor value",
		// V23 is a DEFAULT, not a refusal: the case asserts the applied value
		// rather than an error message. See TestClassDefaultsToExecutor.
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
`,
	},
	{
		rule: "V24", name: "limits lease_ttl that is not a duration",
		src: `
[pipeline]
name = "w"
version = 1
[limits]
write = { max = 1, lease_ttl = "a while" }
[[step]]
name = "a"
executor = "x"
emits = "k"
`,
		wants: []string{"[limits]", `"write"`, "`lease_ttl`", "a while"},
	},
	{
		rule: "V25", name: "payload that is not name@version",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "findings"
`,
		wants: []string{`"a"`, "`payload`", "name@version"},
	},
	{
		rule: "V25a", name: "payload names an unregistered schema",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "absent@3"
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "absent@3", "not registered", "docket schema register"},
	},
	{
		rule: "V21a", name: "threshold names a field the schema does not declare",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "risk-report@1"
threshold = { "fix-loop" = "any(rsk == high)" }
[[step]]
name = "fixer"
executor = "x"
emits = "k"
loop = true
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "rsk", "risk-report@1", "does not declare"},
	},
	{
		rule: "V21b", name: "literal is not one of the declared enum values",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "risk-report@1"
threshold = { "fix-loop" = "any(risk == severe)" }
[[step]]
name = "fixer"
executor = "x"
emits = "k"
loop = true
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "severe", "risk-report@1", `"low", "medium", "high"`},
	},
	{
		rule: "V21c", name: "an ordered operator on a field with no declared order",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "risk-report@1"
threshold = { "fix-loop" = "any(stage >= final)" }
[[step]]
name = "fixer"
executor = "x"
emits = "k"
loop = true
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "stage", "ordered_enum", "risk-report@1", ">="},
	},
	{
		rule: "V21d", name: "a threshold with no declared payload registers clean",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
threshold = { "fix-loop" = "any(anything >= whatever)" }
[[step]]
name = "fixer"
executor = "x"
emits = "k"
loop = true
`,
		schemas: registeredRisk(),
	},
	{
		rule: "V27", name: "a step name may not claim the reserved -held suffix",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "reconcile-held"
executor = "x"
emits = "k"
`,
		wants: []string{"reconcile-held", "-held", "reserved"},
	},
	{
		rule: "V28", name: "aggregate with a typo'd method",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "up"
executor = "x"
emits = "f"
[[step]]
name = "a"
after = ["up"]
action = "aggregate"
inputs = ["up.f"]
payload = "risk-report@1"
params = { field = "risk", method = "medain", output = "k" }
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "params.method", "medain", `"median"`},
	},
	{
		rule: "V28", name: "aggregate with a route_at that is not a string",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "up"
executor = "x"
emits = "f"
[[step]]
name = "a"
after = ["up"]
action = "aggregate"
inputs = ["up.f"]
payload = "risk-report@1"
params = { field = "risk", method = "median", output = "k", route_at = 3 }
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "params.route_at", "non-empty string"},
	},
	{
		rule: "V28a", name: "route_at naming a value outside the declared order",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "up"
executor = "x"
emits = "f"
[[step]]
name = "a"
after = ["up"]
action = "aggregate"
inputs = ["up.f"]
payload = "risk-report@1"
params = { field = "risk", method = "median", output = "k", route_at = "urgent" }
`,
		schemas: registeredRisk(),
		wants: []string{`"a"`, "params.route_at", "urgent", "risk-report@1",
			`"low"`, `"medium"`, `"high"`},
	},
	{
		rule: "V28a", name: "route_at naming a declared value registers clean",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "up"
executor = "x"
emits = "f"
[[step]]
name = "a"
after = ["up"]
action = "aggregate"
inputs = ["up.f"]
payload = "risk-report@1"
params = { field = "risk", method = "median", output = "k", route_at = "high" }
`,
		schemas: registeredRisk(),
	},
	{
		rule: "V29", name: "aggregate over a field with no declared order",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "up"
executor = "x"
emits = "f"
[[step]]
name = "a"
after = ["up"]
action = "aggregate"
inputs = ["up.f"]
payload = "risk-report@1"
params = { field = "stage", method = "median", output = "k" }
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "stage", "ordered_enum", "risk-report@1"},
	},
	{
		rule: "V30", name: "the declared schema cannot accept an aggregate document",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "up"
executor = "x"
emits = "f"
[[step]]
name = "a"
after = ["up"]
action = "aggregate"
inputs = ["up.f"]
payload = "closed@1"
params = { field = "risk", method = "median", output = "k" }
`,
		schemas: fakeSchemas{documents: map[string]string{"closed@1": closedRiskSchema}},
		wants:   []string{`"a"`, "closed@1", "members", "aggregate@1"},
	},
	{
		// V31: the builtin's input IS its declared `inputs` (§2), so an
		// aggregate that declares none can never compute — it would reduce an
		// empty set, produce an empty payload, and pass its threshold vacuously.
		// Everything else about this step is valid, so the refusal is genuinely
		// V31's rather than an earlier rule's.
		rule: "V31", name: "aggregate declaring no inputs",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
action = "aggregate"
payload = "risk-report@1"
params = { field = "risk", method = "median", output = "k" }
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "inputs", "aggregate"},
	},
	{
		// V32 is `packet`'s SHAPE rule
		// (docs/tdd/packet-composition.md §1.6). An entry escaping the
		// instance-config directory is refused at register, naming the step
		// and the field — the file's EXISTENCE is deliberately NOT checked
		// here, so a workflow stays registerable before its corpus lands.
		rule: "V32", name: "packet entry escaping the config directory",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
packet = ["../../secrets.md"]
`,
		wants: []string{`"a"`, "packet", ".."},
	},
	{
		// V33 keeps the substitution honest: the token needs a step that HAS a
		// per-sibling executor hint. On a human step it would substitute to
		// nothing and produce a path no author wrote.
		rule: "V33", name: "executor token on a step with no hint",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
type = "human"
on_fail = "abandon-issue"
packet = ["checklists/{executor}.md"]
`,
		wants: []string{`"a"`, "packet", "{executor}"},
	},
	{
		// V11's `issue.latest.<kind>` half (DKT-492): the kind must be one
		// some step produces — an issue can hold artifacts of no other kind,
		// so anything else resolves to nothing on every run and is a typo to
		// refuse now.
		rule: "V11", name: "issue.latest naming a kind no step produces",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "doc"
[[step]]
name = "b"
after = ["a"]
executor = "y"
emits = "findings"
inputs = ["issue.latest.dco"]
`,
		wants: []string{`"b"`, "`inputs`", `"issue.latest.dco"`, "no step in this workflow produces"},
	},
	{
		// And its shape half: the form takes ONE kind, never `*` — "the
		// latest artifact of every kind" answers no question a consumer can
		// ask.
		rule: "V11", name: "issue.latest with a wildcard kind",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "doc"
[[step]]
name = "b"
after = ["a"]
executor = "y"
emits = "findings"
inputs = ["issue.latest.*"]
`,
		wants: []string{`"b"`, "`inputs`", `"issue.latest.*"`, "issue.latest.<kind>"},
	},
	{
		// V34 (DKT-492): `issue.latest` and everything under it are reserved
		// — the engine-served latest-of-kind form is resolved before any step
		// lookup, so a step under that name could never be addressed as an
		// input.
		rule: "V34", name: "a step name may not claim the reserved issue.latest namespace",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "issue.latest"
executor = "x"
emits = "k"
`,
		wants: []string{`"issue.latest"`, "reserved"},
	},
	{
		// V11's `issue.linked.<relation>.<kind>` shape half (DKT-547): the
		// form takes a relation token and ONE kind — a wildcard answers no
		// question a cross-issue consumer can ask.
		rule: "V11", name: "issue.linked with a wildcard kind",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
inputs = ["issue.linked.depends_on.*"]
`,
		wants: []string{`"a"`, "`inputs`", `"issue.linked.depends_on.*"`,
			"issue.linked.<relation>.<kind>"},
	},
	{
		// And its vocabulary half: the relation token must be a relation type
		// or an inverse form — "specified_by" is not in the model's closed
		// vocabulary, so declaring it would bind nothing on every activation.
		rule: "V11", name: "issue.linked naming an unknown relation",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
inputs = ["issue.linked.specified_by.ux-spec"]
`,
		wants: []string{`"a"`, "`inputs`", `"specified_by"`, "not a relation type"},
	},
	{
		// And its reserved-kind half: `gate-results` and `vote-record` are
		// engine-reserved from every `emits` (V11a/V11b), so no linked issue
		// could ever hold an artifact of them.
		rule: "V11", name: "issue.linked naming an engine-reserved kind",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
inputs = ["issue.linked.depends_on.gate-results"]
`,
		wants: []string{`"a"`, "`inputs`", `"gate-results"`, "engine-reserved"},
	},
	{
		// V34 (DKT-547): `issue.linked` and everything under it are reserved
		// step names — V34's issue.latest reasoning for the cross-issue form.
		rule: "V34", name: "a step name may not claim the reserved issue.linked namespace",
		src: `
[pipeline]
name = "p"
version = 1
[[step]]
name = "issue.linked.depends_on"
executor = "x"
emits = "k"
`,
		wants: []string{`"issue.linked.depends_on"`, "reserved"},
	},
	{
		// V36 (DKT-545): a vote step's threshold routes over the CAST SET,
		// and step-name interposition is the saga's machinery — a step-name
		// key would record a routing nothing downstream consumes.
		rule: "V36", name: "vote threshold routing to a step name",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "seed"
executor = "x"
emits = "k"
[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
on_fail = "waiting-human"
threshold = { "seed" = "any(vote == approve-with-concerns)" }
`,
		wants: []string{`"gate"`, "`threshold`", `"seed"`, "fix-loop"},
	},
	{
		// V36: casts have no registered schema, so an ordered comparison
		// would be a guaranteed T3 park on every evaluation.
		rule: "V36", name: "vote threshold with an ordered operator",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "seed"
executor = "x"
emits = "k"
[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
on_fail = "waiting-human"
threshold = { "waiting-human" = "any(vote >= approve)" }
`,
		wants: []string{`"gate"`, "`threshold`", ">=", "ordered"},
	},
	{
		// V36: the field vocabulary is the cast's — anything else would
		// silently never match, V22's kind/labels reasoning over casts.
		rule: "V36", name: "vote threshold addressing a non-cast field",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "seed"
executor = "x"
emits = "k"
[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
on_fail = "waiting-human"
threshold = { "pass" = "any(severity == high)" }
`,
		wants: []string{`"gate"`, "`threshold`", `"severity"`, "cast"},
	},
	{
		// V37 (DKT-870): a floor with half a declaration positions nothing.
		rule: "V37", name: "pass_floor missing its at value",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "risk-report@1"
pass_floor = { field = "risk" }
`,
		wants: []string{`"a"`, "pass_floor", "`field`", "`at`"},
	},
	{
		// V37: without a pinned schema there is no order to position against,
		// so the floor would be permanently inert — a misdeclaration.
		rule: "V37", name: "pass_floor without a declared payload",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
pass_floor = { field = "risk", at = "high" }
`,
		wants: []string{`"a"`, "pass_floor", "payload"},
	},
	{
		// V37a (DKT-870): the exit bar is a position in the declared order,
		// V28a's discipline applied to `pass_floor.at`.
		rule: "V37a", name: "pass_floor at outside the declared order",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "risk-report@1"
pass_floor = { field = "risk", at = "urgent" }
`,
		schemas: registeredRisk(),
		wants: []string{`"a"`, "pass_floor", "urgent", "risk-report@1",
			`"low"`, `"medium"`, `"high"`},
	},
	{
		rule: "V37a", name: "pass_floor over a field with no declared order",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "risk-report@1"
pass_floor = { field = "stage", at = "final" }
`,
		schemas: registeredRisk(),
		wants:   []string{`"a"`, "pass_floor", "stage", "ordered_enum"},
	},
	{
		rule: "V37a", name: "pass_floor naming a declared value registers clean",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "risk-report@1"
pass_floor = { field = "risk", at = "high" }
`,
		schemas: registeredRisk(),
	},
	{
		// V38 (DKT-870): the stall bound fires at THIS step's own loop entry,
		// so a step that never routes `fix-loop` declares a bound that can
		// never fire — V17/V35's inert-declaration discipline.
		rule: "V38", name: "max_stalled_rounds on a step that never routes fix-loop",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
max_stalled_rounds = 3
`,
		wants: []string{`"a"`, "max_stalled_rounds", "fix-loop"},
	},
	{
		// V38's other half: a `type` step records no artifact, so there is no
		// payload whose per-round volume the bound could count.
		rule: "V38", name: "max_stalled_rounds on a type step",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
[[step]]
name = "gate"
after = ["a"]
type = "human"
on_fail = "fix-loop"
max_stalled_rounds = 2
[[step]]
name = "fix"
executor = "y"
emits = "k"
loop = true
after_loop = "a"
`,
		wants: []string{`"gate"`, "max_stalled_rounds", "artifact"},
	},
	{
		rule: "V38", name: "max_stalled_rounds on a routing step registers clean",
		src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
[[step]]
name = "check"
after = ["a"]
executor = "y"
emits = "report"
threshold = { "fix-loop" = "any(status == unmet)" }
max_stalled_rounds = 3
[[step]]
name = "fix"
executor = "z"
emits = "k"
loop = true
after_loop = "a"
`,
	},
}

// closedRiskSchema is riskSchema with `additionalProperties: false` — the exact
// shape V30 exists to catch. It declares an ordered field, so V29 passes and the
// refusal is genuinely V30's rather than an earlier rule's.
const closedRiskSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "risk": {"type": "string", "enum": ["low", "medium", "high"], "ordered_enum": true}
    },
    "additionalProperties": false
  }
}`

func TestValidationTable(t *testing.T) {
	for _, tc := range validationCases {
		t.Run(tc.rule+"_"+tc.name, func(t *testing.T) {
			def, err := Parse([]byte(tc.src))
			if err == nil {
				err = Validate(def)
			}
			// V26 runs after the pure rules, against its own environment.
			if err == nil && tc.resolver != nil {
				err = ValidateVoteRules(def, tc.resolver)
			}
			// V21a-V21d and V25a run last, in the same order the CLI applies
			// them: grammar errors reach an author before environment errors.
			if err == nil && tc.schemas != nil {
				err = ValidateSchemas(def, tc.schemas)
			}

			// V23 is a default rather than a refusal; its behavior is asserted
			// by TestClassDefaultsToExecutor and its row here exists so the
			// documented table and the test table stay in set equality.
			if len(tc.wants) == 0 {
				testsupport.Must(t, err, "%s: unexpected error: %v", tc.rule, err)
				return
			}

			if err == nil {
				t.Fatalf("%s: expected a validation error, got none", tc.rule)
			}

			we, ok := err.(*Error)
			if !ok {
				t.Fatalf("%s: error is %T, want *workflow.Error: %v", tc.rule, err, err)
			}
			if we.Rule != tc.rule {
				t.Errorf("%s: error reports rule %q", tc.rule, we.Rule)
			}
			for _, want := range tc.wants {
				if !strings.Contains(we.Error(), want) {
					t.Errorf("%s: error %q does not mention %q", tc.rule, we.Error(), want)
				}
			}
		})
	}
}

// TestValidationTableIsComplete asserts SET EQUALITY between the documented
// rule IDs and the test cases' rule IDs — never a count. A count is exactly
// the assertion that breaks when a rule is split, which is how V13a came to
// exist, and a table that drifts from its tests documents behavior the code
// does not have.
func TestValidationTableIsComplete(t *testing.T) {
	documented := make(map[string]bool, len(RuleIDs))
	for _, id := range RuleIDs {
		documented[id] = true
	}

	tested := make(map[string]bool, len(validationCases))
	for _, tc := range validationCases {
		tested[tc.rule] = true
	}

	var missing, extra []string
	for id := range documented {
		if !tested[id] {
			missing = append(missing, id)
		}
	}
	for id := range tested {
		if !documented[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("documented rules with no test case: %s", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("test cases naming undocumented rules: %s", strings.Join(extra, ", "))
	}
}

// TestV13AndV13aAreDistinct pins the §4.3.2 argument: the two rules produce two
// different author-facing messages, so a human step that omitted `on_fail` is
// not told "your reject routing is waiting-human" about a field it never wrote.
func TestV13AndV13aAreDistinct(t *testing.T) {
	var v13, v13a string
	for _, tc := range validationCases {
		def, err := Parse([]byte(tc.src))
		if err != nil {
			continue
		}
		err = Validate(def)
		if err == nil {
			continue
		}
		switch tc.rule {
		case "V13":
			v13 = err.Error()
		case "V13a":
			v13a = err.Error()
		}
	}
	if v13 == "" || v13a == "" {
		t.Fatalf("missing a case: V13=%q V13a=%q", v13, v13a)
	}
	if v13 == v13a {
		t.Error("V13 and V13a produce the same message; they are two distinct author-facing errors")
	}
	if strings.Contains(v13a, "may not route rejects") {
		t.Error("V13a should not report V13's message for a field the author never wrote")
	}
}

// TestDefaults covers the §11.1 default column, asserted per field (§4.6).
func TestDefaults(t *testing.T) {
	src := `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
[[step]]
name = "f"
after = ["a"]
fanout = ["p", "q", "r"]
emits = "k"
`
	def, err := Load([]byte(src))
	testsupport.Must(t, err, "Load: %v", err)

	a, f := def.Steps[0], def.Steps[1]

	// V23: class defaults to the executor value.
	if a.Class != "x" {
		t.Errorf("class = %q, want the executor value %q", a.Class, "x")
	}
	// min_siblings defaults to len(fanout).
	if f.MinSiblings == nil || *f.MinSiblings != 3 {
		t.Errorf("min_siblings = %v, want 3 (len(fanout))", f.MinSiblings)
	}
	// on_fail defaults to waiting-human.
	if a.OnFail != OnFailWaitingHuman {
		t.Errorf("on_fail = %q, want %q", a.OnFail, OnFailWaitingHuman)
	}
	// loop defaults to false.
	if a.Loop {
		t.Error("loop = true, want false by default")
	}
	// expected_cost defaults to 0.
	if a.ExpectedCost == nil || *a.ExpectedCost != 0 {
		t.Errorf("expected_cost = %v, want 0", a.ExpectedCost)
	}
}

// TestClassDefaultsToExecutor is V23's own assertion, separate from TestDefaults
// so the rule has a test that names it.
func TestClassDefaultsToExecutor(t *testing.T) {
	def, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "some-hint"
emits = "k"
[[step]]
name = "b"
after = ["a"]
executor = "other-hint"
class = "explicit"
emits = "k"
`))
	testsupport.Must(t, err, "Load: %v", err)
	if got := def.Steps[0].Class; got != "some-hint" {
		t.Errorf("unset class = %q, want the executor value", got)
	}
	if got := def.Steps[1].Class; got != "explicit" {
		t.Errorf("declared class = %q, want it left alone", got)
	}
}

// TestStrictDecoding covers §4.2: an unknown key at each level is an error, and
// a step-level key names its step.
func TestStrictDecoding(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		wants []string
	}{
		{
			name: "pipeline level",
			src: `
[pipeline]
name = "w"
version = 1
maintainer = "someone"
[[step]]
name = "a"
executor = "x"
emits = "k"
`,
			wants: []string{"unknown key", "maintainer"},
		},
		{
			name: "match level",
			src: `
[pipeline]
name = "w"
version = 1
[match]
kind = ["task"]
labels_none = ["x"]
[[step]]
name = "a"
executor = "x"
emits = "k"
`,
			wants: []string{"unknown key", "labels_none"},
		},
		{
			name: "limits level",
			src: `
[pipeline]
name = "w"
version = 1
[limits]
write = { max = 1, ttl = "45m" }
[[step]]
name = "a"
executor = "x"
emits = "k"
`,
			wants: []string{"unknown key", "ttl"},
		},
		{
			name: "step level names the step",
			src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
max_attempt = 2
`,
			// The whole point of strict decoding: `max_attempt` silently
			// defaulting to the config value is invisible until a run
			// misroutes.
			wants: []string{"unknown key", "max_attempt", `"a"`},
		},
		{
			name: "gate table level",
			src: `
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
gates = [{ name = "g", prefix = true }]
`,
			wants: []string{"unknown key", "prefix", `"a"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatal("expected a strict-decoding error, got none")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

// TestGateSpellingsNormalize covers §4.2: both §11.1 gate spellings land in one
// shape, so downstream code never branches on which the author wrote.
func TestGateSpellingsNormalize(t *testing.T) {
	def, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
gates = ["plain", { name = "fenced", source = "fence:ac", pre = true }]
`))
	testsupport.Must(t, err, "Load: %v", err)
	gates := def.Steps[0].Gates
	if len(gates) != 2 {
		t.Fatalf("got %d gates, want 2", len(gates))
	}
	if gates[0] != (Gate{Name: "plain"}) {
		t.Errorf("bare string normalized to %+v, want {plain, no source, not pre}", gates[0])
	}
	if gates[1] != (Gate{Name: "fenced", Source: "fence:ac", Pre: true}) {
		t.Errorf("inline table normalized to %+v", gates[1])
	}
}

// TestLimitsShorthand covers §11.1's bare-int form.
func TestLimitsShorthand(t *testing.T) {
	def, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[limits]
write = 1
read = { max = 4, lease_ttl = "45m", max_step_duration = "2h" }
[[step]]
name = "a"
executor = "x"
emits = "k"
`))
	testsupport.Must(t, err, "Load: %v", err)
	if got := def.Limits["write"]; got != (Limit{Max: 1}) {
		t.Errorf(`bare int decoded to %+v, want {Max: 1}`, got)
	}
	want := Limit{Max: 4, LeaseTTL: "45m", MaxStepDuration: "2h"}
	if got := def.Limits["read"]; got != want {
		t.Errorf("table decoded to %+v, want %+v", got, want)
	}
}

// TestProducedKindPerStepClass asserts every row of §4.3.1. Getting this wrong
// rejects the canonical fixture, which is why the table is pinned rather than
// inferred.
func TestProducedKindPerStepClass(t *testing.T) {
	def, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "e"
executor = "x"
emits = "change-summary"
[[step]]
name = "f"
after = ["e"]
fanout = ["p", "q"]
emits = "findings"
[[step]]
name = "act"
after = ["f"]
action = "aggregate"
inputs = ["f.findings"]
params = { field = "severity", method = "median", output = "findings" }
[[step]]
name = "gate"
after = ["act"]
type = "human"
on_fail = "skip"
`))
	testsupport.Must(t, err, "Load: %v", err)

	cases := []struct {
		step     string
		kind     string
		produces bool
	}{
		{"e", "change-summary", true}, // executor -> emits
		{"f", "findings", true},       // fanout   -> emits, per sibling
		{"act", "findings", true},     // action   -> params.output
		{"gate", "", false},           // human    -> produces nothing
	}

	byName := map[string]*Step{}
	for _, s := range def.Steps {
		byName[s.Name] = s
	}
	for _, tc := range cases {
		kind, produces := producedKind(byName[tc.step])
		if kind != tc.kind || produces != tc.produces {
			t.Errorf("step %q produces (%q, %v), want (%q, %v)",
				tc.step, kind, produces, tc.kind, tc.produces)
		}
	}
}

// TestInputsFromAHumanStepIsRejected is §4.3.1's corollary: a step that
// produces nothing has nothing for an `inputs` reference to resolve to.
func TestInputsFromAHumanStepIsRejected(t *testing.T) {
	_, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "gate"
type = "human"
on_fail = "skip"
[[step]]
name = "b"
after = ["gate"]
executor = "y"
emits = "k"
inputs = ["gate.decision"]
`))
	if err == nil {
		t.Fatal("expected inputs from a human step to be rejected")
	}
	for _, want := range []string{`"gate"`, "produces no artifact"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestActionStepKindComesFromParamsOutput is the regression the fixture found:
// a V11 reading only `emits` rejects the canonical register-test fixture,
// because `reconcile` is an action step naming its kind in `params.output`.
func TestActionStepKindComesFromParamsOutput(t *testing.T) {
	_, err := Load([]byte(`
[pipeline]
name = "w"
version = 1
[[step]]
name = "up"
executor = "x"
emits = "findings"
[[step]]
name = "act"
after = ["up"]
action = "aggregate"
inputs = ["up.findings"]
params = { field = "severity", method = "median", output = "findings" }
[[step]]
name = "b"
after = ["act"]
executor = "y"
emits = "k"
inputs = ["act.findings"]
`))
	testsupport.Must(t, err, "an action step's params.output must satisfy inputs: %v", err)
}

// TestFixtureCrossValidatesAgainstTheQASchemas is §8.2's replacement for the
// no-schemas variant, and it is the whole point of the fixture edit.
//
// The committed fixture now declares `payload = "findings@1"` on `reconcile`,
// because V29 requires it: an `aggregate` step reduces by median over an order,
// and there is no order without a declared schema. So the cross-validation
// rules are exercised against the QA fixture's OWN schema file — the same bytes
// ZG registers — rather than against a resolver with nothing in it.
//
// `TestFixtureRegistersClean` (pure, no resolver) is unaffected and still runs:
// Validate stays a function of bytes, which is exactly what §4.9.2's seam buys.
func TestFixtureCrossValidatesAgainstTheQASchemas(t *testing.T) {
	def := loadFixture(t)

	body, err := os.ReadFile(filepath.Join(
		"..", "..", "scripts", "qa", "fixtures", "schemas", "findings@1.json"))
	if err != nil {
		t.Skipf("QA schema fixture unavailable: %v", err)
	}
	registry := fakeSchemas{documents: map[string]string{"findings@1": string(body)}}

	if err := ValidateSchemas(def, registry); err != nil {
		t.Fatalf("the committed fixture no longer cross-validates: %v", err)
	}

	// The premise is not vacuous: the fixture really does declare thresholds,
	// including an ORDERED one, and an `aggregate` step over a declared order.
	var thresholds, ordered, aggregates int
	for _, step := range def.Steps {
		if step.Action == ActionAggregate {
			aggregates++
		}
		for _, predicate := range step.Threshold {
			thresholds++
			p, perr := ParsePredicate(predicate)
			if perr == nil && p.Ordered() {
				ordered++
			}
		}
	}
	if thresholds == 0 || ordered == 0 || aggregates == 0 {
		t.Fatalf("the fixture declares %d thresholds (%d ordered) and %d aggregate "+
			"steps; the assertion above proves nothing",
			thresholds, ordered, aggregates)
	}
}

// TestFixtureKeepsV21dsSchemalessThreshold is V21d on the real thing, and it is
// the half the fixture edit must NOT have taken away.
//
// `verify` declares `any(status == unmet)` and NO `payload`, and it still
// registers clean. Making `payload` mandatory wherever a `threshold` appears
// would be a tidier rule and would break exactly this step: equality has never
// needed an order and still does not.
func TestFixtureKeepsV21dsSchemalessThreshold(t *testing.T) {
	def := loadFixture(t)

	verify := StepByName(def, "verify")
	if verify == nil {
		t.Fatal("the fixture no longer declares a `verify` step")
	}
	if verify.Payload != "" {
		t.Fatalf("`verify` gained `payload = %q`; V21d's case is gone from the "+
			"fixture and this assertion proves nothing", verify.Payload)
	}
	if len(verify.Threshold) == 0 {
		t.Fatal("`verify` declares no threshold")
	}

	// One step declaring a payload and one declaring none, validated together
	// against a registry holding only the first's schema. If V21d had been
	// dropped, `verify` would need one too.
	body, err := os.ReadFile(filepath.Join(
		"..", "..", "scripts", "qa", "fixtures", "schemas", "findings@1.json"))
	if err != nil {
		t.Skipf("QA schema fixture unavailable: %v", err)
	}
	registry := fakeSchemas{documents: map[string]string{"findings@1": string(body)}}
	if err := ValidateSchemas(def, registry); err != nil {
		t.Fatalf("a schema-less threshold now requires a schema: %v", err)
	}
}

// loadFixture reads and parses the committed register-test fixture.
func loadFixture(t *testing.T) *Definition {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "example-workflow.toml"))
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	def, err := Load(src)
	testsupport.Must(t, err, "loading the fixture: %v", err)
	return def
}

// TestReservedActionsAreAllImplemented is the invariant that makes V27's action
// clause honest: every name core RESERVES is a name core COMPUTES.
//
// While the two lists agree, V27's second clause is unreachable through the
// CLI — and that is a tested fact rather than an assumption. The moment a
// builtin name is reserved ahead of its implementation, this fails and V27's
// refusal becomes the thing that catches it.
func TestReservedActionsAreAllImplemented(t *testing.T) {
	for _, name := range ReservedActions {
		if !slices.Contains(BuiltinActions, name) {
			t.Errorf("action %q is reserved but not implemented; V27 will refuse "+
				"every step naming it", name)
		}
	}
	for _, name := range BuiltinActions {
		if !slices.Contains(ReservedActions, name) {
			t.Errorf("action %q is a builtin but is not reserved; a trusted "+
				"command could be looked up under a name core computes", name)
		}
	}
}

// TestLinkedInputRelaxesTheProducedKindTable is DKT-547's lint half, both
// directions at once. V11 holds every same-run form to the produced-kind table
// — `issue.latest.<kind>` requires SOME step of the workflow to produce the
// kind — and the cross-issue form is exactly the case where that requirement
// must NOT apply: the producer is another issue's workflow, so no step here
// produces `ux-spec` and the declaration is still registrable. L4's
// predecessor rule likewise does not apply — the artifact was recorded before
// this run existed — so the form is legal on a ROOT step, which is where a
// design-qa step consuming a spec actually sits.
func TestLinkedInputRelaxesTheProducedKindTable(t *testing.T) {
	const src = `
[pipeline]
name = "ui-change-mini"
version = 1
[[step]]
name = "design-qa"
after = []
executor = "qa"
emits = "qa-report"
inputs = ["issue.body", "issue.linked.depends_on.ux-spec"]
[[step]]
name = "judge-design"
after = ["design-qa"]
executor = "judge"
emits = "verdict"
inputs = ["design-qa.qa-report", "issue.linked.depends-on.ux-spec"]
`
	def, err := Load([]byte(src))
	testsupport.Must(t, err, "loading: %v", err)
	if err := Validate(def); err != nil {
		t.Fatalf("Validate rejects the issue.linked form: %v", err)
	}
	if err := Lint(def); err != nil {
		t.Fatalf("Lint rejects the issue.linked form: %v", err)
	}
}

// TestLinkedInputParser pins LinkedInput's grammar: one parser for the form,
// shared by V11, L4, and the engine's activation-time resolver.
func TestLinkedInputParser(t *testing.T) {
	cases := []struct {
		input          string
		relation, kind string
		ok             bool
	}{
		{"issue.linked.depends_on.ux-spec", "depends_on", "ux-spec", true},
		{"issue.linked.depends-on.ux-spec", "depends-on", "ux-spec", true},
		{"issue.linked.blocked_by.doc", "blocked_by", "doc", true},
		{"issue.linked.depends_on.*", "", "", false},
		{"issue.linked.depends_on", "", "", false},
		{"issue.linked.depends_on.", "", "", false},
		{"issue.linked..doc", "", "", false},
		{"issue.linked.depends_on.a.b", "", "", false},
		{"issue.latest.doc", "", "", false},
		{"other.doc", "", "", false},
	}
	for _, tc := range cases {
		relation, kind, ok := LinkedInput(tc.input)
		if relation != tc.relation || kind != tc.kind || ok != tc.ok {
			t.Errorf("LinkedInput(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.input, relation, kind, ok, tc.relation, tc.kind, tc.ok)
		}
	}
}

// TestReRegisteringAnS3FileIsRefused is §4.9.3's honest consequence, stated
// rather than hidden.
//
// An S3-era definition whose file is re-registered VERBATIM after this stage may
// now be refused, because validation runs before the content-hash idempotency
// path in db.InsertWorkflow. The already-registered row is untouched and every
// run pinning it is untouched; only the act of registering that file AGAIN
// requires bringing it up to standard.
//
// It is a refusal and not a corruption, and this is the test that says so.
func TestReRegisteringAnS3FileIsRefused(t *testing.T) {
	// An S3-era file: `payload` passed V25's SHAPE check at S3, when nothing
	// could resolve the name. It cannot pass V25a.
	const s3Era = `
[pipeline]
name = "s3-era"
version = 1
[[step]]
name = "a"
executor = "x"
emits = "k"
payload = "findings@1"
`
	def, err := Load([]byte(s3Era))
	testsupport.Must(t, err, "loading: %v", err)

	// The PURE rules still accept it, which is the point: the bytes did not
	// become ungrammatical. Only the environment question is new.
	if err := Validate(def); err != nil {
		t.Fatalf("the pure rules now reject an S3-era file: %v", err)
	}

	err = ValidateSchemas(def, fakeSchemas{documents: map[string]string{}})
	if err == nil {
		t.Fatal("expected V25a to refuse a re-registration naming an unregistered schema")
	}
	we, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *workflow.Error: %v", err, err)
	}
	if we.Rule != "V25a" {
		t.Errorf("rule = %q, want V25a", we.Rule)
	}
	if !strings.Contains(we.Error(), "docket schema register") {
		t.Errorf("the refusal does not name the remedy: %v", we)
	}
}

// TestV26RemedyPointsAtTheSharedFixWhenOtherProjectsHaveTheRule is DKT-264 (c).
//
// A corpus workflow is shared across every project and its thresholds are not,
// so a freshly-registered project refuses a definition that works everywhere
// else in the store. "vote_rule X is not registered" is true and useless there:
// it sends the operator to a per-project `config set` that leaves the NEXT
// project in exactly the same place — which is how thirteen projects came to
// hold thirteen copies of the same three thresholds while the store-wide
// fallback that already existed sat empty.
func TestV26RemedyPointsAtTheSharedFixWhenOtherProjectsHaveTheRule(t *testing.T) {
	def, err := Parse([]byte(twoStepPipeline(
		"type = \"vote\"\nvote_rule = \"security-acceptance\"\n" +
			"voters = [\"a\",\"b\",\"c\"]\nemits = \"v\"\nafter = [\"a\"]\n")))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	resolver := fakeVoteRules{
		elsewhere: map[string]struct {
			projects int
			value    string
		}{
			"security-acceptance": {projects: 13, value: "0.67"},
		},
	}
	err = ValidateVoteRules(def, resolver)
	if err == nil {
		t.Fatal("expected V26 to refuse an unregistered rule")
	}

	msg := err.Error()
	for _, needle := range []string{
		"13 other project",    // this is not a new rule
		"--global",            // the fix that covers every project at once
		"0.67",                // a real value, not the <0-1> placeholder
		"security-acceptance", // which rule
	} {
		if !strings.Contains(msg, needle) {
			t.Errorf("the refusal does not mention %q:\n%s", needle, msg)
		}
	}
}

// TestV26KeepsTheOriginalRemedyForAGenuinelyNewRule is the containment half.
//
// A rule nobody has anywhere IS new, and telling that operator about `--global`
// and quoting a threshold from nowhere would be worse than the original
// message. The remedy has to change only when the case does.
func TestV26KeepsTheOriginalRemedyForAGenuinelyNewRule(t *testing.T) {
	def, err := Parse([]byte(twoStepPipeline(
		"type = \"vote\"\nvote_rule = \"brand-new\"\n" +
			"voters = [\"a\",\"b\",\"c\"]\nemits = \"v\"\nafter = [\"a\"]\n")))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	err = ValidateVoteRules(def, fakeVoteRules{})
	if err == nil {
		t.Fatal("expected V26 to refuse an unregistered rule")
	}
	msg := err.Error()
	if strings.Contains(msg, "--global") || strings.Contains(msg, "other project") {
		t.Errorf("a genuinely new rule was given the shared-corpus remedy:\n%s", msg)
	}
	if !strings.Contains(msg, "vote.rule.brand-new.threshold") {
		t.Errorf("the refusal does not name the key to set:\n%s", msg)
	}
}

// TestVoteThresholdAndVoteRecordRegister (DKT-545) is the positive half of
// V36/V11's vote clauses: a vote step routing its concerned approvals into the
// fix loop, with a downstream consumer reading the panel's record, is exactly
// the declaration this grammar exists to admit.
func TestVoteThresholdAndVoteRecordRegister(t *testing.T) {
	src := `
[pipeline]
name = "concern-loop"
version = 1
[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"
[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["a", "b", "c"]
vote_rule = "majority"
on_fail = "waiting-human"
threshold = { "fix-loop" = "count>=2(vote == approve-with-concerns)" }
[[step]]
name = "fix"
executor = "x"
emits = "findings"
loop = true
after_loop = "gate"
inputs = ["gate.vote-record"]
[[step]]
name = "report"
after = ["gate"]
executor = "y"
emits = "record"
inputs = ["gate.vote-record"]
`
	if _, err := Load([]byte(src)); err != nil {
		t.Fatalf("a concern-routing vote workflow failed to register: %v", err)
	}
}

// TestV22ConnectiveGrammar pins the shape V22 admits after DKT-548: clauses
// joined by `and` throughout or by `or` throughout, and nothing else.
//
// It goes through Load — the real register path — rather than calling
// validateWhen, because the point of the rule is what an operator's TOML is
// allowed to say. The rejection rows assert the RULE ID too: a mixed predicate
// refused as V22 tells the author which grammar bit them; refused as a parse
// error it would not.
func TestV22ConnectiveGrammar(t *testing.T) {
	cases := []struct {
		name    string
		when    string
		wantErr bool
	}{
		{name: "single clause", when: "kind == bug"},
		{name: "and throughout", when: "kind == bug and labels contains backend"},
		{
			name: "three ands",
			when: "kind == bug and labels contains backend and labels != docs-only",
		},
		// The motivating case: DKT-548's two byte-identical author lanes existed
		// only because this string could not be written.
		{
			name: "or throughout",
			when: "labels contains security-load-bearing or labels contains security",
		},
		{
			name: "three ors over both fields",
			when: "kind == bug or kind == chore or labels contains security",
		},
		{name: "quoted values across a disjunction", when: `kind == "bug" or labels == "urgent"`},
		{name: "extra whitespace around the connective", when: "kind == bug  or   labels contains x"},

		// `contains-any` (DKT-550): a set-membership CLAUSE, so it registers
		// alone, beside an ordinary `and` clause, and beside an `or` one.
		{name: "contains-any alone", when: "labels contains-any (security-change, security)"},
		{name: "contains-any single element", when: "labels contains-any (security)"},
		{name: "contains-any three elements", when: "labels contains-any (a, b, c)"},
		{name: "contains-any without inner spaces", when: "labels contains-any(a,b)"},
		{name: "contains-any with generous whitespace", when: "labels contains-any   (  a ,  b  )"},
		{name: "contains-any quoted elements", when: `labels contains-any ("a", "b")`},
		// DKT-550's motivating predicate, as ONE homogeneous-`and` clause list.
		{
			name: "contains-any beside an and clause",
			when: "kind == doc:tdd and labels contains-any (security-change, security)",
		},
		{
			name: "contains-any beside an or clause",
			when: "labels contains-any (a, b) or kind == bug",
		},

		// `contains_any [...]` (DKT-1000): the same clause under the underscore
		// spelling and the bracketed list. Both are accepted alongside the
		// registered `contains-any (…)` form, and the spelling and the
		// delimiter are independent — all four combinations register.
		{name: "contains_any bracketed", when: "labels contains_any [security-change, security]"},
		{name: "contains_any single element", when: "labels contains_any [security]"},
		{name: "contains_any three elements", when: "labels contains_any [a, b, c]"},
		{name: "contains_any without inner spaces", when: "labels contains_any[a,b]"},
		{name: "contains_any with generous whitespace", when: "labels contains_any   [  a ,  b  ]"},
		{name: "contains_any quoted elements", when: `labels contains_any ["a", "b"]`},
		{name: "contains_any with parens", when: "labels contains_any (a, b)"},
		{name: "contains-any with brackets", when: "labels contains-any [a, b]"},
		// DKT-1000's motivating predicate, verbatim.
		{
			name: "contains_any beside an and clause",
			when: "labels contains doc:tdd and labels contains_any " +
				"[security-change, security-load-bearing, security]",
		},
		{
			name: "contains_any beside an or clause",
			when: "labels contains_any [a, b] or kind == bug",
		},

		// The delimiters must PAIR. RE2 has no backreference, so the two
		// spellings are two branches — a mismatched pair that registered would
		// mean the closing delimiter is decoration.
		{name: "contains_any bracket opened, paren closed", when: "labels contains_any [a, b)", wantErr: true},
		{name: "contains_any paren opened, bracket closed", when: "labels contains_any (a, b]", wantErr: true},
		// The bracketed form inherits every list-shape refusal of the paren one.
		{name: "contains_any empty list", when: "labels contains_any []", wantErr: true},
		{name: "contains_any unclosed list", when: "labels contains_any [a, b", wantErr: true},
		{name: "contains_any unopened list", when: "labels contains_any a, b]", wantErr: true},
		{name: "contains_any bare value, no brackets", when: "labels contains_any a", wantErr: true},
		{name: "contains_any trailing comma", when: "labels contains_any [a, b,]", wantErr: true},
		{name: "contains_any leading comma", when: "labels contains_any [, a]", wantErr: true},
		{name: "contains_any doubled comma", when: "labels contains_any [a,, b]", wantErr: true},
		{name: "contains_any nested brackets", when: "labels contains_any [a, [b]]", wantErr: true},
		{name: "contains_any element with whitespace", when: "labels contains_any [a b]", wantErr: true},
		// `contains_any` is labels-only, exactly as `contains-any` is: a scalar
		// has no set form, and the field vocabulary is still kind/labels only.
		{name: "contains_any over kind", when: "kind contains_any [bug, task]", wantErr: true},
		{name: "contains_any over a field outside the grammar",
			when: "assignee contains_any [a, b]", wantErr: true},
		// A near-miss spelling is not the operator. It must be refused rather
		// than falling through to `labels contains "_anyx"`-style readings.
		{name: "contains_anyx is not the operator", when: "labels contains_anyx [a, b]", wantErr: true},
		{name: "containsany is not the operator", when: "labels containsany [a, b]", wantErr: true},

		// Malformed list syntax. Each is refused as V22 rather than admitted
		// and read as something else — an unclosed or empty list that
		// registered would evaluate false forever and silently skip the lane.
		{name: "contains-any empty list", when: "labels contains-any ()", wantErr: true},
		{name: "contains-any unclosed list", when: "labels contains-any (a, b", wantErr: true},
		{name: "contains-any unopened list", when: "labels contains-any a, b)", wantErr: true},
		{name: "contains-any bare value, no parens", when: "labels contains-any a", wantErr: true},
		{name: "contains-any trailing comma", when: "labels contains-any (a, b,)", wantErr: true},
		{name: "contains-any leading comma", when: "labels contains-any (, a)", wantErr: true},
		{name: "contains-any doubled comma", when: "labels contains-any (a,, b)", wantErr: true},
		{name: "contains-any nested parens", when: "labels contains-any (a, (b))", wantErr: true},
		// `kind` is one value, so it has no set form — membership over it is
		// `==`, and admitting `kind contains-any` would invite a list read
		// against a scalar.
		{name: "contains-any over kind", when: "kind contains-any (bug, task)", wantErr: true},
		{name: "contains-any over a field outside the grammar",
			when: "assignee contains-any (a, b)", wantErr: true},

		{name: "and then or", when: "kind == bug and labels contains a or labels contains b", wantErr: true},
		{name: "or then and", when: "labels contains a or labels contains b and kind == bug", wantErr: true},
		{name: "or over a field outside the grammar", when: "kind == bug or assignee == someone", wantErr: true},
		{name: "dangling connective", when: "kind == bug or", wantErr: true},
		{name: "doubled connective", when: "kind == bug or or labels contains a", wantErr: true},
		// A literal that merely CONTAINS a connective is not a split point: the
		// connective is a whitespace-delimited token, so this stays one clause
		// and validates.
		{name: "label whose name embeds a connective", when: "labels contains and-then"},
		{name: "value that is a bare connective is a clause, not a separator", when: "labels contains or"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(
				"\n[pipeline]\nname = \"w\"\nversion = 1\n"+
					"[[step]]\nname = \"a\"\nexecutor = \"x\"\nemits = \"k\"\nwhen = %q\n",
				tc.when)

			_, err := Load([]byte(src))
			if !tc.wantErr {
				testsupport.Must(t, err, "when = %q should register: %v", tc.when, err)
				return
			}

			if err == nil {
				t.Fatalf("when = %q registered; V22 must refuse it", tc.when)
			}
			we, ok := err.(*Error)
			if !ok {
				t.Fatalf("when = %q: error is %T, want *workflow.Error: %v", tc.when, err, err)
			}
			if we.Rule != "V22" {
				t.Errorf("when = %q refused as %s, want V22: %v", tc.when, we.Rule, err)
			}
		})
	}
}

// TestV22AndWhenHoldsShareOneGrammar is the single-regex discipline as an
// assertion: every predicate V22 admits must be evaluable, and the evaluator
// must not accept a spelling the validator refuses.
//
// The direction that matters is the second one. A predicate that evaluates but
// cannot register is merely unreachable; one that registers but evaluates as
// something else routes work through a lane nobody wrote — which is exactly
// what a second spelling of the connective grammar would eventually produce.
func TestV22AndWhenHoldsShareOneGrammar(t *testing.T) {
	subject := Subject{Kind: "bug", Labels: []string{"security"}}

	for _, when := range []string{
		"kind == bug",
		"kind == bug and labels contains security",
		"labels contains security-load-bearing or labels contains security",
		"kind == task or labels contains security",
		"labels contains and-then",
		"labels contains-any (security-change, security)",
		"kind == doc:tdd and labels contains-any (security-change, security)",
		"labels contains-any (a, b) or kind == bug",
		"labels contains_any [security-change, security-load-bearing, security]",
		"labels contains doc:tdd and labels contains_any [security-change, security]",
		"labels contains_any [a, b] or kind == bug",
		`labels contains_any ["a", "b"]`,
	} {
		src := fmt.Sprintf(
			"\n[pipeline]\nname = \"w\"\nversion = 1\n"+
				"[[step]]\nname = \"a\"\nexecutor = \"x\"\nemits = \"k\"\nwhen = %q\n", when)
		if _, err := Load([]byte(src)); err != nil {
			t.Errorf("V22 refused %q, which WhenHolds evaluates: %v", when, err)
			continue
		}
		// Evaluability, not the value: every clause parses, so no clause falls
		// through whenClauseHolds's unparseable branch.
		clauses, _, mixed := splitWhen(when)
		if mixed {
			t.Errorf("%q registered but splitWhen calls it mixed", when)
		}
		for _, clause := range clauses {
			if !whenShape.MatchString(clause) {
				t.Errorf("%q registered but clause %q does not match whenShape", when, clause)
			}
		}
		_ = WhenHolds(when, subject)
	}

	// The other direction: a mix evaluates FALSE and never registers, so the two
	// halves agree that it is not a predicate.
	const mixed = "kind == bug and labels contains security or labels contains x"
	if WhenHolds(mixed, subject) {
		t.Errorf("WhenHolds(%q) = true; a mixed predicate V22 refuses must not hold", mixed)
	}
}
