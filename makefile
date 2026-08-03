# Server Agent build and release.
#
# Release artifacts are what other people download and run on their own machines, so the
# release target is explicit and never a side effect of anything else.

VERSION ?= 0.1.0
DIST    ?= bin/dist
TARGETS ?= linux/amd64 linux/arm64

LDFLAGS = -s -w -X 'main.AgentVersion=$(VERSION)'

.PHONY: build release metrics test lint clean

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

## clean: remove build output
clean:
	rm -rf bin
