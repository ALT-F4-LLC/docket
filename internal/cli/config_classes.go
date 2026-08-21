package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The `lease.ttl.<class>` discoverability warning (DKT-260).
//
// `lease.ttl.<class>` never bound for the classes the documentation implied.
// Every unforced reap of the current epoch — 10 of them — killed a HEALTHY step
// 15.4 to 30.3 minutes into its claim against a 15m default, and 312 completed
// non-write attempts show a survivorship wall at 14.4 minutes: the shape of a
// population truncated by a timer rather than by work finishing.
//
// THE ENGINE WAS INTERNALLY CONSISTENT. `config set`'s help called the class an
// opaque string and `engineconfig.go` matched it as one. What was missing was
// the fact that decides whether an operator's key binds anything: a step's
// `class` DEFAULTS TO ITS EXECUTOR NAME when the definition declares none
// (workflow/expand.go), so the only strings that ever appeared in that column
// were executor names — which no operator reading "per-class lease TTL" would
// think to configure.
//
// Core cannot fix this by learning what `read` and `write` mean. engine-spec §2
// makes the class name INSTANCE POLICY, `writeClassOf` is keyed on the declared
// bound precisely because "a core mechanism keyed on a class NAMED `write`
// would be unimplementable", and TestNoWriteClassLiteral greps core for the
// literal. So the fix is to make the mismatch VISIBLE at the moment it is made,
// and to say plainly where the strings come from.

// declaredClasses collects every `class` string the project's registered
// workflows declare, including the executor names steps inherit when they
// declare none.
//
// It reads the PARSED definitions rather than the TOML: `Parsed` is the pinned
// interpretation the engine itself acts on, so a class this reports is a class
// that can actually appear in the column. Re-parsing the source would be a
// second interpretation, and the one that disagreed would be this one.
func declaredClasses(conn *sql.DB, projectID int) ([]string, error) {
	workflows, _, err := db.ListWorkflows(conn, db.WorkflowListOptions{
		ProjectID: projectID,
	})
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for _, wf := range workflows {
		var def workflow.Definition
		if json.Unmarshal([]byte(wf.Parsed), &def) != nil {
			// A definition that will not decode is not this warning's problem.
			// Skipping it can only make the warning quieter, never wronger.
			continue
		}
		for _, step := range def.Steps {
			if step.Class != "" {
				seen[step.Class] = true
				continue
			}
			// The DEFAULT, and the whole reason this issue exists: a step with
			// no declared class carries its executor name instead.
			if step.Executor != "" {
				seen[step.Executor] = true
			}
			for _, hint := range step.Fanout {
				if hint != "" {
					seen[hint] = true
				}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for class := range seen {
		out = append(out, class)
	}
	sort.Strings(out)
	return out, nil
}

// leaseTTLClassWarning reports the warning for a `lease.ttl.<class>` key whose
// class no registered workflow declares, or "" when there is nothing to say.
//
// IT WARNS AND DOES NOT REFUSE. A workflow registered after the config is a
// perfectly ordinary order of operations, and a project may deliberately
// pre-configure a class it is about to introduce. What is not ordinary is
// setting a TTL that binds nothing and finding out 20 minutes into a run when a
// healthy step is reaped — so the fact is stated, and the decision stays the
// operator's. Same stance the activation preflight takes.
//
// It is also silent when the project has NO registered workflows: there is
// nothing to disagree with, and a warning naming an empty list would be noise
// on a fresh project's very first configuration.
func leaseTTLClassWarning(conn *sql.DB, projectID int, key string) string {
	class, ok := strings.CutPrefix(key, db.KeyLeaseTTLPrefix)
	if !ok || class == "" || key == db.KeyLeaseTTLDefault {
		return ""
	}

	classes, err := declaredClasses(conn, projectID)
	if err != nil || len(classes) == 0 {
		return ""
	}
	for _, declared := range classes {
		if declared == class {
			return ""
		}
	}

	return fmt.Sprintf(
		"warning: no registered workflow declares the class %q, so this TTL "+
			"will not bind to any step. A step's class is its `class` field, "+
			"which DEFAULTS TO THE EXECUTOR NAME when the definition declares "+
			"none — which is why the classes this project actually uses are: "+
			"%s. Declare `class = %q` on the steps you meant, or set the TTL "+
			"on one of those names.",
		class, strings.Join(classes, ", "), class)
}
