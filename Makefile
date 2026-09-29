# The Makefile is the single source of truth for how the project is built and
# checked. CI calls these targets and nothing else, so `make ci` on a laptop
# and a green pipeline mean the same thing.

SHELL       := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BIN     := bin/aishiterud
PKG     := github.com/AIShie-Education/AIShie-Core
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
db-test-sql: ## psql suite: migrations up (over the fixtures in src/tests/up), seed, constraint tests, the newest two down and up again over the seed, down (over the fixtures in src/tests/down), up again
	@createdb $(SQLTEST_DB)
	@trap 'dropdb --if-exists $(SQLTEST_DB)' EXIT; \
	ups=$$(ls src/migrations/*.up.sql | sort); \
	downs=$$(ls src/migrations/*.down.sql | sort -r); \
	for f in $$ups; do \
		n=$$(basename $$f | cut -c1-4); \
		if [ -f src/tests/up/$$n.before.sql ]; then echo "fixt  src/tests/up/$$n.before.sql"; $(PSQL) -d $(SQLTEST_DB) -f src/tests/up/$$n.before.sql; fi; \
		echo "up    $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; \
		if [ -f src/tests/up/$$n.after.sql ]; then $(PSQL) -d $(SQLTEST_DB) -f src/tests/up/$$n.after.sql; fi; \
	done; \
	echo "seed  src/seed/presets.sql"; $(PSQL) -d $(SQLTEST_DB) -f src/seed/presets.sql; \
	out=$$(psql -X -d $(SQLTEST_DB) -f src/tests/constraints_test.sql 2>&1) || { echo "$$out" | grep -E 'FAIL|ERROR' ; exit 1; }; \
	echo "$$out" | grep -q 'All checks passed.' || { echo "$$out" | tail -5; exit 1; }; \
	echo "tests $$(echo "$$out" | grep -c 'NOTICE:  PASS') checks passed"; \
	$(PSQL) -d $(SQLTEST_DB) -c "CREATE TABLE redo_builtins AS SELECT * FROM permission_preset WHERE dept_id IS NULL"; \
	for f in $$(echo "$$downs" | head -2); do echo "redo  $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; done; \
	for f in $$(echo "$$ups" | tail -2); do echo "redo  $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; done; \
	$(PSQL) -d $(SQLTEST_DB) -f src/tests/redo_builtins.sql; \
	for f in $$downs; do \
		n=$$(basename $$f | cut -c1-4); \
		if [ -f src/tests/down/$$n.before.sql ]; then echo "fixt  src/tests/down/$$n.before.sql"; $(PSQL) -d $(SQLTEST_DB) -f src/tests/down/$$n.before.sql; fi; \
		echo "down  $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; \
		if [ -f src/tests/down/$$n.after.sql ]; then $(PSQL) -d $(SQLTEST_DB) -f src/tests/down/$$n.after.sql; fi; \
	done; \
	left=$$(psql -X -At -d $(SQLTEST_DB) -c "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'"); \
	[ "$$left" = "0" ] || { echo "down left $$left tables behind"; exit 1; }; \
	for f in $$ups; do echo "up    $$f"; $(PSQL) -d $(SQLTEST_DB) -f $$f; done

.PHONY: test-s3
test-s3: ## the S3 store against a real S3-compatible server; needs S3_TEST_ENDPOINT and keys (make minio builds one)
	@[ -n "$$S3_TEST_ENDPOINT" ] || { echo "S3_TEST_ENDPOINT is not set; try: make minio && (bin/minio server var/minio &) && S3_TEST_ENDPOINT=localhost:9000 S3_TEST_ACCESS_KEY=minioadmin S3_TEST_SECRET_KEY=minioadmin make test-s3"; exit 1; }
	go test -count=1 -run TestS3Store -v ./internal/blob/

# MinIO stands in for S3 in `make test-s3`. It no longer publishes an image
# anyone can pull (Docker Hub dropped minio/minio; quay.io wants a login) or
# binaries (dl.min.io answers 410 Gone), so it is built from source, at the
# commit pinned here, through the Go module proxy. That takes a few minutes,
# once; CI caches the binary.
MINIO_VERSION ?= v0.0.0-20260212201848-7aac2a2c5b7c

.PHONY: minio
minio: ## build MinIO into bin/minio, for make test-s3 (once per MINIO_VERSION)
	@if [ "$$(go version -m bin/minio 2>/dev/null | awk '$$1 == "mod" { print $$3 }')" = "$(MINIO_VERSION)" ]; then \
		echo "bin/minio is $(MINIO_VERSION)"; \
	else \
		GOTOOLCHAIN=local CGO_ENABLED=0 GOBIN=$(CURDIR)/bin go install github.com/minio/minio@$(MINIO_VERSION); \
	fi

.PHONY: minio-version
minio-version: ## the MinIO commit make minio builds
	@echo $(MINIO_VERSION)

.PHONY: e2e
e2e: build ## the real binary and curl: bootstrap, then docs/schema.md §5 over the REST API
	scripts/e2e.sh

.PHONY: fmt-check
fmt-check: ## fail if any file needs gofmt
	@out=$$(gofmt -l . | grep -v '^internal/db/dbq/' || true); \
	[ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

.PHONY: tidy-check
tidy-check: ## fail if go.mod or go.sum is not tidy
	go mod tidy -diff

.PHONY: lint
lint: fmt-check tidy-check actionlint ## gofmt, go mod tidy, the workflows, go vet, golangci-lint
	go vet ./...
	golangci-lint run

# actionlint also runs shellcheck over every `run:` block when shellcheck is
# installed, as it is on GitHub's runners.
ACTIONLINT_VERSION ?= v1.7.12

.PHONY: actionlint
actionlint: ## the GitHub Actions workflows
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

.PHONY: script-test
script-test: ## the tests of scripts/ and deploy/, and shellcheck over deploy/ where it is installed
	scripts/release-notes_test.sh
	deploy/aishiteru-deploy_test.sh
	@if command -v shellcheck >/dev/null; then shellcheck -s sh deploy/aishiteru-deploy deploy/aishiterud deploy/setup-server.sh && shellcheck deploy/aishiteru-deploy_test.sh; else echo "shellcheck is not installed: deploy/ not checked"; fi

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
ci: lint script-test sqlc-check db-test-sql test e2e ## everything CI runs, except docker, test-s3 and vuln

.PHONY: dev-db
dev-db: ## Postgres in Docker, for machines without a local server
	docker compose up -d

.PHONY: clean
clean: ## remove build output
	rm -rf bin dist cover.out

.PHONY: clean-testdb
clean-testdb: ## drop cached test templates and any stray test databases
	@psql -X -At -d postgres -c "SELECT datname FROM pg_database WHERE datname ~ '^aishiteru_(tmpl|t|sqltest)_'" \
		| while read -r d; do echo "drop $$d"; dropdb --if-exists --force "$$d"; done
