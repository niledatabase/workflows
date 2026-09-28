# The release workflow publishes a release only if the binaries differ from the pinned release, so the build must be reproducible.
BUILD_FLAGS := -trimpath -buildvcs=false
LDFLAGS := -s -w -buildid=
GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := bin/golangci-lint-$(GOLANGCI_LINT_VERSION)

.PHONY: build test integration lint format

build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -ldflags '$(LDFLAGS)' -o bin/buildkit-cache-mounts-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -ldflags '$(LDFLAGS)' -o bin/buildkit-cache-mounts-linux-arm64 .

test:
	go test -race ./...

integration: build
	BUILDKIT_CACHE_MOUNTS_EXECUTABLE="$(CURDIR)/bin/buildkit-cache-mounts-linux-$$(go env GOARCH)" go test -count=1 -tags integration ./...

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

format: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt

$(GOLANGCI_LINT):
	GOBIN="$(CURDIR)/bin" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv bin/golangci-lint $@
