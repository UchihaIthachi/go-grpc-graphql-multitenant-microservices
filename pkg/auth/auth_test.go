package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

const tenant = tenancy.ID("11111111-1111-4111-8111-111111111111")

func keys(t *testing.T) (Issuer, Verifier, []byte) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte("k"), 32)
	return Issuer{Key: priv}, Verifier{Public: pub, ServiceSecret: secret}, secret
}

func TestUserToken(t *testing.T) {
	iss, v, _ := keys(t)
	tok, _, err := iss.Issue("user-1", tenant, "acme", RoleCustomer)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "user-1" || p.Tenant != tenant || p.Role != RoleCustomer || p.IsService() {
		t.Fatalf("principal: %+v", p)
	}
}

func TestServiceToken(t *testing.T) {
	_, v, secret := keys(t)
	tok, err := ServiceToken(secret, "order", tenant)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(tok)
	if err != nil || !p.IsService() || p.Subject != "svc:order" {
		t.Fatalf("%+v %v", p, err)
	}
	// a verifier without the secret (the public gateway) refuses service tokens
	v.ServiceSecret = nil
	if _, err := v.Verify(tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("gateway accepted a service token")
	}
	if _, err := ServiceToken([]byte("short"), "order", tenant); err == nil {
		t.Fatal("short secret accepted")
	}
}

func TestRejects(t *testing.T) {
	iss, v, secret := keys(t)
	otherIss, _, _ := keys(t)
	sign := func(m jwt.SigningMethod, key any, c claims) string {
		s, err := jwt.NewWithClaims(m, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	now := time.Now()
	base := func(iss, role string) claims {
		return claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: iss, Subject: "u", Audience: jwt.ClaimStrings{audience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}, Tenant: string(tenant), Role: role}
	}
	expired := base(userIssuer, RoleCustomer)
	expired.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Hour))
	wrongAud := base(userIssuer, RoleCustomer)
	wrongAud.Audience = jwt.ClaimStrings{"other"}
	badTenant := base(userIssuer, RoleCustomer)
	badTenant.Tenant = "acme"
	good, _, _ := iss.Issue("u", tenant, "acme", RoleAdmin)

	for name, tok := range map[string]string{
		"signed by another key": func() string { s, _, _ := otherIss.Issue("u", tenant, "acme", RoleAdmin); return s }(),
		"tampered":              good[:len(good)-2] + "xx",
		"expired":               sign(jwt.SigningMethodEdDSA, iss.Key, expired),
		"wrong audience":        sign(jwt.SigningMethodEdDSA, iss.Key, wrongAud),
		"tenant not a uuid":     sign(jwt.SigningMethodEdDSA, iss.Key, badTenant),
		// alg confusion: HMAC "signed" with the public key, claiming to be the account service
		"hs256 with public key": sign(jwt.SigningMethodHS256, []byte(iss.Key.Public().(ed25519.PublicKey)), base(userIssuer, RoleAdmin)),
		// a service secret can't mint user tokens, nor can a user token claim the service role
		"hs256 as account":      sign(jwt.SigningMethodHS256, secret, base(userIssuer, RoleAdmin)),
		"user token, svc role":  sign(jwt.SigningMethodEdDSA, iss.Key, base(userIssuer, RoleService)),
		"svc token, admin role": sign(jwt.SigningMethodHS256, secret, base("svc:order", RoleAdmin)),
		"alg none":              sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, base(userIssuer, RoleAdmin)),
		"unknown role":          sign(jwt.SigningMethodEdDSA, iss.Key, base(userIssuer, "root")),
	} {
		if _, err := v.Verify(tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: accepted", name)
		}
	}
}
