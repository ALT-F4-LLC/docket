package verb

import (
	"context"
	"path/filepath"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// DoctorInput is one doctor invocation. Run and Source are optional; each
// one left empty makes its check SKIP.
type DoctorInput struct {
	// Run names the run whose pins are verified, e.g. "RUN-3".
	Run string
	// Source is a dotfiles-shaped checkout root compared against the shared
	// config root and bin.
	Source string
	// DBPath is the store's database file, which the store check re-opens
	// read-write.
	DBPath string
}

// DoctorResult is one row per check, in a fixed order, plus the two summary
// bits engine.DoctorReport defines.
type DoctorResult struct {
	Clean   bool
	Skipped bool
	Checks  []engine.DoctorCheck
}

// Doctor runs the engine's environment checks against env.Cwd and appends the
// postgres check. It is a read: an identity no project holds is reported as
// unbound and never registered.
func Doctor(ctx context.Context, env Env, in DoctorInput) (DoctorResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return DoctorResult{}, err
	}

	var runID int
	if in.Run != "" {
		if runID, err = ParseRunRef(in.Run); err != nil {
			return DoctorResult{}, err
		}
	}

	// The seat check compares the cwd against git's toplevel, which git
	// reports with symlinks resolved (macOS: /var vs /private/var).
	cwd := env.Cwd
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}

	report := engine.DoctorStore(env.Store, engine.DoctorOptions{
		Cwd: cwd, DBPath: in.DBPath, ProjectID: projectID, Identity: env.Project.Identity,
		RunID: runID, SourceRoot: in.Source, NowMS: model.NowMS(),
	})
	return DoctorResult{
		Clean:   report.Clean,
		Skipped: report.Skipped,
		Checks:  append(report.Checks, postgresCheck(config.PostgresFromEnv().DSN)),
	}, nil
}

// postgresCheck reports whether a PostgreSQL DSN is configured. Its detail is
// one of two constants and never derives from the DSN: even the redacted form
// carries the username, and the DSN may embed a password by misconfiguration.
// It is a report and never moves Clean.
func postgresCheck(dsn config.PostgresDSN) engine.DoctorCheck {
	detail := "not configured"
	if dsn.IsSet() {
		detail = "configured; DSN withheld"
	}
	return engine.DoctorCheck{Check: "postgres", Verdict: engine.DoctorOK, Detail: detail}
}
