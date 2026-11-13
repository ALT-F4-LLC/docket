package verb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// ReportExecutorsInput bounds the ledger's window. Since is a run (`RUN-N`,
// or a bare N) or an instant (`2026-09-01`, or RFC 3339); empty sets no
// floor. AllProjects reads every project's runs instead of the caller's.
type ReportExecutorsInput struct {
	Since       string
	AllProjects bool
}

// ReportExecutorsResult is the ledger plus the window it was read over. Scope
// is `project` or `store`; Since is the input's canonical form, empty when
// none was given.
type ReportExecutorsResult struct {
	Ledger *engine.ExecutorLedger
	Scope  string
	Since  string
}

// ReportExecutors rolls up, across the runs in the window, what became of the
// steps each executor hint ran and the panels each voter name sat on. It is a
// read: a project-scoped window from an identity no project holds is refused
// rather than answered as an empty ledger, and nothing is registered.
func ReportExecutors(ctx context.Context, env Env, in ReportExecutorsInput) (ReportExecutorsResult, error) {
	sinceRun, sinceMS, sinceLabel, err := parseLedgerSince(in.Since)
	if err != nil {
		return ReportExecutorsResult{}, newError(err, output.ErrValidation)
	}

	opts := engine.ExecutorLedgerOptions{SinceRun: sinceRun, SinceMS: sinceMS}
	scope := "store"
	if !in.AllProjects {
		projectID, _, err := env.ResolveProject(ProjectReadOnly)
		if err != nil {
			return ReportExecutorsResult{}, err
		}
		if projectID == store.UnregisteredProjectID {
			return ReportExecutorsResult{}, newError(fmt.Errorf(
				"%s has no docket project binding; run `docket report executors "+
					"--all-projects`, or from inside the repository", env.Cwd),
				output.ErrValidation)
		}
		opts.ProjectID = projectID
		scope = "project"
	}

	ledger, err := engine.LoadExecutorLedgerStore(env.Store, opts)
	if err != nil {
		return ReportExecutorsResult{}, engineError(err)
	}
	return ReportExecutorsResult{Ledger: ledger, Scope: scope, Since: sinceLabel}, nil
}

// parseLedgerSince reads since as either a run id (`RUN-N`, or a bare N) or
// an instant (`2026-09-01`, or RFC 3339), and returns the floor in the form
// the loader takes plus the canonical label the result echoes.
//
// A bare number is a run id and never a year: a retro window is "every run
// since the last retro", which is a run, and a date carries dashes.
func parseLedgerSince(raw string) (sinceRun int, sinceMS int64, label string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0, "", nil
	}
	if id, err := model.ParseRunID(raw); err == nil {
		return id, 0, model.FormatRunID(id), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if at, err := time.Parse(layout, raw); err == nil {
			return 0, at.UnixMilli(), at.UTC().Format(time.RFC3339), nil
		}
	}
	return 0, 0, "", fmt.Errorf(
		"--since %q: want a run (RUN-N), a date (2026-09-01), or an RFC 3339 timestamp", raw)
}
