package engine

import (
	"os"

	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// ActionNames returns every step name declared `action = "<name>"` across the
// instance-config workflow roots (DKT-1283 AC5) — the shared corpus and this
// repository's own `.docket/config/workflows/`.
//
// It exists so a trust roster entry can be classified `gate` or `action`
// without grepping the TOML files, the way `docket-run`'s gate-probe.js used
// to: it read `action = "<name>"` out of every `workflows/*.toml` by regex
// because the trust roster itself carries no class marker. The engine already
// walks these same roots for auto-registration (scanConfigDirs); this reuses
// that walk and reads each workflow's OWN parsed grammar instead of a regex.
//
// A workflow file that fails to parse is SKIPPED rather than refused: this is
// a classification aid for `trust list`/`trust probe`, not a registration
// gate, and a genuinely broken definition is refused where registration
// itself matters. The set is best-effort and errs toward under-classifying —
// a name this misses is reported as a "gate", which is the conservative
// direction (an action mistaken for a gate merely fails its stdin-less probe;
// a gate mistaken for an action would go unprobed).
func ActionNames() (map[string]bool, error) {
	names := make(map[string]bool)

	scan, err := scanConfigDirs(resolvePaths().InstanceConfigDirs())
	if err != nil {
		return nil, err
	}
	if scan == nil {
		// F17 dormancy: no instance-config root exists, so no workflow declares
		// any action. An empty set, not an error.
		return names, nil
	}

	for _, path := range scan.paths {
		if isSchemaConfigPath(path) {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		def, err := workflow.Parse(src)
		if err != nil {
			continue
		}
		for _, step := range def.Steps {
			if step.Action != "" {
				names[step.Action] = true
			}
		}
	}
	return names, nil
}
