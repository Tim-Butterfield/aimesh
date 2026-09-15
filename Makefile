# aimesh developer Makefile. The release workflow (.github/workflows/release.yml) runs `dist` and
# `smoke-dist` on a `v*` tag; locally they build unstamped archives in ./dist/ (gitignored).
.PHONY: build install test race race-all vet vet-all staticcheck-all fmt fmt-check dist smoke-dist windows-build build-all test-all boundary-check golden-run golden-run-exploremesh gate

# gate is the standard check. CI runs this target and nothing else, so this file defines green.
gate: fmt-check vet-all staticcheck-all build-all test-all race-all boundary-check golden-run golden-run-exploremesh
	@echo "gate: ALL GREEN"

# The repository root is a go.work container rather than a module, so a root `go … ./...` matches
# nothing. Module-wide targets iterate every workspace module under the repository root.
REPO_ROOT := $(shell pwd)

# staticcheck is pinned and run with `go run`, so the gate needs no pre-installed binary.
STATICCHECK ?= go run honnef.co/go/tools/cmd/staticcheck@v0.8.1

build: build-all

# install installs aimesh into GOBIN (or GOPATH/bin) and removes any reviewmesh or exploremesh binary
# there, since a stale one would still run against state locations this build does not use.
install:
	go install ./cmd/aimesh
	@bin="$$(go env GOBIN)"; [ -n "$$bin" ] || bin="$$(go env GOPATH)/bin"; \
	for old in reviewmesh exploremesh; do \
	  if [ -e "$$bin/$$old" ]; then rm -f "$$bin/$$old" && echo "removed superseded binary $$bin/$$old"; fi; \
	done

build-all:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in \
			"$(REPO_ROOT)/scripts") echo "== skip build $$dir (stdlib-only tooling; compiled via 'make boundary-check') ==" ;; \
			"$(REPO_ROOT)"*) echo "== build $$dir =="; (cd "$$dir" && go build ./...) || exit 1 ;; \
		esac; \
	done

# -count=1 bypasses the test cache: a cached result is keyed on build inputs, not on the files a test
# reads, so it can report a pass that no longer holds.
test-all:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in "$(REPO_ROOT)"*) echo "== test $$dir =="; (cd "$$dir" && go test ./... -count=1) || exit 1 ;; esac; \
	done

# boundary-check enforces the meshcore import boundary and its vocabulary denylist.
boundary-check:
	@go run ./scripts/boundarycheck

# golden-run compares review output with its golden baseline. GOLDEN_UPDATE=1 re-captures it.
golden-run:
	@bash scripts/golden-run.sh

# golden-run-exploremesh compares fake-adapter explore runs (map and shortlist modes) with
# testdata/golden-run-exploremesh/. GOLDEN_UPDATE=1 re-captures it.
golden-run-exploremesh:
	@bash scripts/golden-run-exploremesh.sh

test: test-all

# race-all runs every package under the race detector.
race-all:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in "$(REPO_ROOT)"*) echo "== race $$dir =="; (cd "$$dir" && go test -race ./... -count=1) || exit 1 ;; esac; \
	done

# race is a quick local loop over the most concurrent packages; the gate runs race-all.
race:
	go test -race ./internal/review/surface/acp ./internal/review/surface/mcp ./internal/review/manager/run ./meshcore/model/shell ./internal/explore/pipeline ./internal/explore/surface/mcp

vet-all vet:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in "$(REPO_ROOT)"*) echo "== vet $$dir =="; (cd "$$dir" && go vet ./...) || exit 1 ;; esac; \
	done

staticcheck-all:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in "$(REPO_ROOT)"*) echo "== staticcheck $$dir =="; (cd "$$dir" && $(STATICCHECK) ./...) || exit 1 ;; esac; \
	done

# fmt-check tests gofmt's output, because `gofmt -l` exits 0 even when files need formatting.
fmt-check:
	@out=$$(gofmt -l cmd internal meshcore scripts); \
	if [ -n "$$out" ]; then \
		echo "gofmt drift (run 'make fmt'):"; echo "$$out"; exit 1; \
	fi; \
	echo "== gofmt: clean =="

fmt:
	gofmt -w .

# windows-build cross-compiles every module for Windows. It is not part of the gate.
windows-build:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in \
			"$(REPO_ROOT)/scripts") : ;; \
			"$(REPO_ROOT)"*) echo "== windows-build $$dir =="; (cd "$$dir" && GOOS=windows go build ./...) || exit 1 ;; \
		esac; \
	done

dist:
	bash scripts/build-dist.sh

smoke-dist:
	bash scripts/smoke-dist.sh
