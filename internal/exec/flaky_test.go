package exec

import (
	"errors"
	"os"
	"path/filepath"
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
