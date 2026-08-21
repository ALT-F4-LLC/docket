package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

type contextKey string

const (
	dbKey       contextKey = "db"
	cfgKey      contextKey = "cfg"
	projectKey  contextKey = "project"
	readerDBKey contextKey = "readerDB"
)

// CmdError wraps an error with a machine-readable error code for structured output.
type CmdError struct {
	Err  error
	Code output.ErrorCode
}

func (e *CmdError) Error() string { return e.Err.Error() }

func cmdErr(err error, code output.ErrorCode) *CmdError {
	return &CmdError{Err: err, Code: code}
}

// errSkipRun tells Execute that PersistentPreRunE fully handled this
// invocation: the output is already written and the process should exit 0
// without running RunE.
//
// It is NOT a failure, and it is the only way to stop cobra short of RunE —
// returning nil would run the command against a database that is not there.
// Execute recognizes it explicitly and never renders it.
var errSkipRun = errors.New("docket: handled in pre-run")

// isGuardCmd reports whether cmd is one of the `docket guard` predicates.
//
// Walks the parent chain rather than matching names, so a guard added later is
// covered without editing this, and so an unrelated verb that happens to be
// called "stop" is not.
func isGuardCmd(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "guard" {
			return true
		}
	}
	return false
}

// emitGuardNotApplicable writes the allow verdict for "this repo has no
// engine".
//
// It reuses the ordinary allow payload so a caller parsing `allowed` needs no
// special case, and adds `not_applicable` plus a reason so the distinction is
// still legible to anyone who looks. In human mode the reason goes to stderr,
// leaving stdout clean for a hook that captures it.
func emitGuardNotApplicable(cmd *cobra.Command, dbPath string) {
	reason := fmt.Sprintf(
		"no docket database at %s; nothing here to allow or deny", dbPath)

	w := getWriter(cmd)
	w.Success(struct {
		Allowed       bool   `json:"allowed"`
		NotApplicable bool   `json:"not_applicable"`
		Reason        string `json:"reason"`
	}{Allowed: true, NotApplicable: true, Reason: reason}, "allowed")

	if isJSON, _ := jsonModeOf(cmd); !isJSON {
		fmt.Fprintf(cmd.ErrOrStderr(), "guard: %s\n", reason)
	}
}

var rootCmd = &cobra.Command{
	Use:     "docket",
	Short:   "Local-first CLI issue tracker",
	Version: fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, buildDate),
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Reject an unknown --json value up front, so it can never degrade
		// silently into human output further down.
		rawJSON, _ := cmd.Flags().GetString("json")
		if _, err := output.ParseJSONMode(rawJSON); err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		// Same contract for --format: an unknown value fails loudly here rather
		// than degrading into human output the caller did not ask for.
		//
		// ONLY when --format is the ROOT flag. `export` declares its own
		// --format (json|csv|markdown) and owns those values; validating them
		// against the alias's vocabulary would reject `--format csv` on the one
		// verb where it has always been correct.
		if ownsLocalFormatFlag(cmd) == nil {
			if raw, _ := cmd.Flags().GetString("format"); raw != "" &&
				parseFormat(raw) == output.JSONNone {
				return cmdErr(fmt.Errorf(
					"invalid value %q for --format: want json", raw),
					output.ErrValidation)
			}
		}

		cfg, err := config.Resolve()
		if err != nil {
			return err
		}

		ctx := context.WithValue(cmd.Context(), cfgKey, cfg)

		watchMode, _ := cmd.Flags().GetBool("watch")
		if watchMode {
			if !isWatchEligible(cmd) {
				return cmdErr(
					fmt.Errorf("%s", watchRejectionMessage()),
					output.ErrValidation,
				)
			}
		}
		// The interval floor applies to EVERY polling mode, not only `--watch`.
		// `events list --follow` polls on the same flag (events-follow.md §4.1),
		// and a floor that checked one mode and not the other would let the same
		// number be legal or illegal depending on which flag turned the loop on.
		if follow, _ := cmd.Flags().GetBool("follow"); watchMode || follow {
			interval, _ := cmd.Flags().GetDuration("interval")
			if interval < 500*time.Millisecond {
				return cmdErr(
					fmt.Errorf("--interval must be at least 500ms"),
					output.ErrValidation,
				)
			}
		}

		if _, ok := cmd.Annotations["skipDB"]; ok {
			cmd.SetContext(ctx)
			return nil
		}

		if _, err := os.Stat(cfg.DBPath); os.IsNotExist(err) {
			// A GUARD MUST NOT REPORT "no database" AS A DENIAL.
			//
			// Guards answer a boolean on exit 0 (allow) / exit 2 (deny), and
			// exit 2 is also NOT_FOUND's code — a collision that is fine for
			// every other verb but not here. A hook wired as
			// `docket guard stop || exit` cannot tell "the engine says no"
			// from "there is no engine", so in a repo that never ran `docket
			// init` every such hook denied. Observed: run-guard blocked a
			// mid-bootstrap turn-end before init had happened.
			//
			// NOT-APPLICABLE IS AN ALLOW. The guard's question is "does engine
			// state forbid this?", and where there is no engine state, nothing
			// forbids it. Answering "deny" would let installing docket
			// anywhere on a machine start blocking unrelated repositories,
			// which is the opposite of what a predicate about THIS repo's
			// engine should do.
			//
			// The reason still travels, so an operator debugging a
			// surprisingly-permissive hook can see why: `not_applicable` in
			// the JSON envelope, and a stderr note in human mode. It is not
			// silent, just not a denial.
			if isGuardCmd(cmd) {
				emitGuardNotApplicable(cmd, cfg.DBPath)
				return errSkipRun
			}
			return cmdErr(
				fmt.Errorf("no docket database found, run 'docket init' to create one"),
				output.ErrNotFound,
			)
		}

		conn, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}

		if err := db.Migrate(conn); err != nil {
			return fmt.Errorf("failed to migrate database: %w", err)
		}

		// Resolve this invocation's project (v12): the store is shared, and
		// which rows this command reads and writes is decided ONCE, here, from
		// the identity config.Resolve derived — never re-derived downstream.
		projectID, err := resolveInvocationProject(cmd, conn, cfg)
		if err != nil {
			return err
		}

		// The project's display prefix, installed process-wide before any
		// command formats an id: `VOR-42` and `DKT-42` are the same issue —
		// the number is the global identity — but the board reads in the
		// project's own voice.
		if prefix, err := db.ProjectPrefix(conn, projectID); err == nil {
			model.SetDisplayPrefix(prefix)
		}

		// The store's whole prefix roster, so id PARSING accepts exactly the
		// prefixes some project holds (DKT-110): `AMS-2` round-trips from a
		// cross-project listing, while `ANSI-16` — an id-shaped term no
		// project owns — is refused instead of silently resolving issue 16.
		if prefixes, err := db.AllProjectPrefixes(conn); err == nil {
			model.SetKnownProjectPrefixes(prefixes)
		}

		// WHO OWNS AN ID, for rendering it in its own project's voice and for
		// refusing a reference that names someone else's (DKT-256). Issue ids
		// are store-global — DKT-267 and DOT-268 were minted consecutively
		// across two projects — so a prefix rendered from the READER's cwd made
		// every cross-project reference silently wrong: the same report row
		// showed DOT-81 from one checkout and ART-81 from another, and
		// `issue link add DOT-268 relates_to DKT-267` reported success as
		// "Linked DOT-268 relates_to DOT-267".
		//
		// A closure with a memo rather than a preloaded table: most invocations
		// render a handful of ids and some render none, so the whole store's
		// worth of rows would be paid for every time to answer five questions.
		// The memo makes a listing cost one lookup per DISTINCT id.
		//
		// A SEPARATE reader connection, not conn: this resolver is called from
		// deep inside engine code — activate, reconcile, and every other
		// fat-transaction verb format ids into their result and error messages
		// WHILE their transaction is open, and that transaction already holds
		// conn's one pooled connection (db.Open caps it at one). Resolving
		// through conn there self-deadlocks: database/sql has no second
		// connection to hand the query, and the query is what the open
		// transaction is waiting on to finish. db.OpenReader's own connection
		// never contends with it.
		//
		// Not concurrency-guarded, for the same reason SetDisplayPrefix is not:
		// these are per-invocation constants installed before any command runs,
		// and rendering happens on the command's own goroutine.
		readerDB, err := db.OpenReader(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("failed to open reader database connection: %w", err)
		}

		owners := map[int]string{}
		model.SetIssueOwnerPrefixResolver(func(id int) string {
			if prefix, ok := owners[id]; ok {
				return prefix
			}
			prefix, err := db.IssueOwnerPrefix(readerDB, id)
			if err != nil {
				prefix = ""
			}
			owners[id] = prefix
			return prefix
		})

		ctx = context.WithValue(ctx, dbKey, conn)
		ctx = context.WithValue(ctx, readerDBKey, readerDB)
		cmd.SetContext(context.WithValue(ctx, projectKey, projectID))
		return nil
	},
	PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
		if readerDB, ok := cmd.Context().Value(readerDBKey).(*sql.DB); ok && readerDB != nil {
			readerDB.Close()
		}
		conn, ok := cmd.Context().Value(dbKey).(*sql.DB)
		if ok && conn != nil {
			return conn.Close()
		}
		return nil
	},
}

func init() {
	// --json is a string flag with NoOptDefVal so a bare --json still behaves
	// like the Bool flag it used to be: it takes the value "v1" and emits the
	// byte-identical v1 envelope. --json=v2 selects the uniform envelope.
	// See internal/output.ParseJSONMode for the accepted values.
	rootCmd.PersistentFlags().String("json", "", "Output in JSON format (--json, or --json=v2)")
	rootCmd.PersistentFlags().Lookup("json").NoOptDefVal = "v1"
	// --format is the conventional spelling operators reach for first, and
	// guessing it cost a retry against `unknown flag`. It selects v2:
	// a caller writing --format json today wants the current envelope, and v1
	// stays reachable by its own explicit spelling for the scripts frozen on it.
	rootCmd.PersistentFlags().String(
		"format", "", "Output format: json (alias for --json=v2)")
	rootCmd.PersistentFlags().BoolP("quiet", "q", false, "Suppress non-essential output")
	rootCmd.PersistentFlags().BoolP("watch", "w", false, "Watch for changes and refresh output")
	rootCmd.PersistentFlags().Duration(
		"interval", 2*time.Second, "Poll interval for --watch and --follow (minimum 500ms)")
	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true
}

// initWatchFlags must be called after all subcommands are registered to hide
// --watch and --interval on ineligible commands. This is invoked from Execute.
func initWatchFlags() {
	hideWatchFlags(rootCmd)
}

// jsonVersionOf reports the JSON envelope version requested on cmd. It is the
// single reader of the --json flag value, so every call site parses it
// identically. An unparseable value yields JSONNone here; the value is
// validated up front in PersistentPreRunE so the command fails with a
// VALIDATION_ERROR rather than silently falling back to human output.
func jsonVersionOf(cmd *cobra.Command) output.JSONVersion {
	raw := lookupFlagValue(cmd, "json")
	version, err := output.ParseJSONMode(raw)
	if err != nil {
		return output.JSONNone
	}
	// --format json is an alias for --json=v2. An explicit --json
	// wins, so a caller who spells out --json=v1 --format=json gets v1 rather
	// than having the alias silently upgrade their envelope.
	if version == output.JSONNone && formatIsJSON(cmd) {
		return output.JSONV2
	}
	return version
}

// ownsLocalFormatFlag reports whether cmd declares its OWN --format, rather
// than only inheriting the persistent root one.
//
// `export` declares --format json|csv|markdown and has always owned those
// values. The --format alias must not reinterpret them: on a command
// with its own --format, the local flag is the meaning and the alias steps
// aside entirely. Returning an error-shaped nil/non-nil would be needless
// indirection, so this is a plain predicate.
func ownsLocalFormatFlag(cmd *cobra.Command) error {
	if cmd.LocalNonPersistentFlags().Lookup("format") != nil {
		return errOwnsFormat
	}
	return nil
}

// errOwnsFormat marks "this command defines --format itself".
var errOwnsFormat = errors.New("command declares its own --format")

// formatIsJSON reports whether --format requested JSON. The flag is absent on
// commands built outside the root tree (tests construct bare cobra.Commands),
// and a missing flag simply reads as not-requested.
//
// A command with its OWN --format opts out: its vocabulary is its own, and
// `export --format json` must keep meaning "export as JSON" rather than
// silently switching the envelope to v2.
func formatIsJSON(cmd *cobra.Command) bool {
	if ownsLocalFormatFlag(cmd) != nil {
		return false
	}
	return parseFormat(lookupFlagValue(cmd, "format")) == output.JSONV2
}

// lookupFlagValue reads a flag that may be declared LOCALLY or inherited from
// an ancestor's persistent set, returning "" when it is neither.
//
// Both flagsets are consulted because cobra merges persistent flags into
// cmd.Flags() only once a command is being executed; before that a child sees
// an inherited flag through InheritedFlags() alone. Reading just one of them
// makes a helper's answer depend on WHEN in the lifecycle it is called, which
// is a difference no caller should have to reason about.
func lookupFlagValue(cmd *cobra.Command, name string) string {
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag.Value.String()
	}
	if flag := cmd.InheritedFlags().Lookup(name); flag != nil {
		return flag.Value.String()
	}
	return ""
}

// parseFormat maps a --format value to the envelope it selects. "" is absent.
// Anything else is invalid and is rejected in PersistentPreRunE rather than
// degrading to human output.
func parseFormat(raw string) output.JSONVersion {
	switch raw {
	case "json", "json=v2", "v2":
		return output.JSONV2
	default:
		return output.JSONNone
	}
}

// jsonModeOf reports whether cmd requested JSON output, and at which version.
// Use this instead of reading the --json flag directly: it is a string flag,
// so GetBool("json") returns (false, err) and would silently drop JSON mode.
func jsonModeOf(cmd *cobra.Command) (bool, output.JSONVersion) {
	version := jsonVersionOf(cmd)
	return version != output.JSONNone, version
}

func getWriter(cmd *cobra.Command) *output.Writer {
	quietMode, _ := cmd.Flags().GetBool("quiet")
	return output.NewWithVersion(jsonVersionOf(cmd), quietMode)
}

func getCfg(cmd *cobra.Command) *config.Config {
	cfg, _ := cmd.Context().Value(cfgKey).(*config.Config)
	return cfg
}

func getDB(cmd *cobra.Command) *sql.DB {
	conn, _ := cmd.Context().Value(dbKey).(*sql.DB)
	if conn == nil {
		panic("bug: getDB called on a command with no database connection (missing PersistentPreRunE guard?)")
	}
	return conn
}

// getProjectID returns the invocation's resolved project (v12). Commands built
// outside the root hook — tests wiring a bare command — fall back to the
// default project, which is the pre-v12 single-tenant behavior exactly.
func getProjectID(cmd *cobra.Command) int {
	if id, ok := cmd.Context().Value(projectKey).(int); ok && id != 0 {
		return id
	}
	return db.DefaultProjectID
}

// Execute runs the root command and returns an exit code.
func Execute() int {
	initWatchFlags()
	if err := rootCmd.Execute(); err != nil {
		// PersistentPreRunE already wrote this invocation's output and asked
		// to stop before RunE. Not a failure: exit 0, render nothing.
		if errors.Is(err, errSkipRun) {
			return 0
		}
		raw, _ := rootCmd.PersistentFlags().GetString("json")
		// An invalid --json value falls back to human error rendering, which
		// is the correct channel for reporting that the value was invalid.
		version, _ := output.ParseJSONMode(raw)
		// --format json must route ERRORS through the JSON envelope too, not
		// only successes: a caller parsing stdout gets a parse failure instead
		// of a structured error otherwise.
		if version == output.JSONNone {
			rawFormat, _ := rootCmd.PersistentFlags().GetString("format")
			version = parseFormat(rawFormat)
		}
		quietMode, _ := rootCmd.PersistentFlags().GetBool("quiet")
		w := output.NewWithVersion(version, quietMode)

		var ce *CmdError
		if errors.As(err, &ce) {
			return w.Error(ce.Err, ce.Code)
		}
		return w.Error(err, output.ErrGeneral)
	}
	return 0
}
