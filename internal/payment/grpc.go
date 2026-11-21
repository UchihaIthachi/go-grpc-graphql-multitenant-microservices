package payment

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/common/v1"
	paymentv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/payment/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

// Error reasons for FAILED_PRECONDITION.
const (
	ReasonDeclined     = "DECLINED"
	ReasonInvalidState = "INVALID_STATE"
)

// Server is the gRPC face of Service. Money movements are saga steps: services only.
// Admins may read payments and run reconciliation.
type Server struct {
	paymentv1.UnimplementedPaymentServiceServer
	Svc Service
}

func (s Server) Authorize(ctx context.Context, r *paymentv1.AuthorizeRequest) (*paymentv1.Payment, error) {
	if _, err := auth.Require(ctx, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	amt, err := money.New(r.GetAmount().GetAmountMinor(), r.GetAmount().GetCurrency())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return intent(s.Svc.Authorize(ctx, AuthorizeInput{OrderID: r.OrderId, CustomerID: r.CustomerId, Amount: amt, PaymentMethod: r.PaymentMethod}))
}

func (s Server) Capture(ctx context.Context, r *paymentv1.OrderRef) (*paymentv1.Payment, error) {
	if _, err := auth.Require(ctx, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	return intent(s.Svc.Capture(ctx, r.OrderId))
}

func (s Server) Void(ctx context.Context, r *paymentv1.OrderRef) (*paymentv1.Payment, error) {
	if _, err := auth.Require(ctx, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	return intent(s.Svc.Void(ctx, r.OrderId))
}

func (s Server) Refund(ctx context.Context, r *paymentv1.OrderRef) (*paymentv1.Payment, error) {
	if _, err := auth.Require(ctx, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	return intent(s.Svc.Refund(ctx, r.OrderId))
}

func (s Server) GetPayment(ctx context.Context, r *paymentv1.OrderRef) (*paymentv1.Payment, error) {
	if _, err := auth.Require(ctx, auth.RoleAdmin, auth.RoleService); err != nil {
		return nil, grpcx.AuthError(err)
	}
	return intent(s.Svc.Get(ctx, r.OrderId))
}

func (s Server) Reconcile(ctx context.Context, r *paymentv1.ReconcileRequest) (*paymentv1.ReconcileResponse, error) {
	if _, err := auth.Require(ctx, auth.RoleAdmin); err != nil {
		return nil, grpcx.AuthError(err)
	}
	day, err := time.Parse(time.DateOnly, r.Day)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "day must be YYYY-MM-DD")
	}
	res, err := s.Svc.Reconcile(ctx, day)
	if err != nil {
		return nil, toStatus(err)
	}
	out := &paymentv1.ReconcileResponse{RunId: res.RunID, Matched: int32(res.Matched)}
	for _, b := range res.Breaks {
		pb := &paymentv1.ReconBreak{Kind: b.Kind, ChargeId: b.ChargeID}
		if b.Ledger != nil {
			pb.LedgerMinor = *b.Ledger
		}
		if b.PSP != nil {
			pb.PspMinor = *b.PSP
		}
		out.Breaks = append(out.Breaks, pb)
	}
	return out, nil
}

// intent converts a Service result. A declined payment is an error to the caller, as in Service.
func intent(it Intent, err error) (*paymentv1.Payment, error) {
	if err != nil {
		return nil, toStatus(err)
	}
	return &paymentv1.Payment{Id: it.ID, OrderId: it.OrderID, Status: it.Status,
		Amount:   &commonv1.Money{AmountMinor: it.Amount.Amount, Currency: it.Amount.Currency},
		FeeMinor: it.Fee, FailureReason: it.FailureReason, ChargeId: it.ChargeID}, nil
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrDeclined):
		return grpcx.Error(codes.FailedPrecondition, ReasonDeclined, "payment", err.Error())
	case errors.Is(err, ErrInvalidState):
		return grpcx.Error(codes.FailedPrecondition, ReasonInvalidState, "payment", err.Error())
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrUnavailable):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Unavailable, "payment: temporary failure")
}
