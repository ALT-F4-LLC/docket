package verb

import (
	"context"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// EventsDefaultLimit is the page size a zero Limit asks for. A cursor feed
// pages by default, so a first-time reader is not handed the whole history.
const EventsDefaultLimit = 100

// EventsListInput selects a page of the event log. Since is the cursor
// (events with a greater seq); Tail, when positive, selects the newest Tail
// matches instead; Limit zero is EventsDefaultLimit. Kind and Step (`STEP-N`)
// filter before Limit and Tail. Run (`RUN-N`) names one run in whichever
// project owns it. AllProjects reads every project's events instead of the
// caller's.
type EventsListInput struct {
	Since       int64
	Limit       int
	Tail        int
	Kind        string
	Step        string
	Run         string
	AllProjects bool
}

// EventsListResult is the page and the count of every match. Limit is the
// bound that applied, Tail under a tail read, so a caller can tell whether
// the page was truncated.
type EventsListResult struct {
	Events []engine.Event
	Total  int
	Limit  int
}

// EventsList reads one page of the event log, oldest first. It is a read: the
// feed is scoped to the caller's project unless the input names a run or all
// projects, and nothing is registered.
func EventsList(ctx context.Context, env Env, in EventsListInput) (EventsListResult, error) {
	q, err := EventsQuery(env, in)
	if err != nil {
		return EventsListResult{}, err
	}
	page, err := engine.ListEventsStore(env.Store, q)
	if err != nil {
		return EventsListResult{}, engineError(err)
	}
	limit := q.Limit
	if q.Tail > 0 {
		limit = q.Tail
	}
	return EventsListResult{Events: page.Events, Total: page.Total, Limit: limit}, nil
}

// EventsQuery validates in and resolves its references into the query
// EventsList reads. It is exported for the CLI's `--follow`, which streams
// the same feed and so must accept and refuse exactly what EventsList does.
func EventsQuery(env Env, in EventsListInput) (engine.EventQuery, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return engine.EventQuery{}, err
	}
	if in.AllProjects {
		projectID = 0
	}

	if in.Since < 0 {
		return engine.EventQuery{}, newError(
			fmt.Errorf("--since must not be negative: %d", in.Since), output.ErrValidation)
	}
	if in.Tail < 0 {
		return engine.EventQuery{}, newError(
			fmt.Errorf("--tail must not be negative: %d", in.Tail), output.ErrValidation)
	}
	limit := in.Limit
	if limit == 0 {
		limit = EventsDefaultLimit
	}

	if err := engine.ValidateEventKind(in.Kind); err != nil {
		return engine.EventQuery{}, engineError(err)
	}
	stepID, err := engine.ResolveStepFilterStore(env.Store, in.Step)
	if err != nil {
		return engine.EventQuery{}, engineError(err)
	}
	runID, err := engine.ResolveRunFilterStore(env.Store, in.Run)
	if err != nil {
		return engine.EventQuery{}, engineError(err)
	}

	return engine.EventQuery{
		Since: in.Since, RunID: runID, Limit: limit, Tail: in.Tail,
		Kind: in.Kind, StepID: stepID, ProjectID: projectID,
	}, nil
}

// EventsPruneInput names what to delete: events below seq Before (narrowed
// to one run by Run), or every event of BeforeRun (`RUN-N`). DryRun reports
// the deletion without making it; Yes confirms a deletion.
type EventsPruneInput struct {
	Before    int64
	BeforeRun string
	Run       string
	DryRun    bool
	Yes       bool
}

// EventsPruneResult is what the prune deleted, or would delete. BeforeRun is
// the input's run in canonical form, empty when none was given. Registered is
// set when the call registered the caller's project, for the caller to record
// as an event.
type EventsPruneResult struct {
	Prune      *engine.PruneResult
	BeforeRun  string
	Registered *engine.ProjectRegistration
}

// EventsPrune deletes events permanently. The engine refuses live runs'
// events and events inside the retention window; this verb refuses a
// deletion that Yes does not confirm. It is addressed by run, so it is
// run-addressed.
func EventsPrune(ctx context.Context, env Env, in EventsPruneInput) (EventsPruneResult, error) {
	registered, err := resolveRunAddressedProject(env, "events prune")
	res := EventsPruneResult{Registered: registered}
	if err != nil {
		return res, err
	}

	var beforeRun int
	if in.BeforeRun != "" {
		id, err := model.ParseRunID(in.BeforeRun)
		if err != nil {
			return res, newError(err, output.ErrValidation)
		}
		beforeRun = id
		res.BeforeRun = model.FormatRunID(id)
	}

	runID, err := engine.ResolveRunFilterStore(env.Store, in.Run)
	if err != nil {
		return res, engineError(err)
	}

	// Required whether or not a terminal is attached, so piping the output
	// somewhere never turns a preview into a deletion.
	if !in.DryRun && !in.Yes {
		return res, newError(errors.New("events prune deletes events permanently; "+
			"pass --yes to confirm, or --dry-run to see what it would delete"),
			output.ErrValidation)
	}

	res.Prune, err = engine.PruneEventsStore(env.Store, engine.PruneQuery{
		Before:    in.Before,
		BeforeRun: beforeRun,
		RunID:     runID,
		DryRun:    in.DryRun,
		NowMS:     model.NowMS(),
	})
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}
