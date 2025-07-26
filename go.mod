module github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices

go 1.22

require (
	github.com/99designs/gqlgen v0.10.2
	github.com/gocql/gocql v1.1.0
	github.com/kelseyhightower/envconfig v1.4.0
	github.com/lib/pq v1.3.0
	github.com/segmentio/ksuid v1.0.2
	github.com/tinrab/retry v1.0.0
	github.com/vektah/gqlparser v1.2.1
	google.golang.org/grpc v1.60.0
	google.golang.org/protobuf v1.33.0
	gopkg.in/olivere/elastic.v5 v5.0.84
)

require (
	github.com/agnivade/levenshtein v1.0.1 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/golang/snappy v0.0.3 // indirect
	github.com/gorilla/websocket v1.2.0 // indirect
	github.com/hailocab/go-hostpool v0.0.0-20160125115350-e80d13ce29ed // indirect
	github.com/hashicorp/golang-lru v0.5.0 // indirect
	github.com/mailru/easyjson v0.0.0-20180730094502-03f2033d19d5 // indirect
	github.com/pkg/errors v0.8.1 // indirect
	golang.org/x/net v0.16.0 // indirect
	golang.org/x/sys v0.13.0 // indirect
	golang.org/x/text v0.13.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20231002182017-d307bd883b97 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
)

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/pb => ./account-service/pb

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/catalog-service/pb => ./catalog-service/pb

replace github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/order-service/pb => ./order-service/pb
