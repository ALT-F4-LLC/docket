package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// linkFileVersion is the only link-file format version this build reads or
// writes.
const linkFileVersion = 1

// errLinkFileParse marks a link file that passes the integrity checks but
// does not parse.
var errLinkFileParse = errors.New("link file parse")

// linkFile is the user-level link file: which server profile and daemon
// project each local project path uses. It lives beside client.toml and is
// never read from or written to a repository.
//
// Entries are a TOML table keyed by project path, so one path can hold only
// one entry: the decoder refuses a duplicate key, and relinking a path
// replaces its value.
type linkFile struct {
	Version int                  `toml:"version"`
	Links   map[string]linkEntry `toml:"link"`
}

// linkEntry is one project path's link. An empty Profile means the local
// daemon socket.
type linkEntry struct {
	Profile   string `toml:"profile"`
	ProjectID int32  `toml:"project_id"`
}

var projectLinkCmd = &cobra.Command{
	Use:   "link",
	Short: "Link this project to a daemon server profile and project",
	Long: `Record which daemon server profile and daemon project this project uses.

The link is written to links.toml in the user config directory
($XDG_CONFIG_HOME/docket, default ~/.config/docket), keyed by this project's
path. Nothing is written to the repository. Running link again in the same
project replaces its entry.

--profile names a [profile.<name>] table in client.toml; omit it to use the
local daemon socket.`,
	Args:        cobra.NoArgs,
	Annotations: map[string]string{"skipDB": ""},
	RunE:        runProjectLink,
}

func runProjectLink(cmd *cobra.Command, args []string) error {
	profile, _ := cmd.Flags().GetString("profile")
	projectID, _ := cmd.Flags().GetInt32("project-id")
	if projectID <= 0 {
		return cmdErr(fmt.Errorf("--project-id must be a positive daemon project ID, got %d", projectID),
			output.ErrValidation)
	}

	cfg := getCfg(cmd)
	if cfg == nil || cfg.Identity == "" {
		return cmdErr(errors.New("the project path could not be resolved"), output.ErrValidation)
	}

	client, err := config.LoadClientConfig()
	if err != nil {
		return linkCmdError(err)
	}
	if _, ok := client.Profiles[profile]; profile != "" && !ok {
		return cmdErr(fmt.Errorf("profile %q is not defined: %s has no [profile.%s] table", profile, client.Path, profile),
			output.ErrValidation)
	}

	path := filepath.Join(filepath.Dir(client.Path), "links.toml")
	links, err := loadLinkFile(path)
	if err != nil {
		return linkCmdError(err)
	}
	links.Links[cfg.Identity] = linkEntry{Profile: profile, ProjectID: projectID}
	if err := writeLinkFile(path, links); err != nil {
		return linkCmdError(err)
	}

	target := profile
	if target == "" {
		target = "the local daemon socket"
	}
	getWriter(cmd).Success(struct {
		Path      string `json:"path"`
		Profile   string `json:"profile"`
		ProjectID int32  `json:"project_id"`
		LinkFile  string `json:"link_file"`
	}{cfg.Identity, profile, projectID, path},
		fmt.Sprintf("linked %s to project %d on %s", cfg.Identity, projectID, target))
	return nil
}

// loadLinkFile reads the link file after the trust file's integrity checks
// pass on it. A missing file is an empty set of links.
func loadLinkFile(path string) (*linkFile, error) {
	if err := trust.CheckPrivateFile(path, "the link file"); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &linkFile{Version: linkFileVersion, Links: map[string]linkEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the link file %s: %w", path, err)
	}

	var lf linkFile
	md, err := toml.Decode(string(data), &lf)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not valid TOML: %v", errLinkFileParse, path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("%w: %s has unknown key %q", errLinkFileParse, path, undecoded[0].String())
	}
	if lf.Version != linkFileVersion {
		return nil, fmt.Errorf("%w: %s declares version %d, but this build understands version %d only", errLinkFileParse, path, lf.Version, linkFileVersion)
	}
	if lf.Links == nil {
		lf.Links = map[string]linkEntry{}
	}
	return &lf, nil
}

// writeLinkFile publishes the link file atomically: the directory is created
// 0700, and a 0600 temp file in it is synced and renamed over the target, so
// a reader sees the whole old file or the whole new one.
func writeLinkFile(path string, lf *linkFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	// Checked again at write time: the directory may have been created or
	// replaced since the read.
	if err := trust.CheckPrivateFile(path, "the link file"); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".links-*.toml")
	if err != nil {
		return fmt.Errorf("writing the link file %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // no-op once the rename succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("writing the link file %s: %w", path, err)
	}
	if err := toml.NewEncoder(tmp).Encode(lf); err != nil {
		return fmt.Errorf("writing the link file %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("writing the link file %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the link file %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("writing the link file %s: %w", path, err)
	}
	return nil
}

// linkCmdError maps a client-config or link-file failure onto the error
// taxonomy: a file that fails its integrity checks or does not parse is the
// operator's to fix (VALIDATION_ERROR); a filesystem refusal is not.
func linkCmdError(err error) error {
	switch {
	case errors.Is(err, trust.ErrIntegrity),
		errors.Is(err, config.ErrClientConfigParse),
		errors.Is(err, errLinkFileParse):
		return cmdErr(err, output.ErrValidation)
	default:
		return cmdErr(err, output.ErrGeneral)
	}
}

func init() {
	projectLinkCmd.Flags().String("profile", "",
		"server profile from client.toml (omit for the local daemon socket)")
	projectLinkCmd.Flags().Int32("project-id", 0,
		"daemon project ID to use for this project (required)")
	_ = projectLinkCmd.MarkFlagRequired("project-id")
	rootCmd.AddCommand(projectLinkCmd)
}
