package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
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
	dbKey  contextKey = "db"
	cfgKey contextKey = "cfg"
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

var rootCmd = &cobra.Command{
	Use:     "docket",
	Short:   "Local-first CLI issue tracker",
	Version: fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, buildDate),
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Resolve()
		if err != nil {
			return err
		}

		ctx := context.WithValue(cmd.Context(), cfgKey, cfg)

		if _, ok := cmd.Annotations["skipDB"]; ok {
			cmd.SetContext(ctx)
			return nil
		}

		if _, err := os.Stat(cfg.DBPath); os.IsNotExist(err) {
			return cmdErr(
				fmt.Errorf("no docket database found, run 'docket init' to create one"),
				output.ErrNotFound,
			)
		}

		conn, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}

		cmd.SetContext(context.WithValue(ctx, dbKey, conn))
		return nil
	},
	PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
		conn, ok := cmd.Context().Value(dbKey).(*sql.DB)
		if ok && conn != nil {
			return conn.Close()
		}
		return nil
	},
}

func init() {
	rootCmd.PersistentFlags().Bool("json", false, "Output in JSON format")
	rootCmd.PersistentFlags().BoolP("quiet", "q", false, "Suppress non-essential output")
	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true
}

func getWriter(cmd *cobra.Command) *output.Writer {
	jsonMode, _ := cmd.Flags().GetBool("json")
	quietMode, _ := cmd.Flags().GetBool("quiet")
	return output.New(jsonMode, quietMode)
}

func getCfg(cmd *cobra.Command) *config.Config {
	cfg, _ := cmd.Context().Value(cfgKey).(*config.Config)
	return cfg
}

func getDB(cmd *cobra.Command) *sql.DB {
	conn, _ := cmd.Context().Value(dbKey).(*sql.DB)
	return conn
}

// Execute runs the root command and returns an exit code.
func Execute() int {
	if err := rootCmd.Execute(); err != nil {
		jsonMode, _ := rootCmd.PersistentFlags().GetBool("json")
		quietMode, _ := rootCmd.PersistentFlags().GetBool("quiet")
		w := output.New(jsonMode, quietMode)

		var ce *CmdError
		if errors.As(err, &ce) {
			return w.Error(ce.Err, ce.Code)
		}
		return w.Error(err, output.ErrGeneral)
	}
	return 0
}
