#!/usr/bin/env bash
#
# ZS — THE DIFF-SCOPE GATE (scripts/qa/diff-scope.sh).
#
# Each case builds a fresh scratch repository, commits one change on top of a
# base commit, and runs the gate through its real entry point with
# DOCKET_GATE_BASE naming that base, the way the engine runs a completion gate
# after the step has committed. A refusal must exit 1 AND name the rule it
# broke, so a fixture defect that happens to exit 1 cannot pass as a refusal.
# The last two cases pin the fail-closed contract: no usable base is exit 2.

# Hermetic git for the fixtures and the gate, as in gate-baseref-regression.sh:
# an operator's global commit.gpgsign or hooks must not decide these cases.
zs_git() {
  GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git -C "$ZS_REPO" "$@"
}

# zs_fixture DIR — a repository with one base commit holding a document, a Go
# file, two files in a/ and one in b/. Sets ZS_REPO and ZS_BASE.
zs_fixture() {
  ZS_REPO="$1"
  mkdir -p "$ZS_REPO/a" "$ZS_REPO/b"
  printf 'guide\n' > "$ZS_REPO/guide.md"
  printf 'package main\n' > "$ZS_REPO/main.go"
  printf 'one\n' > "$ZS_REPO/a/one.txt"
  printf 'two\n' > "$ZS_REPO/a/two.txt"
  printf 'three\n' > "$ZS_REPO/b/three.txt"
  zs_git init -q
  zs_git config user.email "qa@example.invalid"
  zs_git config user.name "qa"
  zs_git add -A
  zs_git commit -q -m base
  ZS_BASE=$(zs_git rev-parse HEAD)
}

zs_commit() {
  zs_git add -A
  zs_git commit -q -m change
}

# zs_run TRACK BASE — run the gate from the scratch repository. Sets ZS_EXIT
# and ZS_OUT (stdout and stderr). An empty BASE runs with the variable unset.
zs_run() {
  local track="$1" base="$2" out
  out=$(qa_mktemp)
  ZS_EXIT=0
  if [ -n "$base" ]; then
    (cd "$ZS_REPO" && GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
      DOCKET_GATE_BASE="$base" bash "$SCRIPT_DIR/qa/diff-scope.sh" "$track") \
      >"$out" 2>&1 || ZS_EXIT=$?
  else
    (cd "$ZS_REPO" && unset DOCKET_GATE_BASE \
      && GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null \
      bash "$SCRIPT_DIR/qa/diff-scope.sh" "$track") \
      >"$out" 2>&1 || ZS_EXIT=$?
  fi
  ZS_OUT=$(cat "$out")
  rm -f "$out"
}

# zs_expect ID EXIT NEEDLE — one check: the exit status and the line that
# explains it.
zs_expect() {
  local id="$1" want="$2" needle="$3"
  if [ "$ZS_EXIT" -eq "$want" ] && printf '%s\n' "$ZS_OUT" | grep -qF -- "$needle"; then
    check "ZS" "$id" "PASS"
  else
    check "ZS" "$id" "FAIL" "expected exit $want with '$needle', got exit $ZS_EXIT: $(printf '%s\n' "$ZS_OUT" | head -3 | tr '\n' ' ')"
  fi
}

test_zs_diff_scope() {
  printf "Section ZS: diff-scope track rules"

  local ZS i
  ZS=$(qa_mktemp_d)

  # docs-only: every touched path is a document.
  zs_fixture "$ZS/docs-md"
  printf 'more\n' >> "$ZS_REPO/guide.md"
  zs_commit
  zs_run docs-only "$ZS_BASE"
  zs_expect "ZS1_docs_md" 0 "diff-scope: ok (docs-only: 1 paths"

  zs_fixture "$ZS/docs-go"
  printf 'func main() {}\n' >> "$ZS_REPO/main.go"
  zs_commit
  zs_run docs-only "$ZS_BASE"
  zs_expect "ZS2_docs_go" 1 "not a document: main.go"

  # trivial: one path, nothing added or deleted, at most ten changed lines.
  zs_fixture "$ZS/trivial-edit"
  printf 'uno\n' > "$ZS_REPO/a/one.txt"
  zs_commit
  zs_run trivial "$ZS_BASE"
  zs_expect "ZS3_trivial_edit" 0 "diff-scope: ok (trivial: 1 paths, 2 changed lines)"

  zs_fixture "$ZS/trivial-add"
  printf 'new\n' > "$ZS_REPO/a/new.txt"
  zs_commit
  zs_run trivial "$ZS_BASE"
  zs_expect "ZS4_trivial_added" 1 "added file(s): a/new.txt; trivial adds nothing"

  zs_fixture "$ZS/trivial-lines"
  for i in 1 2 3 4 5 6 7 8 9 10 11; do
    printf 'line %s\n' "$i" >> "$ZS_REPO/a/one.txt"
  done
  zs_commit
  zs_run trivial "$ZS_BASE"
  zs_expect "ZS5_trivial_11_lines" 1 "11 changed lines; trivial allows 10"

  # small: at most two paths, one directory, no added file.
  zs_fixture "$ZS/small-pair"
  printf 'more\n' >> "$ZS_REPO/a/one.txt"
  printf 'more\n' >> "$ZS_REPO/a/two.txt"
  zs_commit
  zs_run small "$ZS_BASE"
  zs_expect "ZS6_small_two_files" 0 "diff-scope: ok (small: 2 paths"

  zs_fixture "$ZS/small-three"
  printf 'more\n' >> "$ZS_REPO/guide.md"
  printf 'more\n' >> "$ZS_REPO/main.go"
  printf 'more\n' >> "$ZS_REPO/a/one.txt"
  zs_commit
  zs_run small "$ZS_BASE"
  zs_expect "ZS7_small_three_paths" 1 "3 paths touched; small allows two"

  zs_fixture "$ZS/small-dirs"
  printf 'more\n' >> "$ZS_REPO/a/one.txt"
  printf 'more\n' >> "$ZS_REPO/b/three.txt"
  zs_commit
  zs_run small "$ZS_BASE"
  zs_expect "ZS8_small_two_dirs" 1 "paths span two directories (a, b)"

  zs_fixture "$ZS/small-add"
  printf 'new\n' > "$ZS_REPO/a/new.txt"
  zs_commit
  zs_run small "$ZS_BASE"
  zs_expect "ZS9_small_added" 1 "added file(s): a/new.txt; small adds no file"

  # No usable base: every track fails closed rather than measuring the
  # committed tree, which is empty.
  zs_run small ""
  zs_expect "ZS10_base_unset" 2 "DOCKET_GATE_BASE is unset"

  zs_run small "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
  zs_expect "ZS11_base_unresolved" 2 "does not resolve to a commit"

  rm -rf "$ZS"
}
