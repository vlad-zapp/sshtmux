BINARY = sshtmux
GO_IMAGE = golang:1.24
DOCKER_RUN = docker run --rm -v $(CURDIR):/app -v sshtmux-gomod:/go/pkg/mod -w /app $(GO_IMAGE)

.PHONY: build test test-race lint clean install

build:
	$(DOCKER_RUN) go build -buildvcs=false -ldflags "-s -w" -o $(BINARY) ./cmd/sshtmux/

test:
	$(DOCKER_RUN) go test -timeout 30s ./...

test-race:
	$(DOCKER_RUN) go test -race -timeout 60s ./...

lint:
	$(DOCKER_RUN) go vet ./...

clean:
	rm -f $(BINARY)

install: build
	install -m 755 $(BINARY) $(HOME)/.local/bin/$(BINARY)

tidy:
	$(DOCKER_RUN) go mod tidy
