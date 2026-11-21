// Package account owns tenants, users and passwords, and signs user tokens.
package account

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

//go:embed migrations/*.sql
var Migrations embed.FS

const Schema = "account"

var (
	ErrInvalid  = errors.New("account: invalid request")
	ErrTaken    = errors.New("account: already registered")
	ErrBadLogin = errors.New("account: wrong tenant, email or password") // deliberately unspecific
	ErrNotFound = errors.New("account: not found")
)

// User as the rest of the system sees it.
type User struct {
	ID         string
	TenantID   tenancy.ID
	TenantSlug string
	Email      string
	Role       string
}

// Session is a signed-in user.
type Session struct {
	User      User
	Token     string
	ExpiresAt time.Time
}

// Service implements sign-up, registration and login.
type Service struct {
	DB         db.Beginner
	Issuer     auth.Issuer
	BcryptCost int // default bcrypt.DefaultCost; tests use bcrypt.MinCost
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// dummyHash is compared against when the user doesn't exist, so a login for an unknown email
// takes as long as one with a wrong password (no user enumeration by timing).
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

func (s Service) cost() int {
	if s.BcryptCost == 0 {
		return bcrypt.DefaultCost
	}
	return s.BcryptCost
}

func normalize(email, password string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		return "", fmt.Errorf("%w: email is not valid", ErrInvalid)
	}
	if len(password) < 10 || len(password) > 72 { // bcrypt reads at most 72 bytes
		return "", fmt.Errorf("%w: password must be 10 to 72 characters", ErrInvalid)
	}
	return email, nil
}

// SignUp creates a tenant and its first admin, and returns a token for the admin.
func (s Service) SignUp(ctx context.Context, name, slug, email, password string) (Session, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if !slugRe.MatchString(slug) || strings.TrimSpace(name) == "" {
		return Session{}, fmt.Errorf("%w: shop name and a slug of 3-40 lower-case letters, digits or dashes are required", ErrInvalid)
	}
	email, err := normalize(email, password)
	if err != nil {
		return Session{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cost())
	if err != nil {
		return Session{}, err
	}
	u := User{TenantSlug: slug, Email: email, Role: auth.RoleAdmin}
	// The tenant doesn't exist yet, so this one transaction sets app.tenant_id itself once it
	// has the new id; the user insert is then checked by RLS like any other.
	err = db.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		var tid string
		if err := tx.QueryRow(ctx, `INSERT INTO account.tenants (slug, name) VALUES ($1, $2) RETURNING id`, slug, strings.TrimSpace(name)).Scan(&tid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tid); err != nil {
			return err
		}
		u.TenantID = tenancy.ID(tid)
		return tx.QueryRow(ctx, `INSERT INTO account.users (tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'admin') RETURNING id`,
			tid, email, string(hash)).Scan(&u.ID)
	})
	if db.Code(err) == db.UniqueViolation {
		return Session{}, fmt.Errorf("%w: shop slug %q is taken", ErrTaken, slug)
	}
	if err != nil {
		return Session{}, err
	}
	return s.token(u)
}

// Register adds a customer to a tenant.
func (s Service) Register(ctx context.Context, slug, email, password string) (Session, error) {
	email, err := normalize(email, password)
	if err != nil {
		return Session{}, err
	}
	u, err := s.tenant(ctx, slug)
	if err != nil {
		return Session{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cost())
	if err != nil {
		return Session{}, err
	}
	u.Email, u.Role = email, auth.RoleCustomer
	err = db.InTenantTx(tenancy.With(ctx, u.TenantID), s.DB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO account.users (tenant_id, email, password_hash, role)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, 'customer') RETURNING id`, email, string(hash)).Scan(&u.ID)
	})
	if db.Code(err) == db.UniqueViolation {
		return Session{}, fmt.Errorf("%w: %s already has an account here", ErrTaken, email)
	}
	if err != nil {
		return Session{}, err
	}
	return s.token(u)
}

// Login checks a password and returns a token.
func (s Service) Login(ctx context.Context, slug, email, password string) (Session, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := s.tenant(ctx, slug)
	if errors.Is(err, ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return Session{}, ErrBadLogin
	}
	if err != nil {
		return Session{}, err
	}
	var hash string
	err = db.InTenantTx(tenancy.With(ctx, u.TenantID), s.DB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id, role, password_hash FROM account.users WHERE email = $1`, email).Scan(&u.ID, &u.Role, &hash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return Session{}, ErrBadLogin
	}
	if err != nil {
		return Session{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return Session{}, ErrBadLogin
	}
	u.Email = email
	return s.token(u)
}

// Get returns a user of the tenant in ctx.
func (s Service) Get(ctx context.Context, id string) (User, error) {
	var u User
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		var tid string
		err := tx.QueryRow(ctx, `SELECT u.id, u.tenant_id, t.slug, u.email, u.role FROM account.users u
			JOIN account.tenants t ON t.id = u.tenant_id WHERE u.id = $1`, id).Scan(&u.ID, &tid, &u.TenantSlug, &u.Email, &u.Role)
		u.TenantID = tenancy.ID(tid)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || db.Code(err) == "22P02" {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s Service) tenant(ctx context.Context, slug string) (User, error) {
	var u User
	err := db.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		var tid string
		err := tx.QueryRow(ctx, `SELECT id, slug FROM account.tenants WHERE slug = $1`, strings.ToLower(strings.TrimSpace(slug))).Scan(&tid, &u.TenantSlug)
		u.TenantID = tenancy.ID(tid)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

func (s Service) token(u User) (Session, error) {
	tok, exp, err := s.Issuer.Issue(u.ID, u.TenantID, u.TenantSlug, u.Role)
	return Session{User: u, Token: tok, ExpiresAt: exp}, err
}
