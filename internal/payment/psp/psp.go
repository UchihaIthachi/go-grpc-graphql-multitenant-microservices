// Package psp is the payment service's view of a card processor (PSP): the operations, their
// errors, an HTTP client, and webhook signing. internal/fakepsp implements the other side.
//
// Every mutating call carries an Idempotency-Key. The payment service derives keys from its own
// intent id ("auth:<intent>", "capture:<intent>"), so a retry after a timeout reaches the same
// charge instead of charging the customer twice.
package psp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrDeclined is a final answer from the processor (card declined, authorization expired).
	ErrDeclined = errors.New("psp: declined")
	// ErrUnavailable is transient (timeout, 5xx, rate limit): retry with the same idempotency key.
	ErrUnavailable = errors.New("psp: unavailable")
	ErrNotFound    = errors.New("psp: not found")
)

// Charge statuses.
const (
	Authorized = "authorized"
	Captured   = "captured"
	Voided     = "voided"
	Declined   = "declined"
)

type Charge struct {
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Captured      int64             `json:"captured"`
	Fee           int64             `json:"fee"` // known once captured
	DeclineReason string            `json:"decline_reason,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type Refund struct {
	ID       string `json:"id"`
	ChargeID string `json:"charge"`
	Amount   int64  `json:"amount"`
	Status   string `json:"status"` // pending → succeeded | failed (sent by webhook)
}

type AuthorizeRequest struct {
	IdempotencyKey string            `json:"-"`
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	PaymentMethod  string            `json:"payment_method"` // test tokens: pm_ok, pm_decline, ...
	Metadata       map[string]string `json:"metadata"`       // tenant_id, order_id, intent_id
}

// Client is everything the payment service needs from a processor.
type Client interface {
	Authorize(ctx context.Context, r AuthorizeRequest) (Charge, error)
	ChargeByKey(ctx context.Context, idempotencyKey string) (Charge, error) // was an earlier attempt accepted?
	Capture(ctx context.Context, chargeID, idempotencyKey string, amount int64) (Charge, error)
	Void(ctx context.Context, chargeID, idempotencyKey string) (Charge, error)
	Refund(ctx context.Context, chargeID, idempotencyKey string, amount int64) (Refund, error)
	Settlement(ctx context.Context, day time.Time) ([]SettlementLine, error)
}

// SettlementLine is one money movement in the processor's daily statement.
type SettlementLine struct {
	Type     string            `json:"type"` // capture | refund
	ChargeID string            `json:"charge"`
	RefundID string            `json:"refund,omitempty"`
	Amount   int64             `json:"amount"`
	Fee      int64             `json:"fee"`
	Currency string            `json:"currency"`
	Metadata map[string]string `json:"metadata"`
}

// HTTPClient talks to the processor's JSON API.
type HTTPClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client // default: 5 s timeout
}

func (c HTTPClient) Authorize(ctx context.Context, r AuthorizeRequest) (Charge, error) {
	var ch Charge
	err := c.do(ctx, http.MethodPost, "/v1/charges", r.IdempotencyKey, r, &ch)
	if err == nil && ch.Status == Declined {
		return ch, fmt.Errorf("%w: %s", ErrDeclined, ch.DeclineReason)
	}
	return ch, err
}

func (c HTTPClient) ChargeByKey(ctx context.Context, key string) (Charge, error) {
	var ch Charge
	err := c.do(ctx, http.MethodGet, "/v1/charges?idempotency_key="+url.QueryEscape(key), "", nil, &ch)
	return ch, err
}

func (c HTTPClient) Capture(ctx context.Context, id, key string, amount int64) (Charge, error) {
	var ch Charge
	err := c.do(ctx, http.MethodPost, "/v1/charges/"+url.PathEscape(id)+"/capture", key, map[string]int64{"amount": amount}, &ch)
	return ch, err
}

func (c HTTPClient) Void(ctx context.Context, id, key string) (Charge, error) {
	var ch Charge
	err := c.do(ctx, http.MethodPost, "/v1/charges/"+url.PathEscape(id)+"/void", key, struct{}{}, &ch)
	return ch, err
}

func (c HTTPClient) Refund(ctx context.Context, id, key string, amount int64) (Refund, error) {
	var rf Refund
	err := c.do(ctx, http.MethodPost, "/v1/refunds", key, map[string]any{"charge": id, "amount": amount}, &rf)
	return rf, err
}

func (c HTTPClient) Settlement(ctx context.Context, day time.Time) ([]SettlementLine, error) {
	var out struct {
		Lines []SettlementLine `json:"lines"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/settlements?date="+day.UTC().Format("2006-01-02"), "", nil, &out)
	return out.Lines, err
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c HTTPClient) do(ctx context.Context, method, path, key string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err) // timeouts, refused connections
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return json.Unmarshal(b, out)
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	default:
		var e apiError
		_ = json.Unmarshal(b, &e)
		if e.Code == "declined" || e.Code == "authorization_expired" {
			return fmt.Errorf("%w: %s", ErrDeclined, e.Message)
		}
		return fmt.Errorf("psp: HTTP %d %s: %s", resp.StatusCode, e.Code, e.Message)
	}
}

// ---- webhooks ---------------------------------------------------------------------------------

// SignatureHeader carries "t=<unix seconds>,v1=<hex hmac-sha256(secret, t + "." + body)>".
const SignatureHeader = "Psp-Signature"

// Tolerance is how old a signed webhook may be; older ones are refused as possible replays.
const Tolerance = 5 * time.Minute

var ErrBadSignature = errors.New("psp: webhook signature invalid or too old")

// Sign returns the signature header value for body at time now.
func Sign(secret string, body []byte, now time.Time) string {
	t := strconv.FormatInt(now.Unix(), 10)
	return "t=" + t + ",v1=" + mac(secret, t, body)
}

func mac(secret, t string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(t + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks the signature in constant time and that it was made within Tolerance of now.
func Verify(secret string, body []byte, header string, now time.Time) error {
	var t, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	ts, err := strconv.ParseInt(t, 10, 64)
	if err != nil || v1 == "" {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Tolerance || d < -Tolerance {
		return ErrBadSignature
	}
	if !hmac.Equal([]byte(mac(secret, t, body)), []byte(v1)) {
		return ErrBadSignature
	}
	return nil
}

// Event is a webhook body.
type Event struct {
	ID      string            `json:"id"` // unique per event; redeliveries reuse it
	Type    string            `json:"type"`
	Created int64             `json:"created"`
	Data    EventData         `json:"data"`
	Meta    map[string]string `json:"-"`
}

type EventData struct {
	ChargeID string            `json:"charge"`
	RefundID string            `json:"refund,omitempty"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Reason   string            `json:"reason,omitempty"`
	Metadata map[string]string `json:"metadata"`
}
