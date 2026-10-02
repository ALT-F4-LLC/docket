#!/usr/bin/env bash
#
# vuln-scan — known-vulnerability scan of the dependency graph.
#
# Wired as the `vuln-scan` gate on security-load-bearing.toml's `implement` and
# `fix` steps — the two write steps of the pipeline that handles changes with a
# security dimension.
#
# It is `govulncheck`, not a generic CVE feed: govulncheck reports only
# vulnerabilities REACHABLE from this module's call graph, so a finding here is
# one this binary can actually hit rather than one merely present in go.sum.
# That is the difference between a gate a team acts on and one it learns to
# ignore.
#
# It deliberately checks nothing else. `secret-scan` looks for credentials in
# the diff; this looks at dependencies.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

# govulncheck shells out to a binary named `go`, and this toolchain ships the
# compiler only as go1.26.6 (the name build.sh and tests.sh call). Without a
# `go` on PATH it reports "no go.mod file", which reads as a repo defect and is
# not one. A scratch shim named `go` that execs go1.26.6 is placed first on
# PATH for this gate only.
shim="$(mktemp -d "${TMPDIR:-/tmp}/vuln-scan-go.XXXXXX")"
trap 'rm -rf "$shim"' EXIT
printf '#!/bin/sh\nexec go1.26.6 "$@"\n' > "$shim/go"
chmod +x "$shim/go"
export PATH="$shim:$PATH"
echo "=== vuln-scan: govulncheck ./... ==="

# govulncheck is not vendored. UNAVAILABLE IS A FAILURE, NOT A PASS: the whole
# point of this gate is that a security-load-bearing change was checked, and
# "we couldn't check, so carry on" is what makes a control decorative. The
# engine records an unmatched gate as a failure for exactly this reason
# (internal/engine/gate_exec.go, N3) and this script agrees with it.
if ! command -v govulncheck >/dev/null 2>&1; then
  cat >&2 <<'EOF'
vuln-scan FAILED: govulncheck is not installed.

Install it with:
  go install golang.org/x/vuln/cmd/govulncheck@latest

This gate fails rather than skipping. A security-load-bearing change whose
vulnerability scan did not run has not been scanned, and recording that as a
pass would misreport the change's review coverage.
EOF
  exit 1
fi

# Capture the scan's combined output so a nonzero exit can be CLASSIFIED
# before this script asserts anything about its cause. govulncheck exits
# nonzero both for findings and for failures to run at all — network errors
# reaching the vulnerability database, a malformed module graph — and blaming
# vulnerabilities for a fetch failure sends an operator hunting for a finding
# that never existed (DKT-39: three one-second "scans" that never reached
# vuln.go.dev were each reported as a reachable vulnerability).
SCAN_OUT="$(mktemp "${TMPDIR:-/tmp}/vuln-scan.XXXXXX")"
trap 'rm -f "$SCAN_OUT"' EXIT

set +e
govulncheck ./... >"$SCAN_OUT" 2>&1
SCAN_RC=$?
set -e
cat "$SCAN_OUT"

if [ "$SCAN_RC" -eq 0 ]; then
  echo "vuln-scan: ok"
  exit 0
fi

# The database-unreachable case carries govulncheck's own transport diagnostic
# in the output; a genuine finding never does. Match on those signatures
# rather than on exit codes, which govulncheck does not document as stable.
INFRA_PATTERN='no such host|connection refused|connection reset|i/o timeout|network is unreachable|TLS handshake|tls: |x509: |dial tcp|proxyconnect|failed to fetch|unable to fetch|temporary failure in name resolution'
if grep -qiE "$INFRA_PATTERN" "$SCAN_OUT"; then
  # Name the host it could not reach, from the failing URL if one was printed.
  UNREACHED_HOST="$(grep -oE 'https?://[^/" ]+' "$SCAN_OUT" | head -1 | sed 's|https\?://||')"
  UNREACHED_HOST="${UNREACHED_HOST:-vuln.go.dev}"
  cat >&2 <<EOF

vuln-scan FAILED: INFRASTRUCTURE — could not reach the vulnerability database
(host: ${UNREACHED_HOST}). No scan ran, so nothing is known about
vulnerabilities either way.

This is deliberately still a failure: a security-load-bearing change whose
scan could not run has not been scanned, and passing here would make the
control decorative. But do NOT go hunting for a vulnerability — fix the
network path to ${UNREACHED_HOST} (sandbox egress allowlist, DNS, TLS
interception) and re-run the gate.
EOF
  exit 2
fi

cat >&2 <<'EOF'

vuln-scan FAILED: at least one reachable vulnerability was reported.

Each finding above names the vulnerable symbol and the path that reaches it.
Reachability is the bar: these are callable from this module, not merely
present in the dependency graph.
EOF
exit 1
