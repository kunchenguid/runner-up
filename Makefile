.PHONY: build test fmt lint dist install clean

VERSION ?= dev
LDFLAGS := -X github.com/kunchenguid/runner-up/internal/version.Version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o runner-up .

test:
	go test ./...

fmt:
	gofmt -w .

lint:
	gofmt -l .
	go vet ./...

dist:
	@mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-darwin-arm64 .
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-darwin-amd64 .
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-linux-arm64 .
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-linux-amd64 .
	GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-windows-arm64.exe .
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/runner-up-windows-amd64.exe .

install: build
	cp runner-up $(GOPATH)/bin/ 2>/dev/null || cp runner-up /usr/local/bin/

clean:
	rm -rf runner-up runner-up.exe dist/ coverage.out
