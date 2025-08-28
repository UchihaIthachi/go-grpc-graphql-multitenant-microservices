# 🧩 Go gRPC Microservices With GraphQL Gateway

This project demonstrates a modern **microservices architecture** built with:

✅ **Go** for backend services
✅ **gRPC** for high-performance inter-service communication
✅ **GraphQL** as a flexible API Gateway
✅ **Docker Compose** for local orchestration
✅ **PostgreSQL**, **Elasticsearch**, and optionally **Cassandra** for persistence
✅ Designed for **observability**, and **scalability**

---

## 🏗️ Architecture Overview

This system follows **Domain-Driven Design** and microservices best practices. It is designed for flexibility, fault isolation, and extensibility.

### 🔧 Core Services

| Service           | Responsibility                            | Database      | Protocol |
| ----------------- | ----------------------------------------- | ------------- | -------- |
| `account-service` | Manages accounts, users, and auth context | PostgreSQL    | gRPC     |
| `catalog-service` | Product catalog with search functionality | Elasticsearch | gRPC     |
| `order-service`   | Order placement, storage, and status      | Cassandra     | gRPC     |
| `api-gateway`     | Unified entrypoint for clients            | —             | GraphQL  |

### 🔁 Communication Patterns

- **Service-to-service** → gRPC with Protobuf definitions
- **Client-to-system** → GraphQL via the `api-gateway`

### 📦 Polyglot Persistence

| Data Type       | Backed By     | Notes                                 |
| --------------- | ------------- | ------------------------------------- |
| Account data    | PostgreSQL    | ACID-compliant, normalized structure  |
| Product catalog | Elasticsearch | Full-text search, filtering, scoring  |
| Order records   | Cassandra     | Scalable writes, denormalized storage |

---

## 🗂️ Project Structure

```text
go-grpc-graphql-microservices/
├── account-service/       # Manages accounts
├── catalog-service/       # Searchable product catalog
├── order-service/         # Order management
├── api-gateway/           # GraphQL Gateway
├── docker-compose.yml     # Local orchestration
├── .dockerignore
└── README.md
```

Each service is isolated, built with Go, communicates over gRPC, and is wired into the GraphQL API Gateway.

---

## 🚀 Getting Started

### 🧰 Prerequisites

- Go 1.24+
- Docker + Docker Compose
- `protoc` compiler

### 🧬 Clone & Launch

```bash
git clone https://github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices
cd go-grpc-graphql-multitenant-microservices

docker-compose up -d --build
```

Access GraphQL Playground at:
📍 [http://localhost:8000/playground](http://localhost:8000/playground)

---

## 🔧 gRPC Setup

### 📦 Install protoc + Go plugins

```bash
wget https://github.com/protocolbuffers/protobuf/releases/download/v23.0/protoc-23.0-linux-x86_64.zip
unzip protoc-23.0-linux-x86_64.zip -d protoc
sudo mv protoc/bin/protoc /usr/local/bin/

go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

export PATH="$PATH:$(go env GOPATH)/bin"
source ~/.bashrc
```

### 🛠️ Generate Protobuf files

To generate the gRPC code for a service, run the following command from the root of the project:
```bash
make proto-account
```
This will generate the protobuf files for the account service. You can do the same for the other services by running `make proto-catalog` and `make proto-order`.

---

## 🕸️ GraphQL API Usage

The GraphQL API aggregates data from all services.

### 📘 Accounts

```graphql
query {
  accounts {
    id
    name
  }
}
```

### ➕ Create Account

```graphql
mutation {
  createAccount(account: { name: "John Doe" }) {
    id
    name
  }
}
```

### 🔍 Products

```graphql
query {
  products {
    id
    name
    price
  }
}
```

### 🔎 Search Products with Pagination

```graphql
query {
  products(pagination: { skip: 0, take: 5 }, query: "camera") {
    id
    name
    description
    price
  }
}
```

### 🧾 Orders

```graphql
mutation {
  createOrder(
    order: { accountId: "acc-123", products: [{ id: "prod-1", quantity: 2 }] }
  ) {
    id
    totalPrice
    products {
      name
      quantity
    }
  }
}
```

### 📊 Account with Orders

```graphql
query {
  accounts(id: "acc-123") {
    name
    orders {
      id
      createdAt
      totalPrice
      products {
        name
        quantity
        price
      }
    }
  }
}
```

---

## 📈 Observability & Operations

| Tool       | Purpose                              |
| ---------- | ------------------------------------ |
| Zipkin     | Distributed tracing (optionally add) |
| Prometheus | Metrics collection (optional)        |
| Grafana    | Dashboards and alerts                |
| ELK Stack  | Centralized logging (future)         |

---

## 🧠 Future Enhancements

- JWT authentication isolation via auth service
- GraphQL federation or REST fallback gateway
- Cassandra integration for scalable order write storage
- tRPC or REST proxy layer for frontend-first consumers
- ArgoCD or GitHub Actions CI/CD
- Production-ready Helm chart and k8s manifests

---
