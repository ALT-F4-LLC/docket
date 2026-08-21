// Package exec runs one registered command and reports what it did
// (docs/design/engine-spec.md §4, docs/tdd/gates-trust.md §5).
//
// The package is PURE and unit-testable without the engine: it takes an argv,
// an environment policy, a timeout, and a working directory, and returns an
// exit code, captured output, a truncation flag, and a duration. It has no
// database handle, no knowledge of steps or gates, and no dependency on
// internal/engine.
//
// §4's mechanics sentence is the specification:
//
//	Mechanics: resolved argv, no shell interpolation, cwd repo root, env
//	allowlist, timeout with process-group kill, captured output with explicit
//	truncation, flaky-declared re-runs recorded individually.
//
// THE INVARIANT THIS PACKAGE EXISTS TO HOLD: no command interpreter is ever
// invoked. Argv is a list of strings from end to end. A candidate argv element
// containing a semicolon, a substitution, a backtick, or a glob character is
// ONE ARGUMENT CONTAINING THOSE CHARACTERS — nothing splits it, nothing expands
// it, and no interpreter is given the chance to. The Spec type has no field in
// which a command string could be placed, so producing one would require adding
// an API rather than making a mistake.
package exec

import (
	"errors"
	"os/exec"
	"time"
)

// Spec is one command to run.
//
// THE TYPE MAKES THE UNSAFE THING UNREPRESENTABLE (§5.1). There is no Shell
// field, no Command string, and no way to express one. The guard is structural
// rather than a convention, because a convention is what gets refactored away
// three changes later by someone who never read this comment.
type Spec struct {
	// Argv is the resolved argv. argv[0] is the program; it is NEVER a command
	// string to be parsed by anything.
	Argv []string
	// Dir is the working directory — the repo root. Note that argv[0] is
	// NEVER resolved against it (§5.2, T15).
	Dir string
	// Env is the constructed allowlist (§5.3). It is never os.Environ(): a
	// child gets a variable only because the allowlist names it.
	Env []string
	// Timeout bounds the run. Zero means DefaultTimeout.
	Timeout time.Duration
	// Stdin is fed to the child and then closed, or nil for an empty stdin.
	//
	// It is DATA, never a command. The no-interpreter invariant is untouched by
	// it: nothing here is parsed, split, or expanded, and the child receives
	// bytes on fd 0 exactly as given. It exists because
	// docs/tdd/payloads-thresholds.md §6.2 puts an action's step context on
	// stdin — "Other computations remain user-trusted commands receiving step
	// context on stdin" (engine-spec §2).
	Stdin []byte
	// SplitStdout captures stdout SEPARATELY into Result.Stdout, leaving Output
	// as the stderr capture.
	//
	// A gate's answer is its exit code, so one interleaved capture is the right
	// shape for it and stays the default. An action's answer is a DOCUMENT on
	// stdout, and a command that logs a line to stderr must not be able to
	// corrupt the document core parses — which one shared writer would let it
	// do, by construction.
	SplitStdout bool
}

// Result is one execution's outcome, in the shape §11.4's `gate result`
// consumes.
//
// It deliberately does NOT carry a verdict: mapping an exit code to pass/fail,
// and deciding what an unmatched command means for routing, are the engine's
// decisions in group 2. This package reports facts about a process.
type Result struct {
	// Argv is the argv that was actually executed.
	Argv []string
	// Exit is the process exit code.
	Exit int
	// DurationMS is wall clock around the spawn.
	DurationMS int64
	// Output is the interleaved stdout+stderr capture (§5.5 C1), or the stderr
	// capture alone when Spec.SplitStdout asked for the streams to be kept
	// apart.
	Output string
	// Stdout is the stdout capture, and is set ONLY under Spec.SplitStdout. It
	// is capped and truncated by the same rule Output is: a structured reply is
	// not exempt from §5.5's bound just because something intends to parse it.
	Stdout string
	// Truncated reports that Output hit the cap (§5.5 C2).
	Truncated bool
	// TimedOut reports that the process was killed by the timeout. The caller
	// records verdict = "fail", NOT "unmatched" (X4): the command was trusted
	// and did run; it failed by exceeding its bound.
	TimedOut bool
	// Reason explains a timeout or a refusal, for the operator (§6.3).
	Reason string
}

// DefaultTimeout is X5's bound when a trust entry names none.
//
// A PACKAGE CONSTANT AT THIS STAGE, not a config key. Both this and the capture
// cap are SECURITY BOUNDS, and a config key makes them adjustable by anything
// that can write the database. The per-entry timeout already covers the
// legitimate case of a genuinely long check.
const DefaultTimeout = 5 * time.Minute

// killGrace is X2's interval between SIGTERM and SIGKILL. A process gets a
// chance to exit cleanly and flush its output; if it does not take it, it is
// killed rather than waited on indefinitely.
const killGrace = 2 * time.Second

// ErrRefused is returned when the runner declines to spawn: argv[0] resolved
// into the repository (R3), argv[0] would have been resolved against the
// working directory (T15), or the argv is empty. NOTHING RAN — callers record
// verdict = "unmatched" and never touch the process table.
var ErrRefused = errors.New("execution refused")

// compile-time assertion that this package's own view of a command is a list.
// If a future edit introduces a string-shaped command, this breaks first.
var _ = func(s Spec) []string { return s.Argv }

// exitCodeOf extracts a process exit code from the error os/exec returns.
//
// A signal-terminated process reports its exit code as -1 through ExitCode(),
// which is the honest encoding: it did not exit with a status, it was killed.
// Callers distinguish the timeout case by Result.TimedOut rather than by
// pattern-matching the number.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
