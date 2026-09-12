package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// CROSS-PROJECT TARGETING for the three registry-WRITING verbs (DKT-615).
//
// `workflow register`, `workflow deprecate`, and `schema register` write rows
// that are project-scoped — UNIQUE(project_id, name, version) — but each of
// them picked its project from getProjectID(cmd) and nowhere else. So
// "retire this orphaned name" and "register the current corpus" meant one
// invocation per project, run from inside that project's checkout, with no
// verb anywhere that would say whether the other twelve had been done. A
// deprecate run once from one repository retired the version in project 2 and
// left the identical row active in eleven others, silently.
//
// `docket registry audit` (DKT-614) is the READ half of the same gap and is
// the shape followed here: resolve one project or loop every project, produce
// ONE report, and name the project each outcome belongs to.
//
// THREE RULES the fan-out does not get to bend:
//
//  1. THE DEFAULT IS UNCHANGED. With neither flag the verb runs exactly the
//     code it ran before, against the cwd's project, and emits exactly the
//     payload it emitted before. A consumer parsing `data` as a workflow row
//     keeps parsing a workflow row.
//
//  2. EACH PROJECT IS JUDGED ON ITS OWN. Idempotency, conflict, and
//     not-registered are decided per project by the same taxonomy the
//     single-project path uses (workflowErr / schemaErr). One project's
//     CONFLICT never cancels another project's success, and never hides it:
//     every target appears in the report with its own outcome.
//
//  3. A PARTIAL FAILURE IS STILL A FAILURE. The report is written to stdout —
//     a machine consumer needs the per-project detail precisely when something
//     went wrong — and the process then exits non-zero without a second
//     envelope. See reportedFailure in root.go.

// registryOutcome is the closed vocabulary of what happened in one project.
//
// It is a WORD rather than a bool because the interesting results are not
// success-vs-failure: "unchanged" and "registered" are both successes that mean
// different things to an operator sweeping a corpus, and "already-deprecated"
// is a refusal that means the work was already done.
const (
	outcomeRegistered        = "registered"
	outcomeUnchanged         = "unchanged"
	outcomeDeprecated        = "deprecated"
	outcomeRestored          = "restored"
	outcomeAlreadyBinding    = "already-binding"
	outcomeAlreadyDeprecated = "already-deprecated"
	outcomeConflict          = "conflict"
	outcomeNotFound          = "not-registered"
	outcomeInvalid           = "invalid"
	outcomeError             = "error"
)

// registryFanoutResult is one project's outcome.
type registryFanoutResult struct {
	ProjectID int    `json:"project_id"`
	Project   string `json:"project"`
	Identity  string `json:"identity,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	// Outcome is one of the constants above.
	Outcome string `json:"outcome"`
	// Ref is the name@version as the store holds it, when a row was reached.
	Ref string `json:"ref,omitempty"`
	// Detail carries the failure's own message, verbatim, so the report says
	// WHY this project differed rather than only that it did.
	Detail string `json:"detail,omitempty"`
	// Code is the error code this project's outcome would have exited with on
	// its own. Empty on success; its presence is what makes a result a failure.
	Code output.ErrorCode `json:"code,omitempty"`
}

func (r registryFanoutResult) failed() bool { return r.Code != "" }

// registryFanoutReport is the whole invocation's result.
//
// NOT an output.Collection: the v2 collection envelope replaces `data` with
// {items, total, truncated}, which would drop the counts and the scope — and
// "3 of 13 projects failed" is the line the operator ran the command to read.
// `registry audit` declines the collection envelope for the same reason.
type registryFanoutReport struct {
	// Operation is the verb, as typed: "workflow register", "schema register",
	// "workflow deprecate", "workflow restore".
	Operation string `json:"operation"`
	// Subject is what was operated on — the ref, or the source path.
	Subject string `json:"subject"`
	// Scope is "project" or "all-projects".
	Scope     string                 `json:"scope"`
	Results   []registryFanoutResult `json:"results"`
	Succeeded int                    `json:"succeeded"`
	Failed    int                    `json:"failed"`
}

const (
	scopeOneProject  = "project"
	scopeAllProjects = "all-projects"
)

// addRegistryTargetFlags declares the two targeting flags on a registry-writing
// verb. action is the verb's own word ("Register", "Retire") so each help text
// reads as a sentence about that verb.
func addRegistryTargetFlags(cmd *cobra.Command, action string) {
	cmd.Flags().String("project", "", fmt.Sprintf(
		"%s in this project instead of the one the working directory resolves to "+
			"(its PREFIX, NAME, IDENTITY, or row id)", action))
	cmd.Flags().Bool("all-projects", false, fmt.Sprintf(
		"%s in EVERY project in the store, reporting each project's own outcome", action))
	cmd.MarkFlagsMutuallyExclusive("project", "all-projects")
}

// resolveRegistryTargets answers which projects this invocation writes to.
//
// The bool is FANNED-OUT, not "found something": false means neither flag was
// given and the caller must take its original single-project path, ambient
// project and original payload shape included. It is a separate return rather
// than a nil slice because "no flags" and "an empty store" are different
// answers and only one of them is normal.
//
// The mutual exclusion is checked HERE as well as by cobra's
// MarkFlagsMutuallyExclusive: cobra enforces it during flag parsing, which a
// caller constructing a command directly (every unit test in this package)
// never runs.
func resolveRegistryTargets(
	cmd *cobra.Command, conn *sql.DB,
) (targets []*model.Project, fannedOut bool, err error) {
	ref, _ := cmd.Flags().GetString("project")
	ref = strings.TrimSpace(ref)
	all, _ := cmd.Flags().GetBool("all-projects")

	switch {
	case ref != "" && all:
		return nil, false, cmdErr(fmt.Errorf(
			"--project and --all-projects both name the targets and disagree: "+
				"pass one project's ref, or --all-projects for every project in "+
				"the store"), output.ErrValidation)

	case ref != "":
		target, err := resolveProjectRef(conn, ref)
		if err != nil {
			return nil, false, err
		}
		return []*model.Project{target}, true, nil

	case all:
		projects, err := db.ListProjects(conn)
		if err != nil {
			return nil, false, cmdErr(fmt.Errorf("listing projects: %w", err),
				output.ErrGeneral)
		}
		if len(projects) == 0 {
			// Unreachable through the ordinary store — the default row is
			// created at initialization — but a fan-out that silently reported
			// zero targets would look like a success that wrote nothing.
			return nil, false, cmdErr(fmt.Errorf(
				"--all-projects found no projects in this store"), output.ErrNotFound)
		}
		return projects, true, nil

	default:
		return nil, false, nil
	}
}

// fanoutScope names the scope a resolved target set represents.
func fanoutScope(cmd *cobra.Command) string {
	if all, _ := cmd.Flags().GetBool("all-projects"); all {
		return scopeAllProjects
	}
	return scopeOneProject
}

// registryFailureResult turns one project's error into its row in the report,
// using the verb's OWN error taxonomy (workflowErr / schemaErr) so a fanned-out
// refusal is classified exactly as the single-project refusal would be.
func registryFailureResult(
	p *model.Project, err error, classify func(error) error,
) registryFanoutResult {
	code := output.ErrGeneral
	var ce *CmdError
	if errors.As(classify(err), &ce) {
		code = ce.Code
	}

	outcome := outcomeError
	switch {
	case errors.Is(err, db.ErrWorkflowAlreadyDeprecated):
		// The ONE sentinel workflowErr does not classify, because the
		// single-project path answers it before reaching that mapper. Retiring
		// twice is a CONFLICT there (db.ErrWorkflowAlreadyDeprecated's own
		// header: the second caller's "I am taking this out of service" is
		// wrong), and it has to be a CONFLICT here too — a fan-out that
		// downgraded it would make the same store answer differently depending
		// on which flag was passed.
		outcome, code = outcomeAlreadyDeprecated, output.ErrConflict
	case code == output.ErrConflict:
		outcome = outcomeConflict
	case code == output.ErrNotFound:
		outcome = outcomeNotFound
	case code == output.ErrValidation:
		outcome = outcomeInvalid
	}

	return registryFanoutResult{
		ProjectID: p.ID, Project: projectDisplayName(p),
		Identity: p.Identity, Prefix: p.Prefix,
		Outcome: outcome, Detail: err.Error(), Code: code,
	}
}

// registrySuccessResult is the success counterpart, so both halves of a loop
// build the same row shape from the same place.
func registrySuccessResult(p *model.Project, outcome, ref string) registryFanoutResult {
	return registryFanoutResult{
		ProjectID: p.ID, Project: projectDisplayName(p),
		Identity: p.Identity, Prefix: p.Prefix,
		Outcome: outcome, Ref: ref,
	}
}

// finishRegistryFanout writes the report and decides the invocation's fate.
//
// THE REPORT IS ALWAYS WRITTEN, failure included: the per-project detail is
// what the caller needs most when something went wrong, and an error envelope
// carrying one flattened string would throw away twelve projects' outcomes to
// describe one.
func finishRegistryFanout(w *output.Writer, report *registryFanoutReport) error {
	for _, r := range report.Results {
		if r.failed() {
			report.Failed++
			continue
		}
		report.Succeeded++
	}

	var message string
	if !w.JSONMode {
		message = renderRegistryFanout(report)
	}
	w.Success(report, message)

	if report.Failed == 0 {
		return nil
	}
	return &reportedFailure{Code: fanoutExitCode(report)}
}

// fanoutExitCode picks the code the process exits with when some targets
// failed.
//
// ONE CODE WHEN THEY AGREE, GENERAL_ERROR WHEN THEY DO NOT. The agreeing case
// is the one that matters: `--project X` targets exactly one project, so a
// conflict there exits 4 exactly as the same command without the flag would —
// the flag changes which project is written, not what a conflict costs. Mixed
// failures have no single honest code, and inventing a precedence between
// CONFLICT and NOT_FOUND would tell a script something the store never said;
// exit 1 sends it to the report, which holds both.
func fanoutExitCode(report *registryFanoutReport) output.ErrorCode {
	var code output.ErrorCode
	for _, r := range report.Results {
		if !r.failed() {
			continue
		}
		if code == "" {
			code = r.Code
			continue
		}
		if code != r.Code {
			return output.ErrGeneral
		}
	}
	if code == "" {
		return output.ErrGeneral
	}
	return code
}

// renderRegistryFanout is the human report: one line per project, aligned, then
// the count.
func renderRegistryFanout(report *registryFanoutReport) string {
	var b strings.Builder

	scope := "every project in the store"
	if report.Scope == scopeOneProject {
		scope = "1 project"
	}
	fmt.Fprintf(&b, "%s %s — %s (%d target(s))\n\n",
		report.Operation, report.Subject, scope, len(report.Results))

	nameWidth, outcomeWidth := 4, 4
	for _, r := range report.Results {
		nameWidth = max(nameWidth, len(describeFanoutProject(r)))
		outcomeWidth = max(outcomeWidth, len(r.Outcome))
	}

	for _, r := range report.Results {
		fmt.Fprintf(&b, "  %-*s  %-*s  %s\n",
			nameWidth, describeFanoutProject(r),
			outcomeWidth, r.Outcome,
			fanoutDetail(r))
	}

	b.WriteString("\n")
	if report.Failed == 0 {
		fmt.Fprintf(&b, "%d project(s) succeeded.", report.Succeeded)
		return b.String()
	}
	fmt.Fprintf(&b,
		"%d project(s) succeeded, %d failed. Nothing was rolled back: each "+
			"project's registry is its own, and the projects that succeeded "+
			"are done.", report.Succeeded, report.Failed)
	return b.String()
}

// describeFanoutProject names a project the way `project list` and `registry
// audit` do, so the three outputs can be read against each other.
func describeFanoutProject(r registryFanoutResult) string {
	name := r.Project
	if name == "" {
		name = fmt.Sprintf("project %d", r.ProjectID)
	}
	if r.Prefix != "" {
		return fmt.Sprintf("%s (%s)", name, r.Prefix)
	}
	return name
}

func fanoutDetail(r registryFanoutResult) string {
	if r.Detail != "" {
		return r.Detail
	}
	return r.Ref
}
