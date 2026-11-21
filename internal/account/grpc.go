package account

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/account/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
)

// PublicMethods need no token.
var PublicMethods = []string{
	accountv1.AccountService_SignUp_FullMethodName,
	accountv1.AccountService_Register_FullMethodName,
	accountv1.AccountService_Login_FullMethodName,
}

// Server is the gRPC face of Service.
type Server struct {
	accountv1.UnimplementedAccountServiceServer
	Svc Service
}

func (s Server) SignUp(ctx context.Context, r *accountv1.SignUpRequest) (*accountv1.AuthResponse, error) {
	return session(s.Svc.SignUp(ctx, r.TenantName, r.TenantSlug, r.Email, r.Password))
}

func (s Server) Register(ctx context.Context, r *accountv1.RegisterRequest) (*accountv1.AuthResponse, error) {
	return session(s.Svc.Register(ctx, r.TenantSlug, r.Email, r.Password))
}

func (s Server) Login(ctx context.Context, r *accountv1.LoginRequest) (*accountv1.AuthResponse, error) {
	return session(s.Svc.Login(ctx, r.TenantSlug, r.Email, r.Password))
}

func (s Server) Me(ctx context.Context, _ *accountv1.MeRequest) (*accountv1.User, error) {
	p, err := auth.Require(ctx, auth.RoleAdmin, auth.RoleCustomer)
	if err != nil {
		return nil, grpcx.AuthError(err)
	}
	u, err := s.Svc.Get(ctx, p.Subject)
	if err != nil {
		return nil, toStatus(err)
	}
	return user(u), nil
}

func session(se Session, err error) (*accountv1.AuthResponse, error) {
	if err != nil {
		return nil, toStatus(err)
	}
	return &accountv1.AuthResponse{Token: se.Token, ExpiresAtUnix: se.ExpiresAt.Unix(), User: user(se.User)}, nil
}

func user(u User) *accountv1.User {
	return &accountv1.User{Id: u.ID, TenantId: string(u.TenantID), TenantSlug: u.TenantSlug, Email: u.Email, Role: u.Role}
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrTaken):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrBadLogin), errors.Is(err, ErrNotFound):
		return status.Error(codes.Unauthenticated, ErrBadLogin.Error())
	}
	return status.Error(codes.Internal, "internal error")
}
