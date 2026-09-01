package exec

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// allowedEnv is §5.3's table: the ONLY parent variables a child may inherit.
//
// ALLOWLIST, NEVER DENYLIST. The child environment is CONSTRUCTED, so a
// variable is present only because this list names it. A denylist would fail
// open on the next environment variable anyone invents — and the variables
// worth protecting are exactly the ones nobody has thought of yet.
//
// Each entry earns its place:
//
//	PATH                     argv[0] resolution and the child's own tool
//	                         lookups; without it almost nothing runs
//	HOME                     toolchains resolve caches and config from it;
//	                         absent, many tools write to / or fail
//	USER, LOGNAME            tools that stamp output with a user name
//	SHELL                    passed but NEVER consulted by docket — some tools
//	                         read it for informational output. Docket invokes
//	                         no interpreter regardless (§5.1)
//	LANG, LC_ALL, LC_CTYPE   text encoding; absent, tools produce mojibake or
//	                         refuse non-ASCII paths
//	TZ                       timestamp rendering in a check's own output
//	TMPDIR                   scratch space; absent, tools fall back to a path
//	                         that may be unwritable
//	SSL_CERT_FILE/_DIR       a check that makes TLS connections needs the trust
//	                         store; absent, it fails confusingly
//	XDG_CACHE_HOME           build caches; absent, a rebuild from scratch on
//	                         every run
var allowedEnv = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TZ",
	"TMPDIR",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"XDG_CACHE_HOME",
}

// deniedEnv names variables that must NEVER reach a child, in addition to
// being absent from the allowlist (§5.3).
//
// DOCKET_TOKEN is the core case. A capability token in a child converts code
// execution into ENGINE AUTHORITY: the child could complete a step with a
// forged artifact under the live lease. It is absent from the allowlist AND
// asserted absent from the constructed set before every spawn — two mechanisms,
// because one of them is a table that a future editor might extend carelessly.
//
// DOCKET_PATH is denied because a check that reaches back into the database is
// doing something the trust model does not cover. A check that genuinely needs
// the repository gets DOCKET_REPO, which gives it the path without pointing it
// at the store.
//
// THE GLOBAL STORE CHANGES WHAT THIS DENIAL IS WORTH, in both directions, and
// the residual is recorded here rather than discovered later. With the store
// at repo/.docket the denial was a speed bump — the child's own DOCKET_REPO
// contained the store. With the store at ~/.docket the store is no longer
// derivable from DOCKET_REPO, so the denial becomes a real barrier against
// honest and accidental reach-back — but HOME is on the allowlist (toolchains
// need it), and a WELL-KNOWN location under it is constructible by any child
// that wants to, so the denial cannot stop a hostile TRUSTED command. That is
// the standing posture, not an oversight: trust entries are the boundary for
// hostile code (§2), a hostile trusted command's blast radius was always the
// operator's whole account, and OS-level sandboxing of children — not env
// shaping — is the mechanism that would shrink it.
var deniedEnv = []string{
	"DOCKET_TOKEN",
	"DOCKET_PATH",
}

// networkEnv names the variables forwarded ONLY to a child whose trust entry
// declares a network requirement.
//
// They are not in `allowedEnv` because most gates have no business making
// connections, and a proxy variable is a standing instruction to route traffic
// somewhere. Forwarding them universally would hand every trusted command a
// route out; withholding them from the gates that genuinely need one made a
// proxied environment unusable. Keyed off the declaration, both problems go
// away: a gate that says it needs the network gets the machinery to use it,
// and one that says nothing is unchanged.
//
// The lowercase spellings are included because the convention is genuinely
// split — curl and most C tooling read `http_proxy`, while Go and the JVM read
// `HTTP_PROXY`. Forwarding only one casing works for half the tools an
// operator might name, which is worse than either forwarding both or none.
var networkEnv = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
}

// EnvPolicy describes the context variables docket SETS on a child, as opposed
// to the ones it forwards.
type EnvPolicy struct {
	// Gate is the gate name, exported as DOCKET_GATE so a check can behave
	// differently under docket if its author wants. Opaque to core.
	Gate string
	// Repo is the repo root, exported as DOCKET_REPO — the same value as
	// Spec.Dir, for tools that need it in an environment.
	Repo string
	// Network is the host list the matched trust entry declared.
	// Non-empty opts the child into networkEnv and into DOCKET_GATE_NETWORK.
	Network []string
	// Issue is the issue the gated step belongs to, exported as DOCKET_ISSUE
	// (DKT-63). Opaque to core; a check that wants per-issue behavior finally
	// has the identity to key it on.
	Issue string
	// Scope is the issue's declared scope globs, exported newline-joined as
	// DOCKET_SCOPE (DKT-63) so a diff-shaped check can evaluate the change it
	// is gating instead of the whole dirty tree. Empty means unset: an issue
	// that declared no scope gives the check no narrower answer than the tree,
	// and inventing one would be docket deciding what the issue touches.
	Scope []string
	// Base is the sha of the step's base commit, exported as DOCKET_GATE_BASE
	// (DKT-992) so a gate can scan exactly the step's committed range
	// (base..HEAD of the tree it runs in) instead of guessing at `HEAD~1` or
	// scanning only the working tree — which, for a worktree-recorded step
	// whose executor committed before `step record`, is clean and scans
	// nothing. It is set only for worktree-recorded steps, to the worktree's
	// fork point; empty means unset, and a range-shaped gate that finds the
	// var absent over a clean tree should fail closed rather than pass having
	// measured nothing.
	Base string
}

// BuildEnv constructs the child environment (§5.3).
//
// The parent environment is read here and NOWHERE ELSE in this package, which
// is what makes TestGateChildEnvIsExactlyTheAllowlist's set-equality assertion
// meaningful: there is one construction site, so proving that site correct
// proves every child correct.
func BuildEnv(p EnvPolicy) ([]string, error) {
	env := make([]string, 0, len(allowedEnv)+4)

	for _, name := range allowedEnv {
		// An unset parent variable is OMITTED rather than set empty. An empty
		// PATH or HOME is not the same as an absent one, and inventing a value
		// docket did not have would be docket making a decision it has no
		// basis for.
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}

	// Set by docket, not inherited (§5.3's second table).
	//
	// TERM is `dumb` rather than inherited because a check's output is
	// CAPTURED, not displayed: an inherited TERM makes tools emit ANSI escapes
	// into `output`, which then pollute a run report and a golden diff.
	env = append(env, "TERM=dumb")
	// CI is the near-universal convention for "non-interactive". It makes
	// tools skip prompts and progress spinners without docket having to know
	// each tool individually.
	env = append(env, "CI=1")
	if p.Gate != "" {
		env = append(env, "DOCKET_GATE="+p.Gate)
	}
	if p.Repo != "" {
		env = append(env, "DOCKET_REPO="+p.Repo)
	}
	if p.Issue != "" {
		env = append(env, "DOCKET_ISSUE="+p.Issue)
	}
	// Newline-joined rather than JSON: the consumer is a shell check reading
	// its own environment, and `while IFS= read -r glob` over lines needs no
	// parser. A glob cannot contain a newline — the scope verbs store globs a
	// shell matched in the first place.
	if len(p.Scope) > 0 {
		env = append(env, "DOCKET_SCOPE="+strings.Join(p.Scope, "\n"))
	}
	// Absent — not empty — when no base is known (DKT-992): an empty sha is
	// not a commit, and a gate given one would build a broken git range. The
	// gate's own fail-closed check keys on absence.
	if p.Base != "" {
		env = append(env, "DOCKET_GATE_BASE="+p.Base)
	}

	// The network half, reached ONLY by a gate whose trust entry
	// declared a requirement. A gate that declared none sees exactly the
	// environment it saw before, so this cannot change any existing gate's
	// behaviour.
	if len(p.Network) > 0 {
		for _, name := range networkEnv {
			if v, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+v)
			}
		}
		// The declared hosts, exported so a check can assert its own
		// reachability up front and fail with a useful message instead of
		// whatever its underlying tool prints on a DNS error.
		env = append(env, "DOCKET_GATE_NETWORK="+strings.Join(p.Network, ","))
	}

	// THE BELT-AND-BRACES CHECK (T5). The allowlist above already excludes
	// every denied name, so this can only fire if a future edit adds one to the
	// table or a construction bug duplicates a parent variable in. It fails
	// HARD rather than filtering, because a constructed set that contains a
	// token means the construction logic is wrong, and quietly repairing wrong
	// logic is how the next version ships the leak.
	if err := assertNoDeniedVars(env); err != nil {
		return nil, err
	}

	return env, nil
}

// assertNoDeniedVars is the pre-spawn guard. Exported behavior is asserted by
// TestDocketTokenNeverReachesAGateChild, including the tampered-set case.
func assertNoDeniedVars(env []string) error {
	for _, kv := range env {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		for _, denied := range deniedEnv {
			if name == denied {
				// The VALUE is deliberately not included in the message: it is
				// a credential, and an error string lands in logs.
				return fmt.Errorf("refusing to spawn: %s is present in the constructed child environment, which must never happen", name)
			}
		}
	}
	return nil
}

// AllowedEnvNames returns the allowlist, sorted, for tests and for the
// operator-facing documentation of what a child receives. It returns a COPY:
// handing out the package's own slice would let a caller extend the allowlist
// by appending to it, which is precisely the mechanism §5.3 refuses to ship.
func AllowedEnvNames() []string {
	out := append([]string(nil), allowedEnv...)
	sort.Strings(out)
	return out
}

// DeniedEnvNames returns the explicit denylist, as a copy, for the same reason.
func DeniedEnvNames() []string {
	return append([]string(nil), deniedEnv...)
}

// There is NO WAY to extend the allowlist at this stage — no flag, no config
// key, no trust-entry field. A check that needs a credential is a use case this
// stage does not serve, and adding the escape hatch before there is a real
// requirement means shipping the mechanism that undoes the control. §10 records
// the shape a future amendment would take: a per-entry env_passthrough list,
// never global, with the names visible in `trust list`.
