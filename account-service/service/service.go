package service

import (
	"context"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/domain"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/repository"
	"github.com/segmentio/ksuid"
)

type Service interface {
	PostAccount(ctx context.Context, name string) (*domain.Account, error)
	GetAccount(ctx context.Context, id string) (*domain.Account, error)
	GetAccounts(ctx context.Context, skip uint64, take uint64) ([]domain.Account, error)
}

type accountService struct {
	repository repository.Repository
}

func NewService(r repository.Repository) Service {
	return &accountService{r}
}


func (s *accountService) PostAccount(ctx context.Context, name string) (*domain.Account, error) {
	a := &domain.Account{
		Name:     name,
		ID:       ksuid.New().String(),
	}
	if err := s.repository.PutAccount(ctx, *a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *accountService) GetAccount(ctx context.Context, id string) (*domain.Account, error) {
	return s.repository.GetAccountByID(ctx, id)
}

func (s *accountService) GetAccounts(ctx context.Context, skip uint64, take uint64) ([]domain.Account, error) {
	if take > 100 || (skip == 0 && take == 0) {
		take = 100
	}
	return s.repository.ListAccounts(ctx, skip, take)
}