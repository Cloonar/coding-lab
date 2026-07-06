GO  ?= go
NPM ?= npm

.PHONY: all lab labctl build-ui test lint fmt clean

all: lab labctl

# Build the SPA and copy it where go:embed can reach it
# (internal/webui/dist — go:embed cannot reach outside the package dir).
build-ui:
	cd web && $(NPM) ci --no-audit --no-fund && $(NPM) run build
	rm -rf internal/webui/dist
	mkdir -p internal/webui
	cp -r web/dist internal/webui/dist

# Server binary with the embedded UI (build tag `ui`).
lab: build-ui
	CGO_ENABLED=0 $(GO) build -tags ui -o bin/lab ./cmd/lab

# Agent-side CLI.
labctl:
	CGO_ENABLED=0 $(GO) build -o bin/labctl ./cmd/labctl

test:
	$(GO) test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

clean:
	rm -rf bin internal/webui/dist web/dist
