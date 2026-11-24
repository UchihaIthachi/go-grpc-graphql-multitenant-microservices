package payment

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/bus"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

// Choreographed saga: payment's part.
//
//	stock.reserved      → authorize, then capture → payment.authorized, payment.captured
//	                      declined                → payment.failed
//	                      capture failed          → void → payment.voided
//	stock.commit_failed → refund                  → payment.refund_requested
//
// No inbox table here: every operation is already idempotent on the order id (the intent's
// state machine and the processor's idempotency keys), so a redelivered event is harmless.
const ConsumerGroup = "payment-saga"

// ConsumeTopics are the topics payment reacts to.
var ConsumeTopics = []string{"catalog.events"}

type reservedEvent struct {
	OrderID       string `json:"order_id"`
	CustomerID    string `json:"customer_id"`
	PaymentMethod string `json:"payment_method"`
	Total         int64  `json:"total"`
	Currency      string `json:"currency"`
	Reason        string `json:"reason"`
}

// HandleEvent is payment's event consumer.
func (s Service) HandleEvent(ctx context.Context, m bus.Message) error {
	var p reservedEvent
	if err := bus.Decode(m, &p); err != nil || p.OrderID == "" {
		return nil
	}
	switch bus.Type(m) {
	case "stock.reserved":
		amt, err := money.New(p.Total, p.Currency)
		if err != nil {
			return nil
		}
		_, err = s.Authorize(ctx, AuthorizeInput{OrderID: p.OrderID, CustomerID: p.CustomerID, Amount: amt, PaymentMethod: p.PaymentMethod})
		if errors.Is(err, ErrDeclined) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) {
			return nil // payment.failed is already in the outbox
		}
		if err != nil {
			return err // processor down: the consumer retries
		}
		_, err = s.Capture(ctx, p.OrderID)
		if errors.Is(err, ErrDeclined) {
			if err := s.setReason(ctx, p.OrderID, err); err != nil {
				return err
			}
			_, err = s.Void(ctx, p.OrderID)
		}
		return err
	case "stock.commit_failed":
		_, err := s.Refund(ctx, p.OrderID)
		if errors.Is(err, ErrInvalidState) || errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (s Service) setReason(ctx context.Context, orderID string, cause error) error {
	reason := cause.Error()
	if i := len(ErrDeclined.Error()) + 2; len(reason) > i {
		reason = reason[i:]
	}
	return db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment.intents SET failure_reason = $2 WHERE order_id = $1`, orderID, reason)
		return err
	})
}
