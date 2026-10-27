# leadkit — build/install/test/doctor. See docs/ADOPTION-BRIEF.md for the install +
# adoption path this feeds into.
#
# A local `make build`/`make install` leaves main.go's -dev Version default, which is the
# honest answer for a build that came from a working tree. Release builds stamp the tag.

.PHONY: build install test cover test-install doctor clean check shapes mutate

build:
	go build -o bin/leadkit ./cmd/leadkit

# NEWCOMMANDS lists the top-level subcommands `install`'s own freshness check greps
# the installed binary's usage line for — keep in sync with cmd/leadkit/main.go's
# usage string whenever a new one is added (a missing one here just means the
# staleness check is less thorough, not that the binary itself is wrong).
NEWCOMMANDS := issue hierarchy get list tree spike decision sync

# install ALWAYS produces a current binary. 2026-09 adoption bug: an adopter's
# `make install` served a stale binary (hierarchy apply 404'd) even though the
# recipe was already a plain `go install ./cmd/leadkit`; a direct `go install
# ./cmd/leadkit` fixed it. Root cause wasn't reproducible here, so this recipe
# closes every plausible gap at once instead of guessing at just one:
#   1. `-a` forces every package (this one included) to actually recompile,
#      sidestepping any build-cache-staleness question entirely — the literal
#      "always produces a current binary" fix, at a ~5s cost per install.
#   2. It then self-verifies: prints the FRESHLY INSTALLED binary's own usage
#      line and FAILS the whole target if any of NEWCOMMANDS is missing from
#      it — so a stale/mis-tagged build is caught right here, never silently
#      shipped as a "successful" install.
#   3. It warns if `leadkit` on PATH resolves to something OTHER than what was
#      just installed — an older copy earlier on PATH shadows this install
#      exactly the way "make install ran fine but the shell still 404s" reads.
install:
	go install -a ./cmd/leadkit
	@gopath_bin="$$(go env GOPATH)/bin"; \
	bin="$$gopath_bin/leadkit"; \
	echo "installed: $$bin"; \
	usage="$$("$$bin" 2>&1 || true)"; \
	echo "$$usage"; \
	for cmd in $(NEWCOMMANDS); do \
		echo "$$usage" | grep -q "$$cmd" || { \
			echo "install: FAILED — the installed binary's usage is missing '$$cmd' (stale build — please report this)"; \
			exit 1; \
		}; \
	done; \
	which_bin="$$(command -v leadkit || true)"; \
	if [ -z "$$which_bin" ]; then \
		echo "WARNING: leadkit is not on PATH — add $$gopath_bin to PATH (see docs/ADOPTION-BRIEF.md)"; \
	elif [ "$$which_bin" != "$$bin" ]; then \
		echo "WARNING: the 'leadkit' PATH finds ($$which_bin) is NOT the binary just installed ($$bin) — an older leadkit earlier on PATH will shadow this install"; \
	else \
		echo "verified: leadkit on PATH is the binary just installed, and its usage includes every current subcommand"; \
	fi

# -count=1 always: Go caches passing test results keyed on inputs it can see, and a CLI
# test that execs a rebuilt binary or reads the environment can pass from cache against
# code that now fails.
test:
	go test ./... -count=1

# cover reports BOTH coverage paths, because neither alone is the truth:
#
#   unit        — in-process `go test` coverage.
#   integration — the CLI tests that exec the built binary. They exercise a large part
#                 of cmd/leadkit that in-process coverage cannot see, so without this
#                 half the command surface reads as untested when it is not.
#
# The two OVERLAP, so they are reported separately and deliberately not summed into a
# single flattering number. Read them as two lenses on the same tree.
COVERDIR := $(CURDIR)/.leadkit/coverdata
# Read from go.mod rather than written out, so the per-package list below cannot go stale.
MODULE = $(shell go list -m)
cover:
	@rm -rf "$(COVERDIR)" && mkdir -p "$(COVERDIR)"
	@echo "== unit (in-process) =="
	@# Output is hidden on success (it is the same suite `make test` prints) but shown on
	@# failure, so a CI log for a broken cover target is not just an exit code.
	@out="$$(go test ./... -count=1 -coverprofile=/tmp/leadkit-unit.out 2>&1)" || { echo "$$out"; exit 1; }
	@go tool cover -func=/tmp/leadkit-unit.out | tail -1
	@echo
	@echo "== integration (exec'd binary) =="
	@# Separate invocation on purpose: `go test -coverprofile` sets GOCOVERDIR for its
	@# own subprocesses, which would discard what the exec'd CLI writes.
	@out="$$(GOCOVERDIR="$(COVERDIR)" go test ./cmd/leadkit/ -count=1 2>&1)" || { echo "$$out"; exit 1; }
	@go tool covdata percent -i="$(COVERDIR)" | sed 's|$(MODULE)/||'
	@echo
	@echo "== integration total =="
	@go tool covdata func -i="$(COVERDIR)" | tail -1

test-install:
	sh scripts/test-install.sh

# The eval harness's own tests: no network, no real claude (a fake one records its calls).
# Needs the venv from evals/README.md.
EVAL_PY := $(if $(wildcard .venv/bin/python),.venv/bin/python,python3)
eval-test:
	$(EVAL_PY) -m pytest evals

# doctor builds first (so it's testing today's source, not a stale bin/leadkit) then
# runs the preflight self-check.
doctor: build
	./bin/leadkit doctor

# ---------------------------------------------------------------------------
# check — ci.yml's build-test-lint job, in its order (build, vet, gofmt, test, cover, lint, tidy,
# CONFIG-REFERENCE), plus the secret scan when gitleaks is installed.
#
# Not run: ci.yml's `installer` job (shellcheck, installer tests under dash and bash).
#
# Not run either: `evals` (pytest over evals/, needs the pinned Python harness).
#
# `ci-drift` runs first and fails on: a ci.yml job not classified below, a change to
# build-test-lint's step names or order, a golangci-lint pin that differs from LINT_VERSION, or a
# CHECK_TOOLCHAIN outside go.mod's `go` line. A step whose command changes under an unchanged name
# is not seen.
#
# Pins that move together with ci.yml: LINT_VERSION (the golangci-lint-action `version:`) and
# CHECK_TOOLCHAIN (a Go release of go.mod's `go` line; a newer local Go makes the pinned lint panic).
# ---------------------------------------------------------------------------
LINT_VERSION   := v2.11.3
CHECK_TOOLCHAIN := go1.26.1
CI_YML         := .github/workflows/ci.yml
# Every job id in ci.yml, by whether `check` runs it.
CI_JOBS_RUN     := build-test-lint secret-scan
CI_JOBS_NOT_RUN := installer evals
# build-test-lint's step names in ci.yml's order, one per line.
define CI_STEPS_BUILD_TEST_LINT
Checkout
Set up Go
go build
go vet
gofmt (fail on any unformatted file)
go test
coverage target still works (make cover)
golangci-lint
go mod tidy is clean
CONFIG-REFERENCE is regenerated
endef
export CI_STEPS_BUILD_TEST_LINT

.PHONY: ci-drift
ci-drift:
	@gomod="$$(sed -n 's/^go \([0-9][0-9.]*\) *$$/\1/p' go.mod)"; \
	case "$(CHECK_TOOLCHAIN)" in go$$gomod|go$$gomod.*) ;; *) \
		echo "check: FAILED - CHECK_TOOLCHAIN $(CHECK_TOOLCHAIN) is not a release of go.mod's go $$gomod, which is what $(CI_YML) resolves"; exit 1;; \
	esac; \
	grep -q '^ *go-version-file: go.mod *$$' $(CI_YML) || { echo "check: FAILED - $(CI_YML) no longer takes its Go version from go.mod, so CHECK_TOOLCHAIN is unchecked"; exit 1; }
	@ci_lint="$$(sed -n 's/^ *version: *\(v[0-9][0-9.]*\) *$$/\1/p' $(CI_YML))"; \
	if [ "$$ci_lint" != "$(LINT_VERSION)" ]; then \
		echo "check: FAILED - $(CI_YML) pins golangci-lint '$$ci_lint' but this Makefile pins $(LINT_VERSION); move them together"; \
		exit 1; \
	fi
	@in_ci="$$(awk '/^jobs:/{j=1;next} j&&/^[^ #]/{j=0} j&&/^  [A-Za-z0-9_-]+:[ ]*(#.*)?$$/{sub(/^  /,"");sub(/:.*/,"");print}' $(CI_YML) | sort)"; \
	known="$$(printf '%s\n' $(CI_JOBS_RUN) $(CI_JOBS_NOT_RUN) | sort)"; \
	if [ -z "$$in_ci" ]; then echo "check: FAILED - found no jobs in $(CI_YML); has its layout changed?"; exit 1; fi; \
	if [ "$$in_ci" != "$$known" ]; then \
		echo "check: FAILED - $(CI_YML) and this Makefile disagree about CI's jobs."; \
		echo "  in $(CI_YML): $$(echo $$in_ci)"; echo "  known here:  $$(echo $$known)"; \
		echo "  Classify each new job in CI_JOBS_RUN (and make check run it) or CI_JOBS_NOT_RUN (and say so in the header)."; \
		exit 1; \
	fi
	@steps="$$(awk '/^  [A-Za-z0-9_-]+:/{inj=($$0 ~ /^  build-test-lint:/);next} inj&&/^      - /{sub(/^      - /,"");sub(/^name: */,"");sub(/ +$$/,"");print}' $(CI_YML))"; \
	if [ "$$steps" != "$$CI_STEPS_BUILD_TEST_LINT" ]; then \
		echo "check: FAILED - $(CI_YML)'s build-test-lint steps differ from CI_STEPS_BUILD_TEST_LINT in this Makefile."; \
		echo "  in $(CI_YML):"; echo "$$steps" | sed 's/^/    /'; \
		echo "  known here:"; echo "$$CI_STEPS_BUILD_TEST_LINT" | sed 's/^/    /'; \
		echo "  Make check run the step, then update CI_STEPS_BUILD_TEST_LINT (or say in the header that check does not)."; \
		exit 1; \
	fi

check: export GOTOOLCHAIN := $(CHECK_TOOLCHAIN)
check: ci-drift
	@echo "check: toolchain $$(go version | awk '{print $$3}') (pinned $(CHECK_TOOLCHAIN)), golangci-lint $(LINT_VERSION) (pinned; must equal ci.yml)"
	go build ./...
	go vet ./...
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not gofmt-formatted:"; echo "$$unformatted"; echo "Run: gofmt -w ."; exit 1; \
	fi
	go test ./... -count=1
	$(MAKE) cover
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION) run
	go mod tidy -diff
	@ref=docs/CONFIG-REFERENCE.md; saved="$$(mktemp)"; cp $$ref "$$saved"; \
	go generate ./internal/config; \
	if cmp -s $$ref "$$saved"; then rm -f "$$saved"; else \
		cp "$$saved" $$ref; rm -f "$$saved"; \
		echo "check: FAILED - $$ref is stale; run: go generate ./internal/config"; exit 1; \
	fi
	@if command -v gitleaks >/dev/null 2>&1; then \
		echo "gitleaks $$(gitleaks version) (CI pins 8.30.1)"; gitleaks git --no-banner --redact; \
	else \
		echo "check: SKIPPED the secret scan - gitleaks is not installed (CI runs it over full history)"; \
	fi

# shapes lists candidates for the recurring defect shapes a script can find (history in comments,
# shouted absolutes, hardcoded counts, substring matches on identities). A report, never a failure.
# `make shapes V=-v` lists every hit instead of the counts.
shapes:
	go run ./tools/shapes $(V) .

# mutate runs the mutation harness over the guard and gate packages and FAILS on a surviving mutant
# that is not in tools/mutate/accepted.txt, and on any mutant that did not compile (INVALID) or whose
# edit did not take effect (NOT-APPLIED): those ran no test and are never counted as kills. It takes
# minutes, so `check` does not run it; `check` does run tools/mutate's own tests, which pin that
# each of those categories is reported as itself. See tools/mutate.
mutate:
	go run ./tools/mutate

clean:
	rm -rf bin
