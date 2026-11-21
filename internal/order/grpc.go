package order

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/common/v1"
	orderv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/order/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
)

// Server is the gRPC face of Engine. Customers place and see their own orders; admins see any.
type Server struct {
	orderv1.UnimplementedOrderServiceServer
	Engine Engine
	Wait   time.Duration // how long PlaceOrder waits for the saga (default 3 s)
}

func (s Server) PlaceOrder(ctx context.Context, r *orderv1.PlaceOrderRequest) (*orderv1.Order, error) {
	p, err := auth.Require(ctx, auth.RoleCustomer, auth.RoleAdmin)
	if err != nil {
		return nil, grpcx.AuthError(err)
	}
	if len(r.Lines) > 50 {
		return nil, status.Error(codes.InvalidArgument, "at most 50 lines")
	}
	in := PlaceInput{RequestID: r.RequestId, CustomerID: p.Subject, PaymentMethod: r.PaymentMethod}
	for _, l := range r.Lines {
		in.Lines = append(in.Lines, Line{ProductID: l.ProductId, Qty: int(l.Qty)})
	}
	wait := s.Wait
	if wait == 0 {
		wait = 3 * time.Second
	}
	o, err := s.Engine.Place(ctx, in, wait)
	if err != nil {
		return nil, toStatus(err)
	}
	if o.CustomerID != p.Subject { // request id reused by another customer
		return nil, status.Error(codes.AlreadyExists, "request id already used")
	}
	return s.withHistory(ctx, o)
}

func (s Server) GetOrder(ctx context.Context, r *orderv1.GetOrderRequest) (*orderv1.Order, error) {
	p, err := auth.Require(ctx, auth.RoleCustomer, auth.RoleAdmin)
	if err != nil {
		return nil, grpcx.AuthError(err)
	}
	o, err := s.Engine.Get(ctx, r.Id)
	if err != nil {
		return nil, toStatus(err)
	}
	if p.Role != auth.RoleAdmin && o.CustomerID != p.Subject {
		return nil, toStatus(ErrNotFound) // don't reveal that it exists
	}
	return s.withHistory(ctx, o)
}

func (s Server) ListOrders(ctx context.Context, r *orderv1.ListOrdersRequest) (*orderv1.ListOrdersResponse, error) {
	p, err := auth.Require(ctx, auth.RoleCustomer, auth.RoleAdmin)
	if err != nil {
		return nil, grpcx.AuthError(err)
	}
	os, err := s.Engine.List(ctx, p.Subject, int(r.Limit))
	if err != nil {
		return nil, toStatus(err)
	}
	out := &orderv1.ListOrdersResponse{}
	for _, o := range os {
		po, err := s.withHistory(ctx, o)
		if err != nil {
			return nil, err
		}
		out.Orders = append(out.Orders, po)
	}
	return out, nil
}

func (s Server) withHistory(ctx context.Context, o Order) (*orderv1.Order, error) {
	h, err := s.Engine.History(ctx, o.ID)
	if err != nil {
		return nil, toStatus(err)
	}
	return toProto(o, h), nil
}

func toProto(o Order, history []string) *orderv1.Order {
	out := &orderv1.Order{Id: o.ID, RequestId: o.RequestID, CustomerId: o.CustomerID, Status: o.Status, Reason: o.Reason,
		Total: &commonv1.Money{AmountMinor: o.Total.Amount, Currency: o.Total.Currency}, History: history, CreatedAtUnix: o.CreatedAt.Unix()}
	for _, l := range o.Lines {
		out.Lines = append(out.Lines, &orderv1.OrderLine{ProductId: l.ProductID, Sku: l.SKU, Name: l.Name, Qty: int32(l.Qty),
			UnitPrice: &commonv1.Money{AmountMinor: l.UnitPrice.Amount, Currency: l.UnitPrice.Currency}})
	}
	return out
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.FromContextError(err).Err()
	}
	if _, ok := status.FromError(err); ok {
		return status.Error(codes.Unavailable, "a dependency is unavailable; try again")
	}
	return status.Error(codes.Internal, "internal error")
}
