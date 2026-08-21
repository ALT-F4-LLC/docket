package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestReadTokenFromEnv(t *testing.T) {
	t.Setenv(TokenEnvVar, "env-token")

	got, err := readToken(strings.NewReader("stdin-token"))
	testsupport.Must(t, err, "readToken: %v", err)
	if got != "env-token" {
		t.Errorf("token = %q, want env-token (env takes precedence)", got)
	}
}

func TestReadTokenFromStdin(t *testing.T) {
	t.Setenv(TokenEnvVar, "")

	got, err := readToken(strings.NewReader("stdin-token\n"))
	testsupport.Must(t, err, "readToken: %v", err)
	if got != "stdin-token" {
		t.Errorf("token = %q, want stdin-token (trailing newline trimmed)", got)
	}
}

func TestReadTokenMissingIsValidationError(t *testing.T) {
	t.Setenv(TokenEnvVar, "")

	_, err := readToken(strings.NewReader("   \n"))
	if err == nil {
		t.Fatal("readToken with no token succeeded")
	}

	var ce *CmdError
	if !asCmdError(err, &ce) {
		t.Fatalf("error = %v, want a CmdError", err)
	}
	if ce.Code != "VALIDATION_ERROR" {
		t.Errorf("code = %s, want VALIDATION_ERROR", ce.Code)
	}
	// The message must name both channels: a caller that supplied neither
	// most likely does not know which are accepted.
	msg := ce.Error()
	if !strings.Contains(msg, TokenEnvVar) || !strings.Contains(msg, "stdin") {
		t.Errorf("error %q does not name both accepted channels", msg)
	}
}

func TestOptionalTokenIsEmptyWhenAbsent(t *testing.T) {
	t.Setenv(TokenEnvVar, "")

	if got := optionalToken(strings.NewReader("")); got != "" {
		t.Errorf("optionalToken = %q, want empty", got)
	}
}

// TestNoTokenFlag is the §4 transport guard: tokens pass via env or stdin,
// never argv.
//
// It walks the live command tree rather than grepping source, so it catches a
// flag however it is registered. A --token flag would put a live capability
// into argv, where `ps` exposes it to every user on the host — which defeats
// the capability model at the transport layer regardless of how carefully the
// rest of the code treats the token.
func TestNoTokenFlag(t *testing.T) {
	var offenders []string

	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		check := func(f *pflag.Flag) {
			name := strings.ToLower(f.Name)
			if strings.Contains(name, "token") || strings.Contains(name, "secret") {
				offenders = append(offenders, cmd.CommandPath()+" --"+f.Name)
			}
		}
		cmd.Flags().VisitAll(check)
		cmd.PersistentFlags().VisitAll(check)
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd)

	if len(offenders) > 0 {
		t.Errorf("token-bearing flags found: %v; tokens must pass via %s or stdin "+
			"only (engine-spec.md §4) — argv is world-readable via ps",
			offenders, TokenEnvVar)
	}
}

// TestClaimVerbsExist pins the stage's verb surface, and equally pins what it
// does NOT ship: complete/fail belong to steps at stage 3, where their saga
// semantics are specified.
func TestClaimVerbsExist(t *testing.T) {
	present := map[string]bool{}
	for _, sub := range issueCmd.Commands() {
		present[sub.Name()] = true
	}

	for _, verb := range []string{"claim", "heartbeat", "release"} {
		if !present[verb] {
			t.Errorf("issue %s is missing from the command tree", verb)
		}
	}
	for _, verb := range []string{"complete", "fail"} {
		if present[verb] {
			t.Errorf("issue %s exists; the saga verbs belong to steps at stage 3", verb)
		}
	}
}

// asCmdError is errors.As for *CmdError without importing errors in every test.
func asCmdError(err error, target **CmdError) bool {
	for err != nil {
		if ce, ok := err.(*CmdError); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestTokenEnvVarName pins the environment variable name, which is part of the
// documented contract in SKILL.md and docs/spec/security.md.
func TestTokenEnvVarName(t *testing.T) {
	if TokenEnvVar != "DOCKET_TOKEN" {
		t.Errorf("TokenEnvVar = %q, want DOCKET_TOKEN", TokenEnvVar)
	}
	// Guard against a stray value in the developer's environment leaking into
	// the tests above.
	if os.Getenv(TokenEnvVar) != "" {
		t.Logf("note: %s is set in this environment", TokenEnvVar)
	}
}
