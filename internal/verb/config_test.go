package verb_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

const attemptMax = "attempt.max"

func TestConfigGetReadsTheCallersProjectOverride(t *testing.T) {
	st := openStore(t)
	project, err := st.EnsureProject("/repos/configured", "configured", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := st.SetConfig(project, attemptMax, "7"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	env := projectEnv(st, "/repos/configured", true)

	got, err := verb.ConfigGet(context.Background(), env, verb.ConfigGetInput{Key: attemptMax})
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	if got.Entry != (store.ConfigEntry{Key: attemptMax, Value: "7", Source: "set"}) {
		t.Errorf("ConfigGet(%s) = %+v, want the project's override 7 (set)", attemptMax, got.Entry)
	}

	global, err := verb.ConfigGet(context.Background(), env, verb.ConfigGetInput{Key: attemptMax, Global: true})
	if err != nil {
		t.Fatalf("ConfigGet --global: %v", err)
	}
	if global.Entry.Source != "default" || global.Entry.Value == "7" {
		t.Errorf("ConfigGet(%s, global) = %+v, want the store-wide default, not the project override",
			attemptMax, global.Entry)
	}
}

func TestConfigGetListsEveryKeyWhenNoneIsNamed(t *testing.T) {
	st := openStore(t)
	if err := st.SetConfig(0, attemptMax, "9"); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	got, err := verb.ConfigGet(context.Background(), projectEnv(st, "", false), verb.ConfigGetInput{})
	if err != nil {
		t.Fatalf("ConfigGet: %v", err)
	}
	listed := false
	for _, e := range got.Entries {
		listed = listed || e == (store.ConfigEntry{Key: attemptMax, Value: "9", Source: "set"})
	}
	if !listed {
		t.Errorf("listing omits %s=9 (set): %+v", attemptMax, got.Entries)
	}
}

func TestConfigGetNeverRegistersAProject(t *testing.T) {
	st := openStore(t)
	if _, err := st.EnsureProject("/repos/first", "first", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	env := projectEnv(st, "/repos/unseen", true)

	if _, err := verb.ConfigGet(context.Background(), env, verb.ConfigGetInput{}); err != nil {
		t.Fatalf("ConfigGet list: %v", err)
	}
	if _, err := verb.ConfigGet(context.Background(), env, verb.ConfigGetInput{Key: attemptMax}); err != nil {
		t.Fatalf("ConfigGet key: %v", err)
	}
	requireUnregistered(t, st, "/repos/unseen")
}

func TestConfigGetRefusesAnUnknownKeyAsValidation(t *testing.T) {
	st := openStore(t)

	_, err := verb.ConfigGet(context.Background(), projectEnv(st, "", false), verb.ConfigGetInput{Key: "no.such.key"})
	var ve *verb.Error
	if !errors.As(err, &ve) || ve.Code != output.ErrValidation {
		t.Fatalf("ConfigGet(no.such.key) error = %v, want a *verb.Error with %s", err, output.ErrValidation)
	}
	if !errors.Is(err, store.ErrUnknownConfigKey) {
		t.Errorf("error %v does not wrap store.ErrUnknownConfigKey", err)
	}
}

func TestConfigSetWritesTheCallersProjectOverride(t *testing.T) {
	st := openStore(t)
	project, err := st.EnsureProject("/repos/configured", "configured", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	env := projectEnv(st, "/repos/configured", true)

	got, err := verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: attemptMax, Value: "6"})
	if err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if got.Entry != (store.ConfigEntry{Key: attemptMax, Value: "6", Source: "set"}) {
		t.Errorf("ConfigSet entry = %+v, want %s=6 (set)", got.Entry, attemptMax)
	}
	if got.Registered != nil {
		t.Errorf("Registered = %+v, want nil for an already registered project", got.Registered)
	}

	stored, err := st.GetConfig(project, attemptMax)
	if err != nil {
		t.Fatalf("GetConfig(project): %v", err)
	}
	if stored.Value != "6" || stored.Source != "set" {
		t.Errorf("project's %s = %+v, want 6 (set)", attemptMax, stored)
	}
	global, err := st.GetConfig(0, attemptMax)
	if err != nil {
		t.Fatalf("GetConfig(0): %v", err)
	}
	if global.Source != "default" {
		t.Errorf("store-wide %s = %+v, want it untouched by a project write", attemptMax, global)
	}
}

func TestConfigSetGlobalWritesTheStoreWideDefault(t *testing.T) {
	st := openStore(t)
	project, err := st.EnsureProject("/repos/configured", "configured", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	env := projectEnv(st, "/repos/configured", true)

	if _, err := verb.ConfigSet(context.Background(), env,
		verb.ConfigSetInput{Key: attemptMax, Value: "8", Global: true}); err != nil {
		t.Fatalf("ConfigSet --global: %v", err)
	}

	global, err := st.GetConfig(0, attemptMax)
	if err != nil {
		t.Fatalf("GetConfig(0): %v", err)
	}
	if global.Value != "8" || global.Source != "set" {
		t.Errorf("store-wide %s = %+v, want 8 (set)", attemptMax, global)
	}
	fallback, err := st.GetConfig(project, attemptMax)
	if err != nil {
		t.Fatalf("GetConfig(project): %v", err)
	}
	if fallback.Value != "8" {
		t.Errorf("project reads %s = %+v, want the store-wide 8 it falls back to", attemptMax, fallback)
	}
}

func TestConfigSetRegistersAnAnchoredProject(t *testing.T) {
	st := openStore(t)
	if _, err := st.EnsureProject("/repos/first", "first", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	env := projectEnv(st, "/repos/new", true)

	got, err := verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: attemptMax, Value: "3"})
	if err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	id, found, err := st.LookupProject("/repos/new")
	if err != nil || !found {
		t.Fatalf("LookupProject(/repos/new) = (%d, %v, %v), want the registered row", id, found, err)
	}
	if got.Registered == nil || got.Registered.ID != id {
		t.Fatalf("Registered = %+v, want the new row %d", got.Registered, id)
	}
	stored, err := st.GetConfig(id, attemptMax)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if stored.Value != "3" || stored.Source != "set" {
		t.Errorf("registered project's %s = %+v, want 3 (set)", attemptMax, stored)
	}
}

// --global does not lift the refusal: the caller's project is resolved before
// the write is redirected to the store-wide default.
func TestConfigSetRefusesAnUnanchoredCallerWithNoProject(t *testing.T) {
	st := openStore(t)
	if _, err := st.EnsureProject("/repos/first", "first", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	env := projectEnv(st, "/scratch/unseen", false)

	for _, global := range []bool{false, true} {
		_, err := verb.ConfigSet(context.Background(), env,
			verb.ConfigSetInput{Key: attemptMax, Value: "3", Global: global})
		requireVerbCode(t, err, output.ErrValidation, "cannot tell which project this belongs to")
		requireUnregistered(t, st, "/scratch/unseen")
	}
	stored, err := st.GetConfig(0, attemptMax)
	if err != nil {
		t.Fatalf("GetConfig(0): %v", err)
	}
	if stored.Source != "default" {
		t.Errorf("store-wide %s = %+v after refused writes, want it untouched", attemptMax, stored)
	}
}

// The registration happens before the write is validated, so a refused write
// still reports the row it registered for the caller to record.
func TestConfigSetReportsTheProjectItRegisteredWhenTheWriteIsRefused(t *testing.T) {
	for _, in := range []verb.ConfigSetInput{
		{Key: "no.such.key", Value: "1"},
		{Key: attemptMax, Value: "lots"},
	} {
		t.Run(in.Key, func(t *testing.T) {
			st := openStore(t)
			if _, err := st.EnsureProject("/repos/first", "first", 1); err != nil {
				t.Fatalf("claiming the default project: %v", err)
			}

			got, err := verb.ConfigSet(context.Background(), projectEnv(st, "/repos/new", true), in)
			requireVerbCode(t, err, output.ErrValidation, in.Key)
			id, found, lerr := st.LookupProject("/repos/new")
			if lerr != nil || !found {
				t.Fatalf("LookupProject(/repos/new) = (%d, %v, %v), want the registered row", id, found, lerr)
			}
			if got.Registered == nil || got.Registered.ID != id || got.Registered.Identity != "/repos/new" {
				t.Errorf("Registered = %+v, want the new row %d for /repos/new", got.Registered, id)
			}
		})
	}
}

// A vote-rule write records a config-changed event naming env.Author as the
// writer; a write outside that family records none.
func TestConfigSetAttributesAVoteRuleChangeToTheAuthor(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "operator-a"
	key := store.VoteRuleThresholdKey("x")

	if _, err := verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: key, Value: "0.6"}); err != nil {
		t.Fatalf("ConfigSet(%s): %v", key, err)
	}
	if _, err := verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: attemptMax, Value: "4"}); err != nil {
		t.Fatalf("ConfigSet(%s): %v", attemptMax, err)
	}

	events := configChangedEvents(t, st)
	if len(events) != 1 {
		t.Fatalf("%d config-changed events, want 1 for the vote-rule write only: %+v", len(events), events)
	}
	if events[0].Key != key || events[0].Value != "0.6" || events[0].ChangedBy != "operator-a" {
		t.Errorf("event = %+v, want %s=0.6 changed by operator-a", events[0], key)
	}
}

type configChangedEvent struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	ChangedBy string `json:"changed_by"`
}

func configChangedEvents(t *testing.T, st store.Store) []configChangedEvent {
	t.Helper()
	tx, err := st.BeginReadSnapshot()
	if err != nil {
		t.Fatalf("BeginReadSnapshot: %v", err)
	}
	defer tx.Rollback()
	rows, err := tx.ListEventsTx(store.EventFilter{Kind: engine.EventConfigChanged}, 0, 0)
	if err != nil {
		t.Fatalf("ListEventsTx: %v", err)
	}
	out := make([]configChangedEvent, 0, len(rows))
	for _, row := range rows {
		var e configChangedEvent
		if err := json.Unmarshal([]byte(row.Data), &e); err != nil {
			t.Fatalf("decoding event data %q: %v", row.Data, err)
		}
		out = append(out, e)
	}
	return out
}

func TestConfigSetRefusesAnUnknownKeyOrInvalidValueAsValidation(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)

	_, err := verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: "no.such.key", Value: "1"})
	requireVerbCode(t, err, output.ErrValidation, "no.such.key")
	if !errors.Is(err, store.ErrUnknownConfigKey) {
		t.Errorf("error %v does not wrap store.ErrUnknownConfigKey", err)
	}

	_, err = verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: attemptMax, Value: "lots"})
	requireVerbCode(t, err, output.ErrValidation, attemptMax)
}

// The lease.ttl.<class> warning: a step's class defaults to its executor
// name, so a TTL keyed on a class no registered workflow declares binds
// nothing. ConfigSet stores the value anyway and says so.

func TestConfigSetWarnsOnAClassNothingDeclares(t *testing.T) {
	st := openStore(t)
	insertClassFixture(t, st)
	key := store.KeyLeaseTTLPrefix + "read"

	got, err := verb.ConfigSet(context.Background(), projectEnv(st, "", false),
		verb.ConfigSetInput{Key: key, Value: "30m"})
	if err != nil {
		t.Fatalf("ConfigSet(%s): %v", key, err)
	}
	if got.Warning == "" {
		t.Fatalf("no warning for %s; it binds nothing", key)
	}
	for _, needle := range []string{"read", "impl", "serialize", "EXECUTOR NAME"} {
		if !strings.Contains(got.Warning, needle) {
			t.Errorf("the warning does not mention %q:\n%s", needle, got.Warning)
		}
	}
	stored, err := st.GetConfig(store.DefaultProjectID, key)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if stored.Value != "30m" {
		t.Errorf("%s = %+v, want the warned value stored anyway", key, stored)
	}
}

func TestConfigSetIsSilentOnAKeyThatBinds(t *testing.T) {
	st := openStore(t)
	insertClassFixture(t, st)

	for _, in := range []verb.ConfigSetInput{
		{Key: store.KeyLeaseTTLPrefix + "impl", Value: "30m"},      // the executor default
		{Key: store.KeyLeaseTTLPrefix + "serialize", Value: "30m"}, // an explicit class
		{Key: store.KeyLeaseTTLDefault, Value: "30m"},              // never a class at all
		{Key: attemptMax, Value: "4"},                              // not a lease key
	} {
		got, err := verb.ConfigSet(context.Background(), projectEnv(st, "", false), in)
		if err != nil {
			t.Fatalf("ConfigSet(%s): %v", in.Key, err)
		}
		if got.Warning != "" {
			t.Errorf("ConfigSet(%s) warned when it should not:\n%s", in.Key, got.Warning)
		}
	}
}

func TestConfigSetIsSilentWithNoWorkflows(t *testing.T) {
	st := openStore(t)

	got, err := verb.ConfigSet(context.Background(), projectEnv(st, "", false),
		verb.ConfigSetInput{Key: store.KeyLeaseTTLPrefix + "read", Value: "30m"})
	if err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if got.Warning != "" {
		t.Errorf("warned on a project with no registered workflows:\n%s", got.Warning)
	}
}

// A --global lease TTL is the default every project falls back to, so its
// class is judged against every project's workflows, not the caller's.
func TestConfigSetGlobalJudgesALeaseClassAgainstEveryProject(t *testing.T) {
	st := openStore(t)
	if _, err := st.EnsureProject("/repos/first", "first", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	insertClassFixture(t, st)
	if _, err := st.EnsureProject("/repos/other", "other", 1); err != nil {
		t.Fatalf("EnsureProject(/repos/other): %v", err)
	}
	env := projectEnv(st, "/repos/other", true)
	read := store.KeyLeaseTTLPrefix + "read"

	local, err := verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: read, Value: "30m"})
	if err != nil {
		t.Fatalf("ConfigSet(%s): %v", read, err)
	}
	if local.Warning != "" {
		t.Errorf("project-scoped %s warned for a project with no workflows:\n%s", read, local.Warning)
	}

	global, err := verb.ConfigSet(context.Background(), env, verb.ConfigSetInput{Key: read, Value: "30m", Global: true})
	if err != nil {
		t.Fatalf("ConfigSet(%s, global): %v", read, err)
	}
	for _, needle := range []string{"read", "impl", "serialize"} {
		if !strings.Contains(global.Warning, needle) {
			t.Errorf("global %s warning does not mention %q:\n%s", read, needle, global.Warning)
		}
	}

	binds, err := verb.ConfigSet(context.Background(), env,
		verb.ConfigSetInput{Key: store.KeyLeaseTTLPrefix + "impl", Value: "30m", Global: true})
	if err != nil {
		t.Fatalf("ConfigSet(lease.ttl.impl, global): %v", err)
	}
	if binds.Warning != "" {
		t.Errorf("global lease.ttl.impl warned though another project declares impl:\n%s", binds.Warning)
	}
}

// insertClassFixture registers, in the default project, a definition with one
// class-declaring step and one that leans on the executor default. It goes
// through workflow.Parse and workflow.Canonical, as `workflow register` does,
// so the Parsed JSON is the interpretation the engine acts on.
func insertClassFixture(t *testing.T, st store.Store) {
	t.Helper()
	const src = `
[pipeline]
name = "classes"
version = 1

[match]
kind = ["task"]

[limits]
serialize = { max = 1 }

[[step]]
name = "build"
executor = "impl"
emits = "out"
after = []

[[step]]
name = "commit"
executor = "impl"
class = "serialize"
emits = "done"
after = ["build"]
`
	def, err := workflow.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	if err := workflow.Validate(def); err != nil {
		t.Fatalf("validating the fixture: %v", err)
	}
	parsed, err := workflow.Canonical(def)
	if err != nil {
		t.Fatalf("canonicalizing the fixture: %v", err)
	}
	if _, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: store.DefaultProjectID, Name: def.Pipeline.Name, Version: def.Pipeline.Version,
		Body: src, Parsed: string(parsed), SourceSHA256: "fixture",
	}, 1); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}
}
