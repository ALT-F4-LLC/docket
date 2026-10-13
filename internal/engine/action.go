package engine

import "context"

// The action seam, with the real runner behind it.
//
// The rule the seam encodes:
//
//	One is builtin and generic: `action = "aggregate"` … Other computations
//	remain user-trusted commands receiving step context on stdin.
//
// So there are exactly two paths and the resolution order between them is a
// security property: BUILTIN FIRST. An action named `aggregate` is computed by
// core and never consults the trust store, which is why register time reserves
// the name rather than letting an operator wonder why their trusted `aggregate`
// command never ran. Everything else goes through internal/trust and
// internal/exec — CALLED, never re-implemented.

// ActionSpec is one action step to compute, normalized from the workflow
// definition.
type ActionSpec struct {
	// Name is the `action` value. Core carries it opaquely: it is either a
	// builtin's name or a trust entry's name, and nothing else about it is read.
	Name string
	// Params is the opaque KV bag, verbatim. CORE NEVER READS A KEY INSIDE IT
	// for a non-builtin action — a trusted command's params are its author's
	// business. The builtin reads exactly six keys (field, method, hold_spread,
	// output, route_at, source_field), and register time refuses any other.
	Params map[string]any
	// Output is `params.output`: the artifact kind this step produces.
	// It is lifted out of Params so the saga never reaches into the bag.
	Output string

	// Inputs is the payload set the computation reduces: THE CONCATENATED
	// PAYLOADS OF THE STEP'S DECLARED `inputs` ARTIFACTS, resolved and already
	// shape-validated.
	//
	// It is NOT the step's own recorded payload. An action step that has never
	// run has none, so reading one would make every aggregate reduce over nil in
	// production and leave the flow producible only by a dispatcher that claimed
	// the step and wrote its input by hand — reconcile.py reborn as a
	// claim+complete shim, which `claim` now refuses outright for an action
	// step.
	//
	// The ORDER source is untouched by this: the register-time check rejects the
	// predecessor's SCHEMA as the ORDER source, which stays the step's own
	// declared `payload`. The ORDER and the
	// DATA are two questions, and `inputs` is the declaration that answers the
	// second — ordinal-aware, `done`-only, and in the author's declared order.
	Inputs []map[string]any
	// Order is the step's PINNED payload schema's ordered index, or nil when the
	// step declares none. The builtin refuses to reduce without it (the runtime
	// half of the register-time ordered-field check); a trusted command never
	// sees it.
	Order OrderResolver
	// Validate validates a produced payload against the step's declared schema,
	// or is nil when the step declares none. It is a function rather than a
	// compiled document so the seam does not drag the schema package into every
	// test that builds a spec.
	Validate func(payload []byte) error
	// Context is the context object as `docket step context --json` emits it —
	// the bytes a trusted command receives on stdin. It is
	// assembled by the saga BEFORE the runner is invoked, because assembling it
	// needs a transaction and a subprocess may never run inside one.
	Context []byte
}

// ActionResultRow is one recorded attempt, in `action_results`' shape plus the
// trust-audit fields the saga writes alongside it.
//
// It mirrors GateResultRow exactly, and the mirror is the point: `run report`
// reads one pattern twice rather than two patterns once.
type ActionResultRow struct {
	Action     string
	Ordinal    int
	Argv       []string
	Exit       *int
	DurationMS int64
	Output     string
	Truncated  bool
	Verdict    string
	Reason     string
	// Builtin marks a result core computed itself — the field that tells an
	// `aggregate` apart from a trusted command that happened to succeed.
	Builtin bool
	// TrustEntry names the entry that authorized this, for the audit record.
	TrustEntry string
	// ArgvSHA256 is the canonical hash of the candidate argv, for trust_cache.
	ArgvSHA256 string
	// Prefix records that a prefix entry authorized it.
	Prefix bool
}

// ActionResult is one action's outcome.
//
// The seam returns Held and Results as well as a payload, and neither was
// avoidable. The aggregate builtin requires the held-cluster outcome, and the
// audit record requires per-attempt records; a seam that could only return a
// payload could express neither.
type ActionResult struct {
	// Kind is the produced artifact's kind — `params.output`.
	Kind string
	// Body is the artifact body.
	Body string
	// Payload is the structured half, as JSON text — the PLAIN ARRAY.
	// The old `{"stub":true,…}` wrapper is history and is never written again;
	// the marker now rides in `artifacts.stub`, which a result this stage
	// produces leaves at 0.
	Payload string

	// Held indexes the output payload's elements whose spread tripped
	// `hold_spread`. A non-empty list defers the step's routing into the saga's
	// `held` stage.
	Held []int

	// Results are the per-attempt records to write into `action_results`.
	// A builtin produces exactly one, with NULL argv and exit; a trusted
	// command produces one per flaky attempt.
	Results []ActionResultRow

	// Failed reports that the computation did not succeed. The step routes per
	// its effective `on_fail` and NO ARTIFACT IS WRITTEN — a computation that
	// could not run has not produced a result to record. This is a STEP
	// failure rather than an engine error: a workflow authoring mistake must
	// not wedge a run.
	Failed bool
	// Reason explains a failure, for the routing record and the operator.
	Reason string
}

// ActionRunner computes one action step's payload.
//
// The saga is written against this interface and calls it OUTSIDE every
// transaction, which is what makes "no subprocess ever executes inside a
// transaction" a property of the saga's structure rather than of any runner's
// good behavior.
type ActionRunner interface {
	Run(ctx context.Context, a ActionSpec, sc StepContext) (ActionResult, error)
}
