# The Makefile is the single source of truth for how the project is built and
# checked. CI calls these targets and nothing else, so `make ci` on a laptop
# and a green pipeline mean the same thing.

SHELL       := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN     := bin/aishiterud
PKG     := github.com/AIShiteru-LMS/AIShiteru-Core
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

# Maintenance database for the Go tests; the role must be able to CREATE
# DATABASE. The default reaches a local server over its unix socket.
TEST_DATABASE_URL ?= postgres:///postgres
export TEST_DATABASE_URL

# The psql suite takes its connection from the usual PG* variables.
SQLTEST_DB := aishiteru_sqltest_$(shell echo $$$$)
PSQL       := psql -X -q -v ON_ERROR_STOP=1

.PHONY: help
help:
	@grep -E '^[a-z][a-z0-9-]*:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

.PHONY: build
build: ## compile the server into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/aishiterud

.PHONY: test
test: ## Go tests, against TEST_DATABASE_URL
	go test -race -shuffle=on -coverprofile=cover.out ./...

.PHONY: db-test-sql
db-test-sql: ## psql suite: migrations up, seed, constraint tests, down, up again
	@createdb $(SQLTEST_DB)
	@trap 'dropdb --if-exists $(SQLTEST_DB)' EXIT; \
	ups=$$(ls src/migrations/*.up.sql | sort); \
	downs=$$(ls src/migrations/*.down.sql | sort -r); \
	for f in $$ups; do echo "up    $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; done; \
	echo "seed  src/seed/presets.sql"; $(PSQL) -d $(SQLTEST_DB) -f src/seed/presets.sql; \
	out=$$(psql -X -d $(SQLTEST_DB) -f src/tests/constraints_test.sql 2>&1) || { echo "$$out" | grep -E 'FAIL|ERROR' ; exit 1; }; \
	echo "$$out" | grep -q 'All checks passed.' || { echo "$$out" | tail -5; exit 1; }; \
	echo "tests $$(echo "$$out" | grep -c 'NOTICE:  PASS') checks passed"; \
	for f in $$downs; do echo "down  $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; done; \
	left=$$(psql -X -At -d $(SQLTEST_DB) -c "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'"); \
	[ "$$left" = "0" ] || { echo "down left $$left tables behind"; exit 1; }; \
	for f in $$ups; do echo "up    $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; done

.PHONY: fmt-check
fmt-check: ## fail if any file needs gofmt
	@out=$$(gofmt -l . | grep -v '^internal/db/dbq/' || true); \
	[ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

.PHONY: tidy-check
tidy-check: ## fail if go.mod or go.sum is not tidy
	go mod tidy -diff

.PHONY: lint
lint: fmt-check tidy-check ## gofmt, go mod tidy, go vet, golangci-lint
	go vet ./...
	golangci-lint run

.PHONY: sqlc
sqlc: ## regenerate internal/db/dbq from the SQL
	sqlc generate

.PHONY: sqlc-check
sqlc-check: ## fail if the generated code is out of date
	sqlc diff

.PHONY: vuln
vuln: ## known vulnerabilities in dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: docker
docker: ## build the image locally; never pushes
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t aishiteru-core:dev .

.PHONY: ci
ci: lint sqlc-check db-test-sql test build ## everything CI runs, except docker and vuln

.PHONY: dev-db
dev-db: ## Postgres and MinIO in Docker, for machines without a local server
	docker compose up -d

.PHONY: clean
clean: ## remove build output
	rm -rf bin dist cover.out

.PHONY: clean-testdb
clean-testdb: ## drop cached test templates and any stray test databases
	@psql -X -At -d postgres -c "SELECT datname FROM pg_database WHERE datname ~ '^aishiteru_(tmpl|t|sqltest)_'" \
		| while read -r d; do echo "drop $$d"; dropdb --if-exists --force "$$d"; done
