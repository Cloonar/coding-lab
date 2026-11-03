GO  ?= go
NPM ?= npm

.PHONY: all lab labctl build-ui test lint fmt clean seccomp-refresh

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

# Re-download the upstream seccomp profile the nested-containers profile is
# generated from (internal/podmanx/seccomp.go, ADR-0086). containers/common
# lives in the container-libs monorepo now; SECCOMP_REF pins its tag — bump
# it together with the pin named in seccomp.go's comment. The download goes
# to a temp file first so a failed fetch never leaves a truncated profile,
# and the podmanx tests then check that the delta still finds its rules.
SECCOMP_REF ?= common/v0.70.0
SECCOMP_URL  = https://raw.githubusercontent.com/podman-container-tools/container-libs/$(SECCOMP_REF)/common/pkg/seccomp/seccomp.json
SECCOMP_DST  = internal/podmanx/seccomp/upstream.json

seccomp-refresh:
	curl -fsSL -o $(SECCOMP_DST).tmp $(SECCOMP_URL)
	mv $(SECCOMP_DST).tmp $(SECCOMP_DST)
	$(GO) test ./internal/podmanx/ -run 'Seccomp'

clean:
	rm -rf bin internal/webui/dist web/dist
