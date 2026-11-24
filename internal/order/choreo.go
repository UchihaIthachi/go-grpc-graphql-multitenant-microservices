package order

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/bus"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
)

// Saga styles.
const (
	Orchestration = "orchestration"
	Choreography  = "choreography"
)

// Choreographed saga: the order service starts it by publishing order.placed, then only
// follows along. Nobody calls anybody:
//
//	order.placed     → catalog reserves    → stock.reserved   → payment authorizes and captures
//	payment.captured → catalog commits     → stock.committed  → order CONFIRMED
//	payment.failed / payment.voided → catalog releases → stock.released → order REJECTED
//	stock.commit_failed (hold expired) → payment refunds → payment.refund_requested → order REJECTED
//
// Events from different topics can arrive in any order (stock.released may overtake
// payment.failed), so an order only ever moves FORWARD: an event for an earlier step than the
// one already reached is ignored. That rule is what makes the order's status converge.
const ConsumerGroup = "order-saga"

// ConsumeTopics are the topics the order service follows.
var ConsumeTopics = []string{"catalog.events", "payment.events"}

// rank orders the statuses; terminal ones are highest.
var rank = map[string]int{
	Pending: 0, StockReserved: 1, PaymentAuthorized: 2, PaymentCaptured: 3,
	VoidingPayment: 4, ReleasingStock: 4, RefundingPayment: 4,
	Confirmed: 10, Rejected: 10,
}

type sagaEvent struct {
	OrderID string `json:"order_id"`
	Reason  string `json:"reason"`
}

// HandleEvent moves a choreographed order according to another service's event.
func (e Engine) HandleEvent(ctx context.Context, m bus.Message) error {
	var to, reason string
	var p sagaEvent
	if err := bus.Decode(m, &p); err != nil || p.OrderID == "" {
		return nil
	}
	switch bus.Type(m) {
	case "stock.reserved":
		to = StockReserved
	case "stock.rejected", "stock.released":
		to, reason = Rejected, p.Reason
	case "payment.authorized":
		to = PaymentAuthorized
	case "payment.captured":
		to = PaymentCaptured
	case "stock.committed":
		to = Confirmed
	case "payment.failed":
		to, reason = ReleasingStock, "payment_declined: "+p.Reason
	case "payment.voided":
		to, reason = ReleasingStock, "capture_failed: "+p.Reason
	case "stock.commit_failed":
		to, reason = RefundingPayment, p.Reason
	case "payment.refund_requested":
		to = Rejected
	default:
		return nil
	}
	var o Order
	moved := false
	err := db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		moved = false
		first, err := outbox.FirstDelivery(ctx, tx, "orders.inbox", ConsumerGroup, m.EventID)
		if err != nil || !first {
			return err
		}
		var mode string
		err = tx.QueryRow(ctx, `SELECT status, reason, customer_id, total_minor, currency, saga_mode FROM orders.orders
			WHERE id = $1 FOR UPDATE`, p.OrderID).Scan(&o.Status, &o.Reason, &o.CustomerID, &o.Total.Amount, &o.Total.Currency, &mode)
		if err == pgx.ErrNoRows || db.Code(err) == "22P02" {
			return nil
		}
		if err != nil || mode != Choreography || rank[to] <= rank[o.Status] {
			return err // orchestrated, or a step we're already past: ignore
		}
		if reason == "" {
			reason = o.Reason
		}
		o.ID = p.OrderID
		if _, err := tx.Exec(ctx, `UPDATE orders.orders SET status = $2, reason = $3, updated_at = now() WHERE id = $1`, o.ID, to, reason); err != nil {
			return err
		}
		moved = true
		return emit(ctx, tx, o, o.Status, to, reason)
	})
	if err == nil && moved {
		kind := lab.KindState
		if compensating(to) {
			kind = lab.KindCompensate
		}
		lab.Emit(ctx, lab.Event{Kind: kind, To: lab.DB(lab.Order), Label: to, OK: to != Rejected, Detail: reason, Order: o.ID})
	}
	return err
}

// placedEvent publishes everything the other services need, so they never call back
// (event-carried state transfer).
func placedEvent(ctx context.Context, tx pgx.Tx, o Order, pm string, lines []OrderLine) error {
	ls := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		ls = append(ls, map[string]any{"product_id": l.ProductID, "qty": l.Qty})
	}
	return outbox.Enqueue(ctx, tx, "orders.outbox", outbox.Event{Topic: Topic, Key: o.ID, Type: "order.placed",
		Payload: map[string]any{"order_id": o.ID, "customer_id": o.CustomerID, "payment_method": pm,
			"total": o.Total.Amount, "currency": o.Total.Currency, "lines": ls}})
}

// waitFinished polls until the order is terminal or ctx ends, then returns it as it is.
func (e Engine) waitFinished(ctx context.Context, id string) (Order, error) {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		o, err := e.Get(context.WithoutCancel(ctx), id)
		if err != nil || Terminal(o.Status) {
			return o, err
		}
		select {
		case <-ctx.Done():
			return o, nil
		case <-t.C:
		}
	}
}
