package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// DKT-1564 — `step claim` must not exit non-zero while the lease it just took
// stands. A wave executor reads a non-zero exit as "stop, claim nothing", so a
// refusal on a committed claim strands the lease AND the token in one step.

// TestClaimWithAPostCommitFailureExitsZeroCarryingTheToken is AC1 at the CLI
// seam: the claim JSON — token included — is what a caller receives, and the
// failure is reported beside it rather than in place of it.
func TestClaimWithAPostCommitFailureExitsZeroCarryingTheToken(t *testing.T) {
	w, buf := bufWriter(true)

	incomplete := &engine.IncompleteClaimError{
		Result: &engine.ClaimResult{
			Step: "STEP-1", Token: "tok-live", LeaseExpiresMS: 1_800_000_600_000,
			Attempt: 1, RowVersion: 3,
		},
		Err: errors.New("database is locked (5) (SQLITE_BUSY)"),
	}

	if err := emitIncompleteClaim(w, incomplete, "step STEP-1"); err != nil {
		t.Fatalf("a committed claim was reported as a command failure: %v", err)
	}

	var envelope struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Data    struct {
			Step       string `json:"step"`
			Token      string `json:"token"`
			ClaimError string `json:"claim_error"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope),
		"the response is not a JSON envelope: %q", buf.String())

	if !envelope.OK {
		t.Error("ok = false; a caller branching on .ok would discard the token it holds")
	}
	// wave.js reads exactly this path. It is the whole point of the change.
	if envelope.Data.Token != "tok-live" {
		t.Errorf("data.token = %q, want the committed claim's token", envelope.Data.Token)
	}
	if !strings.Contains(envelope.Data.ClaimError, "SQLITE_BUSY") {
		t.Errorf("data.claim_error = %q, want the post-commit failure reported",
			envelope.Data.ClaimError)
	}
	if !strings.Contains(envelope.Message, "SQLITE_BUSY") {
		t.Errorf("message = %q, want the failure named for a human reader too",
			envelope.Message)
	}
}

// TestClaimFailureBeforeTheLeaseIsStillARefusal keeps the ordinary path: an
// error carrying no committed claim is the refusal it always was.
func TestClaimFailureBeforeTheLeaseIsStillARefusal(t *testing.T) {
	w, buf := bufWriter(true)

	err := emitIncompleteClaim(w, errors.New("step verify@0 is not ready to claim"),
		"step STEP-1")
	if err == nil {
		t.Fatal("a plain claim refusal was reported as a success")
	}
	if buf.Len() != 0 {
		t.Errorf("a refusal wrote a claim envelope: %q", buf.String())
	}
}

// TestStepClaimCommandReMintsForItsOwnOwner is AC2 through the REAL RunE: the
// executor whose token never arrived re-runs the same command and gets a
// working one, instead of the CONFLICT that sent RUN-90 to a reap panel.
func TestStepClaimCommandReMintsForItsOwnOwner(t *testing.T) {
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	_, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "activate: %v", err)

	var stepID int
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND step_name = 'first'`, runID,
	).Scan(&stepID)
	testsupport.Must(t, err, "reading the claimable step: %v", err)

	cmd := cmdWithDB(conn)
	cmd.Flags().AddFlagSet(stepClaimCmd.Flags())
	t.Cleanup(func() { _ = stepClaimCmd.Flags().Set("owner", "") })
	testsupport.Must(t, cmd.Flags().Set("owner", "wave:STEP-1"), "setting --owner: %v", err)

	arg := model.FormatStepID(stepID)
	secret := secretFile(t, testRecoverySecret, 0o600)
	cmd.SetIn(openStdin(t, secret))
	testsupport.Must(t, stepClaimCmd.RunE(cmd, []string{arg}), "the first claim: %v", err)

	before, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	// The retry a stranded executor makes: same verb, same owner, same step,
	// same secret file.
	cmd.SetIn(openStdin(t, secret))
	testsupport.Must(t, stepClaimCmd.RunE(cmd, []string{arg}),
		"the stranded executor's own re-claim was refused: %v", err)

	after, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if after.TokenHash == before.TokenHash {
		t.Error("the re-claim did not re-mint; the executor still has no usable token")
	}
	if after.Attempt != before.Attempt {
		t.Errorf("attempt = %d after the re-mint, want %d", after.Attempt, before.Attempt)
	}
	if after.Status != db.StepClaimed || after.Owner != "wave:STEP-1" {
		t.Errorf("the re-mint disturbed the claim: status=%q owner=%q",
			after.Status, after.Owner)
	}
}

// TestReMintedClaimSaysSoInItsResponse keeps the two outcomes distinguishable:
// a caller must be able to tell a fresh claim from a re-key of the lease it
// already held, without diffing the attempt against a value it may not have.
func TestReMintedClaimSaysSoInItsResponse(t *testing.T) {
	w, buf := bufWriter(true)

	err := emitClaim(w, &engine.ClaimResult{
		Step: "STEP-1", Token: "tok", LeaseExpiresMS: 1_800_000_600_000,
		Attempt: 1, RowVersion: 4, ReMinted: true,
	}, nil)
	testsupport.Must(t, err, "emitClaim: %v", err)

	var envelope struct {
		Data struct {
			ReMinted bool `json:"re_minted"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope),
		"the response is not a JSON envelope: %q", buf.String())
	if !envelope.Data.ReMinted {
		t.Errorf("data.re_minted is absent from a re-mint: %q", buf.String())
	}

	// An ordinary claim's envelope is UNCHANGED: the field is omitted, not false.
	w, buf = bufWriter(true)
	err = emitClaim(w, &engine.ClaimResult{
		Step: "STEP-1", Token: "tok", LeaseExpiresMS: 1_800_000_600_000, Attempt: 1,
	}, nil)
	testsupport.Must(t, err, "emitClaim: %v", err)
	if strings.Contains(buf.String(), "re_minted") {
		t.Errorf("a clean claim's envelope grew a field: %q", buf.String())
	}
}

// The recovery secret proves a same-owner re-claim. It reaches `step claim`
// only as stdin redirected from a 0600 regular file: argv is readable through
// `ps`, and the environment is inherited by every descendant.

const (
	testRecoverySecret  = "synthetic-recovery-proof-for-claim-tests"
	otherRecoverySecret = "another-synthetic-recovery-proof-for-claim-tests"
)

// claimableStep activates a fresh run and returns its claimable step.
func claimableStep(t *testing.T) (*sql.DB, int) {
	t.Helper()
	conn := newTestDB(t)
	runID, _ := seedRun(t, conn)
	_, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "activate: %v", err)
	var stepID int
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND step_name = 'first'`, runID,
	).Scan(&stepID)
	testsupport.Must(t, err, "reading the claimable step: %v", err)
	return conn, stepID
}

// claimCommand prepares the real `step claim` RunE as owner wave:STEP-1 with
// stdin bound to the given file, and returns the call that runs it.
func claimCommand(t *testing.T, conn *sql.DB, stepID int, stdin *os.File) func() error {
	t.Helper()
	cmd := cmdWithDB(conn)
	cmd.Flags().AddFlagSet(stepClaimCmd.Flags())
	t.Cleanup(func() { _ = stepClaimCmd.Flags().Set("owner", "") })
	err := cmd.Flags().Set("owner", "wave:STEP-1")
	testsupport.Must(t, err, "setting --owner: %v", err)
	cmd.SetIn(stdin)
	return func() error { return stepClaimCmd.RunE(cmd, []string{model.FormatStepID(stepID)}) }
}

func claimWithStdin(t *testing.T, conn *sql.DB, stepID int, stdin *os.File) error {
	t.Helper()
	return claimCommand(t, conn, stepID, stdin)()
}

// secretFile writes content to a fresh file at exactly mode perm. The chmod
// after the write keeps the umask from narrowing the mode under test.
func secretFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.secret")
	err := os.WriteFile(path, []byte(content), 0o600)
	testsupport.Must(t, err, "writing the secret file: %v", err)
	err = os.Chmod(path, perm)
	testsupport.Must(t, err, "chmod the secret file: %v", err)
	return path
}

// openStdin opens path the way a shell redirect does: a fresh read handle.
func openStdin(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	testsupport.Must(t, err, "opening %s: %v", path, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func recoveryHash(t *testing.T, conn *sql.DB, stepID int) sql.NullString {
	t.Helper()
	var hash sql.NullString
	err := conn.QueryRow(`SELECT recovery_hash FROM steps WHERE id = ?`, stepID).Scan(&hash)
	testsupport.Must(t, err, "reading recovery_hash: %v", err)
	return hash
}

func errCode(t *testing.T, err error) output.ErrorCode {
	t.Helper()
	var cmdError *CmdError
	if !errors.As(err, &cmdError) {
		t.Fatalf("error %v is %T, not a *CmdError", err, err)
	}
	return cmdError.Code
}

// assertSecretNeverSurfaced checks the refusal text and every event recorded
// for the step: a secret that leaks into either is a live re-claim proof.
func assertSecretNeverSurfaced(t *testing.T, conn *sql.DB, stepID int, err error, secret string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal echoes the recovery secret: %v", err)
	}
	rows, qerr := conn.Query(`SELECT kind, data FROM events WHERE step_id = ?`, stepID)
	testsupport.Must(t, qerr, "reading events: %v", qerr)
	defer rows.Close()
	for rows.Next() {
		var kind, data string
		serr := rows.Scan(&kind, &data)
		testsupport.Must(t, serr, "scanning an event: %v", serr)
		if strings.Contains(data, secret) {
			t.Errorf("event %s carries the recovery secret: %s", kind, data)
		}
	}
	testsupport.Must(t, rows.Err(), "iterating events: %v", rows.Err())
}

// assertUnclaimed checks a refusal left the step as it was: no lease, no
// owner, no stored hash.
func assertUnclaimed(t *testing.T, conn *sql.DB, stepID int) {
	t.Helper()
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status == db.StepClaimed || step.Owner != "" {
		t.Errorf("a refused claim took the lease: status=%q owner=%q", step.Status, step.Owner)
	}
	if h := recoveryHash(t, conn, stepID); h.Valid {
		t.Error("a refused claim stored a recovery hash")
	}
}

// TestStepClaimTakesNoSecretOnArgv is the argv half of the transport rule. The
// flag set is pinned exactly, in the live flag set and in --help, so a flag
// carrying the secret fails here whatever it is named.
func TestStepClaimTakesNoSecretOnArgv(t *testing.T) {
	want := []string{"cost-multiplier", "executor", "metadata", "owner", "render", "template", "ttl"}

	// Executing or rendering the package-global command merges the root's
	// persistent flags into Flags() and adds cobra's --help. Do both first, as
	// an earlier test or iteration may have, so the result cannot depend on order.
	stepClaimCmd.InitDefaultHelpFlag()
	usage := stepClaimCmd.UsageString()

	// cobra's own --help is not part of the claim's surface. The annotation, not
	// the name, identifies it, so a claim flag named help is still pinned.
	addedByCobra := func(name string) bool {
		f := stepClaimCmd.Flags().Lookup(name)
		return f != nil && len(f.Annotations[cobra.FlagSetByCobraAnnotation]) > 0
	}

	var registered []string
	stepClaimCmd.LocalFlags().VisitAll(func(f *pflag.Flag) { registered = append(registered, f.Name) })
	registered = slices.DeleteFunc(registered, addedByCobra)
	sort.Strings(registered)
	if !slices.Equal(registered, want) {
		t.Errorf("step claim flags = %v, want exactly %v: no flag may carry the "+
			"recovery secret, which is accepted only on stdin", registered, want)
	}

	// The command's own "Flags:" block; "Global Flags:" are the root's.
	_, local, found := strings.Cut(usage, "\nFlags:\n")
	if !found {
		t.Fatalf("step claim --help has no Flags section:\n%s", usage)
	}
	local, _, _ = strings.Cut(local, "\n\n")
	var listed []string
	for _, line := range strings.Split(local, "\n") {
		for _, field := range strings.Fields(line) {
			if name, ok := strings.CutPrefix(field, "--"); ok {
				listed = append(listed, name)
				break
			}
		}
	}
	listed = slices.DeleteFunc(listed, addedByCobra)
	sort.Strings(listed)
	if !slices.Equal(listed, want) {
		t.Errorf("step claim --help lists %v, want exactly %v", listed, want)
	}
}

// TestStepClaimReadsTheRecoverySecretOnlyFromStdin pins the channel: the hash
// stored is the stdin file's, never an environment variable's, and with no
// redirected file no hash is stored even when those variables are set.
func TestStepClaimReadsTheRecoverySecretOnlyFromStdin(t *testing.T) {
	const envToken = "env-token-that-must-not-become-the-secret"
	const envSecret = "env-secret-that-must-not-be-read"
	t.Setenv(TokenEnvVar, envToken)
	t.Setenv("DOCKET_RECOVERY_SECRET", envSecret)

	t.Run("redirected 0600 file", func(t *testing.T) {
		conn, stepID := claimableStep(t)
		stdin := openStdin(t, secretFile(t, testRecoverySecret+"\n", 0o600))
		err := claimWithStdin(t, conn, stepID, stdin)
		testsupport.Must(t, err, "claim: %v", err)

		got := recoveryHash(t, conn, stepID)
		if !got.Valid || got.String != model.HashToken(testRecoverySecret) {
			t.Errorf("recovery_hash = %v, want the hash of the trimmed stdin secret", got)
		}
		if got.String == model.HashToken(envToken) || got.String == model.HashToken(envSecret) {
			t.Error("the recovery secret was read from the environment")
		}
		assertSecretNeverSurfaced(t, conn, stepID, nil, testRecoverySecret)
	})

	t.Run("no redirected file", func(t *testing.T) {
		conn, stepID := claimableStep(t)
		err := claimWithStdin(t, conn, stepID, openStdin(t, os.DevNull))
		testsupport.Must(t, err, "claim: %v", err)
		if h := recoveryHash(t, conn, stepID); h.Valid {
			t.Errorf("recovery_hash = %q with no stdin secret; the environment was read", h.String)
		}
	})
}

// TestStepClaimReMintsOnlyForTheStdinSecret drives the recovery check through
// the CLI: the first claim's stdin secret is what a re-claim must present.
func TestStepClaimReMintsOnlyForTheStdinSecret(t *testing.T) {
	conn, stepID := claimableStep(t)
	path := secretFile(t, testRecoverySecret, 0o600)
	err := claimWithStdin(t, conn, stepID, openStdin(t, path))
	testsupport.Must(t, err, "first claim: %v", err)
	first, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	for _, tc := range []struct {
		name  string
		stdin *os.File
	}{
		{"a different secret", openStdin(t, secretFile(t, otherRecoverySecret, 0o600))},
		{"no secret", openStdin(t, os.DevNull)},
	} {
		err := claimWithStdin(t, conn, stepID, tc.stdin)
		if err == nil {
			t.Fatalf("re-claim with %s re-minted the lease", tc.name)
		}
		if code := errCode(t, err); code != output.ErrConflict {
			t.Errorf("re-claim with %s: code = %v, want %v", tc.name, code, output.ErrConflict)
		}
		after, gerr := db.GetStep(conn, stepID)
		testsupport.Must(t, gerr, "GetStep: %v", gerr)
		if after.TokenHash != first.TokenHash {
			t.Errorf("re-claim with %s voided the holder's token", tc.name)
		}
		assertSecretNeverSurfaced(t, conn, stepID, err, testRecoverySecret)
		assertSecretNeverSurfaced(t, conn, stepID, err, otherRecoverySecret)
	}

	err = claimWithStdin(t, conn, stepID, openStdin(t, path))
	testsupport.Must(t, err, "re-claim with the same secret file: %v", err)
	after, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if after.TokenHash == first.TokenHash {
		t.Error("the re-claim presenting the stored secret did not re-mint")
	}
}

// TestStepClaimRefusesAnExposedSecretFile: a file other users can read is a
// secret other users hold. The refusal names the mode and takes no lease.
func TestStepClaimRefusesAnExposedSecretFile(t *testing.T) {
	for _, perm := range []os.FileMode{0o640, 0o644} {
		t.Run(fmt.Sprintf("%04o", perm), func(t *testing.T) {
			conn, stepID := claimableStep(t)
			err := claimWithStdin(t, conn, stepID,
				openStdin(t, secretFile(t, testRecoverySecret, perm)))
			if err == nil {
				t.Fatalf("a %04o secret file was accepted", perm)
			}
			if code := errCode(t, err); code != output.ErrValidation {
				t.Errorf("code = %v, want %v", code, output.ErrValidation)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%04o", perm)) {
				t.Errorf("refusal %q does not name the file's mode %04o", err, perm)
			}
			assertUnclaimed(t, conn, stepID)
			assertSecretNeverSurfaced(t, conn, stepID, err, testRecoverySecret)
		})
	}
}

// TestStepClaimRefusesAnUnusableSecretFile: a redirected file that holds no
// secret, or far more than one, is a caller mistake. Accepting the empty one
// would leave the caller believing its lease is protected when it is not.
func TestStepClaimRefusesAnUnusableSecretFile(t *testing.T) {
	for name, content := range map[string]string{
		"empty":           "",
		"whitespace only": " \n\t\n",
		"oversize":        strings.Repeat("recovery-proof-", 1024),
	} {
		t.Run(name, func(t *testing.T) {
			conn, stepID := claimableStep(t)
			err := claimWithStdin(t, conn, stepID, openStdin(t, secretFile(t, content, 0o600)))
			if err == nil {
				t.Fatalf("a %s secret file was accepted", name)
			}
			if code := errCode(t, err); code != output.ErrValidation {
				t.Errorf("code = %v, want %v", code, output.ErrValidation)
			}
			assertUnclaimed(t, conn, stepID)
		})
	}
}

// TestStepClaimDoesNotBlockOnAnOpenPipe: an agent whose inherited stdin pipe
// never closes must still claim. Only a regular file is read.
func TestStepClaimDoesNotBlockOnAnOpenPipe(t *testing.T) {
	conn, stepID := claimableStep(t)
	r, w, err := os.Pipe()
	testsupport.Must(t, err, "os.Pipe: %v", err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	claim := claimCommand(t, conn, stepID, r)
	done := make(chan error, 1)
	go func() { done <- claim() }()
	select {
	case err := <-done:
		testsupport.Must(t, err, "claim with an open pipe on stdin: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("step claim blocked reading a pipe that never closes")
	}
	if h := recoveryHash(t, conn, stepID); h.Valid {
		t.Error("a piped stdin was read as the recovery secret")
	}
}
