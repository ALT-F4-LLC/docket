package exec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestVerifiedAttemptsRefusedCheckSpawnsNothingFurther pins the pre-attempt
// check's contract: it runs before EVERY spawn, a refusal is returned as a
// Refusal naming that ordinal rather than as an error, the attempts before it
// stay recorded, and no process runs after it.
func TestVerifiedAttemptsRefusedCheckSpawnsNothingFurther(t *testing.T) {
	refusal := errors.New("the pinned file changed")

	t.Run("a refusal before a flaky re-attempt keeps the earlier attempt", func(t *testing.T) {
		cf := filepath.Join(t.TempDir(), "count")
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env, "WITNESS_COUNT_FILE="+cf, "WITNESS_PASS_ON=0")

		checks := 0
		attempts, refused, err := RunVerifiedAttempts(spec, true, ExitZeroIsPass, func() error {
			checks++
			if checks == 2 {
				return refusal
			}
			return nil
		})
		testsupport.Must(t, err, "RunVerifiedAttempts: %v", err)

		if len(attempts) != 1 {
			t.Fatalf("got %d attempts, want 1: the run before the refusal", len(attempts))
		}
		if attempts[0].Ordinal != 0 || ExitZeroIsPass(attempts[0].Result) {
			t.Errorf("attempt 0 must be the recorded failing run; got %+v", attempts[0])
		}
		if refused == nil || refused.Ordinal != 1 || !errors.Is(refused.Err, refusal) {
			t.Errorf("want the refusal at ordinal 1; got %+v", refused)
		}
		assertCount(t, cf, 1)
		if checks != 2 {
			t.Errorf("the check ran %d times, want 2: once per attempt, none after a refusal", checks)
		}
	})

	t.Run("a refusal before the first attempt spawns nothing", func(t *testing.T) {
		cf := filepath.Join(t.TempDir(), "count")
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env, "WITNESS_COUNT_FILE="+cf, "WITNESS_PASS_ON=1")

		attempts, refused, err := RunVerifiedAttempts(spec, false, ExitZeroIsPass, func() error {
			return refusal
		})
		testsupport.Must(t, err, "RunVerifiedAttempts: %v", err)

		if len(attempts) != 0 || refused == nil || refused.Ordinal != 0 || !errors.Is(refused.Err, refusal) {
			t.Fatalf("want no attempts and a refusal at ordinal 0; got %+v and %+v", attempts, refused)
		}
		if _, statErr := os.Stat(cf); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the command ran after a refusal before attempt 0 (counter stat: %v)", statErr)
		}
	})
}

// TestVerifiedAttemptsRefusalIsNeverAPass pins that a declined spawn cannot be
// judged a pass through this package's API: it is not an Attempt, so neither
// a pass predicate over the returned attempts nor Verdict can read it as one.
func TestVerifiedAttemptsRefusalIsNeverAPass(t *testing.T) {
	refusal := errors.New("the pinned file changed")

	assertNoPass := func(t *testing.T, attempts []Attempt) {
		t.Helper()
		for _, a := range attempts {
			if ExitZeroIsPass(a.Result) {
				t.Errorf("attempt %d passes ExitZeroIsPass in a refused run: %+v", a.Ordinal, a)
			}
		}
	}

	t.Run("refused before ordinal 0", func(t *testing.T) {
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env,
			"WITNESS_COUNT_FILE="+filepath.Join(t.TempDir(), "count"), "WITNESS_PASS_ON=1")

		attempts, refused, err := RunVerifiedAttempts(spec, false, ExitZeroIsPass, func() error {
			return refusal
		})
		testsupport.Must(t, err, "RunVerifiedAttempts: %v", err)

		if len(attempts) != 0 || refused == nil || refused.Ordinal != 0 {
			t.Fatalf("want no attempts and a refusal at ordinal 0; got %+v and %+v", attempts, refused)
		}
		assertNoPass(t, attempts)
		if v, ok := Verdict(attempts); ok {
			t.Errorf("Verdict decided a run that spawned nothing: %+v", v)
		}
	})

	t.Run("refused at ordinal 1 of a flaky run", func(t *testing.T) {
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env,
			"WITNESS_COUNT_FILE="+filepath.Join(t.TempDir(), "count"), "WITNESS_PASS_ON=0")

		checks := 0
		attempts, refused, err := RunVerifiedAttempts(spec, true, ExitZeroIsPass, func() error {
			checks++
			if checks == 2 {
				return refusal
			}
			return nil
		})
		testsupport.Must(t, err, "RunVerifiedAttempts: %v", err)

		if len(attempts) != 1 || refused == nil || refused.Ordinal != 1 {
			t.Fatalf("want one attempt and a refusal at ordinal 1; got %+v and %+v", attempts, refused)
		}
		assertNoPass(t, attempts)
		v, ok := Verdict(attempts)
		if !ok || v.Ordinal != 0 || ExitZeroIsPass(v.Result) {
			t.Errorf("Verdict must be the failing run at ordinal 0; got %+v (ok %v)", v, ok)
		}
	})
}

// spawnRecorder is a caller-supplied spawn that runs no process: it returns
// the scripted outcome for each call in turn, fails a call beyond the script
// with errUnscriptedSpawn, and logs every call, with the Spec it received, to
// an order log it shares with the verify hook.
type spawnRecorder struct {
	outcomes []spawnOutcome
	specs    []Spec
	log      *[]string
}

type spawnOutcome struct {
	res Result
	err error
}

var errUnscriptedSpawn = errors.New("spawn called more times than scripted")

func (s *spawnRecorder) spawn(spec Spec) (Result, error) {
	*s.log = append(*s.log, "spawn")
	s.specs = append(s.specs, spec)
	if len(s.specs) > len(s.outcomes) {
		return Result{}, errUnscriptedSpawn
	}
	o := s.outcomes[len(s.specs)-1]
	return o.res, o.err
}

// unrunnableSpec names an argv[0] that cannot exist, so a loop that called
// Run instead of the supplied spawn would fail rather than pass.
func unrunnableSpec(t *testing.T) Spec {
	t.Helper()
	return Spec{
		Argv: []string{filepath.Join(t.TempDir(), "no-such-program"), "--flag"},
		Dir:  t.TempDir(),
		Env:  []string{"A=1"},
	}
}

var (
	spawnPass = spawnOutcome{res: Result{Exit: 0}}
	spawnFail = spawnOutcome{res: Result{Exit: 1}}
)

// TestSpawnedAttemptsUsesTheSuppliedSpawn pins the loop's contract: each
// attempt goes through the caller's spawn, with the caller's Spec, after the
// verify hook, under the same F1/F2/F3 and refusal rules RunVerifiedAttempts
// follows.
func TestSpawnedAttemptsUsesTheSuppliedSpawn(t *testing.T) {
	refusal := errors.New("the pinned file changed")

	t.Run("each attempt calls the supplied spawn with the given Spec after verify", func(t *testing.T) {
		var log []string
		spec := unrunnableSpec(t)
		rec := &spawnRecorder{outcomes: []spawnOutcome{spawnPass}, log: &log}

		attempts, refused, err := RunSpawnedAttempts(spec, true, ExitZeroIsPass, func() error {
			log = append(log, "verify")
			return nil
		}, rec.spawn)
		testsupport.Must(t, err, "RunSpawnedAttempts: %v", err)

		if len(rec.specs) != 1 || !reflect.DeepEqual(rec.specs[0], spec) {
			t.Fatalf("spawn got %+v, want one call with %+v", rec.specs, spec)
		}
		if got, want := strings.Join(log, ","), "verify,spawn"; got != want {
			t.Errorf("call order %q, want %q", got, want)
		}
		if refused != nil || len(attempts) != 1 || attempts[0].Ordinal != 0 || !ExitZeroIsPass(attempts[0].Result) {
			t.Errorf("want one passing attempt at ordinal 0 and no refusal; got %+v and %+v", attempts, refused)
		}
	})

	t.Run("a verify refusal at ordinal 1 stops before the second spawn", func(t *testing.T) {
		var log []string
		rec := &spawnRecorder{outcomes: []spawnOutcome{spawnFail, spawnFail, spawnFail}, log: &log}
		checks := 0

		attempts, refused, err := RunSpawnedAttempts(unrunnableSpec(t), true, ExitZeroIsPass, func() error {
			log = append(log, "verify")
			checks++
			if checks == 2 {
				return refusal
			}
			return nil
		}, rec.spawn)
		testsupport.Must(t, err, "RunSpawnedAttempts: %v", err)

		if len(rec.specs) != 1 || len(attempts) != 1 || attempts[0].Ordinal != 0 {
			t.Fatalf("want one spawn and one attempt at ordinal 0; got %d spawns and %+v", len(rec.specs), attempts)
		}
		if refused == nil || refused.Ordinal != 1 || !errors.Is(refused.Err, refusal) {
			t.Errorf("want the refusal at ordinal 1; got %+v", refused)
		}
		if got, want := strings.Join(log, ","), "verify,spawn,verify"; got != want {
			t.Errorf("call order %q, want %q", got, want)
		}
	})

	t.Run("the first passing attempt ends a flaky sequence", func(t *testing.T) {
		var log []string
		rec := &spawnRecorder{outcomes: []spawnOutcome{spawnFail, spawnPass, spawnPass}, log: &log}

		attempts, refused, err := RunSpawnedAttempts(unrunnableSpec(t), true, ExitZeroIsPass, nil, rec.spawn)
		testsupport.Must(t, err, "RunSpawnedAttempts: %v", err)

		if len(rec.specs) != 2 || len(attempts) != 2 || refused != nil {
			t.Fatalf("want two spawns, two attempts, no refusal; got %d spawns, %+v, %+v", len(rec.specs), attempts, refused)
		}
		if attempts[0].Ordinal != 0 || attempts[1].Ordinal != 1 || !ExitZeroIsPass(attempts[1].Result) {
			t.Errorf("want a failing ordinal 0 then a passing ordinal 1; got %+v", attempts)
		}
	})

	t.Run("a spawn error is returned unretried with no refusal", func(t *testing.T) {
		var log []string
		spawnErr := fmt.Errorf("%w: pin mismatch", ErrRefused)
		rec := &spawnRecorder{outcomes: []spawnOutcome{{err: spawnErr}, spawnPass, spawnPass}, log: &log}

		attempts, refused, err := RunSpawnedAttempts(unrunnableSpec(t), true, ExitZeroIsPass, nil, rec.spawn)

		if !errors.Is(err, spawnErr) {
			t.Errorf("want the spawn error returned; got %v", err)
		}
		if len(rec.specs) != 1 || len(attempts) != 0 || refused != nil {
			t.Errorf("want one spawn, no attempts, no refusal; got %d spawns, %+v, %+v", len(rec.specs), attempts, refused)
		}
	})

	t.Run("a spec not declared flaky spawns once even when it fails", func(t *testing.T) {
		var log []string
		rec := &spawnRecorder{outcomes: []spawnOutcome{spawnFail, spawnFail, spawnFail}, log: &log}

		attempts, refused, err := RunSpawnedAttempts(unrunnableSpec(t), false, ExitZeroIsPass, nil, rec.spawn)
		testsupport.Must(t, err, "RunSpawnedAttempts: %v", err)

		if len(rec.specs) != 1 || len(attempts) != 1 || refused != nil {
			t.Errorf("want one spawn, one attempt, no refusal; got %d spawns, %+v, %+v", len(rec.specs), attempts, refused)
		}
	})
}
