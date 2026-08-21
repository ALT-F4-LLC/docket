package exec

import (
	"errors"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestNoShellEverInvoked is T2's table.
//
// Every row is an argv element containing a character that a command
// interpreter would treat as SYNTAX. The assertion is made from the CHILD's
// point of view: the witness prints each argument it received, and each row
// asserts the child got EXACTLY ONE argument containing the literal text.
//
// An implementation that passed the argv through an interpreter would fail
// these rows by splitting one argument into several, by expanding a
// substitution into its output, or by expanding a glob into filenames.
func TestNoShellEverInvoked(t *testing.T) {
	tests := []struct {
		name string
		arg  string
	}{
		{"semicolon", "a; rm -rf /tmp/nothing"},
		{"double ampersand", "a && rm -rf /tmp/nothing"},
		{"double pipe", "a || echo pwned"},
		{"single pipe", "a | tee /tmp/nothing"},
		{"command substitution", "$(whoami)"},
		{"backtick substitution", "`whoami`"},
		{"variable", "$HOME"},
		{"braced variable", "${HOME}"},
		{"redirect out", "a > /tmp/nothing"},
		{"redirect in", "a < /etc/passwd"},
		{"append redirect", "a >> /tmp/nothing"},
		{"glob star", "*"},
		{"glob question", "?"},
		{"glob bracket", "[a-z]"},
		{"tilde", "~"},
		{"tilde path", "~/secret"},
		{"newline", "a\nb"},
		{"carriage return", "a\rb"},
		{"tab", "a\tb"},
		{"brace expansion", "{a,b}"},
		{"subshell parens", "(exit 1)"},
		{"background", "a &"},
		{"escaped quote", `a\"b`},
		{"single quote", "it's"},
		{"double quote", `say "hi"`},
		{"backslash", `a\b`},
		{"dollar paren paren", "$((1+1))"},
		{"process substitution", "<(echo hi)"},
		{"history bang", "!!"},
		{"hash comment", "a # not a comment"},
		{"env assignment prefix", "FOO=bar"},
		{"null adjacent high byte", "a\x01b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := witnessSpec(t, "", tt.arg)

			res, err := Run(spec)
			testsupport.Must(t, err, "Run: %v", err)
			if res.Exit != 0 {
				t.Fatalf("witness exited %d: %s", res.Exit, res.Output)
			}

			got := parseArgs(res.Output)
			if len(got) != 1 {
				t.Fatalf("the child must receive EXACTLY ONE argument; got %d: %q\noutput: %s", len(got), got, res.Output)
			}
			if got[0] != tt.arg {
				t.Errorf("the argument must arrive verbatim.\n got: %q\nwant: %q", got[0], tt.arg)
			}
		})
	}
}

func TestNoShellEverInvokedPreservesMultipleArguments(t *testing.T) {
	// The complement of the table above: several arguments stay several, with
	// their boundaries intact, including empty ones and ones containing
	// whitespace.
	args := []string{"first", "a b c", "", "$(whoami)", "last"}
	spec := witnessSpec(t, "", args...)

	res, err := Run(spec)
	testsupport.Must(t, err, "Run: %v", err)
	got := parseArgs(res.Output)
	if len(got) != len(args) {
		t.Fatalf("expected %d arguments, got %d: %q", len(args), len(got), got)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("argument %d: got %q, want %q", i, got[i], args[i])
		}
	}
}

// TestPackageReferencesNoShellBinary is §5.1's SOURCE GUARD.
//
// It is a grep-level check in the same spirit as the no-token-flag guard, and
// for the same reason: the rule is easy to state and easy to violate
// accidentally three refactors later. The structural guard — Spec has no
// Command string field — makes the unsafe thing unrepresentable; this guard
// catches an attempt to ADD it back.
//
// Comments are stripped before the scan, so the prose that explains the rule
// does not trip it.
func TestPackageReferencesNoShellBinary(t *testing.T) {
	// Assembled from pieces so this test's own source does not contain the
	// literals it forbids — otherwise the guard would report itself, and the
	// obvious "fix" would be to exclude test files, which is how the guard
	// stops covering the code that matters.
	interpreters := []string{
		"/bin/" + "sh",
		"/bin/ba" + "sh",
		"ba" + "sh",
		"z" + "sh",
		"cmd" + ".exe",
		"power" + "shell",
		"-c", // the flag every interpreter takes its script through
	}

	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading the package directory: %v", err)

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		code := codeWithoutComments(t, name)

		for _, bad := range interpreters {
			// Only string LITERALS matter: an identifier containing these
			// letters is not a command being invoked.
			if strings.Contains(code, `"`+bad+`"`) {
				t.Errorf("%s contains the literal %q — this package must never name a command interpreter", name, bad)
			}
		}

		// The structural half: no field or API through which a command STRING
		// could be supplied.
		for _, shape := range []string{"Command string", "Shell string", "Script string", "CommandLine string"} {
			if strings.Contains(code, shape) {
				t.Errorf("%s declares %q — the type must make a command string unrepresentable", name, shape)
			}
		}
	}
}

// codeWithoutComments is the exec-side twin of the trust package's helper. It
// exists twice rather than in a shared test package because these two packages
// must not gain a dependency on each other's test scaffolding.
func codeWithoutComments(t *testing.T, filename string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, parser.SkipObjectResolution)
	testsupport.Must(t, err, "parsing %s: %v", filename, err)
	file.Comments = nil
	var b strings.Builder
	if err := printer.Fprint(&b, fset, file); err != nil {
		t.Fatalf("printing %s: %v", filename, err)
	}
	return b.String()
}

// --- The environment allowlist (§5.3, T5) -----------------------------------

// TestGateChildEnvIsExactlyTheAllowlist asserts SET EQUALITY, not membership.
//
// A membership assertion passes for an environment that ALSO leaked forty other
// variables, which is precisely the failure an allowlist exists to prevent. The
// parent is seeded with fifty hostile variables first, so the test has
// something real to catch.
func TestGateChildEnvIsExactlyTheAllowlist(t *testing.T) {
	// Fifty hostile parent variables, including the ones §5.3 names explicitly.
	hostile := []string{
		"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",
		"GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN", "NPM_TOKEN", "PYPI_TOKEN",
		"DOCKER_PASSWORD", "SLACK_TOKEN", "STRIPE_SECRET_KEY", "TWILIO_AUTH_TOKEN",
		"DATABASE_URL", "DATABASE_PASSWORD", "REDIS_PASSWORD", "MYSQL_PWD",
		"PGPASSWORD", "SSH_AUTH_SOCK", "GPG_AGENT_INFO", "KUBECONFIG",
		"VAULT_TOKEN", "CONSUL_HTTP_TOKEN", "NOMAD_TOKEN", "TF_VAR_secret",
		"CIRCLE_TOKEN", "TRAVIS_TOKEN", "JENKINS_API_TOKEN", "BUILDKITE_AGENT_TOKEN",
		"SENTRY_AUTH_TOKEN", "DATADOG_API_KEY", "NEWRELIC_LICENSE_KEY",
		"CLOUDFLARE_API_TOKEN", "DIGITALOCEAN_TOKEN", "HEROKU_API_KEY",
		"AZURE_CLIENT_SECRET", "GOOGLE_APPLICATION_CREDENTIALS", "GCP_SA_KEY",
		"PRIVATE_KEY", "SIGNING_KEY", "ENCRYPTION_KEY", "API_KEY", "SECRET_KEY",
		"CLIENT_SECRET", "REFRESH_TOKEN", "ACCESS_TOKEN", "BEARER_TOKEN",
		"MY_PASSWORD", "ADMIN_PASSWORD", "ROOT_PASSWORD", "DOCKET_PATH",
	}
	if len(hostile) != 50 {
		t.Fatalf("the seed must be fifty variables, got %d", len(hostile))
	}
	for i, name := range hostile {
		t.Setenv(name, fmt.Sprintf("hostile-value-%d", i))
	}

	// Everything below needs temp directories, so they are taken BEFORE the
	// environment is rewritten — TMPDIR is itself on the allowlist, and
	// pointing it at a fake value would break t.TempDir() rather than test
	// anything.
	repo := t.TempDir()
	realTmp := t.TempDir()

	// Give every allowlisted variable a known value so the child should see
	// exactly them and nothing else. TMPDIR gets a REAL directory for the
	// reason above; the others get a marker value, since nothing dereferences
	// them.
	realPath := os.Getenv("PATH")
	for _, name := range AllowedEnvNames() {
		switch name {
		case "TMPDIR":
			t.Setenv(name, realTmp)
		case "PATH":
			// PATH keeps its real value: the witness is spawned by absolute
			// path, but a broken PATH would still break the child's own
			// startup on some platforms, and the test's subject is WHICH
			// variables arrive, not what they contain.
			t.Setenv(name, realPath)
		default:
			t.Setenv(name, "allowed-"+name)
		}
	}
	env, err := BuildEnv(EnvPolicy{Gate: "checks", Repo: repo})
	testsupport.Must(t, err, "BuildEnv: %v", err)
	env = append(env, "WITNESS_MODE=env")

	res, err := Run(Spec{Argv: []string{witness(t)}, Dir: repo, Env: env})
	testsupport.Must(t, err, "Run: %v", err)

	// What the child actually received.
	childVars := map[string]string{}
	for _, line := range strings.Split(res.Output, "\n") {
		if name, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && name != "" {
			childVars[name] = value
		}
	}

	// The expected set: the allowlist (those that are set in the parent), plus
	// the four docket sets, plus the test's own WITNESS_MODE.
	want := map[string]bool{
		"TERM": true, "CI": true, "DOCKET_GATE": true, "DOCKET_REPO": true,
		"WITNESS_MODE": true,
	}
	for _, name := range AllowedEnvNames() {
		if _, ok := os.LookupEnv(name); ok {
			want[name] = true
		}
	}

	// SET EQUALITY, both directions.
	for name := range childVars {
		if !want[name] {
			t.Errorf("THE CHILD RECEIVED %q, WHICH THE ALLOWLIST DOES NOT NAME — the environment is leaking", name)
		}
	}
	for name := range want {
		if _, ok := childVars[name]; !ok {
			t.Errorf("the child is missing %q, which the allowlist names", name)
		}
	}

	// And specifically: not one of the fifty hostile variables arrived.
	for _, name := range hostile {
		if _, leaked := childVars[name]; leaked {
			t.Errorf("hostile variable %q reached the child", name)
		}
	}

	// TERM is SET to dumb, not inherited: a check's output is captured, and an
	// inherited TERM makes tools emit ANSI escapes into it, polluting a run
	// report and a golden diff.
	if childVars["TERM"] != "dumb" {
		t.Errorf("TERM must be set to dumb, got %q", childVars["TERM"])
	}
	if childVars["CI"] != "1" {
		t.Errorf("CI must be set to 1, got %q", childVars["CI"])
	}
	if childVars["DOCKET_GATE"] != "checks" {
		t.Errorf("DOCKET_GATE must carry the gate name, got %q", childVars["DOCKET_GATE"])
	}
}

// TestDocketTokenNeverReachesAGateChild is T5's core case.
//
// A capability token in a child converts code execution into ENGINE AUTHORITY:
// the child could complete a step with a forged artifact under the live lease.
// Two mechanisms guard it — absence from the allowlist, and an explicit
// pre-spawn assertion — because one of them is a table a future editor might
// extend carelessly.
func TestDocketTokenNeverReachesAGateChild(t *testing.T) {
	const sentinel = "super-secret-capability-token-value"
	t.Setenv("DOCKET_TOKEN", sentinel)
	t.Setenv("DOCKET_PATH", "/some/repo/.docket")

	repo := t.TempDir()
	env, err := BuildEnv(EnvPolicy{Gate: "checks", Repo: repo})
	testsupport.Must(t, err, "BuildEnv: %v", err)
	env = append(env, "WITNESS_MODE=env")

	res, err := Run(Spec{Argv: []string{witness(t)}, Dir: repo, Env: env})
	testsupport.Must(t, err, "Run: %v", err)

	// The child's environment is dumped and grepped — for the NAME and for the
	// VALUE, since a leak under a different name is still a leak.
	if strings.Contains(res.Output, "DOCKET_TOKEN") {
		t.Error("DOCKET_TOKEN reached the child")
	}
	if strings.Contains(res.Output, sentinel) {
		t.Error("THE TOKEN VALUE REACHED THE CHILD under some name")
	}
	if strings.Contains(res.Output, "DOCKET_PATH") {
		t.Error("DOCKET_PATH reached the child; a check that reaches back into the store is outside what trust covers")
	}

	// THE PRE-SPAWN GUARD FIRES IF THE CONSTRUCTED SET IS TAMPERED WITH. This
	// is the belt-and-braces half: even a caller that built an Env by some
	// other route cannot spawn with a token in it.
	tampered := append(env, "DOCKET_TOKEN="+sentinel)
	_, err = Run(Spec{Argv: []string{witness(t)}, Dir: repo, Env: tampered})
	if err == nil {
		t.Fatal("a tampered environment containing DOCKET_TOKEN must refuse to spawn")
	}
	if !strings.Contains(err.Error(), "DOCKET_TOKEN") {
		t.Errorf("the refusal must name the offending variable; got: %v", err)
	}
	// The refusal must NOT echo the value: an error string lands in logs.
	if strings.Contains(err.Error(), sentinel) {
		t.Error("the refusal must not echo the token value into an error string")
	}

	// BuildEnv itself refuses too, so the guard is at both call sites.
	if err := assertNoDeniedVars([]string{"DOCKET_TOKEN=x"}); err == nil {
		t.Error("assertNoDeniedVars must reject DOCKET_TOKEN")
	}
}

func TestAllowlistAccessorsReturnCopies(t *testing.T) {
	// Handing out the package's own slice would let a caller EXTEND the
	// allowlist by appending to it — precisely the mechanism §5.3 refuses to
	// ship.
	got := AllowedEnvNames()
	if len(got) == 0 {
		t.Fatal("the allowlist must not be empty")
	}
	got[0] = "MUTATED"
	if AllowedEnvNames()[0] == "MUTATED" {
		t.Error("AllowedEnvNames must return a copy")
	}

	denied := DeniedEnvNames()
	denied[0] = "MUTATED"
	if DeniedEnvNames()[0] == "MUTATED" {
		t.Error("DeniedEnvNames must return a copy")
	}
}

// --- Timeout and the process-group kill (§5.4, T7) --------------------------

// TestTimeoutKillsTheWholeProcessGroup is T7, ASSERTED BY PID.
//
// Testing only the direct child would pass against an implementation that kills
// the pid rather than the group — the exact bug the rule exists to prevent. So
// the witness spawns a grandchild that OUTLIVES it, and the assertion is that
// the GRANDCHILD is gone.
func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	spec := witnessSpec(t, "spawn-grandchild")
	spec.Timeout = 2 * time.Second

	start := time.Now()
	res, err := Run(spec)
	testsupport.Must(t, err, "Run: %v", err)
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Fatalf("the run must report a timeout; output: %s", res.Output)
	}
	// X4: a timed-out command records a FAILURE, not unmatched — it was
	// trusted and it did run; it failed by exceeding its bound.
	if ExitZeroIsPass(res) {
		t.Error("a timed-out command must not count as a pass")
	}
	if res.Reason == "" || !strings.Contains(res.Reason, "timeout") {
		t.Errorf("the reason must name the timeout; got %q", res.Reason)
	}
	// The runner returns promptly rather than waiting out the child's own
	// ten-minute sleep.
	if elapsed > 30*time.Second {
		t.Errorf("the runner took %s; the timeout plus the kill grace should bound it", elapsed)
	}

	// Find the grandchild's pid from the captured output.
	pid := grandchildPID(t, res.Output)

	// Give the kill a moment to propagate through the group.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return // the grandchild is gone: the GROUP was killed
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Clean up before failing, so a bug does not leave a ten-minute sleeper.
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("THE GRANDCHILD (pid %d) SURVIVED THE TIMEOUT — the implementation killed the pid, not the process group", pid)
}

func grandchildPID(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GRANDCHILD "); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(rest))
			testsupport.Must(t, err, "parsing the grandchild pid from %q: %v", line, err)
			return pid
		}
	}
	t.Fatalf("the witness did not report a grandchild pid; output:\n%s", output)
	return 0
}

// processAlive reports whether a pid exists. Signal 0 performs the permission
// and existence checks without delivering anything, which is the portable way
// to ask.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestTimeoutDoesNotHangOnInheritedPipes is X3.
//
// The classic hang: a killed child's SURVIVING GRANDCHILD holds the capture
// pipe's write end open forever, so a runner that waits for EOF never returns.
// Here the direct child exits IMMEDIATELY while its grandchild keeps the pipe
// open — an implementation that waits on EOF hangs, and the test fails by
// timing out rather than by an assertion.
func TestTimeoutDoesNotHangOnInheritedPipes(t *testing.T) {
	spec := witnessSpec(t, "spawn-grandchild-then-exit")
	spec.Timeout = 3 * time.Second

	done := make(chan Result, 1)
	go func() {
		res, err := Run(spec)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- res
	}()

	select {
	case res := <-done:
		// The direct child exited 0; what matters is that Run RETURNED.
		_ = res
	case <-time.After(30 * time.Second):
		t.Fatal("RUN HUNG ON AN INHERITED PIPE — a surviving grandchild holds the write end, and the read loop must be bounded")
	}
}

func TestTimeoutIsBoundedByTheSpecAndDefaults(t *testing.T) {
	// X5: the spec's timeout is used when set. The default is a package
	// constant, asserted here so a change to it is a deliberate edit.
	if DefaultTimeout != 5*time.Minute {
		t.Errorf("DefaultTimeout = %s, want 5m", DefaultTimeout)
	}

	spec := witnessSpec(t, "sleep-forever")
	spec.Timeout = 1 * time.Second

	start := time.Now()
	res, err := Run(spec)
	testsupport.Must(t, err, "Run: %v", err)
	if !res.TimedOut {
		t.Fatal("the run must time out")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("a 1s timeout took %s to return", elapsed)
	}
	// The reason names the LIMIT, so an operator can tell "slow" from "hangs".
	if !strings.Contains(res.Reason, "1s") {
		t.Errorf("the reason must name the limit; got %q", res.Reason)
	}
}

// --- Capture (§5.5, T6) ------------------------------------------------------

// TestCaptureIsCappedAndFlagged is T6.
//
// A generator emitting far past the cap must not exhaust memory and must not be
// able to spin the reader: the writer STOPS CONSUMING at the cap rather than
// reading and discarding forever.
func TestCaptureIsCappedAndFlagged(t *testing.T) {
	spec := witnessSpec(t, "flood")
	spec.Timeout = 60 * time.Second

	res, err := Run(spec)
	testsupport.Must(t, err, "Run: %v", err)

	if !res.Truncated {
		t.Fatal("a capture past the cap must set truncated")
	}
	if len(res.Output) > CaptureCap {
		t.Errorf("the recorded output is %d bytes, which exceeds the %d-byte cap", len(res.Output), CaptureCap)
	}
	// C3/C4: exactly cap-sized, allowing for the rune-boundary back-off. The
	// witness emits ASCII, so the back-off should take nothing at all here.
	if len(res.Output) < CaptureCap-4 {
		t.Errorf("the recorded output is %d bytes; a truncated capture keeps the FIRST cap bytes (%d)", len(res.Output), CaptureCap)
	}
	// C3: the FIRST bytes are kept, not the last. The first bytes of a failing
	// check contain the invocation and the first error.
	if !strings.HasPrefix(res.Output, "xxx") {
		t.Errorf("a truncated capture must keep the first bytes; got a prefix of %q", firstN(res.Output, 40))
	}
}

func TestCaptureTruncationDoesNotSplitARune(t *testing.T) {
	// C4: truncation is backed off to the last valid rune boundary, so a
	// multi-byte character split by the cap does not become invalid UTF-8 —
	// which would, downstream, produce invalid JSON in a run report.
	w := &captureWriter{}
	// Fill to one byte short of the cap, then write a 3-byte rune so it
	// straddles the boundary.
	w.Write([]byte(strings.Repeat("a", CaptureCap-1)))
	w.Write([]byte("→")) // 3 bytes

	out, truncated := w.result()
	if !truncated {
		t.Fatal("the write past the cap must set truncated")
	}
	if !utf8ValidString(out) {
		t.Error("a truncated capture must remain valid UTF-8")
	}
	if strings.HasSuffix(out, "\xe2") || strings.HasSuffix(out, "\xe2\x86") {
		t.Error("the capture ends in a partial UTF-8 sequence")
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestCaptureInterleavesStdoutAndStderrInWriteOrder is C1.
//
// One interleaved stream in write order is what an operator reading a failed
// check wants: an error message and the line that produced it belong next to
// each other. §11.4 has a single output field, so this is the spec's shape.
func TestCaptureInterleavesStdoutAndStderrInWriteOrder(t *testing.T) {
	spec := witnessSpec(t, "interleave")

	res, err := Run(spec)
	testsupport.Must(t, err, "Run: %v", err)

	// All four lines are present...
	for _, want := range []string{"one", "two", "three", "four"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("the capture is missing %q; got:\n%s", want, res.Output)
		}
	}
	// ...and in write order, which is what "interleaved" means. The witness
	// syncs after each write, so the ordering is the kernel's and is stable.
	idx := func(s string) int { return strings.Index(res.Output, s) }
	if !(idx("one") < idx("two") && idx("two") < idx("three") && idx("three") < idx("four")) {
		t.Errorf("stdout and stderr must interleave in WRITE ORDER; got:\n%s", res.Output)
	}
}

// --- Exit codes and refusals -------------------------------------------------

func TestExitCodeIsReported(t *testing.T) {
	for _, code := range []int{0, 1, 2, 42} {
		t.Run(fmt.Sprintf("exit_%d", code), func(t *testing.T) {
			spec := witnessSpec(t, "exit")
			spec.Env = append(spec.Env, "WITNESS_EXIT="+strconv.Itoa(code))

			res, err := Run(spec)
			testsupport.Must(t, err, "Run: %v", err)
			if res.Exit != code {
				t.Errorf("Exit = %d, want %d", res.Exit, code)
			}
			if ExitZeroIsPass(res) != (code == 0) {
				t.Errorf("ExitZeroIsPass disagrees with exit %d", code)
			}
			// A real result carries a real duration and the argv that ran.
			if len(res.Argv) == 0 {
				t.Error("the result must carry the argv that was executed")
			}
		})
	}
}

func TestEmptyArgvIsRefused(t *testing.T) {
	_, err := Run(Spec{Argv: nil, Dir: t.TempDir(), Env: []string{}})
	if !errors.Is(err, ErrRefused) {
		t.Errorf("an empty argv must be refused with ErrRefused; got %v", err)
	}
}

func TestRunDoesNotInheritTheParentEnvironmentByDefault(t *testing.T) {
	// A nil Env must mean NO VARIABLES, never "inherit everything". os/exec's
	// default is the opposite, so this asserts the override is in place — the
	// distinction between a nil and an empty slice is the entire control.
	t.Setenv("A_PARENT_ONLY_VARIABLE", "leaked")

	res, err := Run(Spec{
		Argv: []string{witness(t)},
		Dir:  t.TempDir(),
		Env:  []string{"WITNESS_MODE=env"},
	})
	testsupport.Must(t, err, "Run: %v", err)
	if strings.Contains(res.Output, "A_PARENT_ONLY_VARIABLE") {
		t.Error("a Spec whose Env omits a variable must not inherit it from the parent")
	}
}

// --- Flaky re-runs (§5.6, F1–F5) ---------------------------------------------

func TestFlakyReRuns(t *testing.T) {
	countFile := func(t *testing.T) string {
		t.Helper()
		return filepath.Join(t.TempDir(), "count")
	}

	t.Run("F1 a non-flaky failing command runs ONCE", func(t *testing.T) {
		cf := countFile(t)
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env, "WITNESS_COUNT_FILE="+cf, "WITNESS_PASS_ON=0")

		attempts, err := RunAttempts(spec, false, ExitZeroIsPass)
		testsupport.Must(t, err, "RunAttempts: %v", err)
		if len(attempts) != 1 {
			t.Fatalf("a command not declared flaky must run exactly once; got %d attempts", len(attempts))
		}
		assertCount(t, cf, 1)
	})

	t.Run("F2 a flaky failing command runs three times", func(t *testing.T) {
		cf := countFile(t)
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env, "WITNESS_COUNT_FILE="+cf, "WITNESS_PASS_ON=0")

		attempts, err := RunAttempts(spec, true, ExitZeroIsPass)
		testsupport.Must(t, err, "RunAttempts: %v", err)
		if len(attempts) != MaxFlakyAttempts {
			t.Fatalf("a flaky failing command must run %d times; got %d", MaxFlakyAttempts, len(attempts))
		}
		assertCount(t, cf, MaxFlakyAttempts)

		// F3: EACH ATTEMPT IS ITS OWN RESULT, with an ASCENDING ordinal.
		// Nothing is overwritten and nothing is aggregated.
		for i, a := range attempts {
			if a.Ordinal != i {
				t.Errorf("attempt %d has ordinal %d; ordinals must ascend from 0", i, a.Ordinal)
			}
			if len(a.Result.Argv) == 0 {
				t.Errorf("attempt %d must carry its own argv", i)
			}
			if !strings.Contains(a.Result.Output, "ATTEMPT") {
				t.Errorf("attempt %d must carry its own output; got %q", i, a.Result.Output)
			}
		}

		// F4: the verdict for routing is the LAST attempt's.
		last, ok := Verdict(attempts)
		if !ok {
			t.Fatal("Verdict must return the last attempt")
		}
		if last.Ordinal != MaxFlakyAttempts-1 {
			t.Errorf("the routing verdict must be the last attempt; got ordinal %d", last.Ordinal)
		}
		if ExitZeroIsPass(last.Result) {
			t.Error("a command that failed every attempt must fail")
		}
	})

	t.Run("F2 a flaky command STOPS at the first pass", func(t *testing.T) {
		cf := countFile(t)
		spec := witnessSpec(t, "count")
		// Pass on attempt 2, so the third must never run.
		spec.Env = append(spec.Env, "WITNESS_COUNT_FILE="+cf, "WITNESS_PASS_ON=2")

		attempts, err := RunAttempts(spec, true, ExitZeroIsPass)
		testsupport.Must(t, err, "RunAttempts: %v", err)
		if len(attempts) != 2 {
			t.Fatalf("a flaky command passing on attempt 2 must stop there; got %d attempts", len(attempts))
		}
		assertCount(t, cf, 2)

		// F4: it passes, and the earlier failure remains recorded as evidence
		// that the check is flaky.
		last, _ := Verdict(attempts)
		if !ExitZeroIsPass(last.Result) {
			t.Error("the last attempt passed, so the gate passes")
		}
		if ExitZeroIsPass(attempts[0].Result) {
			t.Error("the first attempt failed and must stay recorded as a failure")
		}
	})

	t.Run("F1 a non-flaky PASSING command also runs once", func(t *testing.T) {
		cf := countFile(t)
		spec := witnessSpec(t, "count")
		spec.Env = append(spec.Env, "WITNESS_COUNT_FILE="+cf, "WITNESS_PASS_ON=1")

		attempts, err := RunAttempts(spec, false, ExitZeroIsPass)
		testsupport.Must(t, err, "RunAttempts: %v", err)
		if len(attempts) != 1 {
			t.Fatalf("expected 1 attempt, got %d", len(attempts))
		}
	})

	t.Run("Verdict on no attempts reports absence", func(t *testing.T) {
		if _, ok := Verdict(nil); ok {
			t.Error("Verdict must report false when there are no attempts")
		}
	})
}

func assertCount(t *testing.T, path string, want int) {
	t.Helper()
	b, err := os.ReadFile(path)
	testsupport.Must(t, err, "reading the attempt counter: %v", err)
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	testsupport.Must(t, err, "parsing the attempt counter %q: %v", b, err)
	if got != want {
		t.Errorf("THE COMMAND RAN %d TIMES, WANT %d", got, want)
	}
}
