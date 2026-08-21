package workflow

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// templateFS holds the shipped `workflow init` templates.
//
// They live under internal/ because the Vorpal build's include list requires
// embeds there (binding repo fact).
//
//go:embed templates
var templateFS embed.FS

const templateDir = "templates"

// TemplateNames returns the shipped template names, sorted.
//
// Template names and their step names are CORE SURFACE and carry the
// genericity rule: `parallel-check` is deliberately not named for what any
// particular team does with that shape, and its steps are `prepare`, `check`,
// `summarize`, `verify`. A template shipped under an instance-specific name
// would put an instance concept into core surface through the one file every
// new user reads first.
func TemplateNames() []string {
	entries, err := fs.ReadDir(templateFS, templateDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".toml"))
	}
	sort.Strings(names)
	return names
}

// Template returns the source of a shipped template by name.
func Template(name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("no template name given; available: %s",
			strings.Join(TemplateNames(), ", "))
	}
	// Reject a path rather than resolving it: a template name is a name, and
	// accepting `../../etc/passwd` here would make `workflow init` a file
	// reader.
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("unknown template %q; available: %s",
			name, strings.Join(TemplateNames(), ", "))
	}
	data, err := templateFS.ReadFile(path.Join(templateDir, name+".toml"))
	if err != nil {
		return nil, fmt.Errorf("unknown template %q; available: %s",
			name, strings.Join(TemplateNames(), ", "))
	}
	return data, nil
}

// TemplateFileName is the file a template is written to by `workflow init`.
func TemplateFileName(name string) string { return name + ".toml" }
