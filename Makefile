.PHONY: build test lint run clean

VERSION := $(shell cat VERSION)
MODULE  := github.com/sethgrid/syl

build:
	go build -ldflags="-X 'main.Version=$(VERSION)'" -o bin/syl ./cmd/syl

test:
	go test ./...

lint:
	gofmt -l -w .
	go vet ./...

run:
	go run ./cmd/syl

clean:
	rm -rf bin/
