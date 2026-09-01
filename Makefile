GO                 ?= go
GOLANGCI_LINT      ?= golangci-lint
COVERPROFILE       ?= coverage.out
COVERAGE_THRESHOLD ?= 80

# The example images are flake checks: .#checks.<system>.<name>. The '#' must be
# escaped or Make treats the rest of the line as a comment.
NIX_SYSTEM         ?= $(shell nix eval --raw --impure --expr builtins.currentSystem)
EXAMPLE_IMAGE      ?= .\#checks.$(NIX_SYSTEM).exampleImage

.PHONY: all build test cover fmt lint hooks repro diff-closures release release-notes release-tag release-tag-preview clean

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
# CGO_ENABLED=0 and the race detector needs cgo.
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

# What did the last `nix flake update` actually change? Rebuilds an attribute
# against the nixpkgs revision committed at HEAD and diffs the two closures --
# the lock diff alone only says a revision moved, not whether the move reaches
# anything we build. Evaluation-only unless the derivations differ.
#
# The Go toolchain is no longer one of nixpkgs' packages, so there is nothing
# useful to pass to -p here: roll back the toolchain input instead, with
# ARGS='-i go-overlay'. Other examples: ARGS='-a checks.x86_64-linux.exampleImage'.
diff-closures:
	@scripts/diff-closures.sh $(ARGS)

# Builds and publishes a release from the current tag. Invoked by CI on tag
# push; needs GITHUB_TOKEN and a clean tagged tree. Re-runs the gates first so a
# local `make release` matches what CI enforces.
# Release notes come from git-cliff (see cliff.toml), not from goreleaser:
# goreleaser's changelog only ever sees commit subjects, so a BREAKING CHANGE
# footer could not reach the release page. --release-notes replaces the body
# entirely, which is why cliff.toml carries the header and footer too.
release: lint test build
	@notes="$$(mktemp)"; \
	git-cliff --latest > "$$notes" || { rm -f "$$notes"; exit 1; }; \
	goreleaser release --clean --release-notes "$$notes"; \
	rc=$$?; rm -f "$$notes"; exit $$rc

# Preview the release notes for the current tag without releasing anything.
release-notes:
	@git-cliff --latest

# Preview the version `release-tag` would cut, without touching anything.
# gsemver derives it from the conventional-commit history since the last tag,
# and it fetches, so this needs network access to the remote.
release-tag-preview:
	@v="$$(gsemver bump)"; \
	test -n "$$v" || { echo "gsemver produced no version — is the remote reachable?" >&2; exit 1; }; \
	printf "next tag: v%s\n" "$$v"

# Computes the next version from conventional-commit history, writes it into
# ./VERSION (the source of truth the flake reads), commits that as the release
# commit, tags it, and pushes branch + tag -- which triggers the release job.
# Because the version is committed *before* the tag, the tagged tree's Nix build,
# the goreleaser artifact, and the tag all report the same version. The chore
# commit is a non-bumping type, so it does not skew the next gsemver bump.
# Override the version with `make release-tag VERSION=1.2.3`.
#
# The tag is signed with -s. tag.forcesignannotated does NOT achieve this: git
# gives an explicit --annotate/-a precedence over that config, so `git tag -a`
# produces an unsigned tag however the config is set.
release-tag:
	$(eval VERSION ?= $(shell gsemver bump))
	@test -n "$(VERSION)" || { \
		echo "gsemver produced no version — refusing to tag." >&2; \
		echo "make hides a failing command inside its shell function, and an empty" >&2; \
		echo "version would tag and push a ref literally named v. Check the remote" >&2; \
		echo "is reachable and gsemver is on PATH." >&2; \
		exit 1; \
	}
	@git config --get user.signingkey >/dev/null || { \
		echo "user.signingkey is unset — the signed tag would fail after the" >&2; \
		echo "release commit was already made, leaving a half-cut release." >&2; \
		exit 1; \
	}
	@printf '%s\n' "$(VERSION)" > VERSION
	git add VERSION
	git commit -m "chore: release v$(VERSION)" VERSION
	git tag -s "v$(VERSION)" -m "Release v$(VERSION)"
	git push origin HEAD "v$(VERSION)"

clean:
	rm -rf result $(COVERPROFILE) dist