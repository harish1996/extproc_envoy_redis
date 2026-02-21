.PHONY: build tidy run test docker-build up down seed

## Fetch dependencies and generate go.sum
tidy:
	go mod tidy

## Build the binary locally (requires Go 1.21+)
build: tidy
	go build -o bin/extproc ./cmd/main.go

## Run locally (requires Redis on localhost:6379)
run: build
	./bin/extproc \
		--redis-addr=localhost:6379 \
		--grpc-addr=:50051 \
		--key-ttl=10m \
		--log-level=debug

## Run all tests
test:
	go test ./...

## Build Docker image
docker-build:
	docker build -t extproc-redis:local .

## Start everything via docker compose
up:
	docker compose up --build

## Stop everything
down:
	docker compose down

## Seed a test key in Redis (requires redis-cli)
seed:
	redis-cli SET /hello "world from redis cache" EX 600
	redis-cli SET /api/users "[{\"id\":1,\"name\":\"alice\"}]" EX 600
	@echo "Seeded. Try: curl -v http://localhost:10000/hello"

## Check cache hit manually
check:
	curl -si http://localhost:10000/hello | grep -E "HTTP|x-cache|body" || curl -si http://localhost:10000/hello