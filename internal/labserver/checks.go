package labserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Check is one invariant or health signal.
type Check struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // invariant: must never break · health: may degrade under faults
	OK     bool   `json:"ok"`
	Value  string `json:"value"`
	Detail string `json:"detail"`
}

// Checker queries each service's database directly (as the owner, across tenants). Any pool
// may be nil, and its checks are then skipped.
type Checker struct {
	Catalog, Payment, Orders *pgxpool.Pool
}

// Run evaluates every check.
func (c *Checker) Run(ctx context.Context) []Check {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var out []Check
	add := func(ch Check, err error) {
		if err != nil {
			ch.OK, ch.Value, ch.Detail = false, "error", err.Error()
		}
		out = append(out, ch)
	}
	if c.Catalog != nil {
		add(c.stock(ctx))
	}
	if c.Payment != nil {
		add(c.ledger(ctx))
	}
	if c.Orders != nil && c.Payment != nil {
		add(c.ordersMatchPayments(ctx))
	}
	if c.Orders != nil {
		add(c.stuck(ctx))
		add(c.outboxLag(ctx, c.Orders, "orders.outbox", "order"))
	}
	if c.Payment != nil {
		add(c.outboxLag(ctx, c.Payment, "payment.outbox", "payment"))
	}
	return out
}

func (c *Checker) stock(ctx context.Context) (Check, error) {
	ch := Check{ID: "no-oversell", Name: "No overselling", Kind: "invariant"}
	var bad int
	err := c.Catalog.QueryRow(ctx, `
		SELECT count(*) FROM catalog.stock s
		 WHERE s.on_hand < 0 OR s.reserved < 0 OR s.reserved > s.on_hand
		    OR s.reserved <> (SELECT coalesce(sum(qty), 0) FROM catalog.reservations r
		                       WHERE r.product_id = s.product_id AND r.status = 'reserved')`).Scan(&bad)
	ch.OK, ch.Value = bad == 0, fmt.Sprintf("%d bad products", bad)
	ch.Detail = "stock never negative, reserved ≤ on hand, reserved = sum of live reservations"
	return ch, err
}

func (c *Checker) ledger(ctx context.Context) (Check, error) {
	ch := Check{ID: "ledger-balanced", Name: "Ledger balances", Kind: "invariant",
		Detail: "every currency's postings sum to zero (double entry)"}
	rows, err := c.Payment.Query(ctx, `SELECT currency, sum(amount_minor) FROM ledger.postings GROUP BY currency HAVING sum(amount_minor) <> 0`)
	if err != nil {
		return ch, err
	}
	defer rows.Close()
	var off []string
	for rows.Next() {
		var cur string
		var sum int64
		if err := rows.Scan(&cur, &sum); err != nil {
			return ch, err
		}
		off = append(off, fmt.Sprintf("%s %+d", strings.TrimSpace(cur), sum))
	}
	ch.OK = len(off) == 0
	ch.Value = "balanced"
	if !ch.OK {
		ch.Value = strings.Join(off, ", ")
	}
	return ch, rows.Err()
}

// ordersMatchPayments: a confirmed order has a captured payment, and a rejected order holds no
// money. Orders that finished in the last 10 s are skipped (refund webhooks may be in flight).
func (c *Checker) ordersMatchPayments(ctx context.Context) (Check, error) {
	ch := Check{ID: "money-matches-orders", Name: "Money matches orders", Kind: "invariant",
		Detail: "CONFIRMED ⇒ payment captured; REJECTED ⇒ no money held (authorized/captured)"}
	orders := map[string]string{}
	rows, err := c.Orders.Query(ctx, `SELECT id::text, status FROM orders.orders
		WHERE status IN ('CONFIRMED', 'REJECTED') AND updated_at < now() - interval '10 seconds'
		  AND updated_at > now() - interval '1 hour'`)
	if err != nil {
		return ch, err
	}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			rows.Close()
			return ch, err
		}
		orders[id] = st
	}
	rows.Close()
	if len(orders) == 0 {
		ch.OK, ch.Value = true, "no finished orders yet"
		return ch, nil
	}
	ids := make([]string, 0, len(orders))
	for id := range orders {
		ids = append(ids, id)
	}
	pay := map[string]string{}
	prow, err := c.Payment.Query(ctx, `SELECT order_id, status FROM payment.intents WHERE order_id = ANY($1)`, ids)
	if err != nil {
		return ch, err
	}
	for prow.Next() {
		var id, st string
		if err := prow.Scan(&id, &st); err != nil {
			prow.Close()
			return ch, err
		}
		pay[id] = st
	}
	prow.Close()
	bad := 0
	var example string
	for id, st := range orders {
		p := pay[id]
		wrong := (st == "CONFIRMED" && p != "captured") || (st == "REJECTED" && (p == "authorized" || p == "captured"))
		if wrong {
			bad++
			example = fmt.Sprintf("order %s is %s but payment is %q", id[:8], st, p)
		}
	}
	ch.OK, ch.Value = bad == 0, fmt.Sprintf("%d of %d orders wrong", bad, len(orders))
	if example != "" {
		ch.Detail = example
	}
	return ch, nil
}

func (c *Checker) stuck(ctx context.Context) (Check, error) {
	ch := Check{ID: "no-stuck-sagas", Name: "Sagas finish", Kind: "health",
		Detail: "orders not CONFIRMED/REJECTED after 60 s (recovery should finish them)"}
	var n int
	var secs *float64
	err := c.Orders.QueryRow(ctx, `SELECT count(*), extract(epoch FROM now() - min(updated_at)) FROM orders.orders
		WHERE status NOT IN ('CONFIRMED', 'REJECTED') AND updated_at < now() - interval '60 seconds'`).Scan(&n, &secs)
	ch.OK, ch.Value = n == 0, fmt.Sprintf("%d stuck", n)
	if secs != nil {
		ch.Value += fmt.Sprintf(" (oldest %.0fs)", *secs)
	}
	return ch, err
}

func (c *Checker) outboxLag(ctx context.Context, p *pgxpool.Pool, table, name string) (Check, error) {
	ch := Check{ID: "outbox-" + name, Name: "Outbox drained (" + name + ")", Kind: "health",
		Detail: "events written but not yet in Kafka; grows if Kafka or the relay is down"}
	var n int
	var secs *float64
	err := p.QueryRow(ctx, `SELECT count(*), extract(epoch FROM now() - min(created_at)) FROM `+table+` WHERE published_at IS NULL`).Scan(&n, &secs)
	ch.OK = n < 100 && (secs == nil || *secs < 10)
	ch.Value = fmt.Sprintf("%d pending", n)
	if secs != nil {
		ch.Value += fmt.Sprintf(", oldest %.1fs", *secs)
	}
	return ch, err
}
