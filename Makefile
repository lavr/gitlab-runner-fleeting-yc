BINARY := fleeting-plugin-yandexcloud

.PHONY: build test lint install cross clean

build:
	go build -o $(BINARY) .

test:
	go test ./... -v -timeout 60s

lint:
	golangci-lint run ./...

install:
	go install .

cross:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 go build -o dist/$(BINARY)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o dist/$(BINARY)-linux-arm64 .

clean:
	rm -f $(BINARY)
	rm -rf dist/
