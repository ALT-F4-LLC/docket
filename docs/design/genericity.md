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
