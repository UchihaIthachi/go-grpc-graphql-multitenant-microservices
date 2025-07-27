package service

import (
	"context"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/domain"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/repository"
	"github.com/segmentio/ksuid"
)

type Service interface {
	CreateTenant(ctx context.Context, name string) (*domain.Tenant, error)
	PostAccount(ctx context.Context, tenantID, name string) (*domain.Account, error)
	GetAccount(ctx context.Context, tenantID, id string) (*domain.Account, error)
	GetAccounts(ctx context.Context, tenantID string, skip uint64, take uint64) ([]domain.Account, error)
}

type accountService struct {
	repository repository.Repository
}

func NewService(r repository.Repository) Service {
	return &accountService{r}
}

func (s *accountService) CreateTenant(ctx context.Context, name string) (*domain.Tenant, error) {
	return s.repository.CreateTenant(ctx, name)
}

func (s *accountService) PostAccount(ctx context.Context, tenantID, name string) (*domain.Account, error) {
	a := &domain.Account{
		Name:     name,
		ID:       ksuid.New().String(),
		TenantID: tenantID,
	}
	if err := s.repository.PutAccount(ctx, *a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *accountService) GetAccount(ctx context.Context, tenantID, id string) (*domain.Account, error) {
	return s.repository.GetAccountByID(ctx, tenantID, id)
}

func (s *accountService) GetAccounts(ctx context.Context, tenantID string, skip uint64, take uint64) ([]domain.Account, error) {
	if take > 100 || (skip == 0 && take == 0) {
		take = 100
	}
	return s.repository.ListAccounts(ctx, tenantID, skip, take)
}