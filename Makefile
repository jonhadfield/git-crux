BINARY=git-crux

build:
	go build -o $(BINARY) .

install:
	go install .

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

.PHONY: build install fmt vet test
