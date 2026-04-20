VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"
BINARY  := webex-scribe

.PHONY: build install clean

build:
	go build $(LDFLAGS) -o $(BINARY) .

install: build
	sudo install -m 755 $(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -f $(BINARY)
