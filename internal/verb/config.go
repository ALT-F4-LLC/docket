package verb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// ConfigGetInput names the engine config key to read; an empty Key lists every
// key. Global reads the store-wide values, ignoring the project's overrides.
type ConfigGetInput struct {
	Key    string
	Global bool
}

// ConfigGetResult holds Entry when a key was named and Entries when none was.
type ConfigGetResult struct {
	Entry   store.ConfigEntry
	Entries []store.ConfigEntry
}

// ConfigGet reads one engine config value, or lists them all, for the caller's
// project. It is a read, so it never registers a project.
func ConfigGet(ctx context.Context, env Env, in ConfigGetInput) (ConfigGetResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return ConfigGetResult{}, err
	}
	if in.Global {
		projectID = 0
	}

	if in.Key == "" {
		entries, err := env.Store.ListConfig(projectID)
		if err != nil {
			return ConfigGetResult{}, newError(fmt.Errorf("listing config: %w", err), output.ErrGeneral)
		}
		return ConfigGetResult{Entries: entries}, nil
	}

	entry, err := env.Store.GetConfig(projectID, in.Key)
	if err != nil {
		if errors.Is(err, store.ErrUnknownConfigKey) {
			return ConfigGetResult{}, newError(err, output.ErrValidation)
		}
		return ConfigGetResult{}, newError(fmt.Errorf("reading config: %w", err), output.ErrGeneral)
	}
	return ConfigGetResult{Entry: entry}, nil
}

// ConfigSetInput is the engine config key and value to store. Global writes
// the store-wide default instead of the caller's project override.
type ConfigSetInput struct {
	Key    string
	Value  string
	Global bool
}

// ConfigSetResult is the stored entry. Warning is non-empty when the value
// binds nothing, as for a lease.ttl.<class> no registered workflow declares.
// Registered is the row this call registered, nil when it already existed;
// the caller records the project-registered event for it, since that write
// is outside store.Store.
type ConfigSetResult struct {
	Entry      store.ConfigEntry
	Warning    string
	Registered *model.Project
}

// ConfigSet stores one engine config value for the caller's project, or the
// store-wide default under Global. It is a write, so it may register an
// anchored project; Registered is set even when the write is refused,
// because the registration has already happened.
func ConfigSet(ctx context.Context, env Env, in ConfigSetInput) (ConfigSetResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return ConfigSetResult{}, err
	}
	var result ConfigSetResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}
	if in.Global {
		projectID = 0
	}

	// Through the engine rather than the store directly (DKT-2767): a write
	// to a vote-rule key records a `config-changed` event in the write's own
	// transaction, attributed to env.Author, so a rewrite of a
	// security-load-bearing key is detectable even though nothing yet
	// prevents it.
	if err := engine.SetConfigStore(env.Store, projectID, in.Key, in.Value, env.Author); err != nil {
		if errors.Is(err, store.ErrUnknownConfigKey) {
			return result, newError(err, output.ErrValidation)
		}
		// A validation failure on the value is the other expected case;
		// anything else is a storage failure.
		if strings.Contains(err.Error(), "storing config") {
			return result, newError(err, output.ErrGeneral)
		}
		return result, newError(err, output.ErrValidation)
	}

	// AFTER the write, not before (DKT-260). The value is valid and was
	// stored; this is a disclosure about what it will bind to, and
	// withholding the write over it would refuse a configuration the
	// operator may be setting ahead of the workflow that uses it.
	result.Entry = store.ConfigEntry{Key: in.Key, Value: in.Value, Source: "set"}
	result.Warning = leaseTTLClassWarning(env.Store, projectID, in.Key)
	return result, nil
}

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
// Core cannot fix this by learning what `read` and `write` mean. The class name
// is INSTANCE POLICY, `writeClassOf` is keyed on the declared
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
func declaredClasses(st store.Store, projectID int) ([]string, error) {
	workflows, _, err := st.ListWorkflows(store.WorkflowListOptions{
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
func leaseTTLClassWarning(st store.Store, projectID int, key string) string {
	class, ok := strings.CutPrefix(key, store.KeyLeaseTTLPrefix)
	if !ok || class == "" || key == store.KeyLeaseTTLDefault {
		return ""
	}

	classes, err := declaredClasses(st, projectID)
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
