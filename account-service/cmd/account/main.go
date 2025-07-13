package main

import (
	"log"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/handler"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/repository"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/account-service/service"
	"github.com/kelseyhightower/envconfig"
	"github.com/tinrab/retry"
)

type Config struct {
	DatabaseURL string `envconfig:"DATABASE_URL"`
}

func main() {
	var cfg Config
	err := envconfig.Process("", &cfg)
	if err != nil {
		log.Fatal(err)
	}

	var r repository.Repository
	retry.ForeverSleep(2*time.Second, func(_ int) (err error) {
		r, err = repository.NewPostgresRepository(cfg.DatabaseURL)
		if err != nil {
			log.Println(err)
		}
		return
	})
	defer r.Close()

	log.Println("Listening on port 8080...")
	s := service.NewService(r)
	log.Fatal(handler.ListenGRPC(s, 8080))
}
