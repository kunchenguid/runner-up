.PHONY: build test test-real-tart fmt lint dist install clean

VERSION ?= dev
LDFLAGS := -X github.com/kunchenguid/runner-up/internal/version.Version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o runner-up .

test:
	go test ./...

# The tests against real Tart, which CI skips: run on a Mac with Apple silicon
# and Tart before a release (CONTRIBUTING.md, "Before a release").
test-real-tart:
	RUNNER_UP_REAL_TART=1 go test -count=1 -v -run '^TestRealTart' ./cmd/

fmt:
	gofmt -w .

lint:
	gofmt -l .
	go vet ./...

dist:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-darwin-arm64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-darwin-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-linux-arm64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-linux-amd64 .
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-windows-arm64.exe .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-windows-amd64.exe .

install: build
	cp runner-up $(GOPATH)/bin/ 2>/dev/null || cp runner-up /usr/local/bin/

clean:
	rm -rf runner-up runner-up.exe dist/ coverage.out
