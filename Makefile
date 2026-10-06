.PHONY: build test check dist install

build:
	go build -o bin/mesh ./cmd/mesh

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...

dist:
	sh scripts/build.sh

install:
	sh scripts/install.sh
