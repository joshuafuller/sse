.PHONY: install build lint test vuln sbom deps clean

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

sbom:
	trivy fs --format cyclonedx --output sbom.cyclonedx.json .

deps:
	go mod tidy

clean:
	go clean
