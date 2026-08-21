package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// configEntryList is the v2 collection shape for `docket config get` with no
// key, so the listing renders as {items,total,truncated} like every other list
// verb (engine-spec.md §5).
type configEntryList struct {
	Entries []db.ConfigEntry `json:"entries"`
	Total   int              `json:"total"`
}

func (c configEntryList) CollectionItems() any      { return c.Entries }
func (c configEntryList) CollectionTotal() int      { return c.Total }
func (c configEntryList) CollectionTruncated() bool { return false }

var _ output.Collection = configEntryList{}

// configSetCmd stores an engine default. Unlike the bare `config` verb it needs
// the database — values live in the meta table — so it carries no skipDB
// annotation. Cobra resolves annotations per-command, so the parent stays
// skipDB and its output is untouched.
var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set an engine configuration default",
	Long: `Set an engine configuration default, stored in the database.

Keys are validated against the known set and values against the key's type, so
a typo fails here rather than silently storing something nothing reads.

  lease.ttl.default    duration   fallback lease TTL
  lease.ttl.<class>    duration   per-class lease TTL; see below for what a class IS
  attempt.max          integer    maximum claims per entity
  budget.default       number     default per-run budget cap; 0 is unlimited
  registration.auto    boolean    auto-register workflows/schemas at run activate; default true
  context.warn_bytes   integer    context size that triggers a warning
  context.error_bytes  integer    context size that triggers an error

A CLASS IS A STEP'S class FIELD, AND IT DEFAULTS TO THE EXECUTOR NAME.
A step that declares no class carries its executor's name in that column, so
lease.ttl.read binds nothing unless your steps actually declare class = "read".
Docket cannot supply read/write classes of its own: the class name is the
workflow author's, and core never learns which one means "writes to the tree".

Setting a TTL for a class no registered workflow declares prints a warning
naming the classes that ARE declared. It is a warning and not a refusal — a
workflow registered after its config is an ordinary order of operations — but
a TTL that binds nothing is otherwise discovered when a healthy step is reaped
mid-run.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		key, value := args[0], args[1]

		// Project-scoped by default (v12): the value applies to this project
		// and no other. --global writes the store-wide default every project
		// without its own override falls back to — which is also what every
		// pre-v12 value already is.
		projectID := getProjectID(cmd)
		if global, _ := cmd.Flags().GetBool("global"); global {
			projectID = 0
		}

		if err := db.SetConfig(conn, projectID, key, value); err != nil {
			if errors.Is(err, db.ErrUnknownConfigKey) {
				return cmdErr(err, output.ErrValidation)
			}
			// A validation failure on the value is the other expected case;
			// anything else is a storage failure.
			if strings.Contains(err.Error(), "storing config") {
				return cmdErr(err, output.ErrGeneral)
			}
			return cmdErr(err, output.ErrValidation)
		}

		// AFTER the write, not before (DKT-260). The value is valid and was
		// stored; this is a disclosure about what it will bind to, and
		// withholding the write over it would refuse a configuration the
		// operator may be setting ahead of the workflow that uses it.
		message := fmt.Sprintf("Set %s = %s", key, value)
		if warning := leaseTTLClassWarning(conn, projectID, key); warning != "" {
			fmt.Fprintln(os.Stderr, warning)
			message += "\n" + warning
		}

		entry := db.ConfigEntry{Key: key, Value: value, Source: "set"}
		w.Success(entry, message)
		return nil
	},
}

// configGetCmd reads an engine default, or lists them all.
var configGetCmd = &cobra.Command{
	Use:   "get [key]",
	Short: "Read an engine configuration default",
	Long: `Read an engine configuration value, or list every value when no key is given.

Each result reports its source: "set" when configured explicitly, "default"
when falling back to the shipped value — so "nobody configured this" is
distinguishable from "somebody configured this to the same value".

An unset lease.ttl.<class> falls back to lease.ttl.default rather than to
nothing.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		projectID := getProjectID(cmd)
		if global, _ := cmd.Flags().GetBool("global"); global {
			projectID = 0
		}

		if len(args) == 0 {
			entries, err := db.ListConfig(conn, projectID)
			if err != nil {
				return cmdErr(fmt.Errorf("listing config: %w", err), output.ErrGeneral)
			}
			w.Success(
				configEntryList{Entries: entries, Total: len(entries)},
				formatConfigEntries(entries),
			)
			return nil
		}

		entry, err := db.GetConfig(conn, projectID, args[0])
		if err != nil {
			if errors.Is(err, db.ErrUnknownConfigKey) {
				return cmdErr(err, output.ErrValidation)
			}
			return cmdErr(fmt.Errorf("reading config: %w", err), output.ErrGeneral)
		}

		w.Success(entry, configValueForHuman(entry))
		return nil
	},
}

// unsetConfigValue is what an unset key prints instead of nothing (DKT-69).
//
// `config get <key>` printed the empty string for a key with no value and no
// shipped default, and exited 0 — indistinguishable from a key deliberately set
// to "". Worse in bulk: several keys read in one block produced FEWER LINES
// THAN KEYS, so the output stopped aligning with the input and a reader
// matching lines to keys positionally read the wrong values. Measured: vote-rule
// keys read as `15m`, a value that came from the neighboring lease keys.
//
// A line that says the key is unset keeps the two counts equal, which is the
// property the positional read was relying on and never had.
const unsetConfigValue = "<unset>"

// configValueForHuman renders one entry's value for a terminal.
//
// Only an entry that is BOTH empty and unconfigured reads as unset: a key
// explicitly set to "" has source `set`, and printing it as `<unset>` would
// erase exactly the distinction `source` exists to carry. JSON is untouched —
// the envelope already distinguishes the two, and a consumer parsing `value`
// must not have to strip a human marker out of it.
func configValueForHuman(entry db.ConfigEntry) string {
	if entry.Value == "" && entry.Source != "set" {
		return unsetConfigValue
	}
	return entry.Value
}

// formatConfigEntries renders the listing for human mode, aligned on the key.
func formatConfigEntries(entries []db.ConfigEntry) string {
	width := 0
	for _, e := range entries {
		if len(e.Key) > width {
			width = len(e.Key)
		}
	}

	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%-*s  %s", width, e.Key, configValueForHuman(e))
		if e.Source == "default" {
			b.WriteString("  (default)")
		}
	}
	return b.String()
}

func init() {
	configSetCmd.Flags().Bool("global", false,
		"Set the store-wide default instead of this project's override")
	configGetCmd.Flags().Bool("global", false,
		"Read the store-wide values, ignoring this project's overrides")
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configGetCmd)
}
