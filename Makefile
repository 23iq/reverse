VERSION ?= dev
PREFIX ?= /usr/local

MODULE := github.com/23iq/reverse
LDFLAGS := -s -w -X $(MODULE)/internal/buildinfo.Version=$(VERSION)

.PHONY: build install test test-race vet clean

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/reverse ./cmd/reverse

install: build
	install -Dm0755 bin/reverse "$(DESTDIR)$(PREFIX)/bin/reverse"

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -f bin/reverse
