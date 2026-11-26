package output

import "errors"

// JSONVersion identifies which JSON envelope shape a command emits.
type JSONVersion int

const (
	// JSONNone means no JSON output was requested (human mode).
	JSONNone JSONVersion = iota
	// JSONV2 is the uniform envelope: collection results
	// render as {items, total, truncated}.
	JSONV2
)

// ParseJSONMode maps the raw --json flag value to a JSONVersion.
//
// The flag is a string with NoOptDefVal "v2", so a bare --json arrives here as
// "v2" and an absent flag as "". The boolean spellings are accepted because
// --json was a Bool flag before v2 existed: --json=true and --json=false parsed
// successfully then, so rejecting them now would be a silent compatibility
// break for any script using the explicit form. The v1 dialect is no longer
// selectable from the flag; the error names only v2 so a caller is never
// pointed back at a retired dialect.
func ParseJSONMode(raw string) (JSONVersion, error) {
	switch raw {
	case "":
		return JSONNone, nil
	case "v2", "true", "1":
		return JSONV2, nil
	case "false", "0":
		return JSONNone, nil
	default:
		return JSONNone, errors.New("invalid value for --json: want v2 (bare --json selects it)")
	}
}
