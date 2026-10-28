# Capy: run from the repository root. Every target needs only Go (macOS for the app).
.PHONY: run run-fresh test lint e2e e2e-live bundle icons fixtures

# The identity that signs the bundle, from the gitignored .signing-identity,
# e.g. "Apple Development: Jane Doe (TEAMID)". Without it, mygo.json's
# macos.signingIdentity applies, else ad hoc.
SIGNING_IDENTITY := $(shell cat .signing-identity 2>/dev/null)

# mygo names the disk image "Capy <version> arm64.dmg"; bundle renames it without
# the spaces so it is one click to open from a terminal.
VERSION := $(shell sed -n 's/^  "version": "\(.*\)",$$/\1/p' mygo.json)

run: ## the menu bar app, unbundled
	CGO_ENABLED=0 go run .

# run-fresh's own data, emptied at each start: the user's library and the
# Capy they may have running are left alone.
FRESH := /tmp/capy-fresh

run-fresh: ## the app on an empty library of its own, as at a first launch; ZOOM_API_KEY=… for a real key
	rm -rf $(FRESH) && mkdir -p $(FRESH)/home $(FRESH)/run
	CGO_ENABLED=0 go build -o $(FRESH)/go-build/capy .
	HOME=$(FRESH)/home XDG_RUNTIME_DIR=$(FRESH)/run ZOOM_API_KEY="$${ZOOM_API_KEY:-fresh-test-key}" $(FRESH)/go-build/capy

test: ## unit and integration tests
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)" || (gofmt -l . && false)
	go vet ./... && go vet -tags e2e ./e2e/ && GOOS=linux go vet ./...

e2e: ## replay the fixtures through the binary against the fake Zoom API; reports in e2e/out/
	go test -tags e2e -count=1 -run Fake ./e2e/

e2e-live: ## the same against the Zoom API; needs ZOOM_API_KEY
	CAPY_E2E_LIVE=1 go test -tags e2e -count=1 -run Live -timeout 5m -v ./e2e/

bundle: ## build/darwin-arm64/Capy.app and Capy-<version>.dmg
	go tool mygo build $(if $(SIGNING_IDENTITY),-sign "$(SIGNING_IDENTITY)")
	mv "build/darwin-arm64/Capy $(VERSION) arm64.dmg" build/darwin-arm64/Capy-$(VERSION).dmg

icons: ## redraw the menu bar and app icons
	go run ./scripts/icons

fixtures: ## re-speak the E2E audio with macOS voices
	go run ./scripts/fixtures
