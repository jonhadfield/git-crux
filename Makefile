BINARY=git-crux

build:
	go build -o $(BINARY) .

install:
	go install .

fmt:
	go fmt ./...

vet:
	go vet ./...

.PHONY: build install fmt vet
