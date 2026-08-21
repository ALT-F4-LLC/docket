# CI routing-sweep corpus

`TestShippedWorkflowsRouteEveryIssueState`, `TestShippedWorkflowsRouteFullPowerset`,
`TestNoKindListEnumeratesTheClosedSet`, and `TestAddingASixthKindKeepsEveryStateRoutable`
in `../../routing_sweep_test.go` sweep whatever workflows
`config.Config.InstanceConfigDirs` resolves — by design, so the check can
never certify a corpus the engine would never load. On a developer's
provisioned machine that resolves to a real, operator-owned shared store
(`~/.docket/config/workflows`); a bare CI checkout has no such store, so
before this fixture existed the sweep hard-failed on every PR with "no
workflows found" (see `loadShippedWorkflows`'s "KNOWN GAP" comment).

This directory is a small, self-contained, hermetic corpus that exists
SOLELY to give CI something to sweep. `.github/workflows/ci.yaml` points
`go test` at it with `DOCKET_SWEEP_WORKFLOWS_DIR` — the override
`workflowDirs()` already supports, which REPLACES rather than unions with
the normally-resolved directories. That is the load-bearing property: a
developer's shared store may ship its own catch-all default workflow, and
this fixture ships one too (`ci-standard`, below); two independent
catch-alls loaded together always multi-match on an issue with none of
either corpus's labels, so this fixture must never be discoverable through
the normal (unioning) resolution path — it is reached only via the
override, never by living under `.docket/config/`.

The seven workflows form a strict precedence chain (`ci-urgent` >
`ci-security-review` > `ci-breaking-change` > `ci-infra` >
`ci-experimental` > `ci-docs-only` > the `ci-standard` catch-all), each
excluding, via `unless_labels`, every label that outranks it — the same
construction the routing sweep itself is asking for: exactly one workflow
binds any (kind, label-subset) pair, including a kind this repository's
closed set doesn't define yet. None of the seven declare a `[match].kind`
clause, so they route every current and future issue kind identically.
Every label is `ci-`-prefixed to keep this corpus's vocabulary visibly
distinct from any real operator corpus, even though the override means it
is never actually unioned with one.
