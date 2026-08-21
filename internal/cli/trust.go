package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/spf13/cobra"
)

// `docket trust` — the user-level allowlist of commands docket may execute
// (docs/design/engine-spec.md §4, docs/tdd/gates-trust.md §3.5).
//
// THE STORE IS NEVER REPO-SHIPPED AND NEVER REPO-POINTED. There is no
// --trust-file flag, no DOCKET_TRUST_FILE env var, no config key, and no
// repo-local path: the store is $XDG_CONFIG_HOME/docket/trust.toml or
// $HOME/.config/docket/trust.toml and nothing else (§3.1). Every additional way
// to point docket at a trust file is another way for repo content — a
// checked-in .envrc, a direnv hook, a Makefile — to point it at a file the repo
// controls. TestNoTrustPathSurface walks the Cobra tree and the config-key
// registry asserting this holds.
//
// These verbs carry `skipDB`: the trust store is USER-LEVEL and managing it
// does not require a repository. `trust add` binds to the current repo, so it
// needs a repo path, but `trust list` and `trust rm` work anywhere.

var trustCmd = &cobra.Command{
	Use:   "trust",
	Short: "Manage the allowlist of commands docket may execute",
	Long: `Manage the allowlist of commands docket may execute.

Docket runs a registered command only when an entry here authorizes it. An
unmatched command is REPORTED, never run: cloning a repository and running a
workflow executes nothing until you approve a command yourself.

Entries live in $XDG_CONFIG_HOME/docket/trust.toml (default
~/.config/docket/trust.toml), are owned by you, and are never read from a
repository. Each entry binds to the repository it was approved in unless
--global is given, so a command trusted for one project does not run in a
malicious clone of another.

A missing trust file is not an error — it is an empty allowlist, and every gate
reports what it would have needed.`,
	Annotations: map[string]string{"skipDB": ""},
}

var trustAddCmd = newTrustAddCmd()

// newTrustAddCmd builds the `trust add` verb, FLAGS INCLUDED.
//
// It is a constructor rather than a package var whose flags an init() attaches,
// because cobra keeps a flag's parsed value in the FlagSet the command owns: a
// second run against the same command sees the first run's `--tree` still set.
// The verbs are wired once from the constructor at startup, and a test that
// drives one takes a pristine command of its own.
func newTrustAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <name> -- <argv...>",
		Short: "Approve one command for this repository",
		Long: `Approve one command, recording its exact argv.

Everything after -- is taken VERBATIM as argv elements: your shell has already
tokenized it, and docket stores those tokens. Nothing is split, expanded, or
globbed, and no shell is ever involved in running it. A trailing flag after --
is part of the trusted command, not a docket flag:

  docket trust add lint -- golangci-lint run --fix

trusts ["golangci-lint","run","--fix"].

The argv and the repository binding are printed on EVERY add, including --yes
ones. --yes skips the interactive confirmation; it never suppresses the
disclosure.`,
		Args:        cobra.MinimumNArgs(1),
		Annotations: map[string]string{"skipDB": ""},
		RunE:        runTrustAdd,
	}

	cmd.Flags().Bool("global", false,
		"trust this command in EVERY repository, not just this one")
	cmd.Flags().Bool("prefix", false,
		"match any command beginning with this argv (over-authorizing; prints a warning)")
	cmd.Flags().Bool("re-runnable", false,
		"safe to run again after a crash interrupted it")
	cmd.Flags().Bool("tree", false,
		"touches the working tree, so it serializes against other such commands")
	cmd.Flags().Bool("flaky", false,
		"may fail intermittently; re-runs on failure, each attempt recorded")
	// --stub declares HOLLOW ASSURANCE, and it is the operator's word because
	// core has no way to earn it: an argv cannot be inspected to tell a real
	// check from a convincing one, and a heuristic that tried would be wrong in
	// both directions. It changes nothing about how the command runs.
	cmd.Flags().Bool("stub", false,
		"this is a placeholder, not the check its name implies; its passes are marked hollow in reports")
	cmd.Flags().String("timeout", "",
		"per-command timeout (e.g. 90s, 10m); defaults to 5m")
	// --network DECLARES a requirement; it does not grant one. Core cannot
	// widen a sandbox it is running inside. What the declaration buys is proxy
	// variables reaching the child, a failure reported as a possible
	// reachability problem naming the hosts, and the requirement being visible
	// in the trust file rather than discovered mid-run.
	cmd.Flags().StringSlice("network", nil,
		"host this command must reach, e.g. vuln.go.dev (repeatable); declares the need, does not grant access")
	cmd.Flags().Bool("yes", false,
		"skip the interactive confirmation (the argv is still disclosed)")

	return cmd
}

var trustListCmd = newTrustListCmd()

func newTrustListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List approved commands for this repository",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"skipDB": ""},
		RunE:        runTrustList,
	}
	cmd.Flags().Bool("global", false, "show only global entries")
	cmd.Flags().Bool("all", false, "show entries for every repository")
	return cmd
}

var trustRmCmd = newTrustRmCmd()

func newTrustRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "rm <name>",
		Short:       "Remove an approved command",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"skipDB": ""},
		RunE:        runTrustRm,
	}
	cmd.Flags().Bool("global", false, "remove the global entry")
	return cmd
}

// trustAddResult is `trust add`'s wire shape.
//
// Warnings ride in the response under --json rather than only on stderr,
// because a machine consumer must see the over-authorization warning too — it
// is never suppressed by --yes or by --json (§3.3).
type trustAddResult struct {
	Name       string   `json:"name"`
	Argv       []string `json:"argv"`
	ArgvSHA256 string   `json:"argv_sha256"`
	Repo       string   `json:"repo,omitempty"`
	Global     bool     `json:"global"`
	Prefix     bool     `json:"prefix"`
	ReRunnable bool     `json:"re_runnable"`
	Network    []string `json:"network,omitempty"`
	Tree       bool     `json:"tree"`
	Flaky      bool     `json:"flaky"`
	Stub       bool     `json:"stub"`
	Timeout    string   `json:"timeout,omitempty"`
	Idempotent bool     `json:"idempotent"`
	Warnings   []string `json:"warnings,omitempty"`
}

type trustEntryView struct {
	Name       string   `json:"name"`
	Argv       []string `json:"argv"`
	ArgvSHA256 string   `json:"argv_sha256"`
	Repo       string   `json:"repo,omitempty"`
	Global     bool     `json:"global"`
	Prefix     bool     `json:"prefix"`
	ReRunnable bool     `json:"re_runnable"`
	Network    []string `json:"network,omitempty"`
	Tree       bool     `json:"tree"`
	Flaky      bool     `json:"flaky"`
	Stub       bool     `json:"stub"`
	Timeout    string   `json:"timeout,omitempty"`
	AddedAtMS  int64    `json:"added_at_ms"`
}

type trustListResult struct {
	Entries []trustEntryView `json:"entries"`
	Total   int              `json:"total"`
}

func (r trustListResult) CollectionItems() any      { return r.Entries }
func (r trustListResult) CollectionTotal() int      { return r.Total }
func (r trustListResult) CollectionTruncated() bool { return false }

type trustRmResult struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
	Global  bool   `json:"global"`
	Repo    string `json:"repo,omitempty"`
	// Warnings carries the same never-suppressed disclosures `trust add`
	// returns — here, that the revocation went unrecorded because there was no
	// database to record it in.
	Warnings []string `json:"warnings,omitempty"`
}

func runTrustAdd(cmd *cobra.Command, args []string) error {
	w := getWriter(cmd)

	name := args[0]

	// THE -- BOUNDARY. Cobra reports where the terminator was, and everything
	// after it is argv. Using ArgsLenAtDash rather than "args[1:]" is what makes
	// `docket trust add x -- foo --prefix` trust ["foo","--prefix"] instead of
	// silently consuming --prefix as a docket flag.
	dash := cmd.ArgsLenAtDash()
	if dash < 0 || dash > len(args) {
		return cmdErr(fmt.Errorf(
			"the command to trust must follow --, as in: docket trust add %s -- make test",
			name), output.ErrValidation)
	}
	argv := args[dash:]
	if len(argv) == 0 {
		return cmdErr(fmt.Errorf(
			"no command given; the argv to trust follows --, as in: docket trust add %s -- make test",
			name), output.ErrValidation)
	}

	global, _ := cmd.Flags().GetBool("global")
	prefix, _ := cmd.Flags().GetBool("prefix")
	reRunnable, _ := cmd.Flags().GetBool("re-runnable")
	tree, _ := cmd.Flags().GetBool("tree")
	flaky, _ := cmd.Flags().GetBool("flaky")
	stub, _ := cmd.Flags().GetBool("stub")
	timeout, _ := cmd.Flags().GetString("timeout")
	network, _ := cmd.Flags().GetStringSlice("network")

	repoRoot := ""
	if !global {
		cfg := getCfg(cmd)
		if cfg == nil {
			return cmdErr(errors.New("the repository could not be resolved; use --global to trust a command everywhere"),
				output.ErrValidation)
		}
		repoRoot = cfg.Identity
	}

	// The grant is EVENT-LOGGED, and the record is written BEFORE the store
	// (§3.6, see trustEventRecorder): a store that gained an entry can never be
	// a store whose grant went unrecorded.
	recorder := &trustEventRecorder{cmd: cmd, kind: engine.EventTrustAdded}

	res, err := trust.Add(trust.AddRequest{
		Name: name, Argv: argv, RepoRoot: repoRoot, Global: global,
		Prefix: prefix, ReRunnable: reRunnable, Tree: tree, Flaky: flaky,
		Stub:    stub,
		Timeout: timeout, Network: network, NowMS: time.Now().UnixMilli(),
		OnChange: recorder.record,
	})
	if err != nil {
		return trustCmdError(err)
	}

	// The unrecorded-grant note joins the warnings rather than displacing any:
	// both are things the operator must see, and neither is suppressible.
	warnings := append(res.Warnings, recorder.notes()...)

	// THE DISCLOSURE, ON EVERY ADD. --yes suppresses the PROMPT, never this
	// (§3.5): the harness's own command-permission prompt is D14's human
	// backstop, and what makes that backstop work is the argv being visible.
	// Anything docket prints is defense in depth on top of it; anything docket
	// HIDES would erode it.
	//
	// The argv renders through the escaping renderer (§5.7 E3): a fence line an
	// issue author wrote can carry terminal escapes that repaint the very line
	// the operator is approving, so what is displayed must be what is stored.
	if !w.JSONMode {
		binding := "globally, in every repository"
		if !res.Entry.Global {
			binding = "in " + res.Entry.Repo
		}
		fmt.Fprintf(os.Stderr, "trusting %s as %q %s\n",
			exec.RenderArgv(res.Entry.Argv), res.Entry.Name, binding)
		for _, warning := range warnings {
			fmt.Fprintln(os.Stderr, warning)
		}
	}

	data := trustAddResult{
		Name: res.Entry.Name, Argv: res.Entry.Argv, ArgvSHA256: res.Entry.ArgvSHA256,
		Repo: res.Entry.Repo, Global: res.Entry.Global, Prefix: res.Entry.Prefix,
		ReRunnable: res.Entry.ReRunnable, Tree: res.Entry.Tree, Flaky: res.Entry.Flaky,
		Stub:    res.Entry.Stub,
		Network: res.Entry.Network,
		Timeout: res.Entry.Timeout, Idempotent: res.Idempotent, Warnings: warnings,
	}
	msg := fmt.Sprintf("trusted %q", res.Entry.Name)
	if res.Idempotent {
		msg = fmt.Sprintf("%q was already trusted with this argv", res.Entry.Name)
	}
	w.Success(data, msg)
	return nil
}

func runTrustList(cmd *cobra.Command, args []string) error {
	w := getWriter(cmd)

	store, err := trust.Load()
	if err != nil {
		return trustCmdError(err)
	}

	all, _ := cmd.Flags().GetBool("all")
	globalOnly, _ := cmd.Flags().GetBool("global")

	var entries []trust.Entry
	switch {
	case all:
		// --all shows every repo's entries, which is the verb that makes a
		// stale binding visible: a moved repository's entries stop matching,
		// and this is where an operator sees why.
		entries = store.Entries
	case globalOnly:
		for _, e := range store.Entries {
			if e.Global {
				entries = append(entries, e)
			}
		}
	default:
		identity := ""
		if cfg := getCfg(cmd); cfg != nil {
			if id, idErr := trust.RepoIdentity(cfg.Identity); idErr == nil {
				identity = id
			}
		}
		entries = store.List(identity)
	}

	views := make([]trustEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, trustEntryView{
			Name: e.Name, Argv: e.Argv, ArgvSHA256: e.ArgvSHA256, Repo: e.Repo,
			Global: e.Global, Prefix: e.Prefix, ReRunnable: e.ReRunnable,
			Network: e.Network,
			Tree:    e.Tree, Flaky: e.Flaky, Stub: e.Stub,
			Timeout: e.Timeout, AddedAtMS: e.AddedAtMS,
		})
	}

	if !w.JSONMode {
		if len(views) == 0 {
			fmt.Println("no trusted commands")
		}
		for _, e := range views {
			binding := e.Repo
			if e.Global {
				binding = "(global)"
			}
			// The argv renders escaped (§5.7 E3) — it is content an operator
			// may have pasted from an issue body.
			flags := trustFlagSummary(e)
			fmt.Printf("%-20s %s  %s%s\n", e.Name, exec.RenderArgv(e.Argv), binding, flags)
		}
	}

	w.Success(trustListResult{Entries: views, Total: len(views)},
		fmt.Sprintf("%d trusted command(s)", len(views)))
	return nil
}

func runTrustRm(cmd *cobra.Command, args []string) error {
	w := getWriter(cmd)

	name := args[0]
	global, _ := cmd.Flags().GetBool("global")

	repoRoot := ""
	if !global {
		cfg := getCfg(cmd)
		if cfg == nil {
			return cmdErr(errors.New("the repository could not be resolved; use --global to remove a global entry"),
				output.ErrValidation)
		}
		repoRoot = cfg.Identity
	}

	// The record is written from INSIDE the removal, under the store lock and
	// ahead of the write, with the entry as it stood — so it names the hash and
	// flags of what actually went away rather than of whatever a pre-read
	// lookup by name happened to find first.
	recorder := &trustEventRecorder{cmd: cmd, kind: engine.EventTrustRemoved}

	removed, err := trust.Remove(trust.RemoveRequest{
		Name: name, RepoRoot: repoRoot, Global: global, OnChange: recorder.record,
	})
	if err != nil {
		return trustCmdError(err)
	}
	if !removed {
		return cmdErr(fmt.Errorf("no trusted command named %q in this repository", name),
			output.ErrNotFound)
	}

	notes := recorder.notes()
	if !w.JSONMode {
		for _, note := range notes {
			fmt.Fprintln(os.Stderr, note)
		}
	}

	w.Success(trustRmResult{Name: name, Removed: true, Global: global, Repo: repoRoot, Warnings: notes},
		fmt.Sprintf("removed %q", name))
	return nil
}

// trustFlagSummary renders the flags an entry carries, so `trust list` shows
// what an entry authorizes rather than only that it exists.
func trustFlagSummary(e trustEntryView) string {
	out := ""
	for _, f := range []struct {
		on   bool
		name string
	}{
		{e.Prefix, " prefix"},
		{e.ReRunnable, " re-runnable"},
		{e.Tree, " tree"},
		{e.Flaky, " flaky"},
		// Spelled out rather than abbreviated to "stub". This line is what an
		// operator auditing the store reads, and the fact worth reading is not
		// that a word was set but that everything this entry ever passes is
		// hollow (DKT-265).
		{e.Stub, " stub(no-real-check)"},
	} {
		if f.on {
			out += f.name
		}
	}
	// The declared hosts, named rather than flagged: "network" alone would say
	// an entry reaches SOMETHING, and which hosts is the whole content of the
	// declaration an operator is auditing.
	if len(e.Network) > 0 {
		out += " network=" + strings.Join(e.Network, ",")
	}
	return out
}

// errTrustUnrecorded marks the failure to write §3.6's event, so the taxonomy
// can tell it from the store failures around it: nothing is wrong with the
// operator's request, and telling them it was invalid would send them to fix an
// argv that was fine.
var errTrustUnrecorded = errors.New("the trust change was not recorded")

// trustEventRecorder writes the trust-added / trust-removed event, as the hook
// trust.Add and trust.Remove run under their lock BEFORE writing the store
// (§3.6).
//
// MANDATORY INSIDE A REPO, NOT BEST-EFFORT. The record used to be written after
// the store, with its error discarded, which meant a database that refused the
// insert left a granted entry and no trace of the grant. An absent event is not
// a neutral outcome: an auditor reads it as "no grant happened", so a trail that
// can silently lose entries is worse than one that is known to be missing. Being
// the hook is what fixes it — an error here aborts the change with nothing
// written, and the only divergence that survives is a recorded change that then
// failed to land, which is loud rather than silent and errs toward over-reporting
// authority rather than under-reporting it.
//
// OUTSIDE A REPO THE VERB STILL WORKS. The store is user-level (§3.5): requiring
// a database to manage it would mean someone who installed docket could not
// approve a command until they created a tracker. What §3.6 buys there is
// nothing, so the recorder SAYS SO rather than leaving the operator to assume a
// trail exists — the same reasoning that makes the argv disclosure
// unsuppressible.
//
// It is a struct rather than a closure because the note has to reach the JSON
// envelope too, and the caller reads it after the hook has run.
type trustEventRecorder struct {
	cmd  *cobra.Command
	kind string
	// note is the reason no event was recorded, empty when one was.
	note string
}

func (r *trustEventRecorder) record(entry trust.Entry) error {
	cfg := getCfg(r.cmd)
	if cfg == nil {
		r.note = "warning: this trust change was not recorded — the repository could not be resolved, so there is no event log to record it in"
		return nil
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		r.note = fmt.Sprintf(
			"warning: this trust change was not recorded — no docket database at %s. The trust store is user-level and does not need one, but nothing here will show the change later",
			cfg.DBPath)
		return nil
	}

	conn, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("%w: opening %s: %w", errTrustUnrecorded, cfg.DBPath, err)
	}
	defer conn.Close()
	if err := db.Migrate(conn); err != nil {
		return fmt.Errorf("%w: migrating %s: %w", errTrustUnrecorded, cfg.DBPath, err)
	}

	// The ARGV HASH rides in the event, never the argv itself: a trusted
	// command's arguments must not land in an event feed a run report renders.
	// Every other behavior-affecting property does ride along — `tree` and the
	// `network` list above all, since they are what a grant WIDENS and a feed
	// that showed only the name could not tell a re-approval from an escalation.
	// WHO ran the verb rides along too (DKT-263). Both fields are resolved
	// HERE, at the call site, rather than inside the recorder: the engine has
	// no business shelling out to `git config`, and a helper that resolved its
	// own actor would be a helper that could attribute an event to whoever
	// happened to be running the process that called it.
	//
	// A cwd that cannot be read is not a failure to record the grant. The grant
	// is the thing that matters; the empty string says the disambiguator was
	// unavailable, which is a smaller and more honest loss than refusing to
	// write the event at all.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	if err := engine.RecordTrustEvent(conn, r.kind, engine.TrustGrant{
		Name:       entry.Name,
		ArgvSHA256: entry.ArgvSHA256,
		Repo:       entry.Repo,
		Global:     entry.Global,
		Prefix:     entry.Prefix,
		ReRunnable: entry.ReRunnable,
		Tree:       entry.Tree,
		Flaky:      entry.Flaky,
		Network:    entry.Network,
		Timeout:    entry.Timeout,
		Stub:       entry.Stub,
		Actor:      config.DefaultAuthor(),
		Cwd:        cwd,
	}, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("%w: %w", errTrustUnrecorded, err)
	}
	return nil
}

// notes returns the disclosure to surface, or nothing when the change was
// recorded. It is a slice because it joins `trust add`'s warning list.
func (r *trustEventRecorder) notes() []string {
	if r.note == "" {
		return nil
	}
	return []string{r.note}
}

// trustCmdError maps a trust-store failure onto the error taxonomy.
func trustCmdError(err error) error {
	switch {
	case errors.Is(err, trust.ErrConflict):
		return cmdErr(err, output.ErrConflict)
	case errors.Is(err, trust.ErrIntegrity), errors.Is(err, trust.ErrParse):
		return cmdErr(err, output.ErrValidation)
	case errors.Is(err, sql.ErrNoRows):
		return cmdErr(err, output.ErrNotFound)
	case errors.Is(err, errTrustUnrecorded):
		// The request was well-formed and the store is untouched; what failed is
		// this repo's event log. GENERAL_ERROR rather than VALIDATION_ERROR so a
		// caller does not retry with a "corrected" argv against a database that
		// will refuse the next write too.
		return cmdErr(err, output.ErrGeneral)
	default:
		return cmdErr(err, output.ErrValidation)
	}
}

func init() {
	trustCmd.AddCommand(trustAddCmd)
	trustCmd.AddCommand(trustListCmd)
	trustCmd.AddCommand(trustRmCmd)
	rootCmd.AddCommand(trustCmd)
}
