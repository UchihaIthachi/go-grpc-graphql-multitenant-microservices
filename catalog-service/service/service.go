package service

import (
	"context"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/catalog-service/domain"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/catalog-service/repository"
	"github.com/segmentio/ksuid"
)

type Service interface {
	PostProduct(ctx context.Context, name, description string, price float64) (*domain.Product, error)
	GetProduct(ctx context.Context, id string) (*domain.Product, error)
	GetProducts(ctx context.Context, skip uint64, take uint64) ([]domain.Product, error)
	GetProductsByIDs(ctx context.Context, ids []string) ([]domain.Product, error)
	SearchProducts(ctx context.Context, query string, skip uint64, take uint64) ([]domain.Product, error)
}

type catalogService struct {
	repository repository.Repository
}

func NewService(r repository.Repository) Service {
	return &catalogService{r}
}

func (s *catalogService) PostProduct(ctx context.Context, name, description string, price float64) (*domain.Product, error) {
	p := &domain.Product{
		Name:        name,
		Description: description,
		Price:       price,
		ID:          ksuid.New().String(),
	}
	if err := s.repository.PutProduct(ctx, *p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *catalogService) GetProduct(ctx context.Context, id string) (*domain.Product, error) {
	return s.repository.GetProductByID(ctx, id)
}

func (s *catalogService) GetProducts(ctx context.Context, skip uint64, take uint64) ([]domain.Product, error) {
	if take > 100 || (skip == 0 && take == 0) {
		take = 100
	}
	return s.repository.ListProducts(ctx, skip, take)
}

func (s *catalogService) GetProductsByIDs(ctx context.Context, ids []string) ([]domain.Product, error) {
	return s.repository.ListProductsWithIDs(ctx, ids)
}

func (s *catalogService) SearchProducts(ctx context.Context, query string, skip uint64, take uint64) ([]domain.Product, error) {
	if take > 100 || (skip == 0 && take == 0) {
		take = 100
	}
	return s.repository.SearchProducts(ctx, query, skip, take)
}
