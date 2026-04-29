.PHONY: build run clean test docker-up docker-down docker-build docker-cp-up docker-cp-down docker-cp-up-full docker-edge-up docker-edge-down docker-edge-up-full

build:
	go build -o bin/control-plane ./cmd/control-plane
	go build -o bin/edge-agent ./cmd/edge-agent
	go build -o bin/origin ./cmd/origin
	go build -o bin/stress ./cmd/stress
	go build -o bin/dns-adapter ./cmd/dns-adapter
	go build -o bin/gateway ./cmd/gateway

build-quic:
	go build -tags quic -o bin/control-plane ./cmd/control-plane
	go build -tags quic -o bin/edge-agent ./cmd/edge-agent
	go build -tags quic -o bin/origin ./cmd/origin
	go build -tags quic -o bin/stress ./cmd/stress
	go build -tags quic -o bin/dns-adapter ./cmd/dns-adapter
	go build -tags quic -o bin/gateway ./cmd/gateway

run-cp: build
	./bin/control-plane

run-ea: build
	./bin/edge-agent

run-origin: build
	./bin/origin

run-gateway: build
	./bin/gateway

clean:
	rm -rf bin/

# ─── Testing ───────────────────────────────────────────────

test:
	go test ./... -count=1 -timeout 60s

test-verbose:
	go test ./... -count=1 -timeout 60s -v

test-race:
	go test ./... -count=1 -timeout 120s -race

test-cover:
	go test ./... -count=1 -timeout 60s -coverprofile=coverage.out
	go tool cover -func=coverage.out

test-cover-html:
	go test ./... -count=1 -timeout 60s -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

test-short:
	go test ./... -short -count=1 -timeout 30s

test-quic:
	go test ./... -tags quic -count=1 -timeout 60s

test-quic-verbose:
	go test ./... -tags quic -count=1 -timeout 60s -v

test-quic-race:
	go test ./... -tags quic -count=1 -timeout 120s -race

test-quic-cover:
	go test ./... -tags quic -count=1 -timeout 60s -coverprofile=coverage_quic.out
	go tool cover -func=coverage_quic.out

test-tunnel:
	go test ./internal/tunnel/... -count=1 -timeout 60s -v

test-gateway:
	go test ./internal/gateway/... -count=1 -timeout 60s -v

bench:
	go test ./... -bench=. -benchmem -timeout 120s

# ─── Integration Tests (require docker) ────────────────────

integration-test:
	docker compose up -d postgres redis
	@sleep 3
	TEST_PG_URL="postgres://edf:edf@localhost:15432/edf?sslmode=disable" \
	TEST_REDIS_ADDR="localhost:16379" \
	go test ./internal/controlplane/ -run Integration -count=1 -timeout 60s -v
	docker compose down -v

# ─── Stress / Load Testing ─────────────────────────────────

stress-build:
	go build -o bin/stress ./cmd/stress

stress-dispatch:
	@echo "Stress test: dispatch API (302 redirects)"
	go run ./cmd/stress -mode=dispatch -c=20 -d=30s -objects=50

stress-direct:
	@echo "Stress test: direct origin requests"
	go run ./cmd/stress -mode=direct -c=10 -d=30s -objects=50

stress-bench-dispatch:
	@echo "Benchmark: dispatch resolve API"
	go run ./cmd/stress -mode=bench-dispatch -c=50 -d=30s -objects=200

stress-bench-origin:
	@echo "Benchmark: origin file serving"
	go run ./cmd/stress -mode=bench-origin -c=30 -d=30s

stress-bench-edge:
	@echo "Benchmark: edge agent cache serving"
	go run ./cmd/stress -mode=bench-edge -c=50 -d=30s -edge=http://localhost:9090 -objects=100

stress-gateway:
	@echo "Stress test: gateway proxy"
	go run ./cmd/stress -mode=dispatch -c=20 -d=30s -objects=50 -dispatch=http://localhost:8880

# ─── Docker (本地演示) ──────────────────────────────────────

docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down -v

# ─── Docker (分离部署) ──────────────────────────────────────

docker-cp-up:
	docker compose -f docker-compose.cp.yml up -d

docker-cp-down:
	docker compose -f docker-compose.cp.yml down -v

docker-cp-up-full:
	docker compose -f docker-compose.cp.yml --profile full up -d

docker-edge-up:
	docker compose -f docker-compose.edge.yml up -d

docker-edge-down:
	docker compose -f docker-compose.edge.yml down -v

docker-edge-up-full:
	docker compose -f docker-compose.edge.yml --profile full up -d

# ─── Lint / Vet ────────────────────────────────────────────

lint:
	go vet ./...
