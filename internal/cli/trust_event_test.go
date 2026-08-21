package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/spf13/cobra"
)

// §3.6's event as an AUDIT TRAIL (DKT-81).
//
// EVERY TEST HERE DRIVES THE REAL VERB, through cobra, with a real store and a
// real database. The properties are about the ORDER of two writes to two files
// and about what the payload carries, and a test that called the recording
// helper directly would assert the payload while proving nothing about the
// order — which is the half that had the defect.
//
// XDG_CONFIG_HOME AND HOME ARE REDIRECTED IN EVERY TEST, per the trust package's
// own SB1: a test that wrote to the operator's real ~/.config/docket/trust.toml
// would grant trust on the machine running it.

// trustRepo builds an isolated store plus a repository whose database exists,
// and returns the repo root and the resolved config the verbs read.
//
// The temp dirs are taken BEFORE the environment is rewritten, since t.TempDir
// itself reads the environment.
func trustRepo(t *testing.T) (string, *config.Config) {
	t.Helper()
	xdg := t.TempDir()
	repo := t.TempDir()

	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", xdg)

	cfg := trustCfg(repo)
	err := os.MkdirAll(cfg.DocketDir, 0o755)
	testsupport.Must(t, err, "creating %s: %v", cfg.DocketDir, err)

	conn, err := db.Open(cfg.DBPath)
	testsupport.Must(t, err, "Open(%s): %v", cfg.DBPath, err)
	defer conn.Close()
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)

	return repo, cfg
}

// trustNoRepo is the same isolation WITHOUT a database, which is the case §3.5
// keeps working: the store is user-level and managing it requires no repository.
func trustNoRepo(t *testing.T) (string, *config.Config) {
	t.Helper()
	xdg := t.TempDir()
	repo := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", xdg)
	return repo, trustCfg(repo)
}

func trustCfg(repo string) *config.Config {
	// LocalAt fills the whole fact set — store, exec root, and the Identity
	// the trust verbs bind entries to.
	return config.LocalAt(repo)
}

// runTrustVerb drives one verb the way a shell would, on a PRISTINE command:
// cobra keeps parsed flag values in the command's flag set, so a shared one
// would carry the previous case's --tree into this one.
func runTrustVerb(t *testing.T, cfg *config.Config, cmd *cobra.Command, args ...string) error {
	t.Helper()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	ctx := context.WithValue(context.Background(), cfgKey, cfg)
	return cmd.ExecuteContext(ctx)
}

// errorCodeOf pulls the taxonomy code off a verb's failure.
func errorCodeOf(t *testing.T, err error) output.ErrorCode {
	t.Helper()
	var cmdError *CmdError
	if !errors.As(err, &cmdError) {
		t.Fatalf("the failure carries no taxonomy code: %v", err)
	}
	return cmdError.Code
}

// trustEvents reads the repo's event feed, decoding each event's data payload.
func trustEvents(t *testing.T, cfg *config.Config) []map[string]any {
	t.Helper()
	conn, err := db.Open(cfg.DBPath)
	testsupport.Must(t, err, "Open(%s): %v", cfg.DBPath, err)
	defer conn.Close()

	page, err := engine.ListEvents(conn, engine.EventQuery{})
	testsupport.Must(t, err, "ListEvents: %v", err)

	var out []map[string]any
	for _, e := range page.Events {
		if e.Kind != engine.EventTrustAdded && e.Kind != engine.EventTrustRemoved {
			continue
		}
		data := map[string]any{}
		err := json.Unmarshal(e.Data, &data)
		testsupport.Must(t, err, "a %s event's data is not an object: %v", e.Kind, err)
		data["kind"] = e.Kind
		out = append(out, data)
	}
	return out
}

func trustEntries(t *testing.T) []trust.Entry {
	t.Helper()
	store, err := trust.Load()
	testsupport.Must(t, err, "trust.Load: %v", err)
	return store.Entries
}

// TestTrustAddEventCarriesEveryFlag is the payload half of DKT-81.
//
// The feed used to carry the name, the hash and the binding alone, which meant
// the two properties that WIDEN what a command may do — `tree` and the declared
// `network` hosts — were invisible in it. A remove and a re-add milliseconds
// apart could then widen egress while the trail showed only that something with
// the same name had been re-approved.
func TestTrustAddEventCarriesEveryFlag(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(),
		"scan", "--tree", "--flaky", "--re-runnable", "--timeout", "90s",
		"--network", "vuln.go.dev", "--network", "proxy.example",
		"--", "go", "run", "./scan")
	testsupport.Must(t, err, "trust add: %v", err)

	events := trustEvents(t, cfg)
	if len(events) != 1 {
		t.Fatalf("got %d trust events, want exactly the one add", len(events))
	}
	got := events[0]

	// THE KEY SET IS EXACT, in both directions. A missing key is a property the
	// trail cannot show; an unexpected one is a payload drifting from the shape
	// consumers read.
	want := map[string]any{
		"name":        "scan",
		"argv_sha256": trust.ArgvSHA256([]string{"go", "run", "./scan"}),
		"repo":        mustRepoIdentity(t, cfg),
		"global":      false,
		"prefix":      false,
		"re_runnable": true,
		"tree":        true,
		"flaky":       true,
		"network":     []any{"vuln.go.dev", "proxy.example"},
		"timeout":     "90s",
		// FALSE, and asserted rather than skipped. `stub` says the entry
		// authorizes a placeholder (DKT-265), and an add that did not pass
		// --stub must record that fact as false — a missing key would leave a
		// reader unable to tell "not a stub" from "this docket does not record
		// stubs".
		"stub": false,
	}
	want["kind"] = engine.EventTrustAdded

	// `actor` and `cwd` (DKT-263) are ENVIRONMENT-DERIVED, so they belong to the
	// key set without belonging to the value table: pinning "Erik Reinert" here
	// would assert the machine the suite happens to run on. Their values are
	// TestTrustEventNamesWhoGrantedIt's business; what this test owns is that
	// they are part of the exact shape a consumer reads.
	derived := map[string]bool{"actor": true, "cwd": true}
	for key := range derived {
		if _, present := got[key]; !present {
			t.Errorf("the event omits %q; a grant widens what code may execute, "+
				"so its trail must name who did it (DKT-263)", key)
		}
	}

	for key, expected := range want {
		actual, present := got[key]
		if !present {
			t.Errorf("the event omits %q; every behavior-affecting property must ride in it", key)
			continue
		}
		if !sameJSON(t, actual, expected) {
			t.Errorf("event[%q] = %#v, want %#v", key, actual, expected)
		}
	}
	for key := range got {
		if _, expected := want[key]; expected || derived[key] {
			continue
		}
		t.Errorf("the event carries an unexpected key %q", key)
	}

	// THE ARGV ITSELF NEVER RIDES ALONG (§3.6). The hash identifies the command;
	// the arguments must not land in a feed a run report renders.
	raw, err := json.Marshal(got)
	testsupport.Must(t, err, "re-marshaling: %v", err)
	if strings.Contains(string(raw), "./scan") {
		t.Errorf("the event body contains the argv: %s", raw)
	}
}

// TestTrustRemoveEventCarriesTheRemovedFlags: a revocation says what it revoked,
// down to the flags. Without them a `trust-removed` and the `trust-added` that
// follows it are two events about a name, and the pair cannot be read as the
// change it was.
func TestTrustRemoveEventCarriesTheRemovedFlags(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(),
		"scan", "--tree", "--network", "vuln.go.dev", "--", "go", "run", "./scan")
	testsupport.Must(t, err, "trust add: %v", err)

	err = runTrustVerb(t, cfg, newTrustRmCmd(), "scan")
	testsupport.Must(t, err, "trust rm: %v", err)

	events := trustEvents(t, cfg)
	if len(events) != 2 {
		t.Fatalf("got %d trust events, want the add and the remove", len(events))
	}
	removed := events[1]
	if removed["kind"] != engine.EventTrustRemoved {
		t.Fatalf("the second event is %v, want %s", removed["kind"], engine.EventTrustRemoved)
	}
	if removed["tree"] != true {
		t.Errorf("the removal does not record that the entry carried tree; got %#v", removed["tree"])
	}
	if !sameJSON(t, removed["network"], []any{"vuln.go.dev"}) {
		t.Errorf("the removal does not record the revoked hosts; got %#v", removed["network"])
	}
	if removed["argv_sha256"] != trust.ArgvSHA256([]string{"go", "run", "./scan"}) {
		t.Errorf("the removal names the wrong argv hash: %#v", removed["argv_sha256"])
	}
}

// TestIdempotentReAddEmitsNoEvent: an event must PROVE a change.
//
// `trust-added` firing on a re-add of an identical entry made the feed unusable
// for the question it exists to answer — an auditor reading it could not tell a
// new grant from a repeated one, so every event needed the store checked by hand
// before it meant anything.
func TestIdempotentReAddEmitsNoEvent(t *testing.T) {
	_, cfg := trustRepo(t)

	args := []string{"checks", "--tree", "--network", "vuln.go.dev", "--", "make", "test"}
	err := runTrustVerb(t, cfg, newTrustAddCmd(), args...)
	testsupport.Must(t, err, "first add: %v", err)

	err = runTrustVerb(t, cfg, newTrustAddCmd(), args...)
	testsupport.Must(t, err, "an identical re-add is an idempotent success: %v", err)

	if events := trustEvents(t, cfg); len(events) != 1 {
		t.Errorf("got %d events for one grant and one no-op re-add, want 1", len(events))
	}
	if entries := trustEntries(t); len(entries) != 1 {
		t.Errorf("the store holds %d entries, want 1", len(entries))
	}
}

// TestChangedFlagsReAddConflictsAndEmitsNothing pins the existing refusal AND
// its silence: a conflict changes nothing, so there is nothing to record.
//
// The flag chosen is `--network`, the one that had gone uncompared: an add
// asking for new egress at an existing name reported "already trusted", wrote
// nothing and exited 0, which is a requested widening discarded under a success
// message.
func TestChangedFlagsReAddConflictsAndEmitsNothing(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", "make", "test")
	testsupport.Must(t, err, "first add: %v", err)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"network", []string{"checks", "--network", "vuln.go.dev", "--", "make", "test"}},
		{"tree", []string{"checks", "--tree", "--", "make", "test"}},
		{"argv", []string{"checks", "--", "make", "deploy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runTrustVerb(t, cfg, newTrustAddCmd(), tc.args...)
			if err == nil {
				t.Fatalf("a differing %s must be refused, never silently applied", tc.name)
			}
			if code := errorCodeOf(t, err); code != output.ErrConflict {
				t.Errorf("the refusal is %s, want CONFLICT: %v", code, err)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("the refusal must name what would change (%s); got: %v", tc.name, err)
			}
		})
	}

	if events := trustEvents(t, cfg); len(events) != 1 {
		t.Errorf("got %d events; a refused add records nothing", len(events))
	}
	entries := trustEntries(t)
	if len(entries) != 1 || entries[0].Tree || len(entries[0].Network) != 0 {
		t.Errorf("a refused add must leave the entry untouched; got %+v", entries)
	}
}

// TestTrustAddFailsWhenTheEventCannotBeRecorded is the ORDER, and the property
// the old best-effort recording could not hold: inside a repo, a grant that
// cannot be recorded does not happen.
//
// The database is replaced with bytes that are not one, which is a failure the
// verb meets at the moment it tries to record and cannot anticipate — the same
// shape as a corrupt or unwritable file under a real operator.
func TestTrustAddFailsWhenTheEventCannotBeRecorded(t *testing.T) {
	_, cfg := trustRepo(t)
	err := os.WriteFile(cfg.DBPath, []byte("this is not a database"), 0o600)
	testsupport.Must(t, err, "corrupting the database: %v", err)

	runErr := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", "make", "test")
	if runErr == nil {
		t.Fatal("the add must FAIL when its event cannot be recorded; a grant with no trail reads as no grant")
	}

	// THE STORE IS UNTOUCHED. This is the whole point of ordering the event
	// first: what survives a failure is an unrecorded refusal, never an
	// unrecorded grant.
	if entries := trustEntries(t); len(entries) != 0 {
		t.Errorf("the grant landed anyway: %+v", entries)
	}
}

// TestTrustAddOutsideARepoSaysNothingWasRecorded: the verb still works — the
// store is user-level (§3.5) and requiring a database to manage it would mean
// someone who installed docket could not approve a command until they created a
// tracker — but it SAYS the change went unrecorded rather than leaving the
// operator to assume a trail exists.
func TestTrustAddOutsideARepoSaysNothingWasRecorded(t *testing.T) {
	_, cfg := trustNoRepo(t)

	// The disclosure rides in the RESULT, not only on a stream, so a machine
	// consumer sees it too — the same reason §3.3's over-authorization warning
	// is in the payload rather than on stderr alone.
	cmd := newTrustAddCmd()
	cmd.Flags().String("json", "", "")
	err := cmd.Flags().Set("json", "v1")
	testsupport.Must(t, err, "setting --json: %v", err)

	stdout := captureStdout(t)
	err = runTrustVerb(t, cfg, cmd, "checks", "--", "make", "test")
	out := stdout()
	testsupport.Must(t, err, "trust add outside a repo must still work: %v", err)

	entries := trustEntries(t)
	if len(entries) != 1 {
		t.Fatalf("the entry was not written; got %+v", entries)
	}

	var envelope struct {
		Data struct {
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("the add's JSON did not parse: %v\n%s", err, out)
	}
	var said bool
	for _, warning := range envelope.Data.Warnings {
		if strings.Contains(warning, "not recorded") {
			said = true
		}
	}
	if !said {
		t.Errorf("no warning says the change went unrecorded; got %v", envelope.Data.Warnings)
	}
}

// TestIdempotentReAddOutsideARepoSaysNothing is the note's own idempotence:
// nothing changed, so there was nothing to record, and a warning that something
// went unrecorded would be false.
func TestIdempotentReAddOutsideARepoSaysNothing(t *testing.T) {
	_, cfg := trustNoRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "checks", "--", "make", "test")
	testsupport.Must(t, err, "first add: %v", err)

	cmd := newTrustAddCmd()
	cmd.Flags().String("json", "", "")
	err = cmd.Flags().Set("json", "v1")
	testsupport.Must(t, err, "setting --json: %v", err)

	stdout := captureStdout(t)
	err = runTrustVerb(t, cfg, cmd, "checks", "--", "make", "test")
	out := stdout()
	testsupport.Must(t, err, "idempotent re-add: %v", err)

	var envelope struct {
		Data struct {
			Idempotent bool     `json:"idempotent"`
			Warnings   []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("the add's JSON did not parse: %v\n%s", err, out)
	}
	if !envelope.Data.Idempotent {
		t.Error("an identical re-add must report itself idempotent")
	}
	if len(envelope.Data.Warnings) != 0 {
		t.Errorf("a no-op add warns about an unrecorded change that never happened: %v",
			envelope.Data.Warnings)
	}
}

// captureStdout redirects os.Stdout for the duration of one call, because the
// writer the verbs build renders there rather than to the cobra command's
// stream.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	testsupport.Must(t, err, "os.Pipe: %v", err)
	saved := os.Stdout
	os.Stdout = w

	return func() string {
		os.Stdout = saved
		w.Close()
		var buf bytes.Buffer
		_, copyErr := buf.ReadFrom(r)
		testsupport.Must(t, copyErr, "reading the captured stdout: %v", copyErr)
		r.Close()
		return buf.String()
	}
}

func mustRepoIdentity(t *testing.T, cfg *config.Config) string {
	t.Helper()
	id, err := trust.RepoIdentity(filepath.Dir(cfg.DocketDir))
	testsupport.Must(t, err, "RepoIdentity: %v", err)
	return id
}

// sameJSON compares a decoded value against an expectation, tolerating the
// []any a JSON round trip produces for a list.
func sameJSON(t *testing.T, got, want any) bool {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	testsupport.Must(t, err, "marshaling %#v: %v", got, err)
	wantJSON, err := json.Marshal(want)
	testsupport.Must(t, err, "marshaling %#v: %v", want, err)
	return string(gotJSON) == string(wantJSON)
}

// TestTrustEventNamesWhoGrantedIt is DKT-263: the trail says WHO, not just what
// and when.
//
// Before this, `trust-added` recorded the grant's every property and no
// identity, so recovering by-whom meant bracketing runs against wall-clock —
// which this repo's retro had to do, twice, for grants added mid-run. The
// timestamp answers "during which run" and that was always the smaller half:
// two concurrent sessions on one machine bracket identically.
//
// NEITHER FIELD IS AUTHENTICATED and the test does not pretend otherwise. It
// asserts they are POPULATED and WIRED TO THE INVOCATION — an actor that is
// always the empty string, or a cwd that is a constant, would satisfy a
// presence check and answer nothing.
func TestTrustEventNamesWhoGrantedIt(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "scan", "--", "go", "run", "./scan")
	testsupport.Must(t, err, "trust add: %v", err)

	events := trustEvents(t, cfg)
	if len(events) != 1 {
		t.Fatalf("got %d trust events, want exactly the one add", len(events))
	}
	got := events[0]

	// The actor resolves through the same path every comment and activity row
	// uses, which never yields the empty string: git identity, then the OS
	// username, then the literal "unknown". An empty actor therefore means the
	// field was never filled in, not that nobody could be identified.
	actor, ok := got["actor"].(string)
	if !ok {
		t.Fatalf("the event's `actor` is not a string: %#v", got["actor"])
	}
	if actor == "" {
		t.Error("the event's `actor` is empty; the resolver falls back to the " +
			"OS username and then to \"unknown\", so an empty value means the " +
			"call site did not fill the field rather than that nobody was found")
	}
	if actor != config.DefaultAuthor() {
		t.Errorf("the event's `actor` = %q but the identity resolver says %q; "+
			"a grant must be attributed the same way every other authored row "+
			"in the store is", actor, config.DefaultAuthor())
	}

	// The cwd is the DISAMBIGUATOR — on a machine where every grant carries one
	// git identity, it is the only field that separates two concurrent
	// sessions. Comparing against the process's real cwd is what proves it is
	// read from the invocation rather than defaulted.
	wd, err := os.Getwd()
	testsupport.Must(t, err, "Getwd: %v", err)
	if got["cwd"] != wd {
		t.Errorf("the event's `cwd` = %#v, want %q — the field must name where "+
			"the verb actually ran, since that is what tells two concurrent "+
			"sessions apart", got["cwd"], wd)
	}
}

// TestTrustRemoveEventAlsoNamesWhoDidIt: a revocation is an attributable act too.
//
// It is the SAFER direction and still worth attributing — a removal that breaks
// a run's gates is a change someone made, and "which of us dropped this" is the
// same question in the other direction. Recording who on the add alone would
// leave exactly half a trail.
func TestTrustRemoveEventAlsoNamesWhoDidIt(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "scan", "--", "go", "run", "./scan")
	testsupport.Must(t, err, "trust add: %v", err)
	err = runTrustVerb(t, cfg, newTrustRmCmd(), "scan")
	testsupport.Must(t, err, "trust remove: %v", err)

	events := trustEvents(t, cfg)
	if len(events) != 2 {
		t.Fatalf("got %d trust events, want the add and the remove", len(events))
	}
	removed := events[1]
	if removed["kind"] != engine.EventTrustRemoved {
		t.Fatalf("the second event is %v, want %v", removed["kind"], engine.EventTrustRemoved)
	}

	actor, _ := removed["actor"].(string)
	if actor == "" {
		t.Error("the `trust-removed` event names no actor; a revocation is an " +
			"attributable act for the same reason a grant is")
	}
}

// TestTrustAddStubEventSaysTheAssuranceIsHollow is DKT-265's event half.
//
// An operator granting a placeholder is granting the SHAPE of a check without
// the check. That is legitimate — it is how a repo with no scanner installed
// still exercises a workflow — but a feed showing `secret-scan` being trusted,
// without saying it points at `/usr/bin/true`, records the name of an assurance
// rather than the assurance. Same reasoning that puts `tree` and `network` in
// the payload: it is what the grant is really granting.
func TestTrustAddStubEventSaysTheAssuranceIsHollow(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(),
		"secret-scan", "--stub", "--", "/usr/bin/true")
	testsupport.Must(t, err, "trust add --stub: %v", err)

	events := trustEvents(t, cfg)
	if len(events) != 1 {
		t.Fatalf("got %d trust events, want exactly the one add", len(events))
	}
	if events[0]["stub"] != true {
		t.Errorf("the event's `stub` = %#v, want true — a grant that authorizes "+
			"a placeholder must say so where the grant is recorded",
			events[0]["stub"])
	}

	// The declaration must also reach the STORE, not only the feed: the feed is
	// how the grant is audited afterwards, the entry is what the gate runner
	// reads at execution time to stamp the result row.
	entries := trustEntries(t)
	if len(entries) != 1 {
		t.Fatalf("got %d trust entries, want the one add", len(entries))
	}
	if !entries[0].Stub {
		t.Error("the stored trust entry is not marked stub; the gate runner " +
			"reads the entry, so a declaration that reached only the event " +
			"would mark no gate result hollow")
	}
}

// TestReAddFlippingStubConflicts: `stub` joins the conflict set.
//
// A silent flip in either direction is the failure. Turning it OFF converts
// every future hollow pass into one that reads as real; turning it ON does the
// reverse. Either way the store would show only that something of that name was
// re-approved — which is precisely the shape §3.6's flag-carrying payload
// exists to prevent.
func TestReAddFlippingStubConflicts(t *testing.T) {
	_, cfg := trustRepo(t)

	err := runTrustVerb(t, cfg, newTrustAddCmd(), "scan", "--stub", "--", "make", "test")
	testsupport.Must(t, err, "trust add --stub: %v", err)

	err = runTrustVerb(t, cfg, newTrustAddCmd(), "scan", "--", "make", "test")
	if err == nil {
		t.Fatal("re-adding the same argv without --stub succeeded; flipping a " +
			"stub declaration silently turns hollow assurance into assurance " +
			"that reads as real")
	}
	if code := errorCodeOf(t, err); code != output.ErrConflict {
		t.Errorf("the refusal is %v, want %v", code, output.ErrConflict)
	}
	if !strings.Contains(err.Error(), "stub") {
		t.Errorf("the conflict does not name `stub`: %v", err)
	}
}
