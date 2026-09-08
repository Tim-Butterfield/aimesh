# aimesh — developer Makefile.
# `dist`/`smoke-dist` are what the release workflow (.github/workflows/release.yml) runs on a
# `v*` tag; locally they build unstamped dev-local archives. Artifacts land in ./dist/ (gitignored).
.PHONY: build install test race race-all vet vet-all fmt fmt-check dist smoke-dist windows-build build-all test-all boundary-check golden-run golden-run-exploremesh gate

# The aimesh "Standard gate": format + vet + build + test + race + boundary-check + a golden
# equivalence run for EACH shipped binary (reviewmesh + exploremesh).
#
# CI (.github/workflows/ci.yml) invokes THIS target and nothing else, so the cheap static checks
# live here rather than in the workflow: this file is the single definition of green, and a
# workflow that restated the steps would drift from it and the local one would lose.
gate: fmt-check vet-all build-all test-all race-all boundary-check golden-run golden-run-exploremesh
	@echo "gate: ALL GREEN"

# build/test/vet cover EVERY workspace module (a root `go … ./...` does NOT — the repo
# root is a go.work container, not a module, so ./... matches nothing there).
build: build-all

# install the single aimesh binary into GOBIN (or GOPATH/bin), and REMOVE any reviewmesh /
# exploremesh left behind by an older install. Leaving them on PATH is not harmless: a stale binary
# still answers, reads state from `.reviewmesh`/`.exploremesh` locations this build no longer writes,
# and reports success — so the user would be running a tool that silently disagrees with their config.
# The removals are named as they happen rather than done quietly.
install:
	go install ./cmd/aimesh
	@bin="$$(go env GOBIN)"; [ -n "$$bin" ] || bin="$$(go env GOPATH)/bin"; \
	for old in reviewmesh exploremesh; do \
	  if [ -e "$$bin/$$old" ]; then rm -f "$$bin/$$old" && echo "removed superseded binary $$bin/$$old"; fi; \
	done

# --- workspace-aware build/test (aimesh refactor) ---
# Root `go build ./...` does NOT cover nested go.work modules (meshcore/, exploremesh/),
# so build-all/test-all iterate every WORKSPACE-LOCAL module (dirs under the repo root;
# external module-cache deps are filtered out by the repo-root prefix match).
REPO_ROOT := $(shell pwd)

build-all:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in \
			"$(REPO_ROOT)/scripts") echo "== skip build $$dir (stdlib-only tooling; compiled via 'make boundary-check') ==" ;; \
			"$(REPO_ROOT)"*) echo "== build $$dir =="; (cd "$$dir" && go build ./...) || exit 1 ;; \
		esac; \
	done

# -count=1 DISABLES THE TEST CACHE, and that is the point rather than a preference. Go caches a
# test result against the build inputs, not against the state a test happens to read from the
# filesystem — so a suite that passed once can keep reporting `ok (cached)` long after it stopped
# passing. That is not hypothetical here: it hid twenty exploremesh CLI failures behind a green
# gate, and only surfaced because `-race` uses a different cache key and actually re-ran them.
# A gate that trusts the cache is a gate that answers a question nobody asked.
test-all:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in "$(REPO_ROOT)"*) echo "== test $$dir =="; (cd "$$dir" && go test ./... -count=1) || exit 1 ;; esac; \
	done

# Import-boundary + denylist gate (aimesh refactor P0.3). Fails on reverse/app-crossing
# imports, HTTP in meshcore, or denylisted app vocabulary in meshcore exported identifiers.
boundary-check:
	@go run ./scripts/boundarycheck

# reviewmesh "behavior unchanged" golden equivalence gate (aimesh P0.4).
# GOLDEN_UPDATE=1 make golden-run  — (re)capture the baseline.
golden-run:
	@bash scripts/golden-run.sh

# exploremesh "behavior unchanged" golden equivalence gate (aimesh C20): a fake-only profile run
# through `map` (the formulation-free default) and `shortlist` (the count/ballot-bearing governance
# path), diffed against testdata/golden-run-exploremesh/.
# GOLDEN_UPDATE=1 make golden-run-exploremesh  — (re)capture the baseline.
golden-run-exploremesh:
	@bash scripts/golden-run-exploremesh.sh

test: test-all

# race-all runs EVERY package under the race detector, not a curated list. A hand-picked set has to
# be maintained by whoever next adds a goroutine, and nothing makes them do it — the omission is
# silent and indistinguishable from a clean result. Measured at ~40s for the whole repo, which is
# not worth a maintenance burden to avoid. `make race` stays as the quick single-package loop.
race-all:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in "$(REPO_ROOT)"*) echo "== race $$dir =="; (cd "$$dir" && go test -race ./... -count=1) || exit 1 ;; esac; \
	done

# race: the fast loop for the packages that most exercise concurrency (transport goroutines,
# subprocess stdio pumps, the run registry). NOT the gate's check — see race-all.
race:
	go test -race ./internal/review/surface/acp ./internal/review/surface/mcp ./internal/review/manager/run ./meshcore/model/shell ./internal/explore/pipeline ./internal/explore/surface/mcp

vet-all vet:
	@for dir in $$(go list -m -f '{{.Dir}}' 2>/dev/null); do \
		case "$$dir" in "$(REPO_ROOT)"*) echo "== vet $$dir =="; (cd "$$dir" && go vet ./...) || exit 1 ;; esac; \
	done

# fmt-check FAILS on drift. `gofmt -l` prints the offending files and EXITS 0 — dropped into a gate
# unwrapped it is decorative and passes forever, which is the usual way this check gets added wrong.
# The output must be tested, not the exit status.
fmt-check:
	@out=$$(gofmt -l cmd internal meshcore scripts); \
	if [ -n "$$out" ]; then \
		echo "gofmt drift (run 'make fmt'):"; echo "$$out"; exit 1; \
	fi; \
	echo "== gofmt: clean =="

fmt:
	gofmt -w .

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
