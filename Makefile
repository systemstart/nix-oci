GO                 ?= go
GOLANGCI_LINT      ?= golangci-lint
COVERPROFILE       ?= coverage.out
COVERAGE_THRESHOLD ?= 80

# The example images are flake checks: .#checks.<system>.<name>. The '#' must be
# escaped or Make treats the rest of the line as a comment.
NIX_SYSTEM         ?= $(shell nix eval --raw --impure --expr builtins.currentSystem)
EXAMPLE_IMAGE      ?= .\#checks.$(NIX_SYSTEM).exampleImage

.PHONY: all build test cover fmt lint hooks repro release release-tag clean

all: build

# Points git at the version-controlled hooks in .githooks (pre-commit runs the
# linter). Per-clone, so each contributor runs it once.
hooks:
	git config core.hooksPath .githooks
	@echo "installed git hooks from .githooks"

build:
	$(GO) build ./...

# Runs tests with coverage and fails if total coverage drops below
# COVERAGE_THRESHOLD. -race is intentionally omitted: the writer builds with
# CGO_ENABLED=0 (nix/go-bin.nix) and the race detector needs cgo.
test:
	$(GO) test -coverprofile=$(COVERPROFILE) ./...
	@total=$$($(GO) tool cover -func=$(COVERPROFILE) | awk '/^total:/ {gsub(/%/,"",$$NF); print $$NF}'); \
	printf "Total coverage: %s%% (threshold: %d%%)\n" "$$total" "$(COVERAGE_THRESHOLD)"; \
	if awk "BEGIN{exit !($$total < $(COVERAGE_THRESHOLD))}"; then \
		printf "FAIL: coverage %s%% is below %d%%\n" "$$total" "$(COVERAGE_THRESHOLD)"; \
		exit 1; \
	fi

# Opens the per-line coverage report from the last `make test` in a browser.
cover: test
	$(GO) tool cover -html=$(COVERPROFILE)

# Applies the formatters declared in .golangci.yaml (gofumpt).
fmt:
	$(GOLANGCI_LINT) fmt

lint:
	$(GOLANGCI_LINT) run

# Single-machine reproducibility check: build the example image, then rebuild it
# and let Nix verify the output is bit-for-bit identical (--rebuild compares
# against the first result, so it must exist first). CI additionally compares
# digests across two machines (see the reproducibility job).
repro:
	nix build $(EXAMPLE_IMAGE) --no-link
	nix build --rebuild $(EXAMPLE_IMAGE) --no-link

# Builds and publishes a release from the current tag. Invoked by CI on tag
# push; needs GITHUB_TOKEN and a clean tagged tree. Re-runs the gates first so a
# local `make release` matches what CI enforces.
release: lint test build
	goreleaser release --clean

# Computes the next version from conventional-commit history, tags it, and
# pushes the tag -- which is what triggers the release job. Override with
# `make release-tag VERSION=1.2.3`.
release-tag:
	$(eval VERSION ?= $(shell gsemver bump))
	git tag -a "v$(VERSION)" -m "Release v$(VERSION)"
	git push origin "v$(VERSION)"

clean:
	rm -rf result $(COVERPROFILE) dist