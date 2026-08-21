# The golden corpus

`TestCorpus` is the renovate gate of docs/tdd/payloads-thresholds.md §4.1.

A JSON Schema validator is not an ordinary dependency: a patch bump can change
a validation verdict, which changes whether a worker's `complete` is refused —
a behavior change arriving through a dependency PR nobody reads as a behavior
change. Each file here is one (schema, document, verdict, message-substring)
case, so a verdict change surfaces as a red CI on the bump rather than as a
refused `complete` in production.

Every field name in here is an INSTANCE TOKEN — `risk`, `tier`, `count`. They
are test data, exactly as engine-spine §1.1 recorded the fixture's own tokens,
and they name nothing core interprets.
