VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
GO      := CGO_ENABLED=0 go

.PHONY: test build build-windows winres e2e e2e-usbip snapshot check clean

# Numeric part of VERSION for the Windows version resource (dev -> 0.0.0).
WINRES_VERSION := $(or $(shell echo "$(VERSION)" | grep -Eo '^[0-9]+\.[0-9]+\.[0-9]+'),0.0.0)

test:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	go test -count=1 ./...

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o dist/zvt-bridge ./cmd/zvt-bridge
	$(GO) build -ldflags "-s -w" -o dist/fakept  ./cmd/fakept
	$(GO) build -ldflags "-s -w" -o dist/fakeecr ./cmd/fakeecr

# Icon, version information and manifest of zvt-bridge.exe (rsrc_windows_*.syso).
winres:
	go tool go-winres make --in winres/winres.json --out cmd/zvt-bridge/rsrc --arch amd64,arm64 \
		--product-version $(WINRES_VERSION) --file-version $(WINRES_VERSION)

build-windows: winres
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/zvt-bridge.exe ./cmd/zvt-bridge
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "-s -w" -o dist/fakept.exe  ./cmd/fakept
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "-s -w" -o dist/fakeecr.exe ./cmd/fakeecr

e2e: build
	./test/e2e-linux.sh

e2e-usbip: build
	./test/e2e-usbip-linux.sh

check:
	goreleaser check

snapshot:
	goreleaser release --snapshot --clean --skip=publish

clean:
	rm -rf dist
