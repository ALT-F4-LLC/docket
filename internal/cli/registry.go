package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket registry` — the STORE-WIDE view of the two registries (DKT-614).
//
// Every other registry verb resolves ONE project from the cwd: `workflow list`,
// `workflow show`'s drift check, `workflow list --orphans`, `schema list`. That
// is right for them and wrong for the question this group exists for, which is
// about the projects an operator is NOT standing in. Auto-registration only
// runs where a run activates, so the projects that fall behind the shared
// corpus are precisely the ones nobody has cd'd into lately — and every
// existing verb is scoped to somewhere they have.
//
// It is a sibling group to `project`, which is the other store-wide surface,
// rather than a flag on `workflow list`, because the report spans BOTH
// registries. A `--all-projects` on `workflow list` would answer half the
// question and leave the schema half to a second verb that does not exist.

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Inspect the store's workflow and schema registries",
	Long: `Inspect the store's workflow and schema registries.

Registration is a ROW, not a file. Activation auto-registers the instance
config's current contents, so a project's rows track the corpus only as far as
its last activation — and nothing else ever moves them. These verbs read that
divergence across the whole store.`,
}

var registryAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Report every project's registry drift against the shared corpus",
	Long: `Report, for every project in the store, which registered workflow and
schema names are BEHIND the shared corpus, and which are ORPHANED.

Auto-registration adopts the instance config's current contents, but only as a
side effect of activating a run IN THAT PROJECT. A project no run has activated
lately goes stale against a corpus every other project already moved past, and
short of cd'ing into each checkout in turn there was no way to see it.

Two findings, per registered NAME rather than per version:

  behind    the project's highest registered version for that name is lower
            than the version a file in the instance config now declares. The
            next activation there adopts it; until then the project binds the
            older definition.

  orphaned  no file in any scanned root declares that name any more — the
            residue of a rename or a deletion, since registering a new name
            never retires the old one. It still binds until it is deprecated
            ('docket workflow deprecate <name>@<version>').

THE CORPUS IS SCANNED ONCE, not once per project: '~/.docket/config' is shared
by every project in the store, so what "current" means is one answer. The roots
that were scanned are reported, because they are read from THIS invocation's
config — a repository's own '.docket/config/' additions belong to the checkout
you are standing in, so a name another project registered from ITS local config
directory is reported orphaned here. That is why the roots are printed rather
than assumed.

It REPAIRS NOTHING. Adopting a bumped definition is what activation does,
inside a transaction, with the validation and collision rules that go with it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRegistryAudit(cmd, args, getWriter(cmd))
	},
}

func runRegistryAudit(cmd *cobra.Command, _ []string, w *output.Writer) error {
	conn := getDB(cmd)

	var opts engine.RegistryAuditOptions
	if ref, _ := cmd.Flags().GetString("project"); strings.TrimSpace(ref) != "" {
		target, err := resolveProjectRef(conn, ref)
		if err != nil {
			return err
		}
		opts.ProjectID = target.ID
	}

	// THE TWO REFUSALS COME FIRST, and they are separate messages because they
	// send an operator to different places. With nothing scanned, EVERY
	// registered name in the store trivially has no file declaring it and every
	// version trivially has no current version to be behind — so the report
	// would be invented out of having looked nowhere, which is the one output
	// this verb must never produce. `workflow list --orphans` refuses on the
	// same two conditions, with the same codes.
	index, err := engine.ScanCorpus()
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	if !index.Scanned() {
		return cmdErr(fmt.Errorf(
			"no instance-config root exists on this machine, so no registry can "+
				"be compared against anything: with nothing to scan, every "+
				"registered name would look orphaned"), output.ErrValidation)
	}
	if err := index.Err(); err != nil {
		return cmdErr(fmt.Errorf(
			"scanning the instance config for current definitions: %w", err),
			output.ErrGeneral)
	}

	audit, err := engine.AuditRegistries(conn, index, opts)
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}

	// NOT a Collection. The v2 collection envelope replaces `data` with
	// {items, total, truncated}, which would drop `roots` and the store-wide
	// counts — and the roots are not decoration here, they are the record of
	// WHERE "current" was read from, without which a reported orphan cannot be
	// judged. This is a report with a header, like `run report`, not a listing.
	var message string
	if !w.JSONMode {
		message = renderRegistryAudit(audit)
	}
	w.Success(audit, message)
	return nil
}

func renderRegistryAudit(audit *engine.RegistryAudit) string {
	var b strings.Builder

	fmt.Fprintf(&b, "scanned %d instance-config root(s) for current versions:\n",
		len(audit.Roots))
	for _, root := range audit.Roots {
		fmt.Fprintf(&b, "  %s\n", root)
	}

	// The columns are sized over EVERY project's findings, so the report reads
	// as one table rather than as a stack of independently-aligned blocks.
	nameWidth := 4
	for _, p := range audit.Projects {
		for _, lag := range p.Behind {
			nameWidth = max(nameWidth, len(lag.Name))
		}
		for _, orphan := range p.Orphaned {
			nameWidth = max(nameWidth, len(orphan.Name))
		}
	}

	dirty := 0
	for _, p := range audit.Projects {
		b.WriteString("\n")
		fmt.Fprintf(&b, "%s\n", describeAuditedProject(p))
		if p.Clean() {
			if p.Compared == 0 {
				// DISTINCT FROM "up to date". A project with no rows has not
				// been checked and found healthy; it has never registered
				// anything, which for a project that runs workflows is itself
				// the finding.
				b.WriteString("  nothing registered\n")
				continue
			}
			fmt.Fprintf(&b, "  up to date (%d registered name(s))\n", p.Compared)
			continue
		}
		dirty++
		for _, lag := range p.Behind {
			fmt.Fprintf(&b, "  behind    %-9s %-*s  registered %d, current %d\n",
				lag.Kind, nameWidth, lag.Name, lag.RegisteredVersion, lag.CurrentVersion)
		}
		for _, orphan := range p.Orphaned {
			fmt.Fprintf(&b, "  orphaned  %-9s %-*s  registered %s%s\n",
				orphan.Kind, nameWidth, orphan.Name,
				joinVersions(orphan.Versions), retiredMarker(orphan))
		}
	}

	b.WriteString("\n")
	if audit.BehindTotal == 0 && audit.OrphanedTotal == 0 {
		fmt.Fprintf(&b, "%d project(s) checked; every registered name matches the corpus.",
			len(audit.Projects))
		return b.String()
	}
	fmt.Fprintf(&b,
		"%d of %d project(s) carry findings: %d name(s) behind, %d orphaned.\n"+
			"A behind name is adopted by the next `docket run activate` in that "+
			"project; an orphaned one is retired with `docket workflow deprecate`.",
		dirty, len(audit.Projects), audit.BehindTotal, audit.OrphanedTotal)
	return b.String()
}

// describeAuditedProject names a project the way `project list` does — the
// three columns that resolve on `--project` — so the two verbs' output can be
// read against each other.
func describeAuditedProject(p engine.ProjectRegistryAudit) string {
	name := p.Project
	if name == "" {
		name = p.Identity
	}
	if name == "" {
		name = fmt.Sprintf("project %d", p.ProjectID)
	}
	if p.Prefix != "" {
		return fmt.Sprintf("%s (%s)", name, p.Prefix)
	}
	return name
}

func joinVersions(versions []int) string {
	out := make([]string, 0, len(versions))
	for _, v := range versions {
		out = append(out, fmt.Sprintf("@%d", v))
	}
	return strings.Join(out, " ")
}

// retiredMarker distinguishes the orphan an operator has already dealt with
// from the one still binding. Retired orphans stay listed for `workflow list
// --orphans`' reason — a cleanup pass has to be able to see its own work.
func retiredMarker(orphan engine.RegistryOrphan) string {
	if orphan.Retired {
		return "  [all deprecated]"
	}
	return ""
}

func init() {
	registryAuditCmd.Flags().String("project", "",
		"Audit one project (its PREFIX, NAME, IDENTITY, or id) instead of every project")
	registryCmd.AddCommand(registryAuditCmd)
	rootCmd.AddCommand(registryCmd)
}
