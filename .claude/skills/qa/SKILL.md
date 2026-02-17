---
name: qa
description: >
  Run quality assurance checks on the docket CLI binary. Use this skill when the
  user asks to "run QA", "test the build", "verify the binary", "smoke test",
  "run quality assurance", "check the CLI", or wants to validate that docket is
  working correctly after a build.
---

# Docket QA Skill

Run the docket QA test suite by executing the reusable `scripts/qa.sh` script.
This script builds docket, runs all functional checks across every command and
flag, validates output, exit codes, error handling, and JSON contracts, then
prints a structured pass/fail report.

ARGUMENTS: optional path to the docket binary. If not provided, the script
builds from source automatically.

## Workflow

### 1. Run the QA script

```bash
./scripts/qa.sh [optional-binary-path]
```

The script handles everything:
- Builds docket (if no binary path given)
- Creates isolated temp directories so tests don't affect user state
- Runs all test sections in order (A through S)
- Cleans up temp directories on exit
- Prints a full pass/fail report with details on failures
- Exits 0 if all checks pass, 1 if any fail

### 2. Review the output

The script prints a summary table and final result line:

```
QA Result: X/Y checks passed
```

If any checks failed, they are listed at the bottom for visibility.

### 3. Report results to the user

Summarize the QA results. If there are failures, investigate and fix them.

## Test Coverage

The script covers these sections (see `scripts/qa.sh` for full details):

| Section | Description | Checks |
|---------|-------------|--------|
| A | No-DB commands (version, help, config) | 5 |
| B | Init lifecycle | 4 |
| C | Config after init | 2 |
| D | DOCKET_PATH override | 3 |
| E | Quiet mode | 2 |
| F | Create command | 11 |
| G | List command (filters, sorting, tree) | 12 |
| H | Show command | 6 |
| I | Move command (status transitions) | 11 |
| J | Close command | 6 |
| K | Reopen command | 6 |
| L | Edit command (all flags) | 15 |
| M | Edit reparenting (cycles, self-ref) | 11 |
| N | Delete — simple | 5 |
| O | Delete — cascade and orphan | 16 |
| P | Activity log verification | 6 |
| Q | JSON contract validation | 11 |
| R | Exit codes (all commands) | 23 |
| S | Error paths (no DB) | 10 |

## Developer Usage

Developers can run the script directly without Claude:

```bash
# Build and test
./scripts/qa.sh

# Test a specific binary
./scripts/qa.sh /path/to/docket

# After a vorpal build
vorpal build docket --export
./scripts/qa.sh ./result/bin/docket
```

Requires `jq` to be installed for JSON validation.
