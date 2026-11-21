package payment

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// Break is one difference between the ledger and the processor's statement.
type Break struct {
	Kind     string // missing_in_ledger | missing_at_psp | amount_mismatch
	ChargeID string
	LineType string // capture | refund | capture-fee
	Ledger   *int64
	PSP      *int64
}

// ReconResult is one tenant's reconciliation of one day.
type ReconResult struct {
	RunID   string
	Day     time.Time
	Matched int
	Breaks  []Break
}

type side struct {
	gross int64
	fee   int64
}

// Reconcile compares the tenant's ledger for day (UTC) with the processor's settlement statement:
// every capture (gross and fee) and refund must appear on both sides with the same amounts.
// The result and every break are stored, so a finance user can see what didn't match and why.
func (s Service) Reconcile(ctx context.Context, day time.Time) (ReconResult, error) {
	tid, err := tenancy.From(ctx)
	if err != nil {
		return ReconResult{}, err
	}
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	lines, err := s.PSP.Settlement(ctx, day)
	if err != nil {
		return ReconResult{}, err
	}
	pspSide := map[string]side{} // "capture|ch_1" / "refund|re_1|ch_1"
	for _, l := range lines {
		if l.Metadata["tenant_id"] != string(tid) {
			continue // another tenant's line
		}
		k := l.Type + "|" + l.ChargeID
		if l.Type == "refund" {
			k = "refund|" + l.RefundID + "|" + l.ChargeID
		}
		p := pspSide[k]
		p.gross += l.Amount
		p.fee += l.Fee
		pspSide[k] = p
	}

	res := ReconResult{Day: day}
	err = db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.kind, e.memo,
			       COALESCE(sum(p.amount_minor) FILTER (WHERE a.code = 'sales'), 0),
			       COALESCE(sum(p.amount_minor) FILTER (WHERE a.code = 'psp_fees'), 0)
			  FROM ledger.entries e
			  JOIN ledger.postings p ON p.entry_id = e.id
			  JOIN ledger.accounts a ON a.id = p.account_id
			 WHERE e.kind IN ('capture', 'refund') AND e.created_at >= $1 AND e.created_at < $1 + interval '1 day'
			 GROUP BY e.id`, day)
		if err != nil {
			return err
		}
		ledgerSide := map[string]side{}
		for rows.Next() {
			var kind, memo string
			var sales, fees int64
			if err := rows.Scan(&kind, &memo, &sales, &fees); err != nil {
				rows.Close()
				return err
			}
			// capture memo "charge:ch_1"; refund memo "refund:re_1|charge:ch_1"
			var k string
			if kind == "capture" {
				k = "capture|" + strings.TrimPrefix(memo, "charge:")
			} else {
				r, c, _ := strings.Cut(memo, "|")
				k = "refund|" + strings.TrimPrefix(r, "refund:") + "|" + strings.TrimPrefix(c, "charge:")
			}
			// the ledger books sales as a credit (negative); the statement shows captures positive
			// and refunds negative, so sales negated lines up with both
			ledgerSide[k] = side{gross: -sales, fee: fees}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		keys := map[string]bool{}
		for k := range pspSide {
			keys[k] = true
		}
		for k := range ledgerSide {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			parts := strings.Split(k, "|")
			typ, charge := parts[0], parts[len(parts)-1]
			l, inL := ledgerSide[k]
			p, inP := pspSide[k]
			switch {
			case !inL:
				res.Breaks = append(res.Breaks, Break{Kind: "missing_in_ledger", ChargeID: charge, LineType: typ, PSP: ptr(p.gross)})
			case !inP:
				res.Breaks = append(res.Breaks, Break{Kind: "missing_at_psp", ChargeID: charge, LineType: typ, Ledger: ptr(l.gross)})
			case l.gross != p.gross:
				res.Breaks = append(res.Breaks, Break{Kind: "amount_mismatch", ChargeID: charge, LineType: typ, Ledger: ptr(l.gross), PSP: ptr(p.gross)})
			case l.fee != p.fee:
				res.Breaks = append(res.Breaks, Break{Kind: "amount_mismatch", ChargeID: charge, LineType: typ + "-fee", Ledger: ptr(l.fee), PSP: ptr(p.fee)})
			default:
				res.Matched++
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO payment.recon_runs (tenant_id, day, matched, breaks)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3) RETURNING id`, day, res.Matched, len(res.Breaks)).Scan(&res.RunID); err != nil {
			return err
		}
		for _, b := range res.Breaks {
			if _, err := tx.Exec(ctx, `INSERT INTO payment.recon_breaks (run_id, tenant_id, kind, charge_id, line_type, ledger_minor, psp_minor)
				VALUES ($1, current_setting('app.tenant_id')::uuid, $2, $3, $4, $5, $6)`, res.RunID, b.Kind, b.ChargeID, b.LineType, b.Ledger, b.PSP); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

func ptr(v int64) *int64 { return &v }
