//go:generate protoc ./account.proto --go_out=plugins=grpc:./pb
package handler

import (
	"context"
	"fmt"
	"net"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/pb"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

type grpcServer struct {
	pb.UnimplementedAccountServiceServer
	service service.Service
}

func ListenGRPC(s service.Service, port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	serv := grpc.NewServer()
	pb.RegisterAccountServiceServer(serv, &grpcServer{service: s})
	reflection.Register(serv)
	return serv.Serve(lis)
}

func (s *grpcServer) PostAccount(ctx context.Context, r *pb.PostAccountRequest) (*pb.PostAccountResponse, error) {
	tenantID, ok := ctx.Value("tenantID").(string)
	if !ok {
		return nil, fmt.Errorf("tenantID is not present in context")
	}
	a, err := s.service.PostAccount(ctx, tenantID, r.Name)
	if err != nil {
		return nil, err
	}
	return &pb.PostAccountResponse{Account: &pb.Account{
		Id:   a.ID,
		Name: a.Name,
	}}, nil
}

func (s *grpcServer) GetAccount(ctx context.Context, r *pb.GetAccountRequest) (*pb.GetAccountResponse, error) {
	tenantID, ok := ctx.Value("tenantID").(string)
	if !ok {
		return nil, fmt.Errorf("tenantID is not present in context")
	}
	a, err := s.service.GetAccount(ctx, tenantID, r.Id)
	if err != nil {
		return nil, err
	}
	return &pb.GetAccountResponse{
		Account: &pb.Account{
			Id:   a.ID,
			Name: a.Name,
		},
	}, nil
}


func (s *grpcServer) GetAccounts(ctx context.Context, r *pb.GetAccountsRequest) (*pb.GetAccountsResponse, error) {
	tenantID, ok := ctx.Value("tenantID").(string)
	if !ok {
		return nil, fmt.Errorf("tenantID is not present in context")
	}
	res, err := s.service.GetAccounts(ctx, tenantID, r.Skip, r.Take)
	if err != nil {
		return nil, err
	}
	accounts := []*pb.Account{}
	for _, p := range res {
		accounts = append(
			accounts,
			&pb.Account{
				Id:   p.ID,
				Name: p.Name,
			},
		)
	}
	return &pb.GetAccountsResponse{Accounts: accounts}, nil
}
