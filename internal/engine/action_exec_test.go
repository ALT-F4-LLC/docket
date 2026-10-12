package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// A1–A8 of docs/tdd/payloads-thresholds.md §6.2: a non-builtin action is a
// USER-TRUSTED COMMAND, through §4's trust model with no exceptions.
//
// The claim these tests exist to make checkable is not "actions are safe" — it
// is "actions are safe FOR THE SAME REASON GATES ARE", which is a fact about
// the call graph. TestActionsUseTheSameTrustPath asserts it structurally; the
// rest drive the behavior through the real internal/exec.

// actionRunner builds a runner over a sandbox trust store rooted at repo.
//
// Every entry's binding is rewritten to the RESOLVED repo identity, because
// that is what the matcher compares against and the comparison is byte-exact by
// design (M6): a temp dir on macOS is `/var/...` while its identity is
// `/private/var/...`, and a test that stored the unresolved path would exercise
// the binding refusal rather than the behavior it means to.
func actionRunner(t *testing.T, repo string, entries ...trust.Entry) *ExecActionRunner {
	t.Helper()
	identity, err := trust.RepoIdentity(repo)
	testsupport.Must(t, err, "resolving the repo identity: %v", err)
	bound := make([]trust.Entry, len(entries))
	for i, e := range entries {
		if e.Repo != "" {
			e.Repo = identity
		}
		bound[i] = e
	}
	store := &trust.Store{Entries: bound}
	return &ExecActionRunner{
		RepoRoot:  repo,
		Identity:  repo,
		LockPath:  filepath.Join(repo, ".docket", "tree.lock"),
		LoadStore: func() (*trust.Store, error) { return store, nil },
	}
}

// actionSpec is the ordinary non-builtin spec these tests drive.
func actionSpec(name string) ActionSpec {
	return ActionSpec{Name: name, Output: "findings", Context: []byte(`{"step":{}}`)}
}

// echoScript writes an executable shell script OUTSIDE the repo — the
// containment rule (A5/R1-R5) refuses argv[0] resolved into it — and returns
// its path.
func echoScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	testsupport.Must(t, err, "writing %s: %v", name, err)
	return path
}

// TestActionsUseTheSameTrustPath is the structural claim, asserted by CALL
// GRAPH rather than by prose: the action runner reaches the process table
// through internal/exec and the trust store through internal/trust, and it
// declares no second path of its own.
//
// A second exec path is the failure this guards against, and it would not look
// like a bug: it would look like a small helper that "just runs the command",
// with its own env handling and its own argv resolution, quietly missing the
// containment check.
func TestActionsUseTheSameTrustPath(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "action_exec.go", nil, parser.SkipObjectResolution)
	testsupport.Must(t, err, "parsing action_exec.go: %v", err)
	file.Comments = nil

	// Every package the spawn path may name. `os/exec` is ABSENT on purpose:
	// the action runner must not construct a command itself.
	allowed := map[string]bool{
		"context": true, "encoding/json": true, "errors": true, "fmt": true,
		"path/filepath": true, "strings": true, "time": true,
		"github.com/ALT-F4-LLC/docket/internal/db":       true,
		"github.com/ALT-F4-LLC/docket/internal/exec":     true,
		"github.com/ALT-F4-LLC/docket/internal/trust":    true,
		"github.com/ALT-F4-LLC/docket/internal/workflow": true,
	}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !allowed[path] {
			t.Errorf("action_exec.go imports %q; the spawn goes through "+
				"internal/exec and the match through internal/trust, and a second "+
				"path is how the containment check comes to be skipped", path)
		}
	}

	// The specific calls that carry the security properties are PRESENT, so a
	// refactor that dropped one fails here rather than at a pen test.
	calls := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok {
			calls[ident.Name+"."+sel.Sel.Name] = true
		}
		return true
	})
	for _, want := range []string{
		"exec.Resolve",             // A5: argv[0] resolution and repo containment
		"exec.BuildEnv",            // A4: the env allowlist and the token exclusion
		"exec.RunVerifiedAttempts", // A6/A8 and the per-spawn pin re-check
		"exec.RunPinned",           // a pinned entry spawns the file it hashed
		"trust.RepoIdentity",
		"trust.ArgvSHA256",
	} {
		if !calls[want] {
			t.Errorf("action_exec.go does not call %s; the property it carries "+
				"is either gone or re-implemented", want)
		}
	}
}

// TestNoStubActionRunnerRemains is §6.3 S1: the stub is DELETED, with its
// constructor call.
//
// A source-level guard rather than a comment, in the family of gates-trust
// §5.1's: the stub's whole purpose was to make the S3→S5 window auditable, and
// a stub that survived past the stage that replaced it would let a green run
// read as computation coverage it does not have.
func TestNoStubActionRunnerRemains(t *testing.T) {
	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading the package directory: %v", err)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		testsupport.Must(t, err, "reading %s: %v", name, err)

		// The identifier, assembled rather than written whole so this guard's
		// own source does not trip it.
		if strings.Contains(string(src), "Stub"+"Runner") {
			t.Errorf("%s still names the stub action runner; S1 deletes it with "+
				"its constructor call", name)
		}
	}
}

// TestUnmatchedActionRoutesPerOnFail is A3, with a WITNESS proving nothing
// spawned.
//
// The witness is the point. "No entry ⇒ unmatched" is easy to implement as "run
// it anyway and record unmatched", and the two are indistinguishable from the
// database. A file the command would have created, and did not, is the
// difference.
func TestUnmatchedActionRoutesPerOnFail(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	witness := filepath.Join(outside, "it-ran")
	script := echoScript(t, outside, "reconcile.sh",
		fmt.Sprintf("touch %q; echo '{\"payload\":[]}'", witness))

	// A store with an entry for a DIFFERENT action name: the lookup must fail
	// on the name, not on the store being empty.
	runner := actionRunner(t, repo, trust.Entry{
		Name: "some-other-action", Argv: []string{script}, Repo: repo,
	})

	result, err := runner.Run(context.Background(),
		actionSpec("reconcile-cmd"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)

	if !result.Failed {
		t.Error("an unmatched action did not fail the step; a computation that " +
			"could not run has not succeeded (A3, §6.2 N3)")
	}
	if len(result.Results) != 1 ||
		result.Results[0].Verdict != db.ActionVerdictUnmatched {
		t.Fatalf("results = %+v, want one `unmatched` row", result.Results)
	}
	if result.Results[0].Argv != nil || result.Results[0].Exit != nil {
		t.Error("the unmatched row carries an argv or an exit; NULL is the " +
			"honest encoding of `no process existed`")
	}
	if result.Results[0].Reason == "" {
		t.Error("the unmatched row records no reason; four causes need four remedies")
	}
	if _, err := os.Stat(witness); err == nil {
		t.Fatal("THE COMMAND RAN. An unmatched action must not touch the " +
			"process table at all.")
	}
}

// TestActionStdinCarriesTheStepContext is §6.2's stdin half: the §11.4 context
// object, one JSON document, then EOF.
func TestActionStdinCarriesTheStepContext(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	captured := filepath.Join(outside, "stdin.json")
	script := echoScript(t, outside, "reader.sh",
		fmt.Sprintf("cat > %q; echo '{\"body\":\"ok\",\"payload\":[{\"n\":1}]}'", captured))

	runner := actionRunner(t, repo, trust.Entry{
		Name: "reader", Argv: []string{script}, Repo: repo,
	})

	spec := actionSpec("reader")
	spec.Context = []byte(`{"step":{"instance":"reconcile@0"},"inputs":[]}`)

	result, err := runner.Run(context.Background(), spec,
		StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if result.Failed {
		t.Fatalf("the action failed: %s", result.Reason)
	}

	got, err := os.ReadFile(captured)
	testsupport.Must(t, err, "the command captured no stdin: %v", err)
	if string(got) != string(spec.Context) {
		t.Errorf("stdin = %q, want the context bundle verbatim %q", got, spec.Context)
	}

	// The stdout half: `{body, payload}`, both reaching the artifact.
	if result.Body != "ok" {
		t.Errorf("body = %q, want the command's own", result.Body)
	}
	if result.Payload != `[{"n":1}]` {
		t.Errorf("payload = %q, want the command's own", result.Payload)
	}
	if len(result.Results) != 1 || result.Results[0].Verdict != db.ActionVerdictPass {
		t.Errorf("results = %+v, want one passing row", result.Results)
	}
	if result.Results[0].Builtin {
		t.Error("a trusted command's result is marked builtin")
	}
}

// TestActionReplyDefaults pins the contract's defaults and its refusals.
func TestActionReplyDefaults(t *testing.T) {
	cases := []struct {
		name        string
		stdout      string
		wantBody    string
		wantPayload string
		wantErr     string
	}{
		{name: "both halves", stdout: `{"body":"b","payload":[{"x":1}]}`,
			wantBody: "b", wantPayload: `[{"x":1}]`},
		{name: "body defaults to empty", stdout: `{"payload":[]}`,
			wantBody: "", wantPayload: "[]"},
		{name: "payload defaults to an empty array", stdout: `{"body":"b"}`,
			wantBody: "b", wantPayload: "[]"},
		{name: "an empty object is both defaults", stdout: `{}`,
			wantBody: "", wantPayload: "[]"},
		{name: "whitespace and a trailing newline are fine",
			stdout: "  {\"body\":\"b\"}\n", wantBody: "b", wantPayload: "[]"},

		{name: "not JSON at all", stdout: "boom", wantErr: "object the contract requires"},
		{name: "a bare array", stdout: `[{"x":1}]`, wantErr: "object the contract requires"},
		{name: "a payload that is not an array of objects",
			stdout: `{"payload":[1,2]}`, wantErr: "array of objects"},
		{name: "a key the contract does not name",
			stdout: `{"body":"b","verdict":"pass"}`, wantErr: "object the contract requires"},
		{name: "two documents", stdout: `{"body":"a"} {"body":"b"}`,
			wantErr: "ONE JSON document"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := parseActionReply(tc.stdout)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("accepted %q", tc.stdout)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("the refusal does not say %q:\n%v", tc.wantErr, err)
				}
				return
			}
			testsupport.Must(t, err, "parseActionReply(%q): %v", tc.stdout, err)
			if reply.Body != tc.wantBody || reply.Payload != tc.wantPayload {
				t.Errorf("reply = %+v, want body %q payload %q",
					reply, tc.wantBody, tc.wantPayload)
			}
		})
	}
}

// TestUnparseableStdoutIsRenderedThroughTheEscaper is §6.2's last row: this
// output is ATTACKER-INFLUENCED and reaches a terminal, so it is quoted through
// gates-trust §5.7's renderer rather than printed raw.
//
// A raw quote here would let a command's stdout move an operator's cursor,
// clear their screen, or hide the rest of the reason — which is exactly what
// the renderer exists to prevent for gate output.
func TestUnparseableStdoutIsRenderedThroughTheEscaper(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	// A CR and an ANSI escape, plus enough bytes to exercise the 200-byte cap.
	script := echoScript(t, outside, "garbage.sh",
		`printf 'not json\r\033[2Jgone'; printf 'x%.0s' $(seq 1 400)`)

	runner := actionRunner(t, repo, trust.Entry{
		Name: "garbage", Argv: []string{script}, Repo: repo,
	})

	result, err := runner.Run(context.Background(),
		actionSpec("garbage"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if !result.Failed {
		t.Fatal("unparseable stdout on exit 0 did not fail the step")
	}
	for _, raw := range []string{"\r", "\x1b"} {
		if strings.Contains(result.Reason, raw) {
			t.Errorf("the reason carries a raw control byte %q; it must go "+
				"through the renderer:\n%q", raw, result.Reason)
		}
	}
	if !strings.Contains(result.Reason, `\r`) {
		t.Errorf("the reason does not show the escaped control byte:\n%s", result.Reason)
	}
	// The quote is capped, so a command cannot fill a terminal by printing.
	if len(result.Reason) > 600 {
		t.Errorf("the quoted output is %d bytes; §6.2 caps it at 200",
			len(result.Reason))
	}
}

// TestActionNonZeroExitRoutesPerOnFail is the non-zero-exit row: the step
// fails, the output is recorded, and NO ARTIFACT is written.
func TestActionNonZeroExitRoutesPerOnFail(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	script := echoScript(t, outside, "failing.sh", "echo 'went wrong' >&2; exit 7")

	runner := actionRunner(t, repo, trust.Entry{
		Name: "failing", Argv: []string{script}, Repo: repo,
	})

	result, err := runner.Run(context.Background(),
		actionSpec("failing"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if !result.Failed {
		t.Fatal("a non-zero exit did not fail the step")
	}
	if result.Payload != "" {
		t.Errorf("a failed action produced a payload %q; nothing is recorded "+
			"for a computation that did not succeed", result.Payload)
	}
	if len(result.Results) != 1 {
		t.Fatalf("results = %+v, want one row", result.Results)
	}
	row := result.Results[0]
	if row.Exit == nil || *row.Exit != 7 {
		t.Errorf("exit = %v, want 7", row.Exit)
	}
	if !strings.Contains(row.Output, "went wrong") {
		t.Errorf("the captured output does not carry the command's stderr: %q", row.Output)
	}
	if row.Verdict != db.ActionVerdictFail {
		t.Errorf("verdict = %q, want %q", row.Verdict, db.ActionVerdictFail)
	}
}

// TestActionPayloadIsValidatedAgainstTheDeclaredSchema is §6.2's last clause:
// a non-builtin action's payload is validated exactly as a worker's is, and a
// failure is a STEP failure routed per `on_fail` — not a refusal to a caller,
// because there is no caller; the engine is the caller.
func TestActionPayloadIsValidatedAgainstTheDeclaredSchema(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	script := echoScript(t, outside, "bad-payload.sh",
		`echo '{"payload":[{"severity":"urgent"}]}'`)

	runner := actionRunner(t, repo, trust.Entry{
		Name: "producer", Argv: []string{script}, Repo: repo,
	})

	spec := actionSpec("producer")
	spec.Validate = func(payload []byte) error {
		var elements []map[string]any
		if err := json.Unmarshal(payload, &elements); err != nil {
			return err
		}
		for _, e := range elements {
			if e["severity"] == "urgent" {
				return fmt.Errorf(`payload[0].severity: value "urgent" is not declared`)
			}
		}
		return nil
	}

	result, err := runner.Run(context.Background(), spec,
		StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run should not error; a validation failure is a STEP failure: %v", err)
	if !result.Failed {
		t.Fatal("an invalid payload did not fail the step")
	}
	if !strings.Contains(result.Reason, "declared schema") {
		t.Errorf("the reason does not name the schema:\n%s", result.Reason)
	}
}

// TestDocketTokenNeverReachesAnActionChild is A4's assertion rather than its
// assumption.
//
// The token has already retired by the routing stage, so the exclusion is
// belt-and-braces here — which is exactly why it is asserted: a control that is
// only correct because of a property somewhere else is a control one refactor
// away from being wrong.
func TestDocketTokenNeverReachesAnActionChild(t *testing.T) {
	t.Setenv("DOCKET_TOKEN", "a-live-capability-token")

	repo := t.TempDir()
	outside := t.TempDir()
	captured := filepath.Join(outside, "env.txt")
	script := echoScript(t, outside, "env.sh",
		fmt.Sprintf("env > %q; echo '{\"payload\":[]}'", captured))

	runner := actionRunner(t, repo, trust.Entry{
		Name: "env-dump", Argv: []string{script}, Repo: repo,
	})

	result, err := runner.Run(context.Background(),
		actionSpec("env-dump"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if result.Failed {
		t.Fatalf("the action failed: %s", result.Reason)
	}

	env, err := os.ReadFile(captured)
	testsupport.Must(t, err, "the command captured no environment: %v", err)
	if strings.Contains(string(env), "a-live-capability-token") {
		t.Fatal("DOCKET_TOKEN REACHED AN ACTION CHILD. Code execution in a child " +
			"with the token is engine authority: it could complete a step with a " +
			"forged artifact under the live lease.")
	}
	if !strings.Contains(string(env), "DOCKET_REPO=") {
		t.Error("DOCKET_REPO did not reach the child; the allowlist's own " +
			"additions must still arrive")
	}
}

// TestFlakyActionRecordsEveryAttempt is A8: `flaky` re-runs apply and are
// recorded INDIVIDUALLY with ascending ordinal.
//
// The pass predicate for an action is exit-zero AND a parseable reply, which is
// what makes `flaky` mean something here: a gate's whole answer is its exit
// code, but an action that exits 0 having printed nothing usable has not
// answered.
func TestFlakyActionRecordsEveryAttempt(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	counter := filepath.Join(outside, "attempts")
	script := echoScript(t, outside, "flaky.sh", fmt.Sprintf(`
echo x >> %q
n=$(wc -l < %q | tr -d ' ')
if [ "$n" -lt 2 ]; then exit 1; fi
echo '{"body":"eventually","payload":[]}'
`, counter, counter))

	runner := actionRunner(t, repo, trust.Entry{
		Name: "flaky-action", Argv: []string{script}, Repo: repo,
		Flaky: true,
	})

	result, err := runner.Run(context.Background(),
		actionSpec("flaky-action"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if result.Failed {
		t.Fatalf("a flaky action that eventually passed still failed: %s", result.Reason)
	}
	if len(result.Results) < 2 {
		t.Fatalf("recorded %d attempts, want each one individually: %+v",
			len(result.Results), result.Results)
	}
	for i, row := range result.Results {
		if row.Ordinal != i {
			t.Errorf("attempt %d has ordinal %d; ordinals ascend per (step, action)",
				i, row.Ordinal)
		}
	}
	first, last := result.Results[0], result.Results[len(result.Results)-1]
	if first.Verdict != db.ActionVerdictFail {
		t.Errorf("the first attempt is %q, want a recorded failure", first.Verdict)
	}
	if last.Verdict != db.ActionVerdictPass {
		t.Errorf("the last attempt is %q, want the pass that routes", last.Verdict)
	}
	if result.Body != "eventually" {
		t.Errorf("body = %q, want the last attempt's reply", result.Body)
	}
}

// TestTreeActionSerializesLikeATreeGate is A7: an entry declaring `tree = true`
// takes the SAME per-repo flock a tree gate takes.
//
// A computation that reads the working tree races a build exactly as a gate
// does, so the lock is the same lock — not a second one that would serialize
// actions against actions while a gate ran underneath them. The proof is that
// holding the GATE lock stops the ACTION: a second lock would not.
func TestTreeActionSerializesLikeATreeGate(t *testing.T) {
	repo := t.TempDir()
	docket := filepath.Join(repo, ".docket")
	err := os.MkdirAll(docket, 0o755)
	testsupport.Must(t, err, "creating .docket: %v", err)
	outside := t.TempDir()
	witness := filepath.Join(outside, "it-ran")
	script := echoScript(t, outside, "tree.sh",
		fmt.Sprintf("touch %q; echo '{\"payload\":[]}'", witness))

	entry := trust.Entry{
		Name: "tree-action", Argv: []string{script}, Repo: repo, Tree: true,
	}

	// The blocked half runs with a 10ms entry timeout, which bounds the LOCK
	// WAIT: the assertion is about the refusal, not about how long a test is
	// willing to sit. The free half runs with the default, so the second
	// assertion is not racing its own bound.
	blocked := actionRunner(t, repo, withTimeout(entry, "10ms"))
	blocked.LockPath = filepath.Join(docket, "tree.lock")
	free := actionRunner(t, repo, entry)
	free.LockPath = filepath.Join(docket, "tree.lock")

	held, err := acquireTreeLock(filepath.Join(docket, "tree.lock"), 0)
	testsupport.Must(t, err, "taking the tree lock: %v", err)

	result, err := blocked.Run(context.Background(),
		actionSpec("tree-action"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if !result.Failed {
		t.Error("a tree action ran while the tree lock was held; the " +
			"serialization it declared was not provided")
	}
	if _, statErr := os.Stat(witness); statErr == nil {
		t.Fatal("THE COMMAND RAN unserialized. L4/L7: a computation whose " +
			"declared serialization cannot be provided fails rather than racing.")
	}
	held.release()

	// And with the lock free it runs, so the refusal above was the lock and not
	// something else about the entry.
	result, err = free.Run(context.Background(),
		actionSpec("tree-action"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run after release: %v", err)
	if result.Failed {
		t.Errorf("the action failed with the lock free: %s", result.Reason)
	}
	if _, statErr := os.Stat(witness); statErr != nil {
		t.Error("the command did not run with the lock free; the first " +
			"assertion proves nothing")
	}
}

func withTimeout(e trust.Entry, d string) trust.Entry {
	e.Timeout = d
	return e
}

// TestActionTreeLockTimeoutReasonNamesTheTimeout: when a tree action's lock
// wait exceeds its bound, the recorded reason names the timeout rather than
// carrying the bare lock error, so a reader can tell "the lock wait timed out"
// from any other action failure. No process spawned and no tree was read.
func TestActionTreeLockTimeoutReasonNamesTheTimeout(t *testing.T) {
	repo := t.TempDir()
	docket := filepath.Join(repo, ".docket")
	err := os.MkdirAll(docket, 0o755)
	testsupport.Must(t, err, "creating .docket: %v", err)
	outside := t.TempDir()
	witness := filepath.Join(outside, "it-ran")
	script := echoScript(t, outside, "tree.sh",
		fmt.Sprintf("touch %q; echo '{\"payload\":[]}'", witness))

	entry := trust.Entry{
		Name: "tree-action", Argv: []string{script}, Repo: repo, Tree: true,
	}

	blocked := actionRunner(t, repo, withTimeout(entry, "10ms"))
	blocked.LockPath = filepath.Join(docket, "tree.lock")

	held, err := acquireTreeLock(filepath.Join(docket, "tree.lock"), 0)
	testsupport.Must(t, err, "taking the tree lock: %v", err)
	defer held.release()

	result, err := blocked.Run(context.Background(),
		actionSpec("tree-action"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if !result.Failed {
		t.Fatal("a tree action ran while the tree lock was held")
	}
	if len(result.Results) == 0 {
		t.Fatal("no result row recorded for the timed-out action")
	}
	row := result.Results[0]
	if !strings.HasPrefix(row.Reason, "tree lock not acquired: ") {
		t.Errorf("reason = %q, want prefix %q", row.Reason, "tree lock not acquired: ")
	}
	if row.Verdict != db.ActionVerdictFail {
		t.Errorf("verdict = %q, want %q", row.Verdict, db.ActionVerdictFail)
	}
	if _, statErr := os.Stat(witness); statErr == nil {
		t.Fatal("THE COMMAND RAN unserialized despite the lock wait timing out")
	}
}

// TestActionRunnerResolvesBuiltinFirst is B1: an action core computes is never
// looked up in the trust store, so an entry cannot shadow a builtin and
// removing one cannot disable it.
func TestActionRunnerResolvesBuiltinFirst(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	witness := filepath.Join(outside, "shadowed")
	script := echoScript(t, outside, "shadow.sh",
		fmt.Sprintf("touch %q; echo '{\"payload\":[]}'", witness))

	// An entry named exactly like the builtin. V27 reserves the name at
	// register time; this proves the run-time half.
	runner := actionRunner(t, repo, trust.Entry{
		Name: workflow.ActionAggregate, Argv: []string{script}, Repo: repo,
	})

	result, err := runner.Run(context.Background(), ActionSpec{
		Name: workflow.ActionAggregate, Output: "findings",
		Params: map[string]any{
			"field": "severity", "method": "max", "output": "findings",
		},
		Inputs: []map[string]any{{"severity": []any{"low", "high"}}},
		Order:  severityOrder,
	}, StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if result.Failed {
		t.Fatalf("the builtin failed: %s", result.Reason)
	}
	if _, err := os.Stat(witness); err == nil {
		t.Fatal("A TRUST ENTRY SHADOWED THE BUILTIN. Resolution is builtin-first " +
			"(B1), which is why V27 reserves the name rather than letting an " +
			"operator wonder why their command never ran.")
	}
	if !strings.Contains(result.Payload, `"high"`) {
		t.Errorf("the builtin did not compute: %s", result.Payload)
	}
}

// TestActionResultCarriesNoStubMarker is S1/S2's other half: nothing this
// runner produces sets the retired marker, on any path.
func TestActionResultCarriesNoStubMarker(t *testing.T) {
	repo := t.TempDir()
	runner := actionRunner(t, repo)

	result, err := runner.Run(context.Background(),
		actionSpec("no-such-action"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)
	if strings.Contains(result.Payload, "stub") {
		t.Errorf("payload = %q; the wrapper is retired", result.Payload)
	}
}

// TestWrappedStubPayloadsStillRead is §6.3 S3: `parsePayload` keeps ONE
// documented tolerance, and it exists to read HISTORY.
//
// The migration deliberately does not rewrite the S3/S4 wrapper — doing so
// would destroy the evidence that a computation did not run — so a v8-era
// artifact must still be readable by a threshold today.
func TestWrappedStubPayloadsStillRead(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"a v8-era wrapped payload unwraps",
			`{"stub":true,"payload":[{"severity":"low"},{"severity":"high"}]}`, 2},
		{"a wrapped EMPTY payload unwraps to nothing",
			`{"stub":true,"payload":[]}`, 0},
		{"a bare array is used as is", `[{"severity":"low"}]`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePayload([]byte(tc.raw))
			testsupport.Must(t, err, "parsePayload(%s): %v", tc.raw, err)
			if len(got) != tc.want {
				t.Errorf("read %d elements, want %d: %+v", len(got), tc.want, got)
			}
		})
	}

	// The tolerance is EXACTLY those two keys. A wider one would start
	// unwrapping objects a worker meant as data.
	for _, raw := range []string{
		`{"stub":true}`,
		`{"payload":[{"x":1}]}`,
		`{"stub":true,"payload":[],"extra":1}`,
		`{"severity":"low"}`,
	} {
		if _, err := parsePayload([]byte(raw)); err == nil {
			t.Errorf("parsePayload(%s) succeeded; the tolerance is exactly "+
				"{stub, payload}", raw)
		}
	}
}

// TestStubIsNotWrittenAtV9 is S2 and S4 at the storage layer: a result this
// stage records leaves `artifacts.stub` at 0 and carries no `stub` key.
func TestStubIsNotWrittenAtV9(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	id, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: run.ID, Kind: "findings", Body: "b",
		Payload: `[{"severity":"low"}]`, SHA256: "x",
	}, 1000)
	testsupport.Must(t, err, "InsertArtifactTx: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	var stub int
	err = conn.QueryRow(`SELECT stub FROM artifacts WHERE id = ?`, id).
		Scan(&stub)
	testsupport.Must(t, err, "reading the artifact: %v", err)
	if stub != 0 {
		t.Error("a v9-written artifact carries stub = 1")
	}

	// S4: `omitempty`, so the key is absent from the serialized form entirely.
	encoded, err := json.Marshal(db.Artifact{Kind: "findings"})
	testsupport.Must(t, err, "marshalling: %v", err)
	if strings.Contains(string(encoded), "stub") {
		t.Errorf("an unstubbed artifact serializes with a `stub` key: %s", encoded)
	}
}

// actionReplyLine is the minimal well-formed reply, so a pinned script that
// runs passes and a test's red is "the script ran", never a parse failure.
const actionReplyLine = `echo '{"body":"","payload":[]}'` + "\n"

// pinnedActionEntry trusts script as action "pinned-action", pinned by sum.
func pinnedActionEntry(repo, script, sum string) trust.Entry {
	return trust.Entry{
		Name: "pinned-action", Argv: []string{script}, Argv0SHA256: sum, Repo: repo,
	}
}

// runTreeActionAcrossHeldLock runs a pinned tree action while the test holds
// the tree lock, calls during() once the action is blocked on it, then
// releases the lock and returns what the action recorded.
func runTreeActionAcrossHeldLock(
	t *testing.T, repo string, entry trust.Entry, during func(),
) ActionResult {
	t.Helper()
	entry.Tree = true
	err := os.MkdirAll(filepath.Join(repo, ".docket"), 0o755)
	testsupport.Must(t, err, "creating .docket: %v", err)
	runner := actionRunner(t, repo, entry)

	lock, err := acquireTreeLock(runner.LockPath, time.Minute)
	testsupport.Must(t, err, "holding the tree lock: %v", err)

	type outcome struct {
		result ActionResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runner.Run(context.Background(),
			actionSpec(entry.Name), StepContext{Instance: "reconcile@0"})
		done <- outcome{result, err}
	}()

	waitBlockedOnTreeLock(t)
	during()
	lock.release()

	got := <-done
	testsupport.Must(t, got.err, "running the tree action: %v", got.err)
	return got.result
}

// TestActionPinRecheckAfterTreeLock: the pinned script is rewritten while the
// action waits for the tree lock, after its Lookup already matched. The runner
// re-verifies the pin once the lock is held and spawns nothing.
func TestActionPinRecheckAfterTreeLock(t *testing.T) {
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script, pinned := pinnedScript(t, "/usr/bin/touch "+marker+"\n"+actionReplyLine)

	result := runTreeActionAcrossHeldLock(t, repo, pinnedActionEntry(repo, script, pinned),
		func() { rewritePinByte(t, script) })

	if sentinelExists(t, marker) {
		t.Error("the rewritten script RAN after the tree lock was released")
	}
	if !result.Failed {
		t.Error("the action did not fail after its pinned script changed")
	}
	if len(result.Results) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(result.Results), result.Results)
	}
	row := result.Results[0]
	if row.Verdict != db.ActionVerdictUnmatched {
		t.Errorf("row verdict = %q, want %q", row.Verdict, db.ActionVerdictUnmatched)
	}
	if row.Exit != nil {
		t.Errorf("exit = %d on an action that spawned nothing, want nil", *row.Exit)
	}
	// A Lookup-time refusal carries no entry; this row must come from the
	// re-check that follows the lock, or the test lost its race.
	if row.TrustEntry != "pinned-action" {
		t.Errorf("the row names entry %q, want %q: the refusal did not come "+
			"from the post-lock re-check", row.TrustEntry, "pinned-action")
	}
	content, err := os.ReadFile(script)
	testsupport.Must(t, err, "reading the rewritten script: %v", err)
	resolved, err := filepath.EvalSymlinks(script)
	testsupport.Must(t, err, "resolving the script path: %v", err)
	for _, want := range []string{resolved, pinned, trustHashOf(string(content))} {
		if !strings.Contains(row.Reason, want) {
			t.Errorf("the reason does not name %s: %q", want, row.Reason)
		}
	}
}

// TestPinnedFlakyActionNeverRerunsRewrittenScript: a flaky pinned script whose
// first attempt rewrites one byte of itself and fails. A content-pinned entry
// spawns once through exec.RunPinned, so the failure routes and the rewritten
// script never runs.
func TestPinnedFlakyActionNeverRerunsRewrittenScript(t *testing.T) {
	repo := t.TempDir()
	markers := t.TempDir()
	first := filepath.Join(markers, "marker-1")
	second := filepath.Join(markers, "marker-2")
	script := filepath.Join(t.TempDir(), "action.sh")
	body := fmt.Sprintf(
		"if [ -e %[1]s ]; then /usr/bin/touch %[2]s; %[5]s exit 0; fi\n"+
			"/usr/bin/touch %[1]s\n"+
			"printf B | /bin/dd of=%[3]s bs=1 seek=%[4]d conv=notrunc 2>/dev/null\n"+
			"exit 1\n",
		first, second, script, len(pinScriptHeader),
		`echo '{"body":"","payload":[]}';`)
	content := pinScriptHeader + "A\n" + body
	writeErr := os.WriteFile(script, []byte(content), 0o755)
	testsupport.Must(t, writeErr, "writing the self-rewriting script: %v", writeErr)

	entry := pinnedActionEntry(repo, script, trustHashOf(content))
	entry.Flaky = true
	runner := actionRunner(t, repo, entry)

	result, err := runner.Run(context.Background(),
		actionSpec("pinned-action"), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)

	if !sentinelExists(t, first) {
		t.Fatal("the first attempt did not run")
	}
	if sentinelExists(t, second) {
		t.Error("the re-attempt RAN the rewritten script")
	}
	if !result.Failed {
		t.Error("the action did not fail after its pinned script changed")
	}
	if len(result.Results) != 1 {
		t.Fatalf("got %d rows, want 1 (the failed run): %+v",
			len(result.Results), result.Results)
	}
	if r := result.Results[0]; r.Ordinal != 0 || r.Verdict != db.ActionVerdictFail {
		t.Errorf("row 0 = ordinal %d verdict %q, want 0 and %q",
			r.Ordinal, r.Verdict, db.ActionVerdictFail)
	}
}

// TestActionPinRecheckUnchangedScriptRuns is the benign half: a pinned tree
// action whose script is untouched across a contended lock still runs.
func TestActionPinRecheckUnchangedScriptRuns(t *testing.T) {
	repo := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script, pinned := pinnedScript(t, "/usr/bin/touch "+marker+"\n"+actionReplyLine)

	result := runTreeActionAcrossHeldLock(t, repo,
		pinnedActionEntry(repo, script, pinned), func() {})

	if !sentinelExists(t, marker) {
		t.Errorf("the unchanged pinned script did not run: %+v", result.Results)
	}
	if result.Failed {
		t.Errorf("the unchanged pinned action failed: %s", result.Reason)
	}
	if len(result.Results) != 1 || result.Results[0].Verdict != db.ActionVerdictPass {
		t.Errorf("rows = %+v, want one %q row", result.Results, db.ActionVerdictPass)
	}
}

// TestActionPinRecheckRefusesSymlinkRetarget: the entry trusts link/action.sh
// with link -> trusted. While the action waits for the tree lock, a writer
// copies the pinned bytes to decoy/action.sh, rewrites trusted/action.sh, and
// repoints link -> decoy. The entry path now hashes clean, but the runner
// would spawn trusted/action.sh, so a pinned entry is refused. An unpinned
// entry carries no pin to protect and still runs.
func TestActionPinRecheckRefusesSymlinkRetarget(t *testing.T) {
	type layout struct {
		script, trustedPath, decoyPath, pinnedMarker, rewrittenMarker string
		pinnedContent                                                 string
		retarget                                                      func()
	}
	build := func(t *testing.T) layout {
		t.Helper()
		dir, markers := t.TempDir(), t.TempDir()
		trustedDir := filepath.Join(dir, "trusted")
		decoyDir := filepath.Join(dir, "decoy")
		link := filepath.Join(dir, "link")
		for _, d := range []string{trustedDir, decoyDir} {
			err := os.Mkdir(d, 0o755)
			testsupport.Must(t, err, "creating %s: %v", d, err)
		}
		err := os.Symlink(trustedDir, link)
		testsupport.Must(t, err, "linking %s: %v", link, err)

		l := layout{
			script:          filepath.Join(link, "action.sh"),
			trustedPath:     filepath.Join(trustedDir, "action.sh"),
			decoyPath:       filepath.Join(decoyDir, "action.sh"),
			pinnedMarker:    filepath.Join(markers, "pinned-ran"),
			rewrittenMarker: filepath.Join(markers, "rewritten-ran"),
		}
		l.pinnedContent = pinScriptHeader + "A\n/usr/bin/touch " + l.pinnedMarker + "\n" + actionReplyLine
		rewritten := pinScriptHeader + "A\n/usr/bin/touch " + l.rewrittenMarker + "\n" + actionReplyLine
		err = os.WriteFile(l.trustedPath, []byte(l.pinnedContent), 0o755)
		testsupport.Must(t, err, "writing the trusted script: %v", err)
		l.retarget = func() {
			err := os.WriteFile(l.decoyPath, []byte(l.pinnedContent), 0o755)
			testsupport.Must(t, err, "writing the decoy: %v", err)
			err = os.WriteFile(l.trustedPath, []byte(rewritten), 0o755)
			testsupport.Must(t, err, "rewriting the trusted script: %v", err)
			err = os.Remove(link)
			testsupport.Must(t, err, "removing %s: %v", link, err)
			err = os.Symlink(decoyDir, link)
			testsupport.Must(t, err, "repointing %s: %v", link, err)
		}
		return l
	}

	t.Run("pinned entry is refused", func(t *testing.T) {
		repo := t.TempDir()
		l := build(t)
		resolvedBefore, err := filepath.EvalSymlinks(l.trustedPath)
		testsupport.Must(t, err, "resolving the trusted script: %v", err)

		result := runTreeActionAcrossHeldLock(t, repo,
			pinnedActionEntry(repo, l.script, trustHashOf(l.pinnedContent)), l.retarget)

		if sentinelExists(t, l.rewrittenMarker) {
			t.Error("the rewritten script RAN under the pinned entry")
		}
		if sentinelExists(t, l.pinnedMarker) {
			t.Error("the decoy ran; the runner spawns the path it resolved before the lock")
		}
		if !result.Failed {
			t.Error("the action did not fail after its argv[0] was retargeted")
		}
		if len(result.Results) != 1 {
			t.Fatalf("got %d rows, want 1: %+v", len(result.Results), result.Results)
		}
		row := result.Results[0]
		if row.Verdict != db.ActionVerdictUnmatched || row.Exit != nil ||
			row.TrustEntry != "pinned-action" {
			t.Errorf("row = verdict %q exit %v entry %q, want %q, nil, %q",
				row.Verdict, row.Exit, row.TrustEntry, db.ActionVerdictUnmatched, "pinned-action")
		}
		resolvedNow, err := filepath.EvalSymlinks(l.decoyPath)
		testsupport.Must(t, err, "resolving the decoy: %v", err)
		for _, want := range []string{resolvedBefore, resolvedNow} {
			if !strings.Contains(row.Reason, want) {
				t.Errorf("the reason does not name %s: %q", want, row.Reason)
			}
		}
	})

	t.Run("unpinned entry still runs", func(t *testing.T) {
		repo := t.TempDir()
		l := build(t)

		result := runTreeActionAcrossHeldLock(t, repo,
			pinnedActionEntry(repo, l.script, ""), l.retarget)

		if !sentinelExists(t, l.rewrittenMarker) {
			t.Errorf("the unpinned action did not spawn its resolved path: %+v", result.Results)
		}
		if result.Failed {
			t.Errorf("the unpinned action failed: %s", result.Reason)
		}
		if len(result.Results) != 1 || result.Results[0].Verdict != db.ActionVerdictPass {
			t.Errorf("rows = %+v, want one %q row", result.Results, db.ActionVerdictPass)
		}
	})
}

// declaredFileAction is an action whose trust entry pins scripts/x, laid out
// across three DISTINCT directories: the exec root the action spawns in, the
// identity the entry binds to, and a step worktree. Only distinct roots show
// which one the declared-file check hashed.
type declaredFileAction struct {
	repo, identity, workRoot, marker string
	entry                            trust.Entry
}

const declaredFilePath = "scripts/x"

func newDeclaredFileAction(t *testing.T) declaredFileAction {
	t.Helper()
	d := declaredFileAction{
		repo: t.TempDir(), identity: t.TempDir(), workRoot: t.TempDir(),
		marker: filepath.Join(t.TempDir(), "ran"),
	}
	const pinned = "pinned contents\n"
	for _, root := range []string{d.repo, d.identity, d.workRoot} {
		writeDeclaredFile(t, root, pinned)
	}
	script := echoScript(t, t.TempDir(), "action.sh",
		"/usr/bin/touch "+d.marker+"\n"+actionReplyLine)
	d.entry = trust.Entry{
		Name: "declared-action", Argv: []string{script},
		Repo:          mustResolve(d.identity),
		DeclaredFiles: []trust.DeclaredFile{{Path: declaredFilePath, SHA256: trustHashOf(pinned)}},
	}
	return d
}

func (d declaredFileAction) runner() *ExecActionRunner {
	store := &trust.Store{Entries: []trust.Entry{d.entry}}
	return &ExecActionRunner{
		RepoRoot:  d.repo,
		Identity:  d.identity,
		LockPath:  filepath.Join(d.repo, ".docket", "tree.lock"),
		LoadStore: func() (*trust.Store, error) { return store, nil },
	}
}

func writeDeclaredFile(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, declaredFilePath)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	testsupport.Must(t, err, "creating %s: %v", filepath.Dir(path), err)
	err = os.WriteFile(path, []byte(content), 0o644)
	testsupport.Must(t, err, "writing %s: %v", path, err)
}

// TestActionChecksDeclaredFilesUnderTheSpawnRoot: the declared-file pin is
// verified in the directory the action runs in, the exec root, never in the
// identity the entry binds to nor in a step worktree the action does not run in.
func TestActionChecksDeclaredFilesUnderTheSpawnRoot(t *testing.T) {
	run := func(t *testing.T, d declaredFileAction) ActionResult {
		t.Helper()
		result, err := d.runner().Run(context.Background(), actionSpec(d.entry.Name),
			StepContext{Instance: "reconcile@0", WorkRoot: d.workRoot})
		testsupport.Must(t, err, "Run: %v", err)
		return result
	}

	t.Run("unchanged declared file runs", func(t *testing.T) {
		d := newDeclaredFileAction(t)
		result := run(t, d)
		if result.Failed || !sentinelExists(t, d.marker) {
			t.Errorf("the action with an unchanged declared file did not run: %+v", result.Results)
		}
	})

	t.Run("drift in the spawn root refuses", func(t *testing.T) {
		d := newDeclaredFileAction(t)
		writeDeclaredFile(t, d.repo, "rewritten\n")
		result := run(t, d)
		if sentinelExists(t, d.marker) {
			t.Error("the action RAN with a declared file that changed in the tree it runs in")
		}
		if len(result.Results) != 1 || result.Results[0].Verdict != db.ActionVerdictUnmatched {
			t.Fatalf("rows = %+v, want one %q row", result.Results, db.ActionVerdictUnmatched)
		}
		if !strings.Contains(result.Results[0].Reason, declaredFilePath) {
			t.Errorf("the reason does not name %s: %q", declaredFilePath, result.Results[0].Reason)
		}
	})

	t.Run("drift outside the spawn root does not matter", func(t *testing.T) {
		d := newDeclaredFileAction(t)
		writeDeclaredFile(t, d.identity, "rewritten\n")
		writeDeclaredFile(t, d.workRoot, "rewritten\n")
		result := run(t, d)
		if result.Failed || !sentinelExists(t, d.marker) {
			t.Errorf("the check hashed a tree the action does not run in: %+v", result.Results)
		}
	})
}

// TestActionDeclaredFileRecheckBeforeEachAttempt: a flaky action whose first
// attempt rewrites its declared file in the spawn root and fails. The
// re-attempt is not spawned, because the per-attempt re-lookup checks the same
// root.
func TestActionDeclaredFileRecheckBeforeEachAttempt(t *testing.T) {
	markers := t.TempDir()
	first := filepath.Join(markers, "marker-1")
	second := filepath.Join(markers, "marker-2")
	d := newDeclaredFileAction(t)
	body := fmt.Sprintf(
		"if [ -e %[1]s ]; then /usr/bin/touch %[2]s; %[4]s exit 0; fi\n"+
			"/usr/bin/touch %[1]s\n"+
			"echo rewritten > %[3]s\n"+
			"exit 1\n",
		first, second, filepath.Join(d.repo, declaredFilePath),
		`echo '{"body":"","payload":[]}';`)
	err := os.WriteFile(d.entry.Argv[0], []byte("#!/bin/sh\n"+body), 0o755)
	testsupport.Must(t, err, "writing the action script: %v", err)
	d.entry.Flaky = true

	result, err := d.runner().Run(context.Background(),
		actionSpec(d.entry.Name), StepContext{Instance: "reconcile@0"})
	testsupport.Must(t, err, "Run: %v", err)

	if !sentinelExists(t, first) {
		t.Fatalf("the first attempt did not run: %+v", result.Results)
	}
	if sentinelExists(t, second) {
		t.Error("the re-attempt RAN after its declared file changed")
	}
	if len(result.Results) != 2 {
		t.Fatalf("got %d rows, want 2 (the failed run, then the refusal): %+v",
			len(result.Results), result.Results)
	}
	r := result.Results[1]
	if r.Verdict != db.ActionVerdictUnmatched || !strings.Contains(r.Reason, declaredFilePath) {
		t.Errorf("row 1 = verdict %q reason %q, want %q naming %s",
			r.Verdict, r.Reason, db.ActionVerdictUnmatched, declaredFilePath)
	}
}

// TestActionSymlinkFlipNeverPassesUnpinnedBytes: the entry trusts link, a
// symlink a concurrent writer keeps flipping between a pinned script and an
// unpinned one, across many action runs. Every run either passes with the
// pinned script's reply or is refused as unmatched before anything spawns; the
// unpinned script never runs.
//
// This covers symlink retargeting only. A rename over the resolved file or a
// write into it between the hash and the spawn is outside what a path-based
// spawn binds.
func TestActionSymlinkFlipNeverPassesUnpinnedBytes(t *testing.T) {
	const (
		minRuns = 1000
		maxRuns = 2000
	)
	repo := t.TempDir()
	dir, markers := t.TempDir(), t.TempDir()
	pinnedRan := filepath.Join(markers, "pinned-ran")
	unpinnedRan := filepath.Join(markers, "unpinned-ran")
	script := func(name, body, marker string) (path, content string) {
		path = filepath.Join(dir, name)
		content = fmt.Sprintf("%s%s\necho x >> %q\necho '{\"body\":%q,\"payload\":[]}'\n",
			pinScriptHeader, body, marker, body)
		err := os.WriteFile(path, []byte(content), 0o755)
		testsupport.Must(t, err, "writing %s: %v", name, err)
		return path, content
	}
	pinnedPath, pinnedContent := script("pinned.sh", "P", pinnedRan)
	unpinnedPath, _ := script("unpinned.sh", "U", unpinnedRan)

	link := filepath.Join(dir, "link")
	err := os.Symlink(pinnedPath, link)
	testsupport.Must(t, err, "linking %s: %v", link, err)

	stop, flipped := make(chan struct{}), make(chan error, 1)
	go func() {
		tmp := filepath.Join(dir, "link.tmp")
		targets := [2]string{unpinnedPath, pinnedPath}
		for i := 0; ; i++ {
			select {
			case <-stop:
				flipped <- nil
				return
			default:
			}
			if err := os.Symlink(targets[i%2], tmp); err != nil {
				flipped <- err
				return
			}
			if err := os.Rename(tmp, link); err != nil {
				flipped <- err
				return
			}
			time.Sleep(10 * time.Microsecond)
		}
	}()
	var flipErr error
	stopped := false
	stopFlipper := func() {
		if !stopped {
			stopped = true
			close(stop)
			flipErr = <-flipped
		}
	}
	t.Cleanup(stopFlipper)

	runner := actionRunner(t, repo,
		pinnedActionEntry(repo, link, trustHashOf(pinnedContent)))
	var runs, pinnedPasses, refusals int
	for runs < minRuns || (pinnedPasses == 0 && runs < maxRuns) {
		runs++
		result, err := runner.Run(context.Background(),
			actionSpec("pinned-action"), StepContext{Instance: "reconcile@0"})
		testsupport.Must(t, err, "Run %d: %v", runs, err)
		if len(result.Results) == 0 {
			t.Fatalf("run %d recorded no rows", runs)
		}
		row := result.Results[len(result.Results)-1]
		switch {
		case row.Verdict == db.ActionVerdictPass && !result.Failed && result.Body == "P":
			pinnedPasses++
		case row.Verdict == db.ActionVerdictUnmatched && result.Failed && row.Exit == nil:
			refusals++
		default:
			exit := "none"
			if row.Exit != nil {
				exit = fmt.Sprint(*row.Exit)
			}
			t.Errorf("run %d recorded verdict %q body %q exit %s (%s); want a pass "+
				"with the pinned reply or unmatched with nothing spawned",
				runs, row.Verdict, result.Body, exit, row.Reason)
		}
	}
	stopFlipper()
	testsupport.Must(t, flipErr, "flipping %s: %v", link, flipErr)

	if sentinelExists(t, unpinnedRan) {
		t.Errorf("THE UNPINNED SCRIPT RAN under the pinned entry")
	}
	if pinnedPasses == 0 || !sentinelExists(t, pinnedRan) {
		t.Fatalf("no run passed in %d runs (%d refused); the flip never left "+
			"the pinned script in place long enough to prove anything", runs, refusals)
	}
	t.Logf("runs=%d pinnedPasses=%d refusals=%d", runs, pinnedPasses, refusals)
}
