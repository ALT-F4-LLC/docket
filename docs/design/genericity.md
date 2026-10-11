# The genericity rule

Status: approved — 2026-08-02 · verbatim extract (preamble rule, §7, §9.1) from doc 06
of the graph-engine design set — the upstream design record. This file exists so the
rule travels with the repo as a standing PR review bar.

## The rule (binding on every workflow-engine feature)

Core Docket contains zero agent/LLM vocabulary — no node, model, prompt, brief,
severity, or review concepts. Executors are opaque string hints; execution metadata is
an opaque KV bag; ordered meaning comes from user-registered schemas; computations
beyond scheduling arithmetic are user-trusted commands. The stranger test for every
verb: *a team that has never run an LLM should find it useful as documented.*

## Admitted forms (repository amendment, 2026-10-10)

This section is not part of the upstream extract. The run policy ships a variant
routing key named `model`: a string the engine copies from the policy to the step row.
The engine also reads it: it compares the value against `[security].never`, redirects
a forbidden value through `[escalation.fallback]`, and special-cases `fable` and `opus`
during escalation. The key is persisted (`json:"model,omitempty"` on step
rows and voter assignments), so renaming it would break stored data. `scripts/qa/genericity.sh` therefore
admits these forms of `model`, and only these:

- the routing key: `model = "..."` in policy TOML, and `model=` or `model =` in
  key/value renderings;
- the key as a struct tag: `json:"model..."` and `toml:"model"`;
- the routing triple: `model/effort/variant` and `model, effort`;
- the `MODEL` column header of the policy seat table;
- two shipped policy strings: "still-forbidden model redirected through" and "no
  permitted model reachable from";
- senses unrelated to agents: exported identifiers of the `internal/model` package,
  the retired "link-farm model" config layout, and the `threat-model` seat name.

Every other use of `model` in core surface still fails the gate, including new prose
such as "model routing" in an error string. Admitting a new form means adding it here
and to the gate's `ADMITTED` list in the same change.

## Out of scope (hard boundaries)

No daemon; no network; no model calls; no prompt/agent vocabulary anywhere in core
(the genericity rule — a PR introducing `model`, `prompt`, or `llm` into core surface
fails review by definition; metadata KV exists precisely so such things ride through
opaquely); no execution outside the trust model (engine-spec §4); no cross-repo
federation in v1 (upstream D6); no VCS coupling beyond the declared `issue.diff`
provider (git in v1, engine-spec §11.1); board/UI growth limited to opt-in run/step
columns.

## The stranger test (acceptance criterion 1)

A human-only demo — a docs-review workflow with two steps, one fenced-command gate, no
agents anywhere — is definable, runnable, and comprehensible from public docs alone,
with zero references to AI concepts.
