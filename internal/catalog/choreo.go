package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/bus"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
)

// Choreographed saga: catalog's part.
//
//	order.placed      → reserve stock  → stock.reserved | stock.rejected
//	payment.captured  → commit stock   → stock.committed | stock.commit_failed (reservation expired: released)
//	payment.failed    → release stock  → stock.released
//	payment.voided    → release stock  → stock.released
//
// Each handler records the event in the inbox, changes stock and writes the outgoing event in
// ONE transaction, so a redelivered event is skipped and nothing is published that didn't happen.
const (
	Topic         = "catalog.events"
	ConsumerGroup = "catalog-saga"
	inboxTable    = "catalog.inbox"
)

// ConsumeTopics are the topics catalog reacts to.
var ConsumeTopics = []string{"order.events", "payment.events"}

type placedEvent struct {
	OrderID       string `json:"order_id"`
	CustomerID    string `json:"customer_id"`
	PaymentMethod string `json:"payment_method"`
	Total         int64  `json:"total"`
	Currency      string `json:"currency"`
	Lines         []struct {
		ProductID string `json:"product_id"`
		Qty       int    `json:"qty"`
	} `json:"lines"`
}

type paymentEvent struct {
	OrderID string `json:"order_id"`
	Reason  string `json:"reason"`
}

// HandleEvent is catalog's event consumer.
func (s Store) HandleEvent(ctx context.Context, m bus.Message) error {
	switch bus.Type(m) {
	case "order.placed":
		return s.onPlaced(ctx, m)
	case "payment.captured":
		return s.onCaptured(ctx, m)
	case "payment.failed", "payment.voided":
		return s.onPaymentFailed(ctx, m)
	}
	return nil
}

func (s Store) onPlaced(ctx context.Context, m bus.Message) error {
	var p placedEvent
	if err := bus.Decode(m, &p); err != nil || p.OrderID == "" {
		return nil // malformed: nothing sensible to do; retrying won't help
	}
	lines := make([]Line, 0, len(p.Lines))
	for _, l := range p.Lines {
		lines = append(lines, Line{ProductID: l.ProductID, Qty: l.Qty})
	}
	skipped := false
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		first, err := outbox.FirstDelivery(ctx, tx, inboxTable, ConsumerGroup, m.EventID)
		if err != nil || !first {
			skipped = !first
			return err
		}
		if _, err := s.ReserveTx(ctx, tx, p.OrderID, lines); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE catalog.reservations SET via_event = true WHERE order_id = $1`, p.OrderID); err != nil {
			return err
		}
		return enqueue(ctx, tx, p.OrderID, "stock.reserved", map[string]any{"order_id": p.OrderID, "customer_id": p.CustomerID,
			"payment_method": p.PaymentMethod, "total": p.Total, "currency": p.Currency})
	})
	if skipped {
		return nil
	}
	err = reserveErr(err)
	if errors.Is(err, ErrInsufficientStock) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) || errors.Is(err, errReplay) {
		lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "reserve stock", Detail: err.Error()})
		return s.reply(ctx, m, p.OrderID, "stock.rejected", "out_of_stock: "+err.Error(), nil)
	}
	if err == nil {
		lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "reserve stock", OK: true})
		lab.Emit(ctx, lab.Event{Kind: lab.KindOutbox, To: lab.DB(lab.Catalog), Label: "stock.reserved", OK: true})
	}
	return err
}

var errGone = errors.New("reservation gone")

func (s Store) onCaptured(ctx context.Context, m bus.Message) error {
	var p paymentEvent
	if err := bus.Decode(m, &p); err != nil || p.OrderID == "" {
		return nil
	}
	skipped := false
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		first, err := outbox.FirstDelivery(ctx, tx, inboxTable, ConsumerGroup, m.EventID)
		if err != nil || !first {
			skipped = !first
			return err
		}
		if mine, err := viaEvent(ctx, tx, p.OrderID); err != nil || !mine {
			skipped = true // an orchestrated order: the order service commits it
			return err
		}
		if err := CommitTx(ctx, tx, p.OrderID); errors.Is(err, ErrReservationGone) {
			return errGone
		} else if err != nil {
			return err
		}
		return enqueue(ctx, tx, p.OrderID, "stock.committed", map[string]any{"order_id": p.OrderID})
	})
	if skipped {
		return nil
	}
	if errors.Is(err, errGone) {
		// Paid, but the stock hold expired first: give the units back and ask for a refund.
		lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "commit stock", Detail: "reservation expired"})
		return s.reply(ctx, m, p.OrderID, "stock.commit_failed", "stock_reservation_expired", ReleaseTx)
	}
	if err == nil {
		lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "commit stock", OK: true})
		lab.Emit(ctx, lab.Event{Kind: lab.KindOutbox, To: lab.DB(lab.Catalog), Label: "stock.committed", OK: true})
	}
	return err
}

func (s Store) onPaymentFailed(ctx context.Context, m bus.Message) error {
	var p paymentEvent
	if err := bus.Decode(m, &p); err != nil || p.OrderID == "" {
		return nil
	}
	reason := "payment_declined"
	if bus.Type(m) == "payment.voided" {
		reason = "capture_failed"
	}
	if p.Reason != "" {
		reason += ": " + p.Reason
	}
	return s.reply(ctx, m, p.OrderID, "stock.released", reason, ReleaseTx)
}

// reply records the event as handled, optionally runs effect, and publishes typ — in one transaction.
func (s Store) reply(ctx context.Context, m bus.Message, orderID, typ, reason string, effect func(context.Context, pgx.Tx, string) error) error {
	published := false
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		published = false
		first, err := outbox.FirstDelivery(ctx, tx, inboxTable, ConsumerGroup, m.EventID)
		if err != nil || !first {
			return err
		}
		if effect != nil {
			if mine, err := viaEvent(ctx, tx, orderID); err != nil {
				return err
			} else if mine {
				if err := effect(ctx, tx, orderID); err != nil {
					return err
				}
			} else {
				return nil // not a choreographed order: the order service handles it
			}
		}
		published = true
		return enqueue(ctx, tx, orderID, typ, map[string]any{"order_id": orderID, "reason": reason})
	})
	if err == nil && published {
		if effect != nil {
			lab.Emit(ctx, lab.Event{Kind: lab.KindDB, To: lab.DB(lab.Catalog), Label: "release stock", OK: true})
		}
		lab.Emit(ctx, lab.Event{Kind: lab.KindOutbox, To: lab.DB(lab.Catalog), Label: typ, OK: true})
	}
	return err
}

func viaEvent(ctx context.Context, tx pgx.Tx, orderID string) (bool, error) {
	var mine *bool
	err := tx.QueryRow(ctx, `SELECT bool_or(via_event) FROM catalog.reservations WHERE order_id = $1`, orderID).Scan(&mine)
	return mine != nil && *mine, err
}

func enqueue(ctx context.Context, tx pgx.Tx, orderID, typ string, payload map[string]any) error {
	if err := outbox.Enqueue(ctx, tx, "catalog.outbox", outbox.Event{Topic: Topic, Key: orderID, Type: typ, Payload: payload}); err != nil {
		return fmt.Errorf("catalog: enqueue %s: %w", typ, err)
	}
	return nil
}
