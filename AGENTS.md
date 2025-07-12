## Development Process

### Protobuf Generation

When you modify a `.proto` file, you need to regenerate the Go code. Use the following command:

```bash
go generate ./...
```

If you don't have `protoc` installed, you can install it with the following command:

```bash
sudo apt-get update && sudo apt-get install -y protobuf-compiler
```

### Database Migrations

This project uses a `db.dockerfile` for each service to manage database migrations. When you need to make a change to the database schema, you should:

1.  Update the `up.sql` or `up.cql` file with your schema changes.
2.  Rebuild the database container using `docker-compose build <service_name>_db`.
3.  Restart the services with `docker-compose up -d`.

### Multi-tenancy

The application is multi-tenant. The `tenant_id` is passed in the gRPC metadata with the key `tenant`. The `api-gateway` is responsible for extracting the tenant information from the incoming request and passing it to the downstream services. When adding new gRPC methods, make sure to handle the `tenant_id` correctly.

**Next Steps:**

1.  **`account-service`**: Update `handler/server.go` to extract the `tenant_id` from the gRPC metadata and pass it to the service layer.
2.  **`order-service`**: Update `handler/server.go` to extract the `tenant_id` from the gRPC metadata and pass it to the service layer. Also, update the Cassandra queries to be tenant-aware.
3.  **`api-gateway`**: Update the resolvers to extract the `tenant_id` from the incoming HTTP request (e.g., from a JWT token or a custom header) and pass it to the downstream services in the gRPC metadata.
