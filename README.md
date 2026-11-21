# Multitenant order and payments platform (Go)

[![ci](https://github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/actions/workflows/ci.yml/badge.svg?branch=v2)](https://github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/actions/workflows/ci.yml)

**Status: v2 rework in progress** on branch `v2`. The original gRPC + GraphQL demo is kept as tag
[`v0-legacy`](https://github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/tree/v0-legacy).

Goal: a small but correct commerce backend in Go. **Place order → reserve stock → take payment →
confirm**, with compensation when a step fails, exact money, a double-entry ledger, idempotency on
every write, and tenants isolated by Postgres row-level security. The design, decisions and phases
are in [docs/design.md](docs/design.md).

## What works today

| Part | What it guarantees | Proven by |
|---|---|---|
| `pkg/money` | Integer minor units + ISO currency; no floats; overflow and currency-mismatch errors; parsing never rounds | unit tests |
| `pkg/tenancy`, `pkg/db` | Tenant in the context → `SET LOCAL app.tenant_id` per transaction; calls without a tenant are refused; serialization failures and deadlocks retried | ledger integration tests |
| `internal/payment/ledger` | Double-entry ledger: every entry balances per currency, **enforced by the database at COMMIT**; postings are append-only, even for the owner; balances can't go negative unless allowed; posting is idempotent on a key (same key + different request = conflict) | 10 integration tests on real Postgres, race detector on |

The integration tests that matter most:

- **20 identical requests at once** → exactly one entry, balance moved once.
- **10 workers × 30 random transfers** between 5 accounts → balances still sum to 0, and cached balances match the postings.
- **Raw SQL as the app role** writing an unbalanced entry → rejected by the database.
- **Tenant B with tenant A's account id** → can't read it and can't post against it. Foreign-key checks
  ignore row-level security, so the tenant is part of the posting → account key; a mutation test
  (dropping it) shows tenant B could otherwise move tenant A's money.

## Run the tests

Needs Go 1.27 and Docker.

```bash
make test        # starts postgres:17 on :55432, runs everything with -race
make lint        # go vet, gofmt, staticcheck
```

Without `TEST_PG_DSN` the Postgres tests skip; CI fails if any of them skipped.

## Roadmap

| Phase | Scope | Status |
|---|---|---|
| P0 | Branch `v2`, legacy tagged, vendor/binaries/Cassandra/Elasticsearch removed, CI | ✅ |
| P1 | Money, tenancy, tenant-scoped transactions, migrations | ✅ |
| P2 | Payment ledger | ✅ ledger · ⏳ payment intents (authorize / capture / refund) |
| P3 | Catalog + stock reservations (no oversell under a 50-way race) | ⏳ |
| P4 | Order saga (orchestrated, persisted), transactional outbox, Redpanda, idempotent consumers | ⏳ |
| P5 | Fake card processor, signed webhooks, replay protection, daily reconciliation | ⏳ |
| P6 | Account service, JWT, GraphQL gateway on current gqlgen, docker compose e2e | ⏳ |
| P7 | OpenTelemetry → Jaeger, Prometheus, k6 load test, failure write-ups | ⏳ |
| P8 | Architecture atlas, diagrams, Terraform for ECS Fargate + Aurora (validate only) | ⏳ |
