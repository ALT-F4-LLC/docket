package verb_test

import (
	"context"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// seedEvents appends events in order and returns their seqs. A zero runID
// is a store-level event, one that belongs to no run.
func seedEvents(t *testing.T, st store.Store, runIDs ...int) []int64 {
	t.Helper()
	tx, err := st.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	for i, runID := range runIDs {
		if err := tx.InsertEventTx(store.NewEvent{
			AtMS: int64(i + 1), Kind: "run-activated", RunID: runID, Data: `{}`,
		}); err != nil {
			t.Fatalf("InsertEventTx: %v", err)
		}
	}
	rows, err := tx.ListEventsTx(store.EventFilter{}, 0, len(runIDs))
	if err != nil {
		t.Fatalf("ListEventsTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var seqs []int64
	for _, row := range rows {
		seqs = append(seqs, row.Seq)
	}
	return seqs
}

func eventRuns(page verb.EventsListResult) []string {
	var runs []string
	for _, e := range page.Events {
		runs = append(runs, e.Run)
	}
	return runs
}

// The feed is the caller's project unless the input names all projects or a
// run, and a run is answered wherever it lives.
func TestEventsListScopesToTheCallersProject(t *testing.T) {
	st := openStore(t)
	here := ensureProject(t, st, "/repos/here", "here")
	there := ensureProject(t, st, "/repos/there", "there")
	hereRun := insertLedgerRun(t, st, here, 1)
	thereRun := insertLedgerRun(t, st, there, 1)
	seedEvents(t, st, hereRun, thereRun, thereRun)
	ctx := context.Background()
	fromHere := projectEnv(st, "/repos/here", true)

	scoped, err := verb.EventsList(ctx, fromHere, verb.EventsListInput{})
	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	if got := eventRuns(scoped); len(got) != 1 ||
		got[0] != model.FormatRunID(hereRun) || scoped.Total != 1 {
		t.Errorf("scoped feed = %v (total %d), want only %s", got, scoped.Total, model.FormatRunID(hereRun))
	}
	if scoped.Limit != verb.EventsDefaultLimit {
		t.Errorf("Limit = %d, want the default %d", scoped.Limit, verb.EventsDefaultLimit)
	}

	all, err := verb.EventsList(ctx, fromHere, verb.EventsListInput{AllProjects: true})
	if err != nil {
		t.Fatalf("EventsList(AllProjects): %v", err)
	}
	if all.Total != 3 {
		t.Errorf("AllProjects total = %d, want 3", all.Total)
	}

	theirs, err := verb.EventsList(ctx, fromHere, verb.EventsListInput{Run: model.FormatRunID(thereRun)})
	if err != nil {
		t.Fatalf("EventsList(Run): %v", err)
	}
	if theirs.Total != 2 {
		t.Errorf("another project's run answered total %d, want its 2", theirs.Total)
	}
}

// Tail selects the newest matches, still oldest first, and reports Tail as
// the bound that applied.
func TestEventsListTailReadsTheNewest(t *testing.T) {
	st := openStore(t)
	seqs := seedEvents(t, st, 0, 0, 0, 0)

	got, err := verb.EventsList(context.Background(), projectEnv(st, "", false),
		verb.EventsListInput{AllProjects: true, Tail: 2})
	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	if len(got.Events) != 2 || got.Events[0].Seq != seqs[2] || got.Events[1].Seq != seqs[3] {
		t.Errorf("tail page = %+v, want seqs %v", got.Events, seqs[2:])
	}
	if got.Limit != 2 || got.Total != 4 {
		t.Errorf("Limit, Total = %d, %d; want 2, 4", got.Limit, got.Total)
	}
}

func TestEventsListRefusesInvalidInput(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	for _, tc := range []struct {
		name string
		in   verb.EventsListInput
		want output.ErrorCode
	}{
		{"negative since", verb.EventsListInput{Since: -1}, output.ErrValidation},
		{"negative tail", verb.EventsListInput{Tail: -1}, output.ErrValidation},
		{"unknown kind", verb.EventsListInput{Kind: "no-such-kind"}, output.ErrValidation},
		{"malformed step", verb.EventsListInput{Step: "nope"}, output.ErrValidation},
		{"missing step", verb.EventsListInput{Step: "STEP-999"}, output.ErrNotFound},
		{"missing run", verb.EventsListInput{Run: "RUN-999"}, output.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verb.EventsList(context.Background(), env, tc.in)
			requireCode(t, err, tc.want)
		})
	}
}

// EventsList is a read: an anchored identity no project holds sees no
// project's events and is not registered.
func TestEventsListLeavesAnUnregisteredIdentityUnregistered(t *testing.T) {
	st := openStore(t)
	home := ensureProject(t, st, "/repos/home", "home")
	seedEvents(t, st, insertLedgerRun(t, st, home, 1))

	got, err := verb.EventsList(context.Background(), projectEnv(st, "/work/elsewhere", true),
		verb.EventsListInput{})
	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	if got.Total != 0 {
		t.Errorf("Total = %d, want 0", got.Total)
	}
	requireUnregistered(t, st, "/work/elsewhere")
}

// A deletion needs Yes; without it nothing is deleted.
func TestEventsPruneRefusesAnUnconfirmedDeletion(t *testing.T) {
	st := openStore(t)
	seqs := seedEvents(t, st, 0, 0, 0)
	env := projectEnv(st, "", false)

	_, err := verb.EventsPrune(context.Background(), env, verb.EventsPruneInput{Before: seqs[2]})
	requireCode(t, err, output.ErrValidation)

	left, err := verb.EventsList(context.Background(), env, verb.EventsListInput{AllProjects: true})
	if err != nil {
		t.Fatalf("EventsList: %v", err)
	}
	if left.Total != 3 {
		t.Errorf("an unconfirmed prune left %d events, want all 3", left.Total)
	}
}

// DryRun reports without deleting; Yes deletes what DryRun reported.
func TestEventsPruneDryRunThenDelete(t *testing.T) {
	st := openStore(t)
	seqs := seedEvents(t, st, 0, 0, 0)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	preview, err := verb.EventsPrune(ctx, env, verb.EventsPruneInput{Before: seqs[2], DryRun: true})
	if err != nil {
		t.Fatalf("EventsPrune(DryRun): %v", err)
	}
	if !preview.Prune.DryRun || preview.Prune.Pruned != 2 {
		t.Errorf("dry run = %+v, want 2 events reported and none deleted", preview.Prune)
	}

	done, err := verb.EventsPrune(ctx, env, verb.EventsPruneInput{Before: seqs[2], Yes: true})
	if err != nil {
		t.Fatalf("EventsPrune(Yes): %v", err)
	}
	if done.Prune.DryRun || done.Prune.Pruned != 2 {
		t.Errorf("prune = %+v, want 2 events deleted", done.Prune)
	}
	// A cursor below what survives is told so rather than handed a short page.
	_, err = verb.EventsList(ctx, env, verb.EventsListInput{AllProjects: true, Since: seqs[0]})
	requireCode(t, err, output.ErrGone)
	rest, err := verb.EventsList(ctx, env, verb.EventsListInput{AllProjects: true, Since: seqs[1]})
	if err != nil {
		t.Fatalf("EventsList from the retained minimum: %v", err)
	}
	if len(rest.Events) == 0 || rest.Events[0].Seq != seqs[2] {
		t.Errorf("after the prune the feed starts at %+v, want seq %d", rest.Events, seqs[2])
	}
}

func TestEventsPruneRefusesAMalformedBeforeRun(t *testing.T) {
	st := openStore(t)
	_, err := verb.EventsPrune(context.Background(), projectEnv(st, "", false),
		verb.EventsPruneInput{BeforeRun: "nope", Yes: true})
	requireCode(t, err, output.ErrValidation)
}

// EventsPrune is run-addressed: an anchored caller's project is registered on
// first contact and reported for its event, even when the prune is refused.
func TestEventsPruneRegistersAnAnchoredCaller(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")

	res, err := verb.EventsPrune(context.Background(), projectEnv(st, "/repos/new", true),
		verb.EventsPruneInput{Before: 1})
	requireCode(t, err, output.ErrValidation)
	id, found, lookupErr := st.LookupProject("/repos/new")
	if lookupErr != nil || !found {
		t.Fatalf("LookupProject: found=%v err=%v, want the registered row", found, lookupErr)
	}
	if reg := res.Registered; reg == nil || reg.ID != id || reg.Verb != "events prune" {
		t.Errorf("Registered = %+v, want project %d registered by events prune", reg, id)
	}

	res, err = verb.EventsPrune(context.Background(), projectEnv(st, "/work/scratch", false),
		verb.EventsPruneInput{Before: 1, DryRun: true})
	if err != nil {
		t.Fatalf("EventsPrune from an unanchored directory: %v", err)
	}
	if res.Registered != nil {
		t.Errorf("an unanchored caller registered %+v", res.Registered)
	}
	requireUnregistered(t, st, "/work/scratch")
}
