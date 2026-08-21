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
		"strings": true, "time": true,
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
		"exec.Resolve",     // A5: argv[0] resolution and repo containment
		"exec.BuildEnv",    // A4: the env allowlist and the token exclusion
		"exec.RunAttempts", // A6/A8: timeout, capture, flaky re-runs
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
