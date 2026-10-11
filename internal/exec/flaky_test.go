package exec

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestVerifiedAttemptsRefusedCheckSpawnsNothingFurther pins the pre-attempt
// check's contract: it runs before EVERY spawn, a refusal is recorded as its
// own attempt at that ordinal rather than returned as an error, the attempts
// before it stay recorded, and no process runs after it.
func TestVerifiedAttemptsRefusedCheckSpawnsNothingFurther(t *testing.T) {
	refusal := errors.New("the pinned file changed")

	t.Run("a refusal before a flaky re-attempt keeps the earlier attempt", func(t *testing.T) {
		cf := filepath.Join(t.TempDir(), "count")
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env, "WITNESS_COUNT_FILE="+cf, "WITNESS_PASS_ON=0")

		checks := 0
		attempts, err := RunVerifiedAttempts(spec, true, ExitZeroIsPass, func() error {
			checks++
			if checks == 2 {
				return refusal
			}
			return nil
		})
		testsupport.Must(t, err, "RunVerifiedAttempts: %v", err)

		if len(attempts) != 2 {
			t.Fatalf("got %d attempts, want 2: one run, then one refused", len(attempts))
		}
		if attempts[0].Refused != nil || ExitZeroIsPass(attempts[0].Result) {
			t.Errorf("attempt 0 must be the recorded failing run; got %+v", attempts[0])
		}
		if attempts[1].Ordinal != 1 || !errors.Is(attempts[1].Refused, refusal) {
			t.Errorf("attempt 1 must be the refusal at ordinal 1; got %+v", attempts[1])
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

		attempts, err := RunVerifiedAttempts(spec, false, ExitZeroIsPass, func() error {
			return refusal
		})
		testsupport.Must(t, err, "RunVerifiedAttempts: %v", err)

		if len(attempts) != 1 || attempts[0].Ordinal != 0 || !errors.Is(attempts[0].Refused, refusal) {
			t.Fatalf("want one refused attempt at ordinal 0; got %+v", attempts)
		}
		if _, statErr := os.Stat(cf); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the command ran after a refusal before attempt 0 (counter stat: %v)", statErr)
		}
	})
}
