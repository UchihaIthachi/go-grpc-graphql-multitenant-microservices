# === Tools ===
PROTOC       = protoc
GO           = go
GO_BUILD     = $(GO) build -mod=mod
GO_TIDY      = $(GO) mod tidy
GO_VENDOR    = $(GO) mod vendor
GQLGEN       = go run github.com/99designs/gqlgen generate

# === Source Directories ===
ACCOUNT_CMD  = ./account-service/cmd/account
CATALOG_CMD  = ./catalog-service/cmd/catalog
ORDER_CMD    = ./order-service/cmd/order
GATEWAY_CMD  = ./api-gateway

# === Binary Outputs ===
ACCOUNT_BIN  = ./account-service/app
CATALOG_BIN  = ./catalog-service/app
ORDER_BIN    = ./order-service/app
GATEWAY_BIN  = ./api-gateway/app

# === Protobuf Definitions ===
ACCOUNT_PROTO = ./account-service/proto/account.proto
CATALOG_PROTO = ./catalog-service/proto/catalog.proto
ORDER_PROTO   = ./order-service/proto/order.proto

# === gRPC Output Directories ===
ACCOUNT_OUT  = ./account-service/pb
CATALOG_OUT  = ./catalog-service/pb
ORDER_OUT    = ./order-service/pb

# === Phony Targets ===
.PHONY: all tidy vendor proto build graphql clean \
        proto-account proto-catalog proto-order \
        build-account build-catalog build-order build-gateway \
        clean-account clean-catalog clean-order clean-gateway

# === High-Level Targets ===

all: tidy vendor proto build

tidy:
	@echo "🧼 Running go mod tidy..."
	$(GO_TIDY)

vendor:
	@echo "📦 Vendoring dependencies..."
	$(GO_VENDOR)

proto: proto-account proto-catalog proto-order

build: build-account build-catalog build-order build-gateway

graphql:
	@echo "🧬 Generating GraphQL types..."
	cd api-gateway && $(GQLGEN)

clean: clean-account clean-catalog clean-order clean-gateway
	@echo "🧹 Cleaning generated gRPC .pb.go files..."
	rm -f $(ACCOUNT_OUT)/*.pb.go
	rm -f $(CATALOG_OUT)/*.pb.go
	rm -f $(ORDER_OUT)/*.pb.go

# === gRPC Code Generation ===

proto-account:
	@echo "📦 Generating gRPC code for Account Service..."
	$(PROTOC) --proto_path=./account-service/proto \
	          --go_out=paths=source_relative:$(ACCOUNT_OUT) \
	          --go-grpc_out=paths=source_relative:$(ACCOUNT_OUT) \
	          $(ACCOUNT_PROTO)

proto-catalog:
	@echo "📦 Generating gRPC code for Catalog Service..."
	$(PROTOC) --proto_path=./catalog-service/proto \
	          --go_out=paths=source_relative:$(CATALOG_OUT) \
	          --go-grpc_out=paths=source_relative:$(CATALOG_OUT) \
	          $(CATALOG_PROTO)

proto-order:
	@echo "📦 Generating gRPC code for Order Service..."
	$(PROTOC) --proto_path=./order-service/proto \
	          --go_out=paths=source_relative:$(ORDER_OUT) \
	          --go-grpc_out=paths=source_relative:$(ORDER_OUT) \
	          $(ORDER_PROTO)

# === Build Commands ===

build-account:
	@echo "🔨 Building Account Service..."
	$(GO_BUILD) -o $(ACCOUNT_BIN) $(ACCOUNT_CMD)

build-catalog:
	@echo "🔨 Building Catalog Service..."
	$(GO_BUILD) -o $(CATALOG_BIN) $(CATALOG_CMD)

build-order:
	@echo "🔨 Building Order Service..."
	$(GO_BUILD) -o $(ORDER_BIN) $(ORDER_CMD)

build-gateway:
	@echo "🔨 Building API Gateway..."
	$(GO_BUILD) -o $(GATEWAY_BIN) $(GATEWAY_CMD)

# === Clean Binaries ===

clean-account:
	@echo "❌ Removing Account binary..."
	rm -f $(ACCOUNT_BIN)

clean-catalog:
	@echo "❌ Removing Catalog binary..."
	rm -f $(CATALOG_BIN)

clean-order:
	@echo "❌ Removing Order binary..."
	rm -f $(ORDER_BIN)

clean-gateway:
	@echo "❌ Removing Gateway binary..."
	rm -f $(GATEWAY_BIN)
