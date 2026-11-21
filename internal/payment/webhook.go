package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/ledger"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/psp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// ErrBadEvent is a signed event we can't use (no tenant, unknown intent).
var ErrBadEvent = errors.New("payment: unusable webhook event")

// HandleWebhook verifies and applies one processor event. In order:
//  1. signature + timestamp (forged or replayed-later requests stop here),
//  2. the tenant from the charge metadata (set by us when authorizing),
//  3. in ONE transaction: record the event id (a redelivery stops here) and apply its effect.
//
// It returns duplicate = true for an event already processed.
func (s Service) HandleWebhook(ctx context.Context, secret string, body []byte, signature string, now time.Time) (duplicate bool, err error) {
	if err := psp.Verify(secret, body, signature, now); err != nil {
		return false, err
	}
	var ev psp.Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		return false, fmt.Errorf("%w: malformed body", ErrBadEvent)
	}
	tid, err := tenancy.Parse(ev.Data.Metadata["tenant_id"])
	if err != nil {
		return false, fmt.Errorf("%w: no tenant in metadata", ErrBadEvent)
	}
	ctx = tenancy.With(ctx, tid)
	orderID := ev.Data.Metadata["order_id"]

	err = db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		duplicate = false
		tag, err := tx.Exec(ctx, `INSERT INTO payment.webhook_events (tenant_id, event_id, type, payload)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3) ON CONFLICT DO NOTHING`, ev.ID, ev.Type, body)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			duplicate = true
			return nil
		}
		switch ev.Type {
		case "refund.succeeded":
			return s.applyRefund(ctx, tx, orderID, ev)
		case "refund.failed":
			return s.applyRefundFailed(ctx, tx, orderID, ev)
		default:
			return nil // recorded, nothing to do (yet)
		}
	})
	return duplicate, err
}

func (s Service) applyRefund(ctx context.Context, tx pgx.Tx, orderID string, ev psp.Event) error {
	tag, err := tx.Exec(ctx, `UPDATE payment.intents SET status = 'refunded', psp_refund_id = $3, updated_at = now()
		WHERE order_id = $1 AND status = 'refund_pending' AND psp_charge_id = $2`, orderID, ev.Data.ChargeID, ev.Data.RefundID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// already refunded, or about a payment we don't know: the event stays recorded (so the
		// processor stops retrying and a redelivery is a no-op), nothing else changes
		return nil
	}
	it, err := get(ctx, tx, orderID)
	if err != nil {
		return err
	}
	acc, err := accounts(ctx, tx, it.Amount.Currency)
	if err != nil {
		return err
	}
	amt := money.Money{Amount: ev.Data.Amount, Currency: it.Amount.Currency}
	if _, _, err := ledger.PostTx(ctx, tx, ledger.EntryRequest{
		IdempotencyKey: "refund:" + ev.Data.RefundID, Kind: "refund", Memo: "refund:" + ev.Data.RefundID + "|charge:" + ev.Data.ChargeID,
		Postings: []ledger.Posting{{AccountID: acc[AccSales], Amount: amt}, {AccountID: acc[AccReceivable], Amount: amt.Neg()}},
	}); err != nil {
		return err
	}
	return outbox.Enqueue(ctx, tx, "payment.outbox", outbox.Event{Topic: Topic, Key: orderID, Type: "payment.refunded", Payload: eventPayload(it)})
}

func (s Service) applyRefundFailed(ctx context.Context, tx pgx.Tx, orderID string, ev psp.Event) error {
	tag, err := tx.Exec(ctx, `UPDATE payment.intents SET status = 'captured', failure_reason = $3, updated_at = now()
		WHERE order_id = $1 AND status = 'refund_pending' AND psp_charge_id = $2`, orderID, ev.Data.ChargeID, "refund failed: "+ev.Data.Reason)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	it, err := get(ctx, tx, orderID)
	if err != nil {
		return err
	}
	return outbox.Enqueue(ctx, tx, "payment.outbox", outbox.Event{Topic: Topic, Key: orderID, Type: "payment.refund_failed", Payload: eventPayload(it)})
}

// WebhookHandler serves POST /webhooks/psp. 2xx tells the processor to stop retrying; 5xx makes
// it retry later. A bad signature is 400 (retrying won't fix it).
func WebhookHandler(s Service, secret string, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		dup, err := s.HandleWebhook(r.Context(), secret, body, r.Header.Get(psp.SignatureHeader), time.Now())
		switch {
		case errors.Is(err, psp.ErrBadSignature), errors.Is(err, ErrBadEvent):
			log.Warn("webhook rejected", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
		case err != nil:
			log.Error("webhook failed", "err", err)
			http.Error(w, "retry later", http.StatusInternalServerError)
		default:
			if dup {
				w.Header().Set("Webhook-Duplicate", "true")
			}
			w.WriteHeader(http.StatusOK)
		}
	})
}
