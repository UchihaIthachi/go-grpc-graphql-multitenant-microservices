package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/domain"
	"github.com/gocql/gocql"
)

type cassandraRepository struct {
	session *gocql.Session
}

func NewCassandraRepository(url string) (Repository, error) {
	cluster := gocql.NewCluster(url)
	cluster.Keyspace = "orders"
	cluster.Consistency = gocql.Quorum
	cluster.Timeout = 20 * time.Second
	session, err := cluster.CreateSession()
	if err != nil {
		return nil, err
	}
	return &cassandraRepository{session}, nil
}

func (r *cassandraRepository) Close() {
	r.session.Close()
}

func (r *cassandraRepository) PutOrder(ctx context.Context, o domain.Order) error {
	// Simple query
	if err := r.session.Query(
		`INSERT INTO orders (id, created_at, account_id, total_price, products) VALUES (?, ?, ?, ?, ?)`,
		o.ID, o.CreatedAt, o.AccountID, o.TotalPrice, o.Products,
	).WithContext(ctx).Exec(); err != nil {
		return fmt.Errorf("failed to put order: %w", err)
	}
	return nil
}

func (r *cassandraRepository) GetOrdersForAccount(ctx context.Context, accountID string) ([]domain.Order, error) {
	var orders []domain.Order
	iter := r.session.Query(
		`SELECT id, created_at, account_id, total_price, products FROM orders WHERE account_id = ?`,
		accountID,
	).WithContext(ctx).Iter()

	var order domain.Order
	for iter.Scan(&order.ID, &order.CreatedAt, &order.AccountID, &order.TotalPrice, &order.Products) {
		orders = append(orders, order)
	}

	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("failed to get orders for account: %w", err)
	}

	return orders, nil
}
