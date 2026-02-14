package main

import (
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:         "init",
	Short:       "Initialize a new docket database",
	Annotations: map[string]string{"skipDB": "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		cfg := getCfg(cmd)

		exists, err := cfg.Exists()
		if err != nil {
			return cmdErr(fmt.Errorf("checking database: %w", err), output.ErrGeneral)
		}

		if exists {
			w.Warn("Database already exists at %s", cfg.DBPath)

			conn, err := db.Open(cfg.DBPath)
			if err != nil {
				return cmdErr(fmt.Errorf("opening database: %w", err), output.ErrGeneral)
			}
			defer conn.Close()

			schemaVersion, err := db.SchemaVersion(conn)
			if err != nil {
				return cmdErr(fmt.Errorf("reading schema version: %w", err), output.ErrGeneral)
			}

			w.Success(struct {
				Path          string `json:"path"`
				DBPath        string `json:"db_path"`
				SchemaVersion int    `json:"schema_version"`
				Created       bool   `json:"created"`
			}{
				Path:          cfg.DocketDir,
				DBPath:        cfg.DBPath,
				SchemaVersion: schemaVersion,
				Created:       false,
			}, "Database already initialized")

			return nil
		}

		if err := os.MkdirAll(cfg.DocketDir, 0o755); err != nil {
			return cmdErr(fmt.Errorf("creating directory: %w", err), output.ErrGeneral)
		}

		conn, err := db.Open(cfg.DBPath)
		if err != nil {
			return cmdErr(fmt.Errorf("opening database: %w", err), output.ErrGeneral)
		}
		defer conn.Close()

		if err := db.Initialize(conn); err != nil {
			return cmdErr(fmt.Errorf("initializing schema: %w", err), output.ErrGeneral)
		}

		if err := db.Migrate(conn); err != nil {
			return cmdErr(fmt.Errorf("migrating schema: %w", err), output.ErrGeneral)
		}

		schemaVersion, err := db.SchemaVersion(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("reading schema version: %w", err), output.ErrGeneral)
		}

		w.Success(struct {
			Path          string `json:"path"`
			DBPath        string `json:"db_path"`
			SchemaVersion int    `json:"schema_version"`
			Created       bool   `json:"created"`
		}{
			Path:          cfg.DocketDir,
			DBPath:        cfg.DBPath,
			SchemaVersion: schemaVersion,
			Created:       true,
		}, "Initialized docket database")

		w.Info("Initialized docket database at %s", cfg.DBPath)
		w.Info("Consider adding .docket/ to your .gitignore")

		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
