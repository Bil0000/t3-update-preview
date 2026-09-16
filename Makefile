.PHONY: build test check

build:
	go build -trimpath -o dist/t3-update-preview ./cmd/t3-update-preview

test:
	go test -race ./...
	python3 -m unittest discover -s internal/host -p 'test_*.py'

check: test build
	go vet ./...
	test -z "$$(gofmt -l cmd internal)"
