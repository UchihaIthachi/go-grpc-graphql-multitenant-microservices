package domain

import "time"

// Order is a struct that represents an order in the domain.
type Order struct {
	ID         string
	CreatedAt  time.Time
	TotalPrice float64
	AccountID  string
	Products   []OrderedProduct
}

// OrderedProduct is a struct that represents a product in an order.
type OrderedProduct struct {
	ID          string
	Name        string
	Description string
	Price       float64
	Quantity    uint32
}
