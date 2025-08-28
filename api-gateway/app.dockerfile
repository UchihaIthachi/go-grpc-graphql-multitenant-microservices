FROM golang:1.24-alpine AS build
RUN apk --no-cache add gcc g++ make ca-certificates
WORKDIR /go/src/github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices
COPY go.mod go.sum ./
RUN go mod vendor
COPY vendor vendor
COPY account-service account-service
COPY catalog-service catalog-service
COPY order-service order-service
COPY api-gateway api-gateway
RUN GO111MODULE=on go build -mod vendor -o /go/bin/app ./api-gateway

FROM alpine:3.11
WORKDIR /usr/bin
COPY --from=build /go/bin .
EXPOSE 8080
CMD ["app"]
