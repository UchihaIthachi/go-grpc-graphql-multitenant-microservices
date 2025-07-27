package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/domain"
)

var (
	ErrInvalidParameter = errors.New("invalid parameter")
)

type mutationResolver struct {
	server *Server
}

func (r *mutationResolver) CreateAccount(ctx context.Context, in AccountInput) (*Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tenantID := ctx.Value("tenant_id").(string)

	a, err := r.server.accountClient.PostAccount(ctx, in.Name, tenantID)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	return &Account{
		ID:   a.ID,
		Name: a.Name,
	}, nil
}

func (r *mutationResolver) CreateProduct(ctx context.Context, in ProductInput) (*Product, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	p, err := r.server.catalogClient.PostProduct(ctx, in.Name, in.Description, in.Price)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	return &Product{
		ID:          p.ID,
		Name:        p.Name,
		Description: p.Description,
		Price:       p.Price,
	}, nil
}

func (r *mutationResolver) CreateOrder(ctx context.Context, in OrderInput) (*Order, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	tenantID := ctx.Value("tenant_id").(string)

	var products []domain.OrderedProduct
	for _, p := range in.Products {
		if p.Quantity <= 0 {
			return nil, ErrInvalidParameter
		}
		products = append(products, domain.OrderedProduct{
			ID:       p.ID,
			Quantity: uint32(p.Quantity),
		})
	}

	o, err := r.server.orderClient.PostOrder(ctx, tenantID, in.AccountID, products)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	var orderedProducts []*OrderedProduct
	for _, p := range o.Products {
		orderedProducts = append(orderedProducts, &OrderedProduct{
			ID:          p.ID,
			Name:        p.Name,
			Description: p.Description,
			Price:       p.Price,
			Quantity:    int(p.Quantity),
		})
	}

	return &Order{
		ID:         o.ID,
		CreatedAt:  o.CreatedAt,
		TotalPrice: o.TotalPrice,
		Products:   orderedProducts,
	}, nil
}