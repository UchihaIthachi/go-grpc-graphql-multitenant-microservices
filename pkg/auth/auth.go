// Package auth issues and verifies the two kinds of token the system uses.
//
// User tokens are signed by the account service with an Ed25519 private key (EdDSA). Every
// other service holds only the public key, so a compromised catalog or gateway can verify
// tokens but cannot mint them.
//
// Service tokens are what one service presents to another for work that no user is waiting
// on (the order saga's stock and payment steps, the recovery loop). They are HS256 with a
// secret shared by the services, live for one minute and carry the tenant they act for.
// Saga-step RPCs accept only service tokens, so a customer's token can't call
// PaymentService.Capture directly even from inside the network.
//
// The verifier picks the key by the token's algorithm AND issuer: an EdDSA token must come from
// "account", an HS256 token from "svc:<name>". A token can't switch kind by changing its header.
package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

const (
	RoleAdmin    = "admin"
	RoleCustomer = "customer"
	RoleService  = "service"

	userIssuer = "account"
	audience   = "shop"
)

var (
	ErrUnauthenticated = errors.New("auth: missing or invalid token")
	ErrForbidden       = errors.New("auth: not allowed")
)

// Principal is who is calling.
type Principal struct {
	Subject    string // user id, or "svc:<name>"
	Tenant     tenancy.ID
	TenantSlug string
	Role       string
	Token      string // the raw token, for forwarding user calls
}

// IsService reports whether the caller is another service.
func (p Principal) IsService() bool { return p.Role == RoleService }

type claims struct {
	jwt.RegisteredClaims
	Tenant string `json:"tid"`
	Slug   string `json:"tslug,omitempty"`
	Role   string `json:"role"`
}

// Issuer signs user tokens (account service only).
type Issuer struct {
	Key ed25519.PrivateKey
	TTL time.Duration // default 1 h
	Now func() time.Time
}

// Issue returns a signed user token and its expiry.
func (i Issuer) Issue(userID string, tenant tenancy.ID, slug, role string) (string, time.Time, error) {
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	ttl := i.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	t := now()
	exp := t.Add(ttl)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: userIssuer, Subject: userID, Audience: jwt.ClaimStrings{audience},
			IssuedAt: jwt.NewNumericDate(t), ExpiresAt: jwt.NewNumericDate(exp)},
		Tenant: string(tenant), Slug: slug, Role: role,
	}).SignedString(i.Key)
	return tok, exp, err
}

// ServiceToken mints a one-minute token for service name acting for tenant.
func ServiceToken(secret []byte, name string, tenant tenancy.ID) (string, error) {
	if len(secret) < 32 {
		return "", errors.New("auth: service secret must be at least 32 bytes")
	}
	t := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "svc:" + name, Subject: "svc:" + name, Audience: jwt.ClaimStrings{audience},
			IssuedAt: jwt.NewNumericDate(t), ExpiresAt: jwt.NewNumericDate(t.Add(time.Minute))},
		Tenant: string(tenant), Role: RoleService,
	}).SignedString(secret)
}

// Verifier checks both kinds of token.
type Verifier struct {
	Public        ed25519.PublicKey
	ServiceSecret []byte // nil: service tokens are refused
}

// Verify parses and checks a token.
func (v Verifier) Verify(raw string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		iss, _ := t.Claims.GetIssuer()
		switch t.Method.Alg() {
		case jwt.SigningMethodEdDSA.Alg():
			if iss != userIssuer || len(v.Public) == 0 {
				return nil, ErrUnauthenticated
			}
			return v.Public, nil
		case jwt.SigningMethodHS256.Alg():
			if !strings.HasPrefix(iss, "svc:") || len(v.ServiceSecret) == 0 {
				return nil, ErrUnauthenticated
			}
			return v.ServiceSecret, nil
		}
		return nil, ErrUnauthenticated
	}, jwt.WithValidMethods([]string{"EdDSA", "HS256"}), jwt.WithAudience(audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	tid, err := tenancy.Parse(c.Tenant)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: bad tenant", ErrUnauthenticated)
	}
	isSvc := strings.HasPrefix(c.Issuer, "svc:")
	if isSvc != (c.Role == RoleService) || (c.Role != RoleService && c.Role != RoleAdmin && c.Role != RoleCustomer) {
		return Principal{}, fmt.Errorf("%w: bad role", ErrUnauthenticated)
	}
	return Principal{Subject: c.Subject, Tenant: tid, TenantSlug: c.Slug, Role: c.Role, Token: raw}, nil
}

type ctxKey struct{}

// With returns ctx carrying p and p's tenant.
func With(ctx context.Context, p Principal) context.Context {
	return tenancy.With(context.WithValue(ctx, ctxKey{}, p), p.Tenant)
}

// From returns the caller, if any.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Require returns the caller if it has one of roles.
func Require(ctx context.Context, roles ...string) (Principal, error) {
	p, ok := From(ctx)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	for _, r := range roles {
		if p.Role == r {
			return p, nil
		}
	}
	return p, ErrForbidden
}

// ParsePrivateKey reads a base64 (std) Ed25519 seed or private key.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, errors.New("auth: private key must be a 32-byte seed or 64-byte key")
}

// ParsePublicKey reads a base64 (std) Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("auth: public key must be 32 bytes")
	}
	return ed25519.PublicKey(b), nil
}
