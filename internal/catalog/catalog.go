// Package catalog owns products, prices, stock and stock reservations.
//
// An order reserves stock (all lines or none), then either commits the reservation (stock
// leaves the warehouse) or releases it. Reservations expire if the order never finishes. The
// database's CHECK constraints make overselling impossible even under concurrent orders:
// a reservation is a conditional UPDATE that only succeeds while enough stock is available.
package catalog

import (
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

//go:embed migrations/*.sql
var Migrations embed.FS

const Schema = "catalog"

// DefaultTTL is how long an unfinished order may hold stock.
const DefaultTTL = 15 * time.Minute

var (
	ErrNotFound          = errors.New("catalog: not found")
	ErrConflict          = errors.New("catalog: already exists")
	ErrInvalid           = errors.New("catalog: invalid request")
	ErrReservationGone   = errors.New("catalog: reservation released or expired")
	ErrInvalidState      = errors.New("catalog: not allowed in the current state")
	ErrInsufficientStock = errors.New("catalog: not enough stock")
)

// OutOfStockError names the product that couldn't be reserved.
type OutOfStockError struct {
	ProductID string
	Wanted    int
	Available int
}

func (e *OutOfStockError) Error() string {
	return fmt.Sprintf("catalog: product %s: wanted %d, %d available", e.ProductID, e.Wanted, e.Available)
}
func (e *OutOfStockError) Unwrap() error { return ErrInsufficientStock }

// Product is a catalog item with its current availability.
type Product struct {
	ID          string
	SKU         string
	Name        string
	Description string
	Price       money.Money
	OnHand      int
	Reserved    int
}

// Available is what can still be reserved.
func (p Product) Available() int { return p.OnHand - p.Reserved }

// Line is one product and quantity of an order.
type Line struct {
	ProductID string
	Qty       int
}

// Reservation is stock held for an order.
type Reservation struct {
	ProductID string
	Qty       int
	Status    string
	ExpiresAt time.Time
}

// Store implements the catalog operations for the tenant in each call's context.
type Store struct {
	DB  db.Beginner
	TTL time.Duration
}

// CreateProduct adds a product with its starting stock.
func (s Store) CreateProduct(ctx context.Context, sku, name, description string, price money.Money, stock int) (Product, error) {
	if strings.TrimSpace(sku) == "" || strings.TrimSpace(name) == "" || price.Amount <= 0 || stock < 0 {
		return Product{}, fmt.Errorf("%w: sku, name, positive price and non-negative stock required", ErrInvalid)
	}
	var p Product
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO catalog.products (tenant_id, sku, name, description, price_minor, currency)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, $4, $5) RETURNING id`,
			sku, name, description, price.Amount, price.Currency).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO catalog.stock (product_id, tenant_id, on_hand)
			VALUES ($1, current_setting('app.tenant_id')::uuid, $2)`, id, stock); err != nil {
			return err
		}
		ps, err := products(ctx, tx, `WHERE p.id = $1`, id)
		if err == nil && len(ps) == 1 {
			p = ps[0]
		}
		return err
	})
	switch db.Code(err) {
	case db.UniqueViolation:
		return Product{}, fmt.Errorf("%w: sku %q", ErrConflict, sku)
	case db.CheckViolation:
		return Product{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return p, err
}

// Products returns the products with these ids (missing ids are left out).
func (s Store) Products(ctx context.Context, ids []string) ([]Product, error) {
	var out []Product
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		out, err = products(ctx, tx, `WHERE p.id = ANY($1::uuid[])`, ids)
		return err
	})
	if db.Code(err) == "22P02" {
		return nil, fmt.Errorf("%w: product ids must be UUIDs", ErrInvalid)
	}
	return out, err
}

// Search finds products by words in name and description (name matches rank higher).
// An empty query lists everything by name. cursor is the opaque value from the previous page.
func (s Store) Search(ctx context.Context, query string, limit int, cursor string) (items []Product, next string, err error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := 0
	if cursor != "" {
		b, derr := base64.RawURLEncoding.DecodeString(cursor)
		n, aerr := strconv.Atoi(string(b))
		if derr != nil || aerr != nil || n < 0 {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrInvalid)
		}
		offset = n
	}
	err = db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		if strings.TrimSpace(query) == "" {
			items, err = products(ctx, tx, `ORDER BY p.name, p.id LIMIT $1 OFFSET $2`, limit+1, offset)
		} else {
			items, err = products(ctx, tx, `WHERE p.search @@ websearch_to_tsquery('simple', $3)
				ORDER BY ts_rank(p.search, websearch_to_tsquery('simple', $3)) DESC, p.name, p.id LIMIT $1 OFFSET $2`, limit+1, offset, query)
		}
		return err
	})
	if len(items) > limit {
		items = items[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset + limit)))
	}
	return items, next, err
}

// AdjustStock adds (or, negative, removes) units on hand. Removing below what is reserved fails.
func (s Store) AdjustStock(ctx context.Context, productID string, delta int) (Product, error) {
	var p Product
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE catalog.stock SET on_hand = on_hand + $2 WHERE product_id = $1`, productID, delta)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		ps, err := products(ctx, tx, `WHERE p.id = $1`, productID)
		if err == nil && len(ps) == 1 {
			p = ps[0]
		}
		return err
	})
	if db.Code(err) == db.CheckViolation {
		return Product{}, fmt.Errorf("%w: would leave less on hand than reserved", ErrInsufficientStock)
	}
	return p, err
}

// Reserve holds stock for every line of an order, or for none (OutOfStockError). Calling it again
// for the same order returns the existing reservations instead of reserving twice; an order whose
// reservations were released gets ErrReservationGone.
func (s Store) Reserve(ctx context.Context, orderID string, lines []Line) ([]Reservation, error) {
	if orderID == "" || len(lines) == 0 {
		return nil, fmt.Errorf("%w: order id and at least one line required", ErrInvalid)
	}
	merged := map[string]int{}
	for _, l := range lines {
		if l.Qty <= 0 {
			return nil, fmt.Errorf("%w: quantity must be positive", ErrInvalid)
		}
		merged[l.ProductID] += l.Qty
	}
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids) // fixed lock order: two orders with overlapping products can't deadlock
	ttl := s.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}

	var out []Reservation
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		out = nil
		for i, id := range ids {
			tag, err := tx.Exec(ctx, `INSERT INTO catalog.reservations (tenant_id, order_id, product_id, qty, expires_at)
				VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, now() + $4::interval)
				ON CONFLICT (tenant_id, order_id, product_id) DO NOTHING`, orderID, id, merged[id], ttl.String())
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				if i > 0 {
					return fmt.Errorf("%w: order %s was reserved with different lines", ErrConflict, orderID)
				}
				// a previous (or concurrent, now committed) call reserved this order: replay it
				return errReplay
			}
			// the conditional update IS the availability check; the CHECK constraint backs it up
			var reserved int
			err = tx.QueryRow(ctx, `UPDATE catalog.stock SET reserved = reserved + $2
				WHERE product_id = $1 AND on_hand - reserved >= $2 RETURNING reserved`, id, merged[id]).Scan(&reserved)
			if errors.Is(err, pgx.ErrNoRows) {
				var avail int
				if e := tx.QueryRow(ctx, `SELECT on_hand - reserved FROM catalog.stock WHERE product_id = $1`, id).Scan(&avail); errors.Is(e, pgx.ErrNoRows) {
					return fmt.Errorf("%w: product %s", ErrNotFound, id)
				}
				return &OutOfStockError{ProductID: id, Wanted: merged[id], Available: avail}
			}
			if err != nil {
				return err
			}
		}
		var err error
		out, err = reservations(ctx, tx, orderID)
		return err
	})
	if errors.Is(err, errReplay) {
		out, err = s.Reservations(ctx, orderID)
		if err == nil {
			for _, r := range out {
				if r.Status == "released" {
					return out, ErrReservationGone
				}
			}
		}
	}
	if db.Code(err) == db.ForeignKeyViolation || db.Code(err) == "22P02" {
		return nil, fmt.Errorf("%w: unknown product", ErrNotFound)
	}
	return out, err
}

var errReplay = errors.New("replay")

// Commit turns an order's reservations into a sale: the units leave stock for good.
// It fails with ErrReservationGone if the reservation expired or was released first.
func (s Store) Commit(ctx context.Context, orderID string) error {
	return db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		rs, err := lockReservations(ctx, tx, orderID)
		if err != nil {
			return err
		}
		for _, r := range rs {
			switch r.Status {
			case "committed":
				continue
			case "released":
				return ErrReservationGone
			}
			if time.Now().After(r.ExpiresAt) {
				return ErrReservationGone // expired; the sweeper will hand the units back
			}
			if _, err := tx.Exec(ctx, `UPDATE catalog.stock SET on_hand = on_hand - $2, reserved = reserved - $2 WHERE product_id = $1`, r.ProductID, r.Qty); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE catalog.reservations SET status = 'committed' WHERE order_id = $1 AND product_id = $2`, orderID, r.ProductID); err != nil {
				return err
			}
		}
		return nil
	})
}

// Release gives an order's reserved units back. Releasing twice is fine; releasing a committed
// order is not (that would need a restock).
func (s Store) Release(ctx context.Context, orderID string) error {
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		rs, err := lockReservations(ctx, tx, orderID)
		if errors.Is(err, ErrNotFound) {
			return nil // nothing was ever reserved: nothing to give back
		}
		if err != nil {
			return err
		}
		for _, r := range rs {
			switch r.Status {
			case "released":
				continue
			case "committed":
				return fmt.Errorf("%w: order %s is committed", ErrInvalidState, orderID)
			}
			if _, err := tx.Exec(ctx, `UPDATE catalog.stock SET reserved = reserved - $2 WHERE product_id = $1`, r.ProductID, r.Qty); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE catalog.reservations SET status = 'released' WHERE order_id = $1 AND product_id = $2`, orderID, r.ProductID); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// Reservations lists an order's reservations.
func (s Store) Reservations(ctx context.Context, orderID string) ([]Reservation, error) {
	var out []Reservation
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		out, err = reservations(ctx, tx, orderID)
		return err
	})
	return out, err
}

// ExpireReservations releases reservations past their expiry, for all tenants (system job).
func (s Store) ExpireReservations(ctx context.Context, max int) (int, error) {
	var n int
	err := db.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT catalog.expire_reservations($1)`, max).Scan(&n)
	})
	return n, err
}

func lockReservations(ctx context.Context, tx pgx.Tx, orderID string) ([]Reservation, error) {
	rows, err := tx.Query(ctx, `SELECT product_id, qty, status, expires_at FROM catalog.reservations
		WHERE order_id = $1 ORDER BY product_id FOR UPDATE`, orderID)
	if err != nil {
		return nil, err
	}
	rs, err := scanReservations(rows)
	if err == nil && len(rs) == 0 {
		return nil, ErrNotFound
	}
	return rs, err
}

func reservations(ctx context.Context, tx pgx.Tx, orderID string) ([]Reservation, error) {
	rows, err := tx.Query(ctx, `SELECT product_id, qty, status, expires_at FROM catalog.reservations WHERE order_id = $1 ORDER BY product_id`, orderID)
	if err != nil {
		return nil, err
	}
	return scanReservations(rows)
}

func scanReservations(rows pgx.Rows) ([]Reservation, error) {
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.ProductID, &r.Qty, &r.Status, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func products(ctx context.Context, tx pgx.Tx, where string, args ...any) ([]Product, error) {
	rows, err := tx.Query(ctx, `SELECT p.id, p.sku, p.name, p.description, p.price_minor, p.currency, s.on_hand, s.reserved
		FROM catalog.products p JOIN catalog.stock s ON s.product_id = p.id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Product
	for rows.Next() {
		var p Product
		var cur string
		if err := rows.Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.Price.Amount, &cur, &p.OnHand, &p.Reserved); err != nil {
			return nil, err
		}
		p.Price.Currency = strings.TrimSpace(cur)
		out = append(out, p)
	}
	return out, rows.Err()
}
