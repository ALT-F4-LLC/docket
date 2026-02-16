package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

type configInfo struct {
	DBPath        string `json:"db_path"`
	DBSizeBytes   int64  `json:"db_size_bytes"`
	SchemaVersion int    `json:"schema_version"`
	IssuePrefix   string `json:"issue_prefix"`
	DocketPathEnv string `json:"docket_path_env"`
	DocketPathSet bool   `json:"docket_path_set"`
}

var configCmd = &cobra.Command{
	Use:         "config",
	Short:       "Display docket configuration",
	Annotations: map[string]string{"skipDB": "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		cfg := getCfg(cmd)

		docketPathEnv := os.Getenv("DOCKET_PATH")

		exists, err := cfg.Exists()
		if err != nil {
			return cmdErr(fmt.Errorf("checking database: %w", err), output.ErrGeneral)
		}

		if !exists {
			w.Warn("No docket database found. Run 'docket init' to create one.")

			info := configInfo{
				DBPath:        cfg.DBPath,
				DBSizeBytes:   0,
				SchemaVersion: 0,
				IssuePrefix:   model.IDPrefix,
				DocketPathEnv: docketPathEnv,
				DocketPathSet: cfg.EnvVarSet,
			}

			w.Success(info, formatConfigHuman(info, true))

			return nil
		}

		conn, err := db.Open(cfg.DBPath)
		if err != nil {
			return cmdErr(fmt.Errorf("opening database: %w", err), output.ErrGeneral)
		}
		defer conn.Close()

		schemaVersion, err := db.SchemaVersion(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("reading schema version: %w", err), output.ErrGeneral)
		}

		stat, err := os.Stat(cfg.DBPath)
		if err != nil {
			return cmdErr(fmt.Errorf("reading database file: %w", err), output.ErrGeneral)
		}
		dbSize := stat.Size()

		info := configInfo{
			DBPath:        cfg.DBPath,
			DBSizeBytes:   dbSize,
			SchemaVersion: schemaVersion,
			IssuePrefix:   model.IDPrefix,
			DocketPathEnv: docketPathEnv,
			DocketPathSet: cfg.EnvVarSet,
		}

		w.Success(info, formatConfigHuman(info, false))

		return nil
	},
}

func formatSize(bytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)

	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func formatEnvValue(val string) string {
	if val == "" {
		return "(not set)"
	}
	return val
}

func formatConfigHuman(info configInfo, notFound bool) string {
	if !render.ColorsEnabled() {
		return formatConfigPlain(info, notFound)
	}

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	valStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))

	var lines string
	lines = headerStyle.Render("Docket Configuration") + "\n\n"

	// DB path with green/red indicator.
	if notFound {
		indicator := lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("●")
		lines += fmt.Sprintf("  %s %s %s\n", keyStyle.Render("Database path:"), indicator, valStyle.Render(info.DBPath+" (not found)"))
	} else {
		indicator := lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("●")
		lines += fmt.Sprintf("  %s %s %s\n", keyStyle.Render("Database path:"), indicator, valStyle.Render(info.DBPath))
	}

	if !notFound {
		lines += fmt.Sprintf("  %s  %s\n", keyStyle.Render("Database size:"), valStyle.Render(formatSize(info.DBSizeBytes)))
		lines += fmt.Sprintf("  %s %s\n", keyStyle.Render("Schema version:"), valStyle.Render(fmt.Sprintf("%d", info.SchemaVersion)))
	}

	lines += fmt.Sprintf("  %s   %s\n", keyStyle.Render("Issue prefix:"), valStyle.Render(info.IssuePrefix))

	envVal := formatEnvValue(info.DocketPathEnv)
	lines += fmt.Sprintf("  %s    %s", keyStyle.Render("DOCKET_PATH:"), valStyle.Render(envVal))

	return lines
}

func formatConfigPlain(info configInfo, notFound bool) string {
	dbPath := info.DBPath
	if notFound {
		dbPath = fmt.Sprintf("%s (not found)", info.DBPath)
	}

	lines := fmt.Sprintf("Database path:   %s\n", dbPath)
	if !notFound {
		lines += fmt.Sprintf("Database size:   %s\n", formatSize(info.DBSizeBytes))
		lines += fmt.Sprintf("Schema version:  %d\n", info.SchemaVersion)
	}
	lines += fmt.Sprintf("Issue prefix:    %s\n", info.IssuePrefix)
	lines += fmt.Sprintf("DOCKET_PATH:     %s", formatEnvValue(info.DocketPathEnv))

	return lines
}

func init() {
	rootCmd.AddCommand(configCmd)
}
