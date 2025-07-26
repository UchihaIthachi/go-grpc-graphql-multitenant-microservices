package repository

import (
	"context"
	"database/sql"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/domain"
	_ "github.com/lib/pq"
	"github.com/segmentio/ksuid"
)

type Repository interface {
	Close()
	CreateTenant(ctx context.Context, name string) (*domain.Tenant, error)
	PutAccount(ctx context.Context, a domain.Account) error
	GetAccountByID(ctx context.Context, tenantID, id string) (*domain.Account, error)
	ListAccounts(ctx context.Context, tenantID string, skip uint64, take uint64) ([]domain.Account, error)
}

type postgresRepository struct {
	db *sql.DB
}

func NewPostgresRepository(url string) (Repository, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	err = db.Ping()
	if err != nil {
		return nil, err
	}
	return &postgresRepository{db}, nil
}

func (r *postgresRepository) Close() {
	r.db.Close()
}

func (r *postgresRepository) Ping() error {
	return r.db.Ping()
}

func (r *postgresRepository) CreateTenant(ctx context.Context, name string) (*domain.Tenant, error) {
	t := &domain.Tenant{
		ID:   ksuid.New().String(),
		Name: name,
	}
	_, err := r.db.ExecContext(ctx, "INSERT INTO tenants(id, name) VALUES($1, $2)", t.ID, t.Name)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (r *postgresRepository) PutAccount(ctx context.Context, a domain.Account) error {
	_, err := r.db.ExecContext(ctx, "INSERT INTO accounts(id, name, tenant_id) VALUES($1, $2, $3)", a.ID, a.Name, a.TenantID)
	return err
}

func (r *postgresRepository) GetAccountByID(ctx context.Context, tenantID, id string) (*domain.Account, error) {
	row := r.db.QueryRowContext(ctx, "SELECT id, name, tenant_id FROM accounts WHERE tenant_id = $1 AND id = $2", tenantID, id)
	a := &domain.Account{}
	if err := row.Scan(&a.ID, &a.Name, &a.TenantID); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *postgresRepository) ListAccounts(ctx context.Context, tenantID string, skip uint64, take uint64) ([]domain.Account, error) {
	rows, err := r.db.QueryContext(
		ctx,
		"SELECT id, name, tenant_id FROM accounts WHERE tenant_id = $1 ORDER BY id DESC OFFSET $2 LIMIT $3",
		tenantID,
		skip,
		take,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accounts := []domain.Account{}
	for rows.Next() {
		a := &domain.Account{}
		if err = rows.Scan(&a.ID, &a.Name, &a.TenantID); err == nil {
			accounts = append(accounts, *a)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}