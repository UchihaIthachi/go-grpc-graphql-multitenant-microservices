package catalog

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	catalogv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/catalog/v1"
	commonv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/common/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

// Error reasons for FAILED_PRECONDITION.
const (
	ReasonOutOfStock      = "OUT_OF_STOCK"
	ReasonReservationGone = "RESERVATION_GONE"
)

// Server is the gRPC face of Store. Admins manage products, anyone signed in can read them,
// and only services (the order saga) may reserve, commit or release stock.
type Server struct {
	catalogv1.UnimplementedCatalogServiceServer
	Store Store
}

func (s Server) CreateProduct(ctx context.Context, r *catalogv1.CreateProductRequest) (*catalogv1.Product, error) {
	if _, err := auth.Require(ctx, auth.RoleAdmin); err != nil {
		return nil, grpcx.AuthError(err)
	}
	price, err := money.New(r.GetPrice().GetAmountMinor(), r.GetPrice().GetCurrency())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p, err := s.Store.CreateProduct(ctx, r.Sku, r.Name, r.Description, price, int(r.InitialStock))
	if err != nil {
		return nil, toStatus(err)
	}
	return product(p), nil
}

func (s Server) AdjustStock(ctx context.Context, r *catalogv1.AdjustStockRequest) (*catalogv1.Product, error) {
	if _, err := auth.Require(ctx, auth.RoleAdmin); err != nil {
		return nil, grpcx.AuthError(err)
	}
	p, err := s.Store.AdjustStock(ctx, r.ProductId, int(r.Delta))
	if err != nil {
		return nil, toStatus(err)
	}
	return product(p), nil
}

func (s Server) GetProducts(ctx context.Context, r *catalogv1.GetProductsRequest) (*catalogv1.GetProductsResponse, error) {
	if _, err := auth.Require(ctx, auth.RoleAdmin, auth.RoleCustomer, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	if len(r.Ids) > 100 {
		return nil, status.Error(codes.InvalidArgument, "at most 100 ids")
	}
	ps, err := s.Store.Products(ctx, r.Ids)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &catalogv1.GetProductsResponse{}
	for _, p := range ps {
		out.Products = append(out.Products, product(p))
	}
	return out, nil
}

func (s Server) SearchProducts(ctx context.Context, r *catalogv1.SearchProductsRequest) (*catalogv1.SearchProductsResponse, error) {
	if _, err := auth.Require(ctx, auth.RoleAdmin, auth.RoleCustomer); err != nil {
		return nil, grpcx.AuthError(err)
	}
	ps, next, err := s.Store.Search(ctx, r.Query, int(r.Limit), r.Cursor)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &catalogv1.SearchProductsResponse{NextCursor: next}
	for _, p := range ps {
		out.Products = append(out.Products, product(p))
	}
	return out, nil
}

func (s Server) ReserveStock(ctx context.Context, r *catalogv1.ReserveStockRequest) (*catalogv1.ReserveStockResponse, error) {
	if _, err := auth.Require(ctx, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	lines := make([]Line, 0, len(r.Lines))
	for _, l := range r.Lines {
		lines = append(lines, Line{ProductID: l.ProductId, Qty: int(l.Qty)})
	}
	rs, err := s.Store.Reserve(ctx, r.OrderId, lines)
	lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "reserve stock", OK: err == nil, Detail: errText(err)})
	if err != nil {
		return nil, toStatus(err)
	}
	out := &catalogv1.ReserveStockResponse{}
	for _, x := range rs {
		out.Reservations = append(out.Reservations, &catalogv1.Reservation{ProductId: x.ProductID, Qty: int32(x.Qty), Status: x.Status, ExpiresAtUnix: x.ExpiresAt.Unix()})
	}
	return out, nil
}

func (s Server) CommitReservation(ctx context.Context, r *catalogv1.OrderRef) (*catalogv1.Empty, error) {
	if _, err := auth.Require(ctx, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	err := s.Store.Commit(ctx, r.OrderId)
	lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "commit stock", OK: err == nil, Detail: errText(err)})
	return &catalogv1.Empty{}, toStatus(err)
}

func (s Server) ReleaseReservation(ctx context.Context, r *catalogv1.OrderRef) (*catalogv1.Empty, error) {
	if _, err := auth.Require(ctx, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	err := s.Store.Release(ctx, r.OrderId)
	lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "release stock", OK: err == nil, Detail: errText(err)})
	return &catalogv1.Empty{}, toStatus(err)
}

func product(p Product) *catalogv1.Product {
	return &catalogv1.Product{Id: p.ID, Sku: p.SKU, Name: p.Name, Description: p.Description,
		Price:  &commonv1.Money{AmountMinor: p.Price.Amount, Currency: p.Price.Currency},
		OnHand: int32(p.OnHand), Reserved: int32(p.Reserved), Available: int32(p.Available())}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrInsufficientStock):
		return grpcx.Error(codes.FailedPrecondition, ReasonOutOfStock, "catalog", err.Error())
	case errors.Is(err, ErrReservationGone):
		return grpcx.Error(codes.FailedPrecondition, ReasonReservationGone, "catalog", err.Error())
	case errors.Is(err, ErrInvalidState):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Unavailable, "catalog: temporary failure") // DB trouble: the caller retries
}
