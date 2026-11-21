// Package tenancy carries the caller's tenant through a request. The gateway takes it from
// the verified JWT, gRPC metadata carries it between services (see pkg/grpcx), and pkg/db
// turns it into a Postgres setting that row-level security policies check.
package tenancy

import (
	"context"
	"errors"
	"regexp"
)

// MetadataKey is the gRPC metadata key that carries the tenant id between services.
const MetadataKey = "x-tenant-id"

// ID is a tenant's UUID in canonical lower-case form.
type ID string

var (
	ErrMissing = errors.New("tenancy: no tenant in context")
	ErrInvalid = errors.New("tenancy: tenant id is not a UUID")
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Parse validates a tenant id. Only canonical lower-case UUIDs are accepted, so the same tenant
// can't appear under two spellings.
func Parse(s string) (ID, error) {
	if !uuidRe.MatchString(s) {
		return "", ErrInvalid
	}
	return ID(s), nil
}

type ctxKey struct{}

// With returns a context that carries tenant id.
func With(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the tenant in ctx, or ErrMissing.
func From(ctx context.Context) (ID, error) {
	id, ok := ctx.Value(ctxKey{}).(ID)
	if !ok || id == "" {
		return "", ErrMissing
	}
	return id, nil
}
