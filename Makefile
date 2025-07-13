# ====================================================================================
# VARIABLES
# ====================================================================================

# Tools
PROTOC := protoc
GO_BUILD := go build -mod=readonly
GO_TIDY := go mod tidy
GQLGEN := go run -v github.com/99designs/gqlgen generate

# Directories
ACCOUNT_DIR := ./account-service
CATALOG_DIR := ./catalog-service
ORDER_DIR := ./order-service
GATEWAY_DIR := ./api-gateway

# Source Commands
ACCOUNT_CMD := $(ACCOUNT_DIR)/cmd/account
CATALOG_CMD := $(CATALOG_DIR)/cmd/catalog
ORDER_CMD := $(ORDER_DIR)/cmd/order
GATEWAY_CMD := $(GATEWAY_DIR)

# Binaries
ACCOUNT_BIN := $(ACCOUNT_DIR)/app
CATALOG_BIN := $(CATALOG_DIR)/app
ORDER_BIN := $(ORDER_DIR)/app
GATEWAY_BIN := $(GATEWAY_DIR)/app

# Protobuf files
ACCOUNT_PROTO := $(ACCOUNT_DIR)/proto/account.proto
CATALOG_PROTO := $(CATALOG_DIR)/proto/catalog.proto
ORDER_PROTO := $(ORDER_DIR)/proto/order.proto

# Protobuf outputs
ACCOUNT_PB := $(ACCOUNT_DIR)/pb
CATALOG_PB := $(CATALOG_DIR)/pb
ORDER_PB := $(ORDER_DIR)/pb

# GraphQL files
GQLGEN_CONFIG := $(GATEWAY_DIR)/gqlgen.yml

# ====================================================================================
# MAIN TARGETS
# ====================================================================================

.PHONY: all
all: proto build
	@echo "✅ All targets built successfully!"

.PHONY: tidy
tidy:
	@echo "🧼 Running go mod tidy..."
	$(GO_TIDY)

.PHONY: build
build: build-account build-catalog build-order build-gateway
	@echo "🔨 All services built."

.PHONY: proto
proto: proto-account proto-catalog proto-order
	@echo "📦 All protobuf files generated."


.PHONY: graphql
graphql:
	@echo "📦 Generating GraphQL code..."
	$(GQLGEN) --config $(GQLGEN_CONFIG)


.PHONY: clean
clean: clean-account clean-catalog clean-order clean-gateway
	@echo "🧹 Cleaning all generated files..."
	rm -f $(ACCOUNT_PB)/*.pb.go
	rm -f $(CATALOG_PB)/*.pb.go
	rm -f $(ORDER_PB)/*.pb.go
	@echo "✅ Cleanup complete."

# ====================================================================================
# SERVICE-SPECIFIC TARGETS
# ====================================================================================

# --- Account Service ---
.PHONY: build-account
build-account:
	@echo "🔨 Building Account Service..."
	$(GO_BUILD) -o $(ACCOUNT_BIN) $(ACCOUNT_CMD)

.PHONY: proto-account
proto-account:
	@echo "📦 Generating gRPC code for Account Service..."
	$(PROTOC) --proto_path=$(ACCOUNT_DIR)/proto \
		--go_out=$(ACCOUNT_PB) --go-grpc_out=$(ACCOUNT_PB) \
		$(ACCOUNT_PROTO)

.PHONY: clean-account
clean-account:
	@echo "❌ Cleaning Account binary..."
	rm -f $(ACCOUNT_BIN)

# --- Catalog Service ---
.PHONY: build-catalog
build-catalog:
	@echo "🔨 Building Catalog Service..."
	$(GO_BUILD) -o $(CATALOG_BIN) $(CATALOG_CMD)

.PHONY: proto-catalog
proto-catalog:
	@echo "📦 Generating gRPC code for Catalog Service..."
	$(PROTOC) --proto_path=$(CATALOG_DIR)/proto \
		--go_out=$(CATALOG_PB) --go-grpc_out=$(CATALOG_PB) \
		$(CATALOG_PROTO)

.PHONY: clean-catalog
clean-catalog:
	@echo "❌ Cleaning Catalog binary..."
	rm -f $(CATALOG_BIN)

# --- Order Service ---
.PHONY: build-order
build-order:
	@echo "🔨 Building Order Service..."
	$(GO_BUILD) -o $(ORDER_BIN) $(ORDER_CMD)

.PHONY: proto-order
proto-order:
	@echo "📦 Generating gRPC code for Order Service..."
	$(PROTOC) --proto_path=$(ORDER_DIR)/proto \
		--go_out=$(ORDER_PB) --go-grpc_out=$(ORDER_PB) \
		$(ORDER_PROTO)

.PHONY: clean-order
clean-order:
	@echo "❌ Cleaning Order binary..."
	rm -f $(ORDER_BIN)

# --- API Gateway ---
.PHONY: build-gateway
build-gateway:
	@echo "🔨 Building API Gateway..."
	$(GO_BUILD) -o $(GATEWAY_BIN) $(GATEWAY_CMD)

.PHONY: clean-gateway
clean-gateway:
	@echo "❌ Cleaning Gateway binary..."
	rm -f $(GATEWAY_BIN)
