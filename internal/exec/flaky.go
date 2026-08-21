package exec

// MaxFlakyAttempts is F2's bound: a flaky command that FAILS re-runs up to 2
// additional times, so 3 attempts total. A package constant, like the timeout
// and the capture cap and for the same reason.
const MaxFlakyAttempts = 3

// Attempt is one execution of a command inside a single gate stage, carrying
// the ordinal that makes F3's "recorded individually" concrete.
type Attempt struct {
	// Ordinal is the re-run index: 0, 1, 2. It becomes the ordinal column on
	// the row this attempt produces.
	Ordinal int
	// Result is what that attempt did.
	Result Result
}

// RunAttempts executes a command, re-running it on failure when and only when
// the operator declared it flaky (§5.6).
//
// F1: ONLY a flaky-declared command re-runs. A command not declared flaky runs
// EXACTLY ONCE per gate stage — there is no retry-on-failure by default,
// because a retry the operator did not ask for turns a deterministic failing
// check into an intermittent one.
//
// F3: EVERY ATTEMPT IS ITS OWN RESULT, with an ascending ordinal. Nothing is
// overwritten and nothing is aggregated. That is what "recorded individually"
// means and it is the point: a check that passes on the third try is a fact the
// operator should be able to see, not one an averaging step hides.
//
// F5: `flaky` is NOT `re_runnable`, and conflating them is the likely
// implementation error. `flaky` governs re-running WITHIN ONE GATE STAGE after
// a failure — this function. `re_runnable` governs re-running AFTER A CRASH,
// when the engine does not know whether the previous attempt ran at all — that
// is the saga's decision in group 2, and nothing here reads it. A command may
// be either, both, or neither.
//
// isPass decides what counts as success. It is a parameter rather than a
// hardcoded `exit == 0` because the pass/fail mapping is the engine's decision,
// and this package reports facts about processes.
func RunAttempts(spec Spec, flaky bool, isPass func(Result) bool) ([]Attempt, error) {
	maxAttempts := 1
	if flaky {
		maxAttempts = MaxFlakyAttempts
	}

	attempts := make([]Attempt, 0, maxAttempts)
	for i := 0; i < maxAttempts; i++ {
		res, err := Run(spec)
		if err != nil {
			// A refusal or a spawn failure is not a flaky failure and is not
			// retried: re-running something that could not start would produce
			// the same refusal three times and record it three times.
			return attempts, err
		}
		attempts = append(attempts, Attempt{Ordinal: i, Result: res})

		// F2: stop at the first pass.
		if isPass(res) {
			break
		}
	}
	return attempts, nil
}

// Verdict returns the attempt whose result decides routing (F4): THE LAST ONE.
//
// A command that passes on attempt 2 passes; one that fails all three fails.
// The earlier attempts remain recorded — they are the evidence that the check
// is flaky — but they do not vote.
func Verdict(attempts []Attempt) (Attempt, bool) {
	if len(attempts) == 0 {
		return Attempt{}, false
	}
	return attempts[len(attempts)-1], true
}

// ExitZeroIsPass is the ordinary pass predicate: exit 0 passes, anything else
// fails. A timeout fails through the same door — X4 records a timed-out command
// as a failure, NOT as unmatched, because the command was trusted and did run;
// it failed by exceeding its bound.
func ExitZeroIsPass(r Result) bool {
	return r.Exit == 0 && !r.TimedOut
}
