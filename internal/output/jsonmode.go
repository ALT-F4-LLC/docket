package output

import "fmt"

// JSONVersion identifies which JSON envelope shape a command emits.
type JSONVersion int

const (
	// JSONNone means no JSON output was requested (human mode).
	JSONNone JSONVersion = iota
	// JSONV1 is the original envelope: {ok, data, message}, where data is the
	// command's own result shape. Frozen — never change its output.
	JSONV1
	// JSONV2 is the uniform envelope (engine-spec.md §5): collection results
	// render as {items, total, truncated}.
	JSONV2
)

// ParseJSONMode maps the raw --json flag value to a JSONVersion.
//
// The flag is a string with NoOptDefVal "v1", so a bare --json arrives here as
// "v1" and an absent flag as "". The boolean spellings are accepted because
// --json was a Bool flag before v2 existed: --json=true and --json=false parsed
// successfully then, so rejecting them now would be a silent compatibility
// break for any script using the explicit form.
func ParseJSONMode(raw string) (JSONVersion, error) {
	switch raw {
	case "":
		return JSONNone, nil
	case "v1", "true", "1":
		return JSONV1, nil
	case "v2":
		return JSONV2, nil
	case "false", "0":
		return JSONNone, nil
	default:
		return JSONNone, fmt.Errorf(
			"invalid value %q for --json: want one of v1, v2, true, false", raw,
		)
	}
}
