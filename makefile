# Server Agent build and release.
#
# Release artifacts are what other people download and run on their own machines, so the
# release target is explicit and never a side effect of anything else.

VERSION ?= 0.1.0
DIST    ?= bin/dist
TARGETS ?= linux/amd64 linux/arm64

LDFLAGS = -s -w -X 'main.AgentVersion=$(VERSION)'

.PHONY: build release metrics test lint clean check-tracked verify-release

## build: compile for the current platform, for local testing
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/serveragent ./cmd/serveragent
	@echo "✅ bin/serveragent ($(VERSION))"

## release: cross-compile release artifacts with checksums
#
# CGO_ENABLED=0 is required, not merely preferred: these binaries run on customer hosts of
# unknown vintage, and a cgo build would bind to the glibc version of whatever machine
# produced it. -s -w strips debug info to keep the download small.
release:
	@set -e; \
	rm -rf $(DIST); mkdir -p $(DIST); \
	for target in $(TARGETS); do \
		os=$$(echo "$$target" | cut -d/ -f1); \
		arch=$$(echo "$$target" | cut -d/ -f2); \
		out="$(DIST)/serveragent-$(VERSION)-$$os-$$arch"; \
		echo "🔨 building $$os/$$arch..."; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o "$$out" ./cmd/serveragent; \
	done; \
	cp install.sh $(DIST)/install.sh; \
	cd $(DIST) && shasum -a 256 serveragent-* > SHA256SUMS; \
	echo ""; \
	echo "✅ Server Agent $(VERSION) artifacts in $(DIST):"; \
	ls -lh serveragent-* install.sh; \
	echo ""; \
	cat SHA256SUMS; \
	echo ""; \
	echo "Next:"; \
	echo "  1. publish $(DIST)/* to <endpoint>/dist/serveragent/"; \
	echo "  2. bump the server's advertised agent version to $(VERSION)"

## metrics: print exactly what the agent collects
metrics:
	@go run ./cmd/serveragent -metrics

## test: run the full suite
test:
	go test -race ./...

## lint: vet and formatting check
lint:
	go vet ./...
	@test -z "$$(gofmt -l . )" || (echo "unformatted files:"; gofmt -l .; exit 1)

## check-tracked: fail if anything needed to build is missing from git
#
# v0.1.0 shipped without cmd/serveragent/ because an unanchored "serveragent" line in
# .gitignore matched the source directory as well as the built binary. The module
# published, resolved, and imported fine — it just had no agent in it. Nothing in a normal
# build catches that, because the working tree has the files.
check-tracked:
	@set -e; \
	missing=0; \
	for f in $$(git ls-files --others --exclude-standard --directory) ; do :; done; \
	for required in cmd/serveragent/main.go cmd/serveragent/collect.go \
	                cmd/serveragent/client.go cmd/serveragent/config.go \
	                cmd/serveragent/spool.go protocol/protocol.go \
	                install.sh LICENSE README.md go.mod go.sum; do \
		if ! git ls-files --error-unmatch "$$required" >/dev/null 2>&1; then \
			echo "❌ not tracked by git: $$required"; \
			missing=1; \
		fi; \
	done; \
	if git ls-files | grep -q '^\.idea/'; then \
		echo "❌ .idea/ is tracked; it should not ship in a public module"; \
		missing=1; \
	fi; \
	if [ "$$missing" = "1" ]; then \
		echo ""; \
		echo "Refusing to tag: the published module would be missing files."; \
		exit 1; \
	fi; \
	echo "✅ every file needed to build is tracked"

## verify-release: check that a published version actually contains the agent
#
# Run after pushing a tag. Downloads the module as a consumer would and asserts the binary
# package is present, rather than trusting that the working tree matched what was pushed.
verify-release:
	@set -e; \
	v="$(VERSION)"; \
	case "$$v" in v*) ;; *) v="v$$v";; esac; \
	url="https://proxy.golang.org/github.com/hosted-status-page/hsp-server-agent/@v/$$v.zip"; \
	tmp=$$(mktemp -d); \
	echo "⬇️  fetching $$v from the module proxy"; \
	curl -fsSL "$$url" -o "$$tmp/m.zip" || { echo "❌ $$v is not on the proxy yet"; rm -rf "$$tmp"; exit 1; }; \
	if unzip -Z1 "$$tmp/m.zip" | grep -q 'cmd/serveragent/main.go'; then \
		echo "✅ $$v contains cmd/serveragent"; \
	else \
		echo "❌ $$v does NOT contain cmd/serveragent — consumers cannot build the agent"; \
		unzip -Z1 "$$tmp/m.zip" | sed 's|.*@[^/]*/||' | sort | sed 's/^/    /'; \
		rm -rf "$$tmp"; exit 1; \
	fi; \
	if unzip -Z1 "$$tmp/m.zip" | grep -q '\.idea/'; then \
		echo "⚠️  $$v also ships .idea/ IDE config"; \
	fi; \
	rm -rf "$$tmp"

## clean: remove build output
clean:
	rm -rf bin
