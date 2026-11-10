APP := fujipic
BIN := bin/$(APP)

.PHONY: all build run test race vet fmt tidy backup docker clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/fujipic

run:
	go run ./cmd/fujipic serve

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal migrations web

tidy:
	go mod tidy

backup:
	go run ./cmd/fujipic backup --out ./backup/fujipic-$$(date +%F).db

docker:
	docker build -f deploy/Dockerfile -t $(APP):local .

clean:
	rm -rf bin backup
