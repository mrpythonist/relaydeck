BINARY := relaydeck
GO := go

.PHONY: build test test-race vet fmt run clean

build:
	$(GO) build -o $(BINARY) ./cmd/relaydeck

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l cmd internal tests web

run:
	$(GO) run ./cmd/relaydeck serve

clean:
	rm -f $(BINARY)
