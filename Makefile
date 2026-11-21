# Order and payments platform (v2). Run `make help`.
GO       ?= go
PG_PORT  ?= 55432
TEST_PG_DSN ?= postgres://postgres:postgres@localhost:$(PG_PORT)/postgres?sslmode=disable

.PHONY: help test test-unit test-db test-db-down vet lint tidy

help:            ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/'

test-db:         ## start Postgres 17 for integration tests (Docker)
	@docker run -d --name p5-test-pg -p $(PG_PORT):5432 -e POSTGRES_PASSWORD=postgres postgres:17-alpine >/dev/null 2>&1 || docker start p5-test-pg >/dev/null
	@until docker exec p5-test-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done; echo "postgres ready on :$(PG_PORT)"

test-db-down:    ## remove the test database container
	@docker rm -f p5-test-pg >/dev/null 2>&1 || true

test: test-db    ## all tests, including Postgres integration tests, with the race detector
	TEST_PG_DSN='$(TEST_PG_DSN)' $(GO) test -race -count=1 ./...

test-unit:       ## unit tests only (integration tests skip without TEST_PG_DSN)
	$(GO) test -race -count=1 ./...

vet:             ## go vet + gofmt check
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "run gofmt -w ."; exit 1)

lint: vet        ## staticcheck
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

tidy:            ## go mod tidy
	$(GO) mod tidy
