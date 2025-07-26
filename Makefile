# Tools
PROTOC = protoc
GO_BUILD = go build -mod=mod
GO_TIDY = go mod tidy

# Source directories
ACCOUNT_CMD = ./account-service/cmd/account
CATALOG_CMD = ./catalog-service/cmd/catalog
ORDER_CMD = ./order-service/cmd/order
GATEWAY_CMD = ./api-gateway

# Binaries
ACCOUNT_BIN = ./account-service/app
CATALOG_BIN = ./catalog-service/app
ORDER_BIN = ./order-service/app
GATEWAY_BIN = ./api-gateway/app

# Protobuf files
ACCOUNT_PROTO = ./account-service/proto/account.proto
CATALOG_PROTO = ./catalog-service/proto/catalog.proto
ORDER_PROTO = ./order-service/proto/order.proto

# Output for gRPC code
ACCOUNT_OUT = ./account-service/pb
CATALOG_OUT = ./catalog-service/pb
ORDER_OUT = ./order-service/pb

.PHONY: all tidy proto build clean \
        proto-account proto-catalog proto-order \
        build-account build-catalog build-order build-gateway \
        clean-account clean-catalog clean-order clean-gateway

# === Main Targets ===

all: tidy proto build

tidy:
	@echo "🧼 Running go mod tidy..."
	$(GO_TIDY)

proto: proto-account proto-catalog proto-order

build: build-account build-catalog build-order build-gateway

graphql:
	@echo "🧬 Generating GraphQL types..."
	cd api-gateway && go run github.com/99designs/gqlgen generate

clean: clean-account clean-catalog clean-order clean-gateway
	@echo "🧹 Cleaning generated gRPC .pb.go files..."
	rm -f $(ACCOUNT_OUT)/*.pb.go
	rm -f $(CATALOG_OUT)/*.pb.go
	rm -f $(ORDER_OUT)/*.pb.go

# === gRPC Code Generation ===

proto-account:
	@echo "📦 Generating gRPC code for Account Service..."
	$(PROTOC) --proto_path=./account-service/proto \
		--go_out=paths=source_relative:./account-service/pb \
		--go-grpc_out=paths=source_relative:./account-service/pb \
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

# === Go Builds ===

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
	@echo "❌ Cleaning Account binary..."
	rm -f $(ACCOUNT_BIN)

clean-catalog:
	@echo "❌ Cleaning Catalog binary..."
	rm -f $(CATALOG_BIN)

clean-order:
	@echo "❌ Cleaning Order binary..."
	rm -f $(ORDER_BIN)

clean-gateway:
	@echo "❌ Cleaning Gateway binary..."
	rm -f $(GATEWAY_BIN)
