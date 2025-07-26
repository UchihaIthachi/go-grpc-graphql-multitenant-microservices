FROM golang:1.22-alpine3.18 AS build
RUN apk --no-cache add gcc g++ make ca-certificates
WORKDIR /go/src/github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices
COPY go.mod go.sum ./
COPY vendor vendor
COPY account-service account-service
RUN GO111MODULE=on go build  -o /go/bin/app ./account-service/cmd/account

FROM alpine:3.11
WORKDIR /usr/bin
COPY --from=build /go/bin .
EXPOSE 8080
CMD ["app"]
