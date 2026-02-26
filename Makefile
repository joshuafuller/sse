.PHONY: install build lint test vuln deps clean

install:
	go install -v

build:
	go build -v ./...

lint:
	golangci-lint run ./...

test:
	go test -v -race -count=1 ./...

vuln:
	govulncheck ./...

deps:
	go mod tidy

clean:
	go clean
