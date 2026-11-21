// Package grpcx holds what every service's gRPC server and client share: authentication,
// tenant propagation, panic recovery, access logs, and error details.
package grpcx

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// ServerOptions returns interceptors for a service: recover → log → authenticate.
// Methods in public (full names, e.g. "/account.v1.AccountService/Login") need no token.
func ServerOptions(v auth.Verifier, log *slog.Logger, public ...string) []grpc.ServerOption {
	pub := map[string]bool{}
	for _, m := range public {
		pub[m] = true
	}
	return []grpc.ServerOption{grpc.ChainUnaryInterceptor(recoverer(log), logger(log), labServer(), authenticate(v, pub))}
}

// labServer applies the Pattern Lab's "pause this service" switch and picks up the order id
// the caller attached, so events emitted while serving are linked to that order.
func labServer() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if !lab.Enabled() || strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			return h(ctx, req)
		}
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get(orderHeader); len(v) > 0 {
				ctx = lab.WithOrder(ctx, v[0])
			}
		}
		if lab.Paused() {
			lab.Emit(ctx, lab.Event{Kind: lab.KindChaos, To: lab.Service(), Label: "paused", Detail: info.FullMethod})
			return nil, status.Error(codes.Unavailable, "service paused by the Pattern Lab")
		}
		return h(ctx, req)
	}
}

const orderHeader = "x-lab-order-id"

// Traced is a client interceptor that reports each call to node `to` to the Pattern Lab and
// passes the order id on. It does nothing when the lab is off.
func Traced(to string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if !lab.Enabled() {
			return inv(ctx, method, req, reply, cc, opts...)
		}
		if id := lab.OrderFrom(ctx); id != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, orderHeader, id)
		}
		start := time.Now()
		err := inv(ctx, method, req, reply, cc, opts...)
		e := lab.Event{Kind: lab.KindRPC, To: to, Label: method[strings.LastIndex(method, "/")+1:], OK: err == nil, Ms: lab.Since(start)}
		if err != nil {
			e.Detail = status.Code(err).String() + ": " + Message(err)
		}
		lab.Emit(ctx, e)
		return err
	}
}

func recoverer(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return h(ctx, req)
	}
}

func logger(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := h(ctx, req)
		code := status.Code(err)
		lvl := slog.LevelInfo
		if code == codes.Internal || code == codes.Unknown {
			lvl = slog.LevelError
		}
		attrs := []any{"method", info.FullMethod, "code", code.String(), "ms", time.Since(start).Milliseconds()}
		if p, ok := auth.From(ctx); ok {
			attrs = append(attrs, "tenant", string(p.Tenant), "sub", p.Subject)
		}
		if err != nil {
			attrs = append(attrs, "err", err.Error())
		}
		log.Log(ctx, lvl, "rpc", attrs...)
		return resp, err
	}
}

func authenticate(v auth.Verifier, public map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			return h(ctx, req)
		}
		raw := bearer(ctx)
		if raw == "" {
			if public[info.FullMethod] {
				return h(ctx, req)
			}
			return nil, status.Error(codes.Unauthenticated, "missing token")
		}
		p, err := v.Verify(raw)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		return h(auth.With(ctx, p), req)
	}
}

func bearer(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if t, ok := strings.CutPrefix(v, "Bearer "); ok {
			return t
		}
	}
	return ""
}

// ForwardUser is a client interceptor (gateway → services) that passes on the caller's own token.
func ForwardUser() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if p, ok := auth.From(ctx); ok && p.Token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+p.Token)
		}
		return inv(ctx, method, req, reply, cc, opts...)
	}
}

// AsService is a client interceptor (service → service) that presents a fresh service token for
// the tenant in ctx. Used for saga steps, which must not run with a customer's own token.
func AsService(name string, secret []byte) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		tid, err := tenancy.From(ctx)
		if err != nil {
			return status.Error(codes.Internal, "service call without a tenant")
		}
		tok, err := auth.ServiceToken(secret, name, tid)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		return inv(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok), method, req, reply, cc, opts...)
	}
}

// AuthError turns auth.Require's errors into status errors.
func AuthError(err error) error {
	if errors.Is(err, auth.ErrForbidden) {
		return status.Error(codes.PermissionDenied, "not allowed for this role")
	}
	return status.Error(codes.Unauthenticated, "missing or invalid token")
}

// Error builds a status error with a machine-readable reason, e.g.
// Error(codes.FailedPrecondition, "OUT_OF_STOCK", "catalog", err.Error()).
func Error(c codes.Code, reason, domain, msg string) error {
	st := status.New(c, msg)
	if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain}); err == nil {
		st = d
	}
	return st.Err()
}

// Reason returns the ErrorInfo reason of a status error, or "".
func Reason(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.Reason
		}
	}
	return ""
}

// Message returns a status error's message without the "rpc error: code = ..." prefix.
func Message(err error) string {
	if st, ok := status.FromError(err); ok {
		return st.Message()
	}
	return err.Error()
}
