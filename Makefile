BINARY  := talops
BUILD   := bootstrap/build/$(BINARY)
MODULE  := ./bootstrap
VERSION ?= dev
LDFLAGS := -s -w -X github.com/jdwlabs/infrastructure/bootstrap/cmd.version=$(VERSION)

.PHONY: build lint test vet clean

build:
	cd $(MODULE) && go build -ldflags "$(LDFLAGS)" -o build/$(BINARY) .

lint:
	cd $(MODULE) && golangci-lint run ./...

test:
	cd $(MODULE) && go test -race -timeout 120s ./...

vet:
	cd $(MODULE) && go vet ./...

clean:
	rf -f $(BUILD)

all: lint vet test build
