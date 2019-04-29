.PHONY: build test clean

GO = CGO_ENABLED=0 GO111MODULE=on go

MICROSERVICES=cmd/device-thingy52

.PHONY: $(MICROSERVICES)

VERSION=$(shell cat ./VERSION)
GIT_SHA=$(shell git rev-parse HEAD)
GOFLAGS=-ldflags "-X github.com/anonymouse64/device-thingy52.Version=$(VERSION)"

build: $(MICROSERVICES)
	$(GO) build ./...

cmd/device-thingy52:
	$(GO) build $(GOFLAGS) -o $@ ./cmd

test:

clean:
	rm -f $(MICROSERVICES)
