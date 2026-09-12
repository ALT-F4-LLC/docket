# Developer targets and the gate targets `docket trust list` invokes.
#
# The gate targets exist so a trust entry can name `make <gate>` instead of an
# absolute path into one worktree's checkout. Trust entries are bound to the
# repository, not the worktree, and the engine spawns each gate with cwd set to
# the step's worktree — an absolute path pins every worktree's gate to whichever
# checkout happened to be current when the entry was approved, and goes dead the
# day that worktree is removed. `make <gate>` resolves against the caller's cwd,
# so it is correct from every worktree and survives their coming and going.
#
# Each gate target's NAME MATCHES ITS TRUST ENTRY NAME exactly, so a workflow's
# `gates = ["build", "tests"]` reads the same as the Makefile and the same as
# `docket trust list`. That is why the compile gate owns the name `build` and
# the binary build is `make bin`.

.PHONY: bin test lint vet install clean demo \
        build tests self-hygiene doc-validate citation-check secret-scan \
        vuln-scan sdet-abuse tdd-preflight reserved-name-check \
        render-verify copy-verify ac-commands

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -X github.com/ALT-F4-LLC/docket/internal/cli.version=$(VERSION) -X github.com/ALT-F4-LLC/docket/internal/cli.commit=$(COMMIT) -X github.com/ALT-F4-LLC/docket/internal/cli.buildDate=$(BUILD_DATE)

QA := scripts/qa

# ---------------------------------------------------------------------------
# Developer targets
# ---------------------------------------------------------------------------

# bin — produce ./bin/docket. Formerly `build`; renamed so the compile gate can
# own that name (see the header).
bin:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o ./bin/docket ./cmd/docket

test:
	go test ./...

lint: vet
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not found, skipping"

vet:
	go vet ./...

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" ./cmd/docket

clean:
	rm -rf ./bin/

demo: bin
	@command -v vhs >/dev/null 2>&1 || { echo "vhs is required: brew install vhs"; exit 1; }
	vhs scripts/demo.tape

# ---------------------------------------------------------------------------
# Gate targets — one per `docket trust list` entry
#
# Each delegates to the script that already carries the gate's logic and its
# failure explanation; this file adds a stable, worktree-independent name and
# nothing else. A gate's behavior is changed in its script, not here.
# ---------------------------------------------------------------------------

build:
	@bash $(QA)/build.sh

tests:
	@bash $(QA)/tests.sh

self-hygiene:
	@bash $(QA)/self-hygiene.sh

doc-validate:
	@bash $(QA)/doc-validate.sh

citation-check:
	@bash $(QA)/citation-check.sh

secret-scan:
	@bash $(QA)/secret-scan.sh

vuln-scan:
	@bash $(QA)/vuln-scan.sh

sdet-abuse:
	@bash $(QA)/sdet-abuse.sh

tdd-preflight:
	@bash $(QA)/tdd-preflight.sh

reserved-name-check:
	@bash $(QA)/reserved-name-check.sh

render-verify:
	@bash $(QA)/render-verify.sh

copy-verify:
	@bash $(QA)/copy-verify.sh

ac-commands:
	@bash $(QA)/ac-commands.sh
