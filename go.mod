module github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices

go 1.24.0

toolchain go1.24.3

require (
	github.com/99designs/gqlgen v0.10.2
	github.com/gocql/gocql v1.1.0
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/lib/pq v1.3.0
	github.com/segmentio/ksuid v1.0.2
	github.com/tinrab/retry v1.0.0
	github.com/vektah/gqlparser v1.2.1
	google.golang.org/grpc v1.75.0
	google.golang.org/protobuf v1.36.8
	gopkg.in/olivere/elastic.v5 v5.0.84
)

require (
	github.com/agnivade/levenshtein v1.0.1 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/golang/snappy v0.0.3 // indirect
	github.com/gorilla/websocket v1.2.0 // indirect
	github.com/hailocab/go-hostpool v0.0.0-20160125115350-e80d13ce29ed // indirect
	github.com/hashicorp/golang-lru v0.5.0 // indirect
	github.com/mailru/easyjson v0.0.0-20180730094502-03f2033d19d5 // indirect
	github.com/pkg/errors v0.8.1 // indirect
	golang.org/x/net v0.43.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
	golang.org/x/text v0.28.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250826171959-ef028d996bc1 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
)

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/pb => ./account-service/pb

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/catalog-service/pb => ./catalog-service/pb

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/pb => ./order-service/pb

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/repository => ./account-service/repository

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/service => ./account-service/service
replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/catalog-service/repository => ./catalog-service/repository
replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/catalog-service/service => ./catalog-service/service
replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/repository => ./order-service/repository
replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/service => ./order-service/service
replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service => ./account-service
replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/catalog-service => ./catalog-service
