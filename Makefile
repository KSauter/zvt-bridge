VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
GO      := CGO_ENABLED=0 go

.PHONY: test build build-windows e2e snapshot check clean

test:
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./...
	go test -count=1 ./...

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o dist/zvt-bridge ./cmd/zvt-bridge
	$(GO) build -ldflags "-s -w" -o dist/fakept  ./cmd/fakept
	$(GO) build -ldflags "-s -w" -o dist/fakeecr ./cmd/fakeecr

build-windows:
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/zvt-bridge.exe ./cmd/zvt-bridge
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "-s -w" -o dist/fakept.exe  ./cmd/fakept
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "-s -w" -o dist/fakeecr.exe ./cmd/fakeecr

e2e: build
	./test/e2e-linux.sh

check:
	goreleaser check

snapshot:
	goreleaser release --snapshot --clean --skip=publish

clean:
	rm -rf dist
