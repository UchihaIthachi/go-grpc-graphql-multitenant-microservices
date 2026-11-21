// Package grpcdeps implements the order saga's Catalog and Payments over gRPC, translating
// status codes and ErrorInfo reasons into the saga's outcomes (the same translation that
// internal/order/local makes from Go errors). Unknown or transport errors stay transient.
package grpcdeps

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	catalogv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/catalog/v1"
	commonv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/common/v1"
	paymentv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/payment/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

// Catalog is order.Catalog over CatalogService.
type Catalog struct {
	C catalogv1.CatalogServiceClient
}

func (c Catalog) Products(ctx context.Context, ids []string) ([]order.Product, error) {
	res, err := c.C.GetProducts(ctx, &catalogv1.GetProductsRequest{Ids: ids})
	if code := status.Code(err); code == codes.InvalidArgument {
		return nil, fmt.Errorf("%w: %s", order.ErrInvalid, grpcx.Message(err))
	}
	if err != nil {
		return nil, err
	}
	out := make([]order.Product, 0, len(res.Products))
	for _, p := range res.Products {
		price, err := money.New(p.GetPrice().GetAmountMinor(), p.GetPrice().GetCurrency())
		if err != nil {
			return nil, err
		}
		out = append(out, order.Product{ID: p.Id, SKU: p.Sku, Name: p.Name, Price: price})
	}
	return out, nil
}

func (c Catalog) Reserve(ctx context.Context, orderID string, lines []order.Line) error {
	req := &catalogv1.ReserveStockRequest{OrderId: orderID}
	for _, l := range lines {
		req.Lines = append(req.Lines, &catalogv1.Line{ProductId: l.ProductID, Qty: int32(l.Qty)})
	}
	_, err := c.C.ReserveStock(ctx, req)
	switch {
	case err == nil:
		return nil
	case grpcx.Reason(err) == "RESERVATION_GONE":
		return fmt.Errorf("%w: %s", order.ErrReservationGone, grpcx.Message(err))
	case grpcx.Reason(err) == "OUT_OF_STOCK", status.Code(err) == codes.NotFound, status.Code(err) == codes.InvalidArgument:
		return fmt.Errorf("%w: %s", order.ErrOutOfStock, grpcx.Message(err))
	}
	return err
}

func (c Catalog) Commit(ctx context.Context, orderID string) error {
	_, err := c.C.CommitReservation(ctx, &catalogv1.OrderRef{OrderId: orderID})
	if grpcx.Reason(err) == "RESERVATION_GONE" {
		return fmt.Errorf("%w: %s", order.ErrReservationGone, grpcx.Message(err))
	}
	return err
}

func (c Catalog) Release(ctx context.Context, orderID string) error {
	_, err := c.C.ReleaseReservation(ctx, &catalogv1.OrderRef{OrderId: orderID})
	return err
}

// Payments is order.Payments over PaymentService.
type Payments struct {
	C paymentv1.PaymentServiceClient
}

func (p Payments) Authorize(ctx context.Context, orderID, customerID string, amount money.Money, pm string) error {
	_, err := p.C.Authorize(ctx, &paymentv1.AuthorizeRequest{OrderId: orderID, CustomerId: customerID, PaymentMethod: pm,
		Amount: &commonv1.Money{AmountMinor: amount.Amount, Currency: amount.Currency}})
	return declined(err)
}

func (p Payments) Capture(ctx context.Context, orderID string) error {
	_, err := p.C.Capture(ctx, &paymentv1.OrderRef{OrderId: orderID})
	return declined(err)
}

func (p Payments) Void(ctx context.Context, orderID string) error {
	_, err := p.C.Void(ctx, &paymentv1.OrderRef{OrderId: orderID})
	switch {
	case status.Code(err) == codes.NotFound:
		return nil // never authorized: nothing to void
	case grpcx.Reason(err) == "INVALID_STATE":
		return fmt.Errorf("%w: %s", order.ErrWrongState, grpcx.Message(err))
	}
	return err
}

func (p Payments) Refund(ctx context.Context, orderID string) error {
	_, err := p.C.Refund(ctx, &paymentv1.OrderRef{OrderId: orderID})
	return err
}

func declined(err error) error {
	switch {
	case err == nil:
		return nil
	case grpcx.Reason(err) == "DECLINED":
		return fmt.Errorf("%w: %s", order.ErrDeclined, strings.TrimPrefix(grpcx.Message(err), "payment: declined: "))
	case status.Code(err) == codes.InvalidArgument, status.Code(err) == codes.AlreadyExists:
		return fmt.Errorf("%w: %s", order.ErrDeclined, grpcx.Message(err))
	}
	return err
}
