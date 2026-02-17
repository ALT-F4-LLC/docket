package config

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const dbFileName = "issues.db"

// Config holds resolved configuration for the docket directory and database.
type Config struct {
	DocketDir string // resolved .docket directory path
	DBPath    string // full path to issues.db
	EnvVarSet bool   // whether DOCKET_PATH was used
}

// Resolve returns the current configuration by checking DOCKET_PATH first,
// then falling back to $PWD/.docket.
func Resolve() (*Config, error) {
	var docketDir string
	var envVarSet bool

	if envPath := os.Getenv("DOCKET_PATH"); envPath != "" {
		docketDir = envPath
		envVarSet = true
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		docketDir = filepath.Join(cwd, ".docket")
	}

	return &Config{
		DocketDir: docketDir,
		DBPath:    filepath.Join(docketDir, dbFileName),
		EnvVarSet: envVarSet,
	}, nil
}

// Exists checks if the docket directory and DB file both exist.
// It returns an error for non-existence failures (e.g. permission errors).
func (c *Config) Exists() (bool, error) {
	if _, err := os.Stat(c.DocketDir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Stat(c.DBPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

var (
	defaultAuthor     string
	defaultAuthorOnce sync.Once
)

// DefaultAuthor returns the default author for comments and activity.
// It tries git config user.name first and falls back to the OS username.
// The result is cached for the lifetime of the process.
func DefaultAuthor() string {
	defaultAuthorOnce.Do(func() {
		defaultAuthor = resolveAuthor()
	})
	return defaultAuthor
}

func resolveAuthor() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "config", "user.name").Output()
	if err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}

	u, err := user.Current()
	if err == nil && u.Username != "" {
		return u.Username
	}

	return "unknown"
}
