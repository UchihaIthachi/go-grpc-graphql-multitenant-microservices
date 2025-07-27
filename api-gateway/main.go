//go:generate go run github.com/99designs/gqlgen
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/99designs/gqlgen/handler"
	"github.com/kelseyhightower/envconfig"
)

type AppConfig struct {
	AccountURL string `envconfig:"ACCOUNT_SERVICE_URL"`
	CatalogURL string `envconfig:"CATALOG_SERVICE_URL"`
	OrderURL   string `envconfig:"ORDER_SERVICE_URL"`
}

func main() {
	var cfg AppConfig
	err := envconfig.Process("", &cfg)
	if err != nil {
		log.Fatal("Failed to process envconfig: ", err)
	}

	// Debug log to verify the actual values from .env
	log.Println("[CONFIG] ACCOUNT_SERVICE_URL =", cfg.AccountURL)
	log.Println("[CONFIG] CATALOG_SERVICE_URL =", cfg.CatalogURL)
	log.Println("[CONFIG] ORDER_SERVICE_URL   =", cfg.OrderURL)

	s, err := NewGraphQLServer(cfg.AccountURL, cfg.CatalogURL, cfg.OrderURL)
	if err != nil {
		log.Fatal("Failed to initialize GraphQL server: ", err)
	}

	// http.Handle("/graphql", authMiddleware(handler.GraphQL(s.ToExecutableSchema())))
	http.Handle("/graphql", handler.GraphQL(s.ToExecutableSchema()))
	http.Handle("/playground", handler.Playground("GraphQL Playground", "/graphql"))

	log.Println("🚀 API Gateway is running at http://localhost:8080/playground")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("x-tenant-id")
		if tenantID == "" {
			http.Error(w, "x-tenant-id header is missing", http.StatusBadRequest)
			return
		}
		ctx := context.WithValue(r.Context(), "tenant_id", tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
