// Package local connects the order saga to the catalog and payment packages in-process.
// It is what the integration tests use, and it holds the one place where catalog and payment
// errors are translated into the saga's outcomes. The gRPC clients (internal/order/grpcdeps)
// make the same translation from status codes.
package local

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/catalog"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

// Catalog adapts catalog.Store to order.Catalog.
type Catalog struct{ Store catalog.Store }

func (c Catalog) Products(ctx context.Context, ids []string) ([]order.Product, error) {
	ps, err := c.Store.Products(ctx, ids)
	if errors.Is(err, catalog.ErrInvalid) {
		return nil, fmt.Errorf("%w: %v", order.ErrInvalid, err)
	}
	if err != nil {
		return nil, err
	}
	out := make([]order.Product, 0, len(ps))
	for _, p := range ps {
		out = append(out, order.Product{ID: p.ID, SKU: p.SKU, Name: p.Name, Price: p.Price})
	}
	return out, nil
}

func (c Catalog) Reserve(ctx context.Context, orderID string, lines []order.Line) error {
	ls := make([]catalog.Line, 0, len(lines))
	for _, l := range lines {
		ls = append(ls, catalog.Line{ProductID: l.ProductID, Qty: l.Qty})
	}
	_, err := c.Store.Reserve(ctx, orderID, ls)
	return CatalogErr(err)
}

func (c Catalog) Commit(ctx context.Context, orderID string) error {
	return CatalogErr(c.Store.Commit(ctx, orderID))
}

func (c Catalog) Release(ctx context.Context, orderID string) error {
	return CatalogErr(c.Store.Release(ctx, orderID))
}

// CatalogErr maps catalog errors to saga outcomes.
func CatalogErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, catalog.ErrInsufficientStock), errors.Is(err, catalog.ErrNotFound), errors.Is(err, catalog.ErrInvalid):
		return fmt.Errorf("%w: %v", order.ErrOutOfStock, err)
	case errors.Is(err, catalog.ErrReservationGone):
		// from Reserve: a lost-response reservation that has since expired; from Commit: too late
		return fmt.Errorf("%w: %v", order.ErrReservationGone, err)
	}
	return err
}

// Payments adapts payment.Service to order.Payments.
type Payments struct{ Service payment.Service }

func (p Payments) Authorize(ctx context.Context, orderID, customerID string, amount money.Money, pm string) error {
	_, err := p.Service.Authorize(ctx, payment.AuthorizeInput{OrderID: orderID, CustomerID: customerID, Amount: amount, PaymentMethod: pm})
	return PaymentErr(err)
}

func (p Payments) Capture(ctx context.Context, orderID string) error {
	_, err := p.Service.Capture(ctx, orderID)
	return PaymentErr(err)
}

func (p Payments) Void(ctx context.Context, orderID string) error {
	_, err := p.Service.Void(ctx, orderID)
	if errors.Is(err, payment.ErrNotFound) {
		return nil // authorization was never attempted: nothing to void
	}
	if errors.Is(err, payment.ErrInvalidState) {
		return fmt.Errorf("%w: %v", order.ErrWrongState, err) // captured after all
	}
	return PaymentErr(err)
}

func (p Payments) Refund(ctx context.Context, orderID string) error {
	_, err := p.Service.Refund(ctx, orderID)
	return PaymentErr(err)
}

// PaymentErr maps payment errors to saga outcomes.
func PaymentErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, payment.ErrDeclined):
		// keep just the processor's reason ("insufficient_funds") for the order's reason
		return fmt.Errorf("%w: %s", order.ErrDeclined, strings.TrimPrefix(err.Error(), payment.ErrDeclined.Error()+": "))
	case errors.Is(err, payment.ErrInvalid), errors.Is(err, payment.ErrConflict):
		return fmt.Errorf("%w: %v", order.ErrDeclined, err)
	}
	return err // ErrUnavailable and anything unexpected: retry
}
