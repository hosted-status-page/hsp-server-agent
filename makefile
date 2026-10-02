# Server Agent build and release.
#
# Release artifacts are what other people download and run on their own machines, so the
# release target is explicit and never a side effect of anything else.
#
# The version of a release is its git tag (vMAJOR.MINOR.PATCH) and nothing else. The
# StatusPage.me server pins that tag in its go.mod and builds, stamps and publishes the
# binaries and the installer itself, so there is no second version to keep in step here:
# VERSION is derived from the tag instead of being written down, and the "0.1.0" left in
# cmd/serveragent/main.go and install.sh is only a placeholder that every build overrides.
# An untagged or modified tree yields something like 0.1.4-3-gabc1234-dirty, which is
# deliberately not a release version: --update and the version check treat it as unparseable.

VERSION ?= $(or $(shell git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null | sed 's/^v//'),dev)
DIST    ?= bin/dist
TARGETS ?= linux/amd64 linux/arm64

LDFLAGS = -s -w -X 'main.AgentVersion=$(VERSION)'

.PHONY: build release metrics test lint clean check-tracked verify-release

## build: compile for the current platform, for local testing
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/serveragent ./cmd/serveragent
	@echo "✅ bin/serveragent ($(VERSION))"

## release: build and checksum the tagged release locally, as a rehearsal
#
# Refuses to run unless HEAD is exactly a clean vMAJOR.MINOR.PATCH tag and VERSION is that
# tag. The binaries written here are for checking the tag builds, not for distribution:
# builds are not reproducible across machines, so what customers download is built once by
# the StatusPage.me repository's `make serveragent-publish` and never replaced afterwards.
#
# CGO_ENABLED=0 is required, not merely preferred: these binaries run on customer hosts of
# unknown vintage, and a cgo build would bind to the glibc version of whatever machine
# produced it. -s -w strips debug info to keep the download small.
release:
	@set -e; \
	tag=$$(git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null) \
		|| { echo "❌ HEAD is not tagged vMAJOR.MINOR.PATCH; releases are built from a tag (git tag -s vX.Y.Z)"; exit 1; }; \
	[ -z "$$(git status --porcelain)" ] || { echo "❌ the working tree has uncommitted changes"; exit 1; }; \
	[ "$(VERSION)" = "$${tag#v}" ] \
		|| { echo "❌ VERSION=$(VERSION) does not match the tag $$tag"; exit 1; }; \
	echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' \
		|| { echo "❌ $$tag is not a plain MAJOR.MINOR.PATCH release tag"; exit 1; }; \
	rm -rf $(DIST); mkdir -p $(DIST); \
	for target in $(TARGETS); do \
		os=$$(echo "$$target" | cut -d/ -f1); \
		arch=$$(echo "$$target" | cut -d/ -f2); \
		out="$(DIST)/serveragent-$(VERSION)-$$os-$$arch"; \
		echo "🔨 building $$os/$$arch..."; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o "$$out" ./cmd/serveragent; \
	done; \
	cd $(DIST) && shasum -a 256 serveragent-* > SHA256SUMS; \
	echo ""; \
	echo "✅ Server Agent $(VERSION) built from $$tag into $(DIST) (rehearsal; not for distribution):"; \
	ls -lh serveragent-*; \
	echo ""; \
	cat SHA256SUMS; \
	echo ""; \
	echo "Next, to publish $$tag:"; \
	echo "  1. git push origin $$tag"; \
	echo "  2. make verify-release VERSION=$(VERSION)      (the module proxy serves it, agent included)"; \
	echo "  3. in the StatusPage.me repository:"; \
	echo "       go get github.com/hosted-status-page/hsp-server-agent@$$tag"; \
	echo "       make serveragent-publish      (builds, stamps and stages binaries, SHA256SUMS, installer)"; \
	echo "       make serveragent-verify"; \
	echo "       make deploy                   (only now does the server advertise $(VERSION))"

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
	                cmd/serveragent/spool.go cmd/serveragent/update.go \
	                cmd/serveragent/preflight.go protocol/protocol.go \
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
