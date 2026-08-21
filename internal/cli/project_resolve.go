package cli

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// WHICH PROJECT AN INVOCATION BELONGS TO — and, separately, whether this
// invocation is allowed to bring one into being (DKT-58).
//
// Those used to be one question. The root hook called db.EnsureProject on every
// verb, so `docket issue list` run once from a directory that was not a
// repository left a permanent project row behind: a judge executor recording a
// step from the shared scratchpad root minted a row named `claude-501`, which
// then collided on the hardcoded display prefix (DKT-60) and could not be
// removed by any supported verb (DKT-59).
//
// The rule is now two-part, and both parts have to hold:
//
//  1. THE IDENTITY MUST BE ANCHORED — a git worktree, or a deliberate `.docket`
//     store. config.Config.Anchored carries this. Outside a repository the
//     identity is whatever directory the process started in, which is a guess,
//     and a guess must not create permanent state.
//
//  2. THE VERB MUST NOT BE A READ. Reading is observation; observation that
//     creates rows is not observation. An unregistered project resolves to
//     db.UnregisteredProjectID for a read, which matches no rows — so a read
//     from a repository docket has never seen answers "nothing here", which is
//     true, rather than answering with some other project's contents.
//
// Everything already registered is untouched by both: the lookup comes first,
// so an existing row — including a junk one from before this change — keeps
// resolving exactly as it did.

// readOnlyLeafVerbs are the leaf command names that only observe.
//
// It is a LEAF-NAME set rather than a set of full paths because this CLI's verb
// vocabulary is consistent across groups: `list` and `show` mean the same thing
// under `issue`, `run`, `step`, `doc`, `vote`, `workflow`, and `schema`, and a
// path list would have to be extended for each new group that spells them the
// same way. TestReadVerbsNeverRegisterAProject walks the REAL command tree and
// classifies BOTH halves — every read verb, and a representative set of writes
// — so a name added here that collides with a writing verb fails there rather
// than silently stopping that verb from registering.
//
// UNKNOWN LEAVES MAY REGISTER. A verb this set does not name gets the
// pre-DKT-58 behavior, so a command added later is never silently prevented
// from registering the project it needs; the cost of a new read verb being
// missed here is one project row for a repository that was going to get one.
var readOnlyLeafVerbs = map[string]bool{
	// Cross-group readers.
	"list": true, "show": true, "get": true, "log": true, "graph": true,
	"context": true, "render": true, "gates": true, "artifact": true,
	"artifacts": true, "report": true, "status": true, "result": true,
	"lint": true, "verify": true,
	// Top-level readers.
	"board": true, "next": true, "plan": true, "stats": true, "export": true,
	"steps": true, "version": true, "help": true, "completion": true,
	"manifest": true,
}

// commandMayRegisterProject reports whether cmd is allowed to create a project
// row on first contact.
func commandMayRegisterProject(cmd *cobra.Command) bool {
	// Every `guard` predicate is a read by contract — its own header calls it a
	// predicate over engine state — and one of them fires on hook paths that run
	// from arbitrary directories, which is the worst possible place to mint a
	// project.
	if isGuardCmd(cmd) {
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		if readOnlyLeafVerbs[c.Name()] {
			return false
		}
	}
	return true
}

// runAddressedGroups are the command groups that identify their project from
// the RUN or STEP they were handed, never from the working directory.
//
// DKT-58's first ask, verbatim: "a step-write verb already knows its run's
// project and must never derive one from cwd". These groups therefore keep
// working from any directory — an executor's scratchpad included — and simply
// carry no ambient project. `trust` is here for the adjacent reason: the
// allowlist is a user-level store, not a project's.
var runAddressedGroups = map[string]bool{
	"step": true, "dispatch": true, "trust": true, "guard": true, "events": true,
}

// commandUsesAmbientProject reports whether cmd's behavior depends on the
// project resolved from the working directory.
func commandUsesAmbientProject(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if runAddressedGroups[c.Name()] {
			return false
		}
	}
	return true
}

// commandPath renders the full verb path (`issue create`, `step complete`) for
// the registration event's `verb` field.
func commandPath(cmd *cobra.Command) string {
	var parts []string
	for c := cmd; c != nil && c.Parent() != nil; c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	return strings.Join(parts, " ")
}

// resolveInvocationProject answers "which project is this?" and registers one
// only when both parts of the rule above hold.
func resolveInvocationProject(
	cmd *cobra.Command, conn *sql.DB, cfg *config.Config,
) (int, error) {
	// An empty identity is the documented pre-v12 fallback and predates the
	// dimension entirely: it means "this store is not multi-tenant", and the
	// default row is the whole store.
	if cfg.Identity == "" {
		return db.DefaultProjectID, nil
	}

	// THE LOOKUP COMES FIRST, unconditionally. Whatever is already registered
	// keeps resolving — including rows minted by the defect this guards against,
	// which must stay reachable so they can be inspected and deleted.
	id, found, err := db.LookupProject(conn, cfg.Identity)
	if err != nil {
		return 0, fmt.Errorf("resolving the project: %w", err)
	}
	if found {
		return id, nil
	}

	// Part 2: a read observes, and finds nothing here to observe.
	if !commandMayRegisterProject(cmd) {
		return db.UnregisteredProjectID, nil
	}

	// Part 1: an unanchored identity never registers.
	if !cfg.Anchored {
		// A verb whose behavior depends on the ambient project is refused BY
		// NAME here, because both alternatives are worse: an opaque foreign-key
		// error at the INSERT, or the row quietly landing in a project the
		// operator never named. A run-addressed verb carries on — it reads its
		// project from the run, which is DKT-58's own first ask.
		if commandUsesAmbientProject(cmd) {
			cwd, _ := os.Getwd()
			return 0, cmdErr(fmt.Errorf(
				"cannot tell which project `docket %s` belongs to: %s is not a "+
					"git worktree and has no docket project.\n"+
					"Run from inside the repository, or create a repo-local "+
					"store there with `docket init --local`",
				commandPath(cmd), cwd), output.ErrValidation)
		}
		return db.UnregisteredProjectID, nil
	}

	name := filepath.Base(cfg.Identity)
	nowMS := time.Now().UnixMilli()
	id, created, err := db.EnsureProjectCreated(conn, cfg.Identity, name, nowMS)
	if err != nil {
		return 0, fmt.Errorf("resolving the project: %w", err)
	}

	if created {
		// DKT-61: the row now says where it came from. A failure to record it
		// does NOT fail the command — the project exists either way, and losing
		// the verb to an event-write error would be a worse trade than an
		// unattributed row — but it is reported, because a silently missing
		// registration event is the exact gap this kind was added to close.
		cwd, _ := os.Getwd()
		prefix, _ := db.ProjectPrefix(conn, id)
		if err := engine.RecordProjectRegisteredEvent(conn, engine.ProjectRegistration{
			ID: id, Name: name, Identity: cfg.Identity,
			Cwd: cwd, Verb: commandPath(cmd), Prefix: prefix,
		}, nowMS); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"docket: registered project %q but could not record the event: %v\n",
				name, err)
		}
	}
	return id, nil
}
