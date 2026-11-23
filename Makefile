PATH := /usr/local/go/bin:$(PATH)
export PATH

.PHONY: test lint vet check dev-up dev-down dev-up-otel db-init build tidy

test: vet
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

check: lint test

build:
	mkdir -p bin
	go build -trimpath -o bin/factoryd ./cmd/factoryd
	go build -trimpath -o bin/factory-worker ./cmd/factory-worker
	go build -trimpath -o bin/kfactory ./cmd/kfactory

tidy:
	go mod tidy

db-init:
	psql -d akf -v ON_ERROR_STOP=1 -c "DO \$$\$$ BEGIN CREATE ROLE akf_app LOGIN PASSWORD 'akf' NOSUPERUSER NOBYPASSRLS; EXCEPTION WHEN duplicate_object THEN NULL; END \$$\$$;"
	psql -d akf -v ON_ERROR_STOP=1 -c "GRANT CONNECT ON DATABASE akf TO akf_app"

dev-up:
	docker compose -f deployments/compose.yaml up -d postgres temporal-postgres temporal temporal-ui factoryd

dev-down:
	docker compose -f deployments/compose.yaml down

dev-up-otel:
	docker compose -f deployments/compose.yaml --profile otel up -d otel-collector
