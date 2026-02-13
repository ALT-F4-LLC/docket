# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Docket is a Go CLI application built with the [Vorpal](https://github.com/ALT-F4-LLC/vorpal) build system and SDK. The project uses Vorpal for reproducible builds, development environments, and artifact management.

## Build System (Vorpal)

Vorpal is a build system that manages toolchains and artifacts. Configuration lives in `Vorpal.toml` and the lockfile is `Vorpal.lock`.

### Common Commands

```bash
# Build the docket binary
vorpal build docket

# Build the development shell environment (provides Go, goimports, gopls, staticcheck, protoc)
vorpal build docket-shell

# Run a built artifact
vorpal run <artifact-name>

# Export a built artifact
vorpal build docket --export
```

The `--system` flag can target: `aarch64-darwin`, `aarch64-linux`, `x8664-darwin`, `x8664-linux`.

## Architecture

### Two Entrypoints

- **`cmd/docket/main.go`** — The actual docket CLI application (the thing being built).
- **`cmd/vorpal/main.go`** — The Vorpal build configuration (defines how to build docket and its dev environment). This is Go code that uses the Vorpal SDK to declare artifacts, not a runtime application.

### Vorpal Build Configuration (`cmd/vorpal/main.go`)

This file defines two artifacts:
1. **`docket-shell`** — A project environment with Go toolchain, linters (staticcheck), formatters (goimports), language server (gopls), and protobuf tools (protoc, protoc-gen-go, protoc-gen-go-grpc). Builds with `CGO_ENABLED=0`.
2. **`docket`** — The Go binary built from `cmd/docket` with `language.NewGo()`.

### Dependencies

- **`github.com/ALT-F4-LLC/vorpal/sdk/go`** — Vorpal SDK for build configuration
- Go 1.24.2

## Linear Issue Tracking

This project uses Linear via MCP tools for issue tracking. See `AGENTS.md` for the full workflow including session initialization, title format conventions (`[<branch>] <description>`), scoping rules, and session completion requirements.
