package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Engine configuration keys (engine-spec.md §1: "engine defaults: lease TTLs
// per class, attempt caps, budget default, context caps"). Values live in the
// meta table, prefixed so they cannot collide with schema_version or any other
// internal key.
const (
	metaConfigPrefix = "config."

	// KeyLeaseTTLDefault is the fallback lease TTL for any class without its
	// own entry.
	KeyLeaseTTLDefault = "lease.ttl.default"
	// KeyLeaseTTLPrefix is the per-class TTL namespace: lease.ttl.<class>.
	// The class is an opaque string — core never interprets its value
	// (engine-spec.md §11.1 [limits]).
	KeyLeaseTTLPrefix = "lease.ttl."
	// KeyAttemptMax caps retries per entity (engine-spec.md §11.1
	// max_attempts).
	KeyAttemptMax = "attempt.max"
	// KeyBudgetDefault is the per-run budget cap; 0 means unlimited.
	// Enforcement lands with runs; the key exists now so the default is
	// pinned before the verbs that read it.
	KeyBudgetDefault = "budget.default"
	// KeyBudgetUnit names WHICH recorded usage unit the run cap counts
	// (docs/tdd/runs-dispatch.md §4.5 B16). Empty — the default and the only
	// value core ships — means `reported` is 0 and the cap rests on the
	// declared-cost floor alone.
	//
	// A config key rather than a workflow field because the cap is a run-level
	// control (engine-spec §11.3), so the unit it counts is run-level too.
	// Putting it on a step would let two steps in one run disagree about what
	// the run's budget means.
	KeyBudgetUnit = "budget.unit"
	// KeyUsageBudgetDefault is the default cap over MEASURED usage — the
	// second budget dimension (DKT-238); 0 means unlimited.
	//
	// It is a SEPARATE key from budget.default because the two count different
	// things and are not commensurable. `budget.default` counts declared
	// expected costs, which is a discipline over how much WORK a run schedules;
	// this counts what the ledger recorded, which is a bound on what the work
	// actually consumed. A run wants both, and one number cannot say both.
	KeyUsageBudgetDefault = "budget.usage.default"
	// KeyUsageBudgetUnit names WHICH recorded usage unit the measured cap
	// counts. Empty — the default — leaves the dimension DORMANT: a cap with
	// no unit has nothing to count, so it enforces nothing.
	//
	// Deliberately distinct from budget.unit. That key names the unit whose
	// reported total may RAISE the declared-cost spend (B16's max); this one
	// names the unit the measured cap is taken over. Sharing one key would
	// make an operator setting a token unit for one dimension silently arm the
	// other.
	KeyUsageBudgetUnit = "budget.usage.unit"
	// KeyDispatchTTL is how long a dispatch manifest stays open before `next`
	// auto-abandons it (docs/tdd/runs-dispatch.md §4.11, §5.5 P12).
	//
	// It is engine-side rather than instance policy for the reason §11.3 gives
	// for every other enforced number: the expiry is what keeps a crashed relay
	// from wedging a run, and a bound only a live relay could set would be a
	// bound the crash case never gets.
	KeyDispatchTTL = "dispatch.ttl"
	// KeyDispatchGrace is how long a claimed step may go unrecorded before it
	// counts as a dispatch discrepancy (§5.8 D1).
	//
	// It is DELIBERATELY a different key from `lease.ttl.default` even though
	// both measure silence. A lease TTL decides when the engine takes work back;
	// this decides when a relay's batch is judged unreconciled. Sharing one key
	// would make an operator lengthening leases silently stop `next` refusing.
	KeyDispatchGrace = "dispatch.grace"

	// KeyContextWarnBytes and KeyContextErrorBytes are the context-size caps.
	KeyContextWarnBytes  = "context.warn_bytes"
	KeyContextErrorBytes = "context.error_bytes"

	// KeyEventsRetain is the RETENTION WINDOW: how long an event must have
	// existed before `events prune` may delete it
	// (docs/tdd/events-follow.md §5.3 P12).
	//
	// It is engine-spec §3's "artifact-retention boundary", implemented as the
	// one window a future artifact GC would also read. §3 lists prune and
	// "artifact GC per run-retention config" as one lifecycle, and no artifact
	// GC ships at stage 7 — so read literally there would be no boundary to
	// cross. Implementing it as this window means the GC inherits a boundary
	// already enforced rather than one retrofitted, and the reading can only
	// ever REFUSE MORE than the literal one would.
	//
	// THE READING IS RECORDED AS AN AMENDMENT rather than made silently,
	// including the objection a reviewer would raise — that a key spelled
	// `events.retain` governing only events is not what "artifact-retention"
	// names.
	//
	// The default is "0", which means RETAIN EVERYTHING: prune refuses every
	// event until an operator states a policy. That is the dormant posture —
	// Docket deletes nothing an operator did not ask it to delete.
	KeyEventsRetain = "events.retain"

	// KeyVoteRulePrefix is the named-threshold-configuration namespace:
	// vote.rule.<name>.threshold and vote.rule.<name>.criticality
	// (gates-trust §8.3).
	//
	// A workflow's `type="vote"` step names a rule rather than passing flags,
	// because a step cannot pass flags. The <name> is an OPAQUE string exactly
	// as lease.ttl.<class>'s class is, and this reuses the config machinery
	// rather than adding a table: a rule "exists" iff its `.threshold` is set.
	KeyVoteRulePrefix = "vote.rule."
	// KeyVoteRuleThresholdSuffix and KeyVoteRuleCriticalitySuffix complete a
	// rule's two keys.
	KeyVoteRuleThresholdSuffix   = ".threshold"
	KeyVoteRuleCriticalitySuffix = ".criticality"

	// KeyVoteHoldRule and KeyVoteHoldVoters configure how a MATERIALIZED HELD
	// step is decided: by one operator (the default) or by a tally.
	//
	// A held step is the one step in a run no author declared — the engine
	// mints it when a `hold_spread` trips, so its `voters` and `vote_rule`
	// cannot come from a `[[step]]` table the way a declared vote step's do.
	// These two keys are where an instance says them instead, and they sit in
	// the `vote.` family beside `vote.rule.<name>` because that is the same
	// subject: how a vote is tallied and who casts it.
	//
	// BOTH ARE EMPTY BY DEFAULT, and empty means EXACTLY the prior behavior —
	// held steps are minted `human` and one operator approves or rejects them.
	// A tally is something an instance opts into, never something core assumes,
	// for the same reason core ships no default threshold: a roster nobody
	// chose is not a roster.
	//
	// They are ONE PAIR rather than per-workflow settings because a hold is the
	// engine's own question about its own computation. It is asked identically
	// whichever pipeline held, so who answers it is a project-level policy.
	KeyVoteHoldRule   = "vote.hold.rule"
	KeyVoteHoldVoters = "vote.hold.voters"

	// KeyAutoRegister toggles §9's auto-registration: whether `run activate`
	// registers a workflow/schema it finds in an instance-config root
	// (~/.docket/config, <repo>/.docket/config) on its own, or leaves that to
	// an explicit `workflow register` / `schema register`.
	//
	// DEFAULT TRUE — auto-registration is the zero-touch behavior §9 exists
	// for, and an operator who wants it off states that, rather than every
	// operator who wants it (the common case) opting in project by project.
	//
	// Project-scoped, like any other config key (v12): `--global` sets the
	// store-wide default every project without its own override falls back
	// to, and a bare `config set` overrides ONE project — the two knobs the
	// requirement asks for ("a given project vs all projects") are exactly
	// GetConfig's existing project-override-then-store-wide resolution, not a
	// second mechanism.
	//
	// It gates ONLY registration, not the pinning half of the same scan
	// (contracts/, fragments/, policy.toml). Pinning has no version to adopt —
	// it is "read the current bytes", which is what a repo with this off
	// still needs to render a step's `packet`. Turning registration off and
	// pinning off together would leave a project unable to activate at all
	// without hand-supplying every `--pin` the corpus already offers, which is
	// not what "I don't want silent version upgrades" is asking for.
	KeyAutoRegister = "registration.auto"
)

// ConfigValueKind classifies a config key for validation at `set` time — so a
// bad value fails where the user can see it, not later at read time.
type ConfigValueKind int

const (
	// KindDuration is a Go duration string ("15m", "2h30m").
	KindDuration ConfigValueKind = iota
	// KindPositiveInt is an integer >= 1.
	KindPositiveInt
	// KindNonNegativeNumber is a number >= 0.
	KindNonNegativeNumber
	// KindNonNegativeInt is an integer >= 0.
	KindNonNegativeInt
	// KindUnitFraction is a float in (0, 1] — a vote rule's approval
	// threshold, which the existing `vote create --threshold` already takes in
	// exactly that range.
	KindUnitFraction
	// KindCriticality is one of low|medium|high|critical, the criticality the
	// existing proposal machinery already understands.
	KindCriticality
	// KindRetentionWindow is a duration that may also be ZERO, where zero means
	// "retain everything" rather than "retain nothing"
	// (docs/tdd/events-follow.md §5.3 P13).
	//
	// It is a separate kind from KindDuration because that one rejects zero —
	// correctly, since a zero lease TTL would expire a claim the instant it was
	// made. Here zero is the DEFAULT and the safe end of the range: a retention
	// window nobody set must protect every event, not expose every event.
	KindRetentionWindow
	// KindUnitName is an OPAQUE unit name, or empty. Core never enumerates
	// units and never has a default one, so the only validation possible is the
	// shape a name must have to be usable as a ledger key — the same caps
	// `--usage` applies to the names it records (§4.9 B36): at most 64 bytes,
	// printable ASCII, no whitespace. Anything beyond that would be core
	// deciding which units exist.
	KindUnitName
	// KindName is a single OPAQUE name, or empty — KindUnitName's rule without
	// the unit vocabulary, for a key whose value names something other than a
	// recorded unit.
	KindName
	// KindNameList is a comma-separated list of OPAQUE names, or empty.
	//
	// It validates each entry's SHAPE and nothing else, for the same reason
	// KindUnitName does: core never enumerates the members of such a list and
	// holds no opinion about what any of them denotes. What it does check is
	// that the list can be split back into the names that were put in — no
	// empty entry, and no duplicate, since a caller that counts the entries
	// would count a repeat twice and demand a decision from a name that can
	// only make one.
	KindNameList
	// KindBool is a boolean, in any of strconv.ParseBool's spellings
	// ("true"/"false", "1"/"0", "t"/"f"). Stored and read back verbatim, like
	// every other kind — a reader that needs the parsed bool calls ParseBool
	// itself, the same way a reader of KindDuration calls ParseDuration.
	KindBool
)

// NameMaxBytes caps an opaque name stored in config or recorded in a ledger,
// per §1.3's security note: such a name is attacker-controlled text that lands
// in a ledger and in a report. ONE number for every opaque-name validator, so
// two of them cannot drift apart.
const NameMaxBytes = 64

// UnitNameMaxBytes is NameMaxBytes under the name `--usage`'s validator has
// always called it.
const UnitNameMaxBytes = NameMaxBytes

// validateName is the shape rule every opaque name obeys: printable ASCII
// without whitespace, within the cap.
//
// Printable ASCII without whitespace, because such a name is rendered into a
// terminal beside other columns and a name carrying control bytes or a newline
// would break the rendering of every row after it. It says nothing about WHICH
// names are meaningful — core has no list.
//
// `noun` names the thing in the refusal, so one rule can serve several keys
// while each still refuses in its own author-facing vocabulary.
func validateName(noun, name string) error {
	if len(name) > NameMaxBytes {
		return fmt.Errorf("%s %q is %d bytes; the limit is %d",
			noun, name, len(name), NameMaxBytes)
	}
	for _, r := range name {
		if r < '!' || r > '~' {
			return fmt.Errorf(
				"%s %q must be printable ASCII without whitespace", noun, name)
		}
	}
	return nil
}

// ValidateUnitName is B36's shape rule for a unit name, shared by
// `--usage`'s parser and `docket config set budget.unit`.
//
// `tokens`, `pages`, and `sheets` are equally valid: core has no list.
func ValidateUnitName(name string) error {
	return validateName("unit name", name)
}

// SplitNameList parses a KindNameList value into its names, ignoring the space
// an author may have written after a comma. An empty value yields no names,
// which is how every KindNameList key spells "unset".
//
// It is the ONE reader of the encoding, so a caller can never disagree with the
// validator about where one name ends and the next begins.
func SplitNameList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ValidateNameList checks a comma-separated list of opaque names: each entry's
// shape, no empty entry, and no duplicate.
//
// `noun` names one entry in the refusal, for validateName's reason.
func ValidateNameList(noun, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			return fmt.Errorf(
				"%q has an empty entry; list %ss separated by commas", value, noun)
		}
		if err := validateName(noun, name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("%s %q is listed twice", noun, name)
		}
		seen[name] = true
	}
	return nil
}

// ConfigSpec describes one engine-configuration key.
type ConfigSpec struct {
	Key     string
	Kind    ConfigValueKind
	Default string
	Doc     string
}

// engineConfigSpecs is the known-key set with core's default values.
//
// These defaults are CORE's, not the reference instance's. engine-spec.md §8 is
// explicit that its numbers are instance data and that "core ships with no
// opinions here", so they are chosen as neutral engineering values rather than
// copied. Rationale per key is in docs/tdd/claims-leases.md §3.3.
var engineConfigSpecs = []ConfigSpec{
	{
		Key:     KeyLeaseTTLDefault,
		Kind:    KindDuration,
		Default: "15m",
		Doc:     "Fallback lease TTL for classes without their own lease.ttl.<class>",
	},
	{
		Key:     KeyAttemptMax,
		Kind:    KindPositiveInt,
		Default: "3",
		Doc:     "Maximum claims per entity before attempts are exhausted",
	},
	{
		Key:     KeyBudgetDefault,
		Kind:    KindNonNegativeNumber,
		Default: "0",
		Doc:     "Default per-run budget cap; 0 means unlimited",
	},
	{
		Key:  KeyBudgetUnit,
		Kind: KindUnitName,
		// Empty is the default AND the only value core ships. The doc line says
		// exactly what §4.5 argues, and it passes the stranger test on its face:
		// a team tracking a documentation sprint reads it and understands that
		// leaving it alone means their cap counts declared costs.
		Default: "",
		Doc: "Which recorded usage unit the run cap counts. Empty (the default) " +
			"means the cap rests on the declared-cost floor alone",
	},
	{
		Key:     KeyUsageBudgetDefault,
		Kind:    KindNonNegativeNumber,
		Default: "0",
		Doc: "Default per-run cap over MEASURED usage of `budget.usage.unit`; " +
			"0 means unlimited. Separate from budget.default, which counts " +
			"declared step costs",
	},
	{
		Key:  KeyUsageBudgetUnit,
		Kind: KindUnitName,
		// Empty leaves the whole dimension dormant, which is the shipped
		// behavior: a cap with no unit has nothing to count.
		Default: "",
		Doc: "Which recorded usage unit the MEASURED cap counts. Empty (the " +
			"default) leaves that cap dormant",
	},
	{
		Key:     KeyDispatchTTL,
		Kind:    KindDuration,
		Default: "30m",
		Doc: "How long a dispatch manifest stays open before `next` " +
			"auto-abandons it",
	},
	{
		Key:     KeyDispatchGrace,
		Kind:    KindDuration,
		Default: "15m",
		Doc: "How long a claimed step may go unrecorded before it counts as a " +
			"dispatch discrepancy",
	},
	{
		Key:     KeyEventsRetain,
		Kind:    KindRetentionWindow,
		Default: "0",
		Doc: "How long events are protected from `events prune`; 0 (the default) " +
			"retains everything",
	},
	{
		Key:  KeyVoteHoldRule,
		Kind: KindName,
		// Empty is the default AND the dormant posture: no rule named, so a
		// held step is minted `human` exactly as it always was. A rule named
		// here must be registered like any other (`vote.rule.<name>.threshold`),
		// and the tally refuses loudly if it is not.
		Default: "",
		Doc: "Vote rule a materialized held step is tallied under. Empty " +
			"(the default) mints held steps as `human` for one operator to decide",
	},
	{
		Key:     KeyVoteHoldVoters,
		Kind:    KindNameList,
		Default: "",
		Doc: "Comma-separated voters on a materialized held step. Empty (the " +
			"default) mints held steps as `human` for one operator to decide",
	},
	{
		Key:     KeyAutoRegister,
		Kind:    KindBool,
		Default: "true",
		Doc: "Whether `run activate` auto-registers workflows/schemas it finds " +
			"in an instance-config root. Default true; set false (project-scoped, " +
			"or --global for every project) to require `workflow register`/" +
			"`schema register` explicitly",
	},
	{
		Key:     KeyContextWarnBytes,
		Kind:    KindNonNegativeInt,
		Default: "65536",
		Doc:     "Context size at which a warning is emitted, in bytes",
	},
	{
		Key:     KeyContextErrorBytes,
		Kind:    KindNonNegativeInt,
		Default: "131072",
		Doc:     "Context size at which an error is raised, in bytes",
	},
}

// ErrUnknownConfigKey is returned for a key outside the known set. A typo'd
// key must not silently store a value nothing reads.
var ErrUnknownConfigKey = errors.New("unknown config key")

// LookupConfigSpec resolves a key to its spec. Per-class lease TTLs
// (lease.ttl.<class>) are matched dynamically: the class is an opaque string,
// so the set of valid keys is open by design.
func LookupConfigSpec(key string) (ConfigSpec, error) {
	for _, spec := range engineConfigSpecs {
		if spec.Key == key {
			return spec, nil
		}
	}

	if class, ok := strings.CutPrefix(key, KeyLeaseTTLPrefix); ok && class != "" {
		return ConfigSpec{
			Key:     key,
			Kind:    KindDuration,
			Default: "", // falls back to lease.ttl.default
			Doc:     fmt.Sprintf("Lease TTL for executor class %q", class),
		}, nil
	}

	// vote.rule.<name>.threshold / .criticality (gates-trust §8.3), matched
	// dynamically for the same reason the per-class TTL is: <name> is an
	// opaque string, so the set of valid keys is open by design.
	if rest, ok := strings.CutPrefix(key, KeyVoteRulePrefix); ok {
		if name, found := strings.CutSuffix(rest, KeyVoteRuleThresholdSuffix); found && name != "" {
			return ConfigSpec{
				Key:  key,
				Kind: KindUnitFraction,
				Doc:  fmt.Sprintf("Approval threshold for vote rule %q", name),
			}, nil
		}
		if name, found := strings.CutSuffix(rest, KeyVoteRuleCriticalitySuffix); found && name != "" {
			return ConfigSpec{
				Key:     key,
				Kind:    KindCriticality,
				Default: string(model.CriticalityMedium),
				Doc:     fmt.Sprintf("Criticality for vote rule %q", name),
			}, nil
		}
	}

	return ConfigSpec{}, fmt.Errorf("%w: %q (known keys: %s)",
		ErrUnknownConfigKey, key, strings.Join(KnownConfigKeys(), ", "))
}

// KnownConfigKeys lists the fixed keys, plus the open-ended patterns.
func KnownConfigKeys() []string {
	keys := make([]string, 0, len(engineConfigSpecs)+3)
	for _, spec := range engineConfigSpecs {
		keys = append(keys, spec.Key)
	}
	keys = append(keys,
		KeyLeaseTTLPrefix+"<class>",
		KeyVoteRulePrefix+"<name>"+KeyVoteRuleThresholdSuffix,
		KeyVoteRulePrefix+"<name>"+KeyVoteRuleCriticalitySuffix,
	)
	return keys
}

// ValidateConfigValue checks a value against its key's kind.
func ValidateConfigValue(spec ConfigSpec, value string) error {
	switch spec.Kind {
	case KindDuration:
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s must be a duration such as \"15m\" or \"2h\": %w", spec.Key, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s must be a positive duration, got %q", spec.Key, value)
		}
	case KindPositiveInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be an integer, got %q", spec.Key, value)
		}
		if n < 1 {
			return fmt.Errorf("%s must be >= 1, got %d", spec.Key, n)
		}
	case KindNonNegativeInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be an integer, got %q", spec.Key, value)
		}
		if n < 0 {
			return fmt.Errorf("%s must be >= 0, got %d", spec.Key, n)
		}
	case KindNonNegativeNumber:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("%s must be a number, got %q", spec.Key, value)
		}
		if f < 0 {
			return fmt.Errorf("%s must be >= 0, got %s", spec.Key, value)
		}
	case KindUnitFraction:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("%s must be a number, got %q", spec.Key, value)
		}
		// The same range `vote create --threshold` already takes: zero would
		// approve on no support at all, and above one is unreachable.
		if f <= 0 || f > 1 {
			return fmt.Errorf("%s must be greater than 0 and at most 1, got %s",
				spec.Key, value)
		}
	case KindCriticality:
		if err := model.ValidateCriticality(model.Criticality(value)); err != nil {
			return fmt.Errorf("%s: %w", spec.Key, err)
		}
	case KindRetentionWindow:
		// "0" is the documented way to say "retain everything", so it is parsed
		// before the duration parser gets it — `time.ParseDuration("0")` happens
		// to accept a bare zero, but relying on that would make the key's
		// contract depend on a parser's tolerance.
		if value == "0" {
			return nil
		}
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf(
				"%s must be a duration such as \"720h\", or 0 to retain everything: %w",
				spec.Key, err)
		}
		if d < 0 {
			return fmt.Errorf(
				"%s must not be negative, got %q — 0 retains everything",
				spec.Key, value)
		}
	case KindUnitName:
		// Empty is legal and is the default: it means "the cap rests on the
		// floor alone" rather than "unset and therefore broken".
		if value == "" {
			return nil
		}
		if err := ValidateUnitName(value); err != nil {
			return fmt.Errorf("%s: %w", spec.Key, err)
		}
	case KindName:
		// Empty is legal and is the default, for KindUnitName's reason.
		if value == "" {
			return nil
		}
		if err := validateName("name", value); err != nil {
			return fmt.Errorf("%s: %w", spec.Key, err)
		}
	case KindNameList:
		// Empty is legal and is the default, for the same reason: an unset list
		// is a policy nobody stated, not a broken one.
		if err := ValidateNameList("name", value); err != nil {
			return fmt.Errorf("%s: %w", spec.Key, err)
		}
	case KindBool:
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf(
				"%s must be a boolean (true/false), got %q", spec.Key, value)
		}
	}
	return nil
}

// VoteRuleThresholdKey and VoteRuleCriticalityKey build a rule's two keys, so
// the string concatenation lives in one place rather than at every reader.
func VoteRuleThresholdKey(rule string) string {
	return KeyVoteRulePrefix + rule + KeyVoteRuleThresholdSuffix
}

func VoteRuleCriticalityKey(rule string) string {
	return KeyVoteRulePrefix + rule + KeyVoteRuleCriticalitySuffix
}

// VoteRuleExists reports whether a rule is registered.
//
// A rule EXISTS IFF its threshold is set (§8.3). Criticality has a default, so
// it cannot be the existence test: a rule with only a criticality set would
// tally at no threshold at all.
func VoteRuleExists(db *sql.DB, projectID int, rule string) (bool, error) {
	entry, err := GetConfig(db, projectID, VoteRuleThresholdKey(rule))
	if err != nil {
		return false, err
	}
	return entry.Source == "set", nil
}

// VoteRuleExistsTx and RegisteredVoteRulesTx are the two above inside a
// CALLER'S transaction, for S6's auto-registration (docs/tdd/runs-dispatch.md
// §9.2 F7/F8): the scan validates a workflow's `vote_rule` references with the
// SAME rules `workflow register` applies, and it does so inside activation's fat
// transaction, where a pool read would deadlock against the one-connection pool.
//
// They read `meta` directly rather than going through GetConfig, because a
// vote rule's key is DYNAMIC (`vote.rule.<name>.threshold`) and has no spec row
// to look up — which is exactly what VoteRuleExists' own `Source == "set"` check
// is testing for. "Set" here means "a row exists", and that is one query.
func VoteRuleExistsTx(tx *sql.Tx, projectID int, rule string) (bool, error) {
	keys := []string{metaConfigPrefix + VoteRuleThresholdKey(rule)}
	if projectID != 0 {
		keys = append(keys, projectConfigKey(projectID, VoteRuleThresholdKey(rule)))
	}
	for _, key := range keys {
		var value string
		err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("reading vote rule %s: %w", rule, err)
		}
	}
	return false, nil
}

// RegisteredVoteRulesTx lists the set rules — store-wide plus the project's
// own — so a refusal inside activation can name the alternatives exactly as
// `workflow register`'s does.
func RegisteredVoteRulesTx(tx *sql.Tx, projectID int) ([]string, error) {
	prefixes := []string{metaConfigPrefix + KeyVoteRulePrefix}
	if projectID != 0 {
		prefixes = append(prefixes,
			fmt.Sprintf("%sp%d.%s", metaConfigPrefix, projectID, KeyVoteRulePrefix))
	}

	seen := map[string]bool{}
	for _, prefix := range prefixes {
		rows, err := tx.Query(
			`SELECT key FROM meta WHERE key LIKE ? ORDER BY key`,
			prefix+"%"+KeyVoteRuleThresholdSuffix,
		)
		if err != nil {
			return nil, fmt.Errorf("listing vote rules: %w", err)
		}
		for rows.Next() {
			var metaKey string
			if err := rows.Scan(&metaKey); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning a vote rule: %w", err)
			}
			rule := strings.TrimPrefix(metaKey, prefix)
			rule = strings.TrimSuffix(rule, KeyVoteRuleThresholdSuffix)
			if rule != "" {
				seen[rule] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("listing vote rules: %w", err)
		}
		rows.Close()
	}

	out := make([]string, 0, len(seen))
	for rule := range seen {
		out = append(out, rule)
	}
	sort.Strings(out)
	return out, nil
}

// RegisteredVoteRules lists the rules that have a threshold set, so a refusal
// can name the alternatives rather than only the mistake.
func RegisteredVoteRules(db *sql.DB, projectID int) ([]string, error) {
	entries, err := ListConfig(db, projectID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Source != "set" {
			continue
		}
		rest, ok := strings.CutPrefix(e.Key, KeyVoteRulePrefix)
		if !ok {
			continue
		}
		if name, found := strings.CutSuffix(rest, KeyVoteRuleThresholdSuffix); found && name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// projectConfigKey renders the meta key for one project's override (v12).
// Global values keep the bare `config.<key>` form — which is exactly what a
// pre-v12 store already holds, so every legacy value becomes the store-wide
// default without a migration touching it.
func projectConfigKey(projectID int, key string) string {
	return fmt.Sprintf("%sp%d.%s", metaConfigPrefix, projectID, key)
}

// storeWideKeys are the keys that are ONE policy for the whole store, never a
// per-project override: `events.retain` governs a prune over a shared event
// stream, and a per-project window would let one project's policy delete rows
// another project's audit still needs. Set and Get both normalize these to
// projectID 0, so `config set events.retain` lands where EventsRetain reads.
var storeWideKeys = map[string]bool{
	KeyEventsRetain: true,
}

// effectiveConfigProject collapses a store-wide key's project to 0.
func effectiveConfigProject(projectID int, key string) int {
	if storeWideKeys[key] {
		return 0
	}
	return projectID
}

// SetConfig stores a validated engine-configuration value. A non-zero
// projectID writes the project's override; zero writes the store-wide
// default every project falls back to.
func SetConfig(db *sql.DB, projectID int, key, value string) error {
	spec, err := LookupConfigSpec(key)
	if err != nil {
		return err
	}
	if err := ValidateConfigValue(spec, value); err != nil {
		return err
	}

	metaKey := metaConfigPrefix + key
	if projectID = effectiveConfigProject(projectID, key); projectID != 0 {
		metaKey = projectConfigKey(projectID, key)
	}
	_, err = db.Exec(
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaKey, value,
	)
	if err != nil {
		return fmt.Errorf("storing config %s: %w", key, err)
	}
	return nil
}

// ConfigEntry is one key's effective value and where it came from.
type ConfigEntry struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"` // "set" or "default"
}

// GetConfig returns a key's effective value for one project, and whether it
// was explicitly set.
//
// Resolution (v12): the project's own override, then the store-wide value,
// then the builtin default. A zero projectID reads the store-wide value
// directly. An unset key returns its default with source "default", so a
// caller can tell "nobody configured this" from "somebody configured this to
// the same value".
func GetConfig(db *sql.DB, projectID int, key string) (ConfigEntry, error) {
	spec, err := LookupConfigSpec(key)
	if err != nil {
		return ConfigEntry{}, err
	}

	var value string
	if projectID = effectiveConfigProject(projectID, key); projectID != 0 {
		err = db.QueryRow(
			`SELECT value FROM meta WHERE key = ?`, projectConfigKey(projectID, key),
		).Scan(&value)
		if err == nil {
			return ConfigEntry{Key: key, Value: value, Source: "set"}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ConfigEntry{}, fmt.Errorf("reading config %s: %w", key, err)
		}
	}

	err = db.QueryRow(
		`SELECT value FROM meta WHERE key = ?`, metaConfigPrefix+key,
	).Scan(&value)
	if err == nil {
		return ConfigEntry{Key: key, Value: value, Source: "set"}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConfigEntry{}, fmt.Errorf("reading config %s: %w", key, err)
	}

	// Unset per-class TTL falls back to the default TTL, not to empty.
	if spec.Default == "" && strings.HasPrefix(key, KeyLeaseTTLPrefix) {
		fallback, err := GetConfig(db, projectID, KeyLeaseTTLDefault)
		if err != nil {
			return ConfigEntry{}, err
		}
		return ConfigEntry{Key: key, Value: fallback.Value, Source: "default"}, nil
	}

	return ConfigEntry{Key: key, Value: spec.Default, Source: "default"}, nil
}

// GetConfigTx is GetConfig inside a CALLER'S transaction, for readers that must
// resolve a key while holding the single pooled connection — the scheduler's
// snapshot, which is loaded entirely inside one transaction and would deadlock
// against a pool read rather than fail (the same constraint VoteRuleExistsTx
// was written for).
//
// It resolves in GetConfig's order — the project's override, the store-wide
// value, the builtin default — by delegating the shape decisions to the SAME
// spec lookup and the same key builders. What it does not share is the
// per-class TTL fallback: nothing reads a lease TTL from inside a transaction,
// and a second implementation of a fallback is how two readers of one key start
// disagreeing. A caller that needs one reaches for LeaseTTL outside the
// transaction, as every current caller already does.
func GetConfigTx(tx *sql.Tx, projectID int, key string) (ConfigEntry, error) {
	spec, err := LookupConfigSpec(key)
	if err != nil {
		return ConfigEntry{}, err
	}

	metaKeys := []string{}
	if projectID = effectiveConfigProject(projectID, key); projectID != 0 {
		metaKeys = append(metaKeys, projectConfigKey(projectID, key))
	}
	metaKeys = append(metaKeys, metaConfigPrefix+key)

	for _, metaKey := range metaKeys {
		var value string
		err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaKey).Scan(&value)
		if err == nil {
			return ConfigEntry{Key: key, Value: value, Source: "set"}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ConfigEntry{}, fmt.Errorf("reading config %s: %w", key, err)
		}
	}
	return ConfigEntry{Key: key, Value: spec.Default, Source: "default"}, nil
}

// ListConfig returns every fixed key's effective value for one project, plus
// every per-class TTL that has been explicitly set — store-wide or as a
// project override, the override winning. Sorted for deterministic output.
func ListConfig(db *sql.DB, projectID int) ([]ConfigEntry, error) {
	entries := make([]ConfigEntry, 0, len(engineConfigSpecs))
	for _, spec := range engineConfigSpecs {
		entry, err := GetConfig(db, projectID, spec.Key)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	// Explicitly-set class TTLs, from the store-wide namespace first and the
	// project namespace second so an override replaces its fallback.
	classTTLs := map[string]ConfigEntry{}
	collect := func(pattern, trimPrefix string) error {
		rows, err := db.Query(
			`SELECT key, value FROM meta WHERE key LIKE ? ORDER BY key`, pattern)
		if err != nil {
			return fmt.Errorf("listing config: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var metaKey, value string
			if err := rows.Scan(&metaKey, &value); err != nil {
				return fmt.Errorf("scanning config: %w", err)
			}
			key := strings.TrimPrefix(metaKey, trimPrefix)
			if key == KeyLeaseTTLDefault {
				continue // already listed among the fixed keys
			}
			classTTLs[key] = ConfigEntry{Key: key, Value: value, Source: "set"}
		}
		return rows.Err()
	}
	if err := collect(metaConfigPrefix+KeyLeaseTTLPrefix+"%", metaConfigPrefix); err != nil {
		return nil, err
	}
	if projectID != 0 {
		prefix := fmt.Sprintf("%sp%d.", metaConfigPrefix, projectID)
		if err := collect(prefix+KeyLeaseTTLPrefix+"%", prefix); err != nil {
			return nil, err
		}
	}

	keys := make([]string, 0, len(classTTLs))
	for k := range classTTLs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entries = append(entries, classTTLs[k])
	}
	return entries, nil
}

// EventsRetain resolves the retention window, with 0 meaning "retain
// everything" (docs/tdd/events-follow.md §5.3).
//
// It returns the DURATION rather than a cutoff timestamp, because the caller
// computes the cutoff inside the prune's own transaction against that
// transaction's clock — a helper that read the clock here would hand back a
// boundary that had already moved by the time it was applied.
// EventsRetain is deliberately STORE-WIDE (projectID 0): the event stream and
// its prune are one machine-level lifecycle, and a per-project window would
// let one project's policy delete rows another project's audit still needs.
func EventsRetain(db *sql.DB) (time.Duration, error) {
	entry, err := GetConfig(db, 0, KeyEventsRetain)
	if err != nil {
		return 0, err
	}
	if entry.Value == "0" || entry.Value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(entry.Value)
	if err != nil {
		return 0, fmt.Errorf("parsing %s %q: %w", KeyEventsRetain, entry.Value, err)
	}
	return d, nil
}

// AutoRegisterEnabledTx resolves KeyAutoRegister inside a CALLER'S
// transaction — activation's own, since that is the only place this reads
// (§9's scan runs inside activateTx, and a pool read there would deadlock
// against the one-connection pool exactly as VoteRuleExistsTx's doc explains).
//
// The parse cannot fail on a value that reached storage: SetConfig already
// ran it through ValidateConfigValue's KindBool case, which is the same
// strconv.ParseBool this calls.
func AutoRegisterEnabledTx(tx *sql.Tx, projectID int) (bool, error) {
	entry, err := GetConfigTx(tx, projectID, KeyAutoRegister)
	if err != nil {
		return false, err
	}
	enabled, err := strconv.ParseBool(entry.Value)
	if err != nil {
		return false, fmt.Errorf("parsing %s %q: %w", KeyAutoRegister, entry.Value, err)
	}
	return enabled, nil
}

// LeaseTTL resolves the effective lease TTL for an executor class, falling back
// to lease.ttl.default when the class has no entry of its own. An empty class
// means "use the default".
func LeaseTTL(db *sql.DB, projectID int, class string) (time.Duration, error) {
	key := KeyLeaseTTLDefault
	if class != "" {
		key = KeyLeaseTTLPrefix + class
	}
	entry, err := GetConfig(db, projectID, key)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(entry.Value)
	if err != nil {
		return 0, fmt.Errorf("parsing lease TTL %q for class %q: %w", entry.Value, class, err)
	}
	return d, nil
}

// VoteRuleSetElsewhere counts how many OTHER projects configure a rule, and
// returns one of the values they use (DKT-264).
//
// It exists because "vote_rule X is not registered" was true and useless in the
// one case that actually happens: a corpus workflow is shared across every
// project and its thresholds are not, so a freshly-registered project refuses a
// definition that works everywhere else in the store. The remedy is one
// store-wide set — the fallback for it has existed all along — and nothing said
// so, which is how thirteen projects came to hold thirteen per-project copies of
// the same three thresholds while `config.vote.rule.*` sat empty.
//
// The VALUE is returned so the refusal can quote a real number instead of
// `<0-1>`. It is one of the values in use rather than a consensus: if two
// projects disagree, the operator is the one who decides which is right, and
// inventing an average would be core forming an opinion about a threshold.
//
// The project's OWN row is excluded, so this answers "elsewhere" literally. A
// caller reaching this has already established the rule is unset here.
func VoteRuleSetElsewhere(
	db *sql.DB, projectID int, rule string,
) (projects int, value string, err error) {
	self := projectConfigKey(projectID, VoteRuleThresholdKey(rule))
	rows, err := db.Query(
		`SELECT key, value FROM meta
		  WHERE key LIKE ? ESCAPE '\' AND key != ?`,
		`config.p%.`+escapeLike(VoteRuleThresholdKey(rule)), self)
	if err != nil {
		return 0, "", fmt.Errorf("looking for vote rule %s elsewhere: %w", rule, err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return 0, "", fmt.Errorf("reading a vote rule row: %w", err)
		}
		projects++
		if value == "" {
			value = v
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("looking for vote rule %s elsewhere: %w", rule, err)
	}
	return projects, value, nil
}

// VoteRuleSetElsewhereTx is VoteRuleSetElsewhere inside a caller's transaction,
// for activation's auto-registration — where a pool read would deadlock against
// the one-connection pool rather than fail.
func VoteRuleSetElsewhereTx(
	tx *sql.Tx, projectID int, rule string,
) (projects int, value string, err error) {
	self := projectConfigKey(projectID, VoteRuleThresholdKey(rule))
	rows, err := tx.Query(
		`SELECT key, value FROM meta
		  WHERE key LIKE ? ESCAPE '\' AND key != ?`,
		`config.p%.`+escapeLike(VoteRuleThresholdKey(rule)), self)
	if err != nil {
		return 0, "", fmt.Errorf("looking for vote rule %s elsewhere: %w", rule, err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return 0, "", fmt.Errorf("reading a vote rule row: %w", err)
		}
		projects++
		if value == "" {
			value = v
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("looking for vote rule %s elsewhere: %w", rule, err)
	}
	return projects, value, nil
}
