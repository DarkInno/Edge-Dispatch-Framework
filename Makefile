.PHONY: build run clean test docker-up docker-down docker-build

build:
	go build -o bin/control-plane ./cmd/control-plane
	go build -o bin/edge-agent ./cmd/edge-agent
	go build -o bin/origin ./cmd/origin

run-cp: build
	./bin/control-plane

run-ea: build
	./bin/edge-agent

run-origin: build
	./bin/origin

clean:
	rm -rf bin/

test:
	go test ./...

docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down -v

lint:
	go vet ./...
