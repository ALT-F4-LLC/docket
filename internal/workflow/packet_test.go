package workflow

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// PACKET COMPOSITION — the grammar half
// (docs/tdd/packet-composition.md §1.1, §1.6).
//
// `packet` is an optional list of file references a step declares it needs in
// its rendered work packet. CORE ATTACHES NO MEANING TO ANY ENTRY: they are
// paths, in declared order, and the engine reads bytes it never interprets.
//
// V32 is the SHAPE rule (non-empty, relative, contained) and V33 is the token
// rule (a `{executor}` token needs a step that HAS a per-sibling executor
// hint). Both are decisions about bytes, so both live in Validate rather than
// ValidateSchemas.
//
// GENERICITY: every fixture below uses neutral vocabulary — a print shop's
// checklists. The reference instance's own directory names are instance data.

func packetWorkflow(t *testing.T, stepTOML string) *Definition {
	t.Helper()
	src := `
[pipeline]
name = "print-shop"
version = 1

[match]
kind = ["task"]

` + stepTOML
	def, err := Parse([]byte(src))
	testsupport.Must(t, err, "parsing the fixture: %v", err)
	return def
}

// TestPacketParses is the grammar itself: `packet` round-trips as a list of
// strings, in DECLARED ORDER, which is the order the assembler inlines them in.
func TestPacketParses(t *testing.T) {
	def := packetWorkflow(t, `
[[step]]
name = "proof"
executor = "proofreader"
emits = "notes"
after = []
packet = ["checklists/proofing.md", "checklists/house-style.md"]
`)

	step := StepByName(def, "proof")
	if step == nil {
		t.Fatal("no step named proof")
	}
	want := []string{"checklists/proofing.md", "checklists/house-style.md"}
	if len(step.Packet) != len(want) {
		t.Fatalf("packet = %v, want %v", step.Packet, want)
	}
	for i := range want {
		if step.Packet[i] != want[i] {
			t.Errorf("packet[%d] = %q, want %q — declared order is the assembly order",
				i, step.Packet[i], want[i])
		}
	}
}

// TestPacketIsOptional is the no-regression assertion: every workflow that
// existed before `packet` was added declares none, and each must validate and render
// exactly as it did.
func TestPacketIsOptional(t *testing.T) {
	def := packetWorkflow(t, `
[[step]]
name = "proof"
executor = "proofreader"
emits = "notes"
after = []
`)
	if err := Validate(def); err != nil {
		t.Fatalf("a workflow with no packet failed validation: %v", err)
	}
	if step := StepByName(def, "proof"); len(step.Packet) != 0 {
		t.Errorf("packet = %v, want empty", step.Packet)
	}
}

// TestPacketV32Shape is V32: entries are non-empty, relative, and lexically
// contained in the config directory.
//
// SHAPE ONLY, in the spirit of V25: a file's EXISTENCE is an activation-time
// fact, not a registration-time one. A workflow must stay registerable before
// the files it names are added — otherwise the zero-touch bootstrap cannot
// register a workflow and its corpus in either order.
func TestPacketV32Shape(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"empty", `""`},
		{"whitespace only", `"   "`},
		{"absolute", `"/etc/passwd"`},
		{"parent escape", `"../../secrets.md"`},
		{"parent escape mid-path", `"checklists/../../secrets.md"`},
		{"home relative", `"~/notes.md"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := packetWorkflow(t, `
[[step]]
name = "proof"
executor = "proofreader"
emits = "notes"
after = []
packet = [`+tc.entry+`]
`)
			err := Validate(def)
			if err == nil {
				t.Fatalf("packet entry %s was accepted, want a V32 refusal", tc.entry)
			}
			var e *Error
			if !asWorkflowError(err, &e) {
				t.Fatalf("err = %v, want a *workflow.Error", err)
			}
			if e.Rule != "V32" {
				t.Errorf("rule = %q, want V32", e.Rule)
			}
			if e.Step != "proof" {
				t.Errorf("step = %q, want proof — a refusal names the step", e.Step)
			}
			if e.Field != "packet" {
				t.Errorf("field = %q, want packet", e.Field)
			}
		})
	}
}

// TestPacketV32AcceptsOrdinaryPaths guards against an over-eager containment
// check: a relative path with interior dots or a nested directory is ordinary.
func TestPacketV32AcceptsOrdinaryPaths(t *testing.T) {
	for _, entry := range []string{
		"checklists/proofing.md",
		"checklists/nested/deep/house-style.md",
		"notes.v2.md",
		"./checklists/proofing.md",
	} {
		def := packetWorkflow(t, `
[[step]]
name = "proof"
executor = "proofreader"
emits = "notes"
after = []
packet = ["`+entry+`"]
`)
		if err := Validate(def); err != nil {
			t.Errorf("packet entry %q was refused: %v", entry, err)
		}
	}
}

// TestPacketV33TokenNeedsAnExecutorHint is V33, and it is the rule that keeps
// the substitution honest.
//
// `{executor}` resolves to a step's per-sibling executor hint. An `action`,
// `human`, or `vote` step HAS no hint, so the token would substitute to nothing
// and produce a path like `checklists/.md` — a missing-pin error much later,
// naming a path no author wrote. Refusing at register time names the real
// mistake at the moment it is made.
func TestPacketV33TokenNeedsAnExecutorHint(t *testing.T) {
	cases := []struct {
		name    string
		step    string
		wantErr bool
	}{
		{
			name: "executor step accepts the token",
			step: `
[[step]]
name = "proof"
executor = "proofreader"
emits = "notes"
after = []
packet = ["checklists/{executor}.md"]
`,
		},
		{
			name: "fanout step accepts the token",
			step: `
[[step]]
name = "proof"
fanout = ["proofreader-copy", "proofreader-colour"]
emits = "notes"
after = []
packet = ["checklists/{executor}.md"]
`,
		},
		{
			name: "human step refuses the token",
			step: `
[[step]]
name = "approve"
type = "human"
after = []
on_fail = "abandon-issue"
packet = ["checklists/{executor}.md"]
`,
			wantErr: true,
		},
		{
			name: "action step refuses the token",
			step: `
[[step]]
name = "reconcile"
action = "aggregate"
after = []
params = { field = "n", op = "sum", output = "tally" }
packet = ["checklists/{executor}.md"]
`,
			wantErr: true,
		},
		{
			name: "a literal entry is fine on a human step",
			step: `
[[step]]
name = "approve"
type = "human"
after = []
on_fail = "abandon-issue"
packet = ["checklists/approval.md"]
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := packetWorkflow(t, tc.step)
			err := Validate(def)
			if !tc.wantErr {
				testsupport.Must(t, err, "unexpected refusal: %v", err)
				return
			}
			if err == nil {
				t.Fatal("the token was accepted on a step with no executor hint, want V33")
			}
			var e *Error
			if !asWorkflowError(err, &e) {
				t.Fatalf("err = %v, want a *workflow.Error", err)
			}
			if e.Rule != "V33" {
				t.Errorf("rule = %q, want V33", e.Rule)
			}
			if !strings.Contains(e.Message, "{executor}") {
				t.Errorf("message = %q, want it to name the token", e.Message)
			}
		})
	}
}

// TestSubstitutePacketEntry is the substitution itself (§1.1.1): DUMB STRING
// REPLACEMENT and nothing else.
//
// Core does not know `checklists/` from any other directory, cannot fall back
// when a file is missing, and cannot collapse a family of hints onto one file.
// The INSTANCE authors the mapping rule as syntax; core replaces a token.
func TestSubstitutePacketEntry(t *testing.T) {
	cases := []struct {
		name     string
		entry    string
		executor string
		want     string
	}{
		{"no token", "checklists/proofing.md", "proofreader", "checklists/proofing.md"},
		{"one token", "checklists/{executor}.md", "proofreader", "checklists/proofreader.md"},
		{"token twice", "{executor}/{executor}.md", "press", "press/press.md"},
		{"token with a hyphenated hint", "c/{executor}.md", "proofreader-colour", "c/proofreader-colour.md"},
		{"unknown token is left alone", "c/{desk}.md", "press", "c/{desk}.md"},
		{"no executor, no token", "c/proofing.md", "", "c/proofing.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SubstitutePacketEntry(tc.entry, tc.executor); got != tc.want {
				t.Errorf("SubstitutePacketEntry(%q, %q) = %q, want %q",
					tc.entry, tc.executor, got, tc.want)
			}
		})
	}
}

// TestPacketExpandsPerSibling is F1 at the expansion layer — the defect the
// first draft of the design could not express.
//
// A `fanout` step is ONE step whose siblings each carry a different hint. A
// literal entry therefore serves every sibling identically, and only the token
// lets one step declare per-sibling files. Both variants are asserted, because
// the mechanism's correctness is that it serves them WITHOUT special-casing
// either.
func TestPacketExpandsPerSibling(t *testing.T) {
	t.Run("divergent siblings via the token", func(t *testing.T) {
		def := packetWorkflow(t, `
[[step]]
name = "proof"
fanout = ["proofreader-copy", "proofreader-colour"]
emits = "notes"
after = []
packet = ["checklists/{executor}.md"]
`)
		rows := Expand(def, Subject{Kind: "task"}, 0)

		seen := make(map[string]string)
		for _, row := range rows {
			if row.Name != "proof" {
				continue
			}
			if len(row.Packet) != 1 {
				t.Fatalf("sibling %s carries %d packet entries, want 1",
					row.Instance, len(row.Packet))
			}
			seen[row.Executor] = row.Packet[0]
		}

		if got := seen["proofreader-copy"]; got != "checklists/proofreader-copy.md" {
			t.Errorf("copy sibling resolved %q, want its OWN file", got)
		}
		if got := seen["proofreader-colour"]; got != "checklists/proofreader-colour.md" {
			t.Errorf("colour sibling resolved %q, want its OWN file", got)
		}
	})

	// The `spec-author-<axis>` shape, genericized: a family of hints sharing one
	// file. They need NO per-hint file and NO prefix-stripping rule — the
	// instance simply writes a literal, and each sibling's identity still
	// reaches the packet through its own executor hint.
	t.Run("a hint family shares one literal entry", func(t *testing.T) {
		def := packetWorkflow(t, `
[[step]]
name = "proof"
fanout = ["proofreader-copy", "proofreader-colour", "proofreader-layout"]
emits = "notes"
after = []
packet = ["checklists/proofing.md"]
`)
		rows := Expand(def, Subject{Kind: "task"}, 0)

		hints := make(map[string]bool)
		for _, row := range rows {
			if row.Name != "proof" {
				continue
			}
			if len(row.Packet) != 1 || row.Packet[0] != "checklists/proofing.md" {
				t.Errorf("sibling %s resolved %v, want the one shared file",
					row.Instance, row.Packet)
			}
			hints[row.Executor] = true
		}
		if len(hints) != 3 {
			t.Errorf("saw %d distinct hints, want 3 — identity must survive a shared file",
				len(hints))
		}
	})

	t.Run("mixed literal and token entries", func(t *testing.T) {
		def := packetWorkflow(t, `
[[step]]
name = "proof"
fanout = ["proofreader-copy"]
emits = "notes"
after = []
packet = ["checklists/house-style.md", "checklists/{executor}.md"]
`)
		rows := Expand(def, Subject{Kind: "task"}, 0)
		for _, row := range rows {
			if row.Name != "proof" {
				continue
			}
			want := []string{"checklists/house-style.md", "checklists/proofreader-copy.md"}
			if len(row.Packet) != 2 {
				t.Fatalf("packet = %v, want 2 entries in declared order", row.Packet)
			}
			for i := range want {
				if row.Packet[i] != want[i] {
					t.Errorf("packet[%d] = %q, want %q", i, row.Packet[i], want[i])
				}
			}
		}
	})
}

// asWorkflowError is errors.As specialized to this package's Error, kept local
// so the table tests above read as assertions rather than as plumbing.
func asWorkflowError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
