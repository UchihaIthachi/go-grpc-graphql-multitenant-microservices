// Package ledger is the payment service's double-entry ledger.
//
// Money never appears or disappears: every Entry has two or more Postings that sum to zero per
// currency. Posting is idempotent on a caller-chosen key, so a retried request returns the
// original entry instead of moving money twice. The database enforces the rules too (see
// migrations/001_ledger.sql), so this package is a convenience, not the only line of defence.
package ledger

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

//go:embed migrations/*.sql
var Migrations embed.FS

// Schema is the Postgres schema the migrations create.
const Schema = "ledger"

var (
	ErrUnbalanced          = errors.New("ledger: postings do not balance")
	ErrInsufficientFunds   = errors.New("ledger: insufficient funds")
	ErrUnknownAccount      = errors.New("ledger: unknown account (or another tenant's)")
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with a different request")
	ErrInvalid             = errors.New("ledger: invalid request")
)

// Kinds of account, as in accounting.
const (
	Asset     = "asset"
	Liability = "liability"
	Revenue   = "revenue"
	Expense   = "expense"
	Equity    = "equity"
)

// Account is a ledger account of one tenant in one currency.
type Account struct {
	ID            string
	Code          string
	Kind          string
	AllowNegative bool
	Balance       money.Money
}

// Posting moves Amount into (positive, debit) or out of (negative, credit) an account.
type Posting struct {
	AccountID string      `json:"account_id"`
	Amount    money.Money `json:"amount"`
}

// EntryRequest is one balanced money movement.
type EntryRequest struct {
	IdempotencyKey string    `json:"-"`
	Kind           string    `json:"kind"`
	Memo           string    `json:"memo"`
	Postings       []Posting `json:"postings"`
}

// Entry is a recorded movement.
type Entry struct {
	ID        string
	Kind      string
	Memo      string
	CreatedAt time.Time
	Postings  []Posting
}

// Store reads and writes the ledger of the tenant in each call's context.
type Store struct {
	DB db.Beginner
}

// EnsureAccount returns the tenant's account with this code and currency, creating it first if
// needed. Creating twice is harmless; kind and allowNegative are fixed at creation.
func (s Store) EnsureAccount(ctx context.Context, code, currency, kind string, allowNegative bool) (Account, error) {
	if code == "" {
		return Account{}, fmt.Errorf("%w: empty account code", ErrInvalid)
	}
	if _, err := money.New(0, currency); err != nil {
		return Account{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var a Account
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger.accounts (tenant_id, code, currency, kind, allow_negative)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, $4)
			ON CONFLICT (tenant_id, code, currency) DO NOTHING`, code, currency, kind, allowNegative); err != nil {
			return err
		}
		return scanAccount(tx.QueryRow(ctx, `SELECT id, code, kind, allow_negative, balance_minor, currency
			FROM ledger.accounts WHERE code = $1 AND currency = $2`, code, currency), &a)
	})
	if db.Code(err) == db.CheckViolation {
		return Account{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return a, err
}

// Account returns one account with its current balance.
func (s Store) Account(ctx context.Context, id string) (Account, error) {
	var a Account
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		return scanAccount(tx.QueryRow(ctx, `SELECT id, code, kind, allow_negative, balance_minor, currency
			FROM ledger.accounts WHERE id = $1`, id), &a)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrUnknownAccount
	}
	return a, err
}

func scanAccount(row pgx.Row, a *Account) error {
	var cur string
	if err := row.Scan(&a.ID, &a.Code, &a.Kind, &a.AllowNegative, &a.Balance.Amount, &cur); err != nil {
		return err
	}
	a.Balance.Currency = strings.TrimSpace(cur)
	return nil
}

// Validate checks a request the way the database will, so callers get a clear error early.
func (r EntryRequest) Validate() error {
	if strings.TrimSpace(r.IdempotencyKey) == "" || len(r.IdempotencyKey) > 200 {
		return fmt.Errorf("%w: idempotency key must be 1-200 characters", ErrInvalid)
	}
	if r.Kind == "" {
		return fmt.Errorf("%w: kind is required", ErrInvalid)
	}
	if len(r.Postings) < 2 {
		return fmt.Errorf("%w: an entry needs at least 2 postings", ErrUnbalanced)
	}
	sums := map[string]int64{}
	for _, p := range r.Postings {
		if p.Amount.IsZero() {
			return fmt.Errorf("%w: zero posting", ErrInvalid)
		}
		if _, err := money.New(0, p.Amount.Currency); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		s, err := money.Money{Amount: sums[p.Amount.Currency], Currency: p.Amount.Currency}.Add(p.Amount)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		sums[p.Amount.Currency] = s.Amount
	}
	for cur, s := range sums {
		if s != 0 {
			return fmt.Errorf("%w: %s sums to %d", ErrUnbalanced, cur, s)
		}
	}
	return nil
}

func (r EntryRequest) hash() []byte {
	// canonical: postings in a fixed order, so the same movement listed differently matches
	ps := append([]Posting(nil), r.Postings...)
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].AccountID != ps[j].AccountID {
			return ps[i].AccountID < ps[j].AccountID
		}
		return ps[i].Amount.Amount < ps[j].Amount.Amount
	})
	b, _ := json.Marshal(EntryRequest{Kind: r.Kind, Memo: r.Memo, Postings: ps})
	h := sha256.Sum256(b)
	return h[:]
}

// Post records the entry once. A retry with the same key and the same request returns the
// original entry with replayed = true; the same key with a different request is refused.
func (s Store) Post(ctx context.Context, r EntryRequest) (e Entry, replayed bool, err error) {
	if err := r.Validate(); err != nil {
		return Entry{}, false, err
	}
	h := r.hash()
	err = db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		replayed = false
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO ledger.entries (tenant_id, idempotency_key, request_hash, kind, memo)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, $4)
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
			RETURNING id`, r.IdempotencyKey, h, r.Kind, r.Memo).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// key already used: same request → replay, different request → conflict
			var prevHash []byte
			if err := tx.QueryRow(ctx, `SELECT id, request_hash FROM ledger.entries WHERE idempotency_key = $1`,
				r.IdempotencyKey).Scan(&id, &prevHash); err != nil {
				return err
			}
			if string(prevHash) != string(h) {
				return ErrIdempotencyConflict
			}
			replayed = true
			e, err = loadEntry(ctx, tx, id)
			return err
		}
		if err != nil {
			return err
		}
		// lock accounts in a fixed order so two concurrent transfers between the same pair
		// can't deadlock
		ps := append([]Posting(nil), r.Postings...)
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].AccountID < ps[j].AccountID })
		for _, p := range ps {
			if _, err := tx.Exec(ctx, `
				INSERT INTO ledger.postings (tenant_id, entry_id, account_id, amount_minor, currency)
				VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, $4)`,
				id, p.AccountID, p.Amount.Amount, p.Amount.Currency); err != nil {
				return err
			}
		}
		e, err = loadEntry(ctx, tx, id)
		return err
	})
	return e, replayed, mapErr(err)
}

func loadEntry(ctx context.Context, tx pgx.Tx, id string) (Entry, error) {
	var e Entry
	if err := tx.QueryRow(ctx, `SELECT id, kind, memo, created_at FROM ledger.entries WHERE id = $1`, id).
		Scan(&e.ID, &e.Kind, &e.Memo, &e.CreatedAt); err != nil {
		return Entry{}, err
	}
	rows, err := tx.Query(ctx, `SELECT account_id, amount_minor, currency FROM ledger.postings WHERE entry_id = $1 ORDER BY id`, id)
	if err != nil {
		return Entry{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Posting
		var cur string
		if err := rows.Scan(&p.AccountID, &p.Amount.Amount, &cur); err != nil {
			return Entry{}, err
		}
		p.Amount.Currency = strings.TrimSpace(cur)
		e.Postings = append(e.Postings, p)
	}
	return e, rows.Err()
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "ledger_insufficient_funds"):
		return fmt.Errorf("%w: %v", ErrInsufficientFunds, err)
	case strings.Contains(msg, "ledger_unbalanced"):
		return fmt.Errorf("%w: %v", ErrUnbalanced, err)
	case db.Code(err) == db.ForeignKeyViolation:
		return fmt.Errorf("%w: %v", ErrUnknownAccount, err)
	case db.Code(err) == "22P02": // invalid uuid text
		return fmt.Errorf("%w: %v", ErrUnknownAccount, err)
	}
	return err
}

// Verify recomputes every account's balance from its postings and reports any account whose
// cached balance differs. An empty result is the invariant holding.
func (s Store) Verify(ctx context.Context) (mismatches []string, err error) {
	err = db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT a.code, a.balance_minor, COALESCE(sum(p.amount_minor), 0)
			  FROM ledger.accounts a LEFT JOIN ledger.postings p ON p.account_id = a.id
			 GROUP BY a.id HAVING a.balance_minor <> COALESCE(sum(p.amount_minor), 0)`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code string
			var cached, actual int64
			if err := rows.Scan(&code, &cached, &actual); err != nil {
				return err
			}
			mismatches = append(mismatches, fmt.Sprintf("%s: cached %d, postings %d", code, cached, actual))
		}
		return rows.Err()
	})
	return mismatches, err
}
