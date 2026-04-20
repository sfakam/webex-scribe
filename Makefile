VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
BINARY  := webex-scribe

.PHONY: build install clean unit-test test

build:
	go build $(LDFLAGS) -o $(BINARY) .

install: build
	sudo install -m 755 $(BINARY) /usr/local/bin/$(BINARY)

unit-test:
	go test -v -race ./...

test: unit-test

clean:
	rm -f $(BINARY)
