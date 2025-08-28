package service

import (
	"context"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/domain"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/repository"
	"github.com/segmentio/ksuid"
)

type Service interface {

	PostOrder(ctx context.Context, accountID string, products []domain.OrderedProduct) (*domain.Order, error)
	GetOrdersForAccount(ctx context.Context, accountID string) ([]domain.Order, error)
}

type orderService struct {
	repository repository.Repository
}

type orderService struct {
	repository repository.Repository // ✅ use the interface
}

// NewService accepts any Repository implementation (e.g., CassandraRepo)
func NewService(r repository.Repository) Service {
	return &orderService{r}
}

func (s orderService) PostOrder(
	ctx context.Context, accountID string,
	products []domain.OrderedProduct,
) (*domain.Order, error) {
	o := &domain.Order{
		ID:        ksuid.New().String(),
		CreatedAt: time.Now().UTC(),
		AccountID: accountID,
		Products:  products,
	}
	// Calculate total price
	o.TotalPrice = 0.0
	for _, p := range products {
		o.TotalPrice += p.Price * float64(p.Quantity)
	}
	if err := s.repository.PutOrder(ctx, *o); err != nil {
		return nil, err
	}
	return o, nil
}

func (s orderService) GetOrdersForAccount(ctx context.Context, accountID string) ([]domain.Order, error) {
	return s.repository.GetOrdersForAccount(ctx, accountID)
}
