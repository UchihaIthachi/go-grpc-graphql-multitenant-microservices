// Package money represents amounts exactly, as integer minor units (cents) plus an
// ISO 4217 currency code. There are no floats anywhere: 0.1 + 0.2 is 30 cents, always.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is an exact amount in the currency's minor unit (e.g. cents for USD, yen for JPY).
type Money struct {
	Amount   int64  // minor units; negative allowed (refunds, credits)
	Currency string // ISO 4217, upper case
}

var (
	ErrCurrency         = errors.New("money: invalid currency")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: amount overflows int64")
	ErrFormat           = errors.New("money: invalid decimal amount")
)

// exponent is the number of minor-unit digits; currencies not listed use 2.
var exponent = map[string]int{"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3}

// Exponent returns the number of decimal places of the currency's minor unit.
func Exponent(currency string) int {
	if e, ok := exponent[currency]; ok {
		return e
	}
	return 2
}

func validCurrency(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// New returns an amount of minor units in currency.
func New(minor int64, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, fmt.Errorf("%w: %q", ErrCurrency, currency)
	}
	return Money{Amount: minor, Currency: currency}, nil
}

// MustNew is New for constants and tests; it panics on an invalid currency.
func MustNew(minor int64, currency string) Money {
	m, err := New(minor, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// Parse reads a decimal string such as "12.34" or "-0.5" in the given currency.
// More fractional digits than the currency allows is an error, never a rounding.
func Parse(s, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, fmt.Errorf("%w: %q", ErrCurrency, currency)
	}
	exp := Exponent(currency)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") || len(frac) > exp || !digits(whole) || !digits(frac) {
		return Money{}, fmt.Errorf("%w: %q for %s", ErrFormat, s, currency)
	}
	frac += strings.Repeat("0", exp-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", ErrFormat, s)
	}
	var f int64
	if exp > 0 {
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return Money{}, fmt.Errorf("%w: %q", ErrFormat, s)
		}
	}
	scale := int64(math.Pow10(exp))
	if w > (math.MaxInt64-f)/scale {
		return Money{}, ErrOverflow
	}
	minor := w*scale + f
	if neg {
		minor = -minor
	}
	return Money{Amount: minor, Currency: currency}, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// String formats as "12.34 USD".
func (m Money) String() string {
	exp := Exponent(m.Currency)
	if exp == 0 {
		return fmt.Sprintf("%d %s", m.Amount, m.Currency)
	}
	scale := int64(math.Pow10(exp))
	sign, a := "", m.Amount
	if a < 0 {
		sign = "-"
	}
	q, r := a/scale, a%scale
	if q < 0 {
		q = -q
	}
	if r < 0 {
		r = -r
	}
	return fmt.Sprintf("%s%d.%0*d %s", sign, q, exp, r, m.Currency)
}

// Add returns m + o; both must have the same currency.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	s := m.Amount + o.Amount
	if (o.Amount > 0 && s < m.Amount) || (o.Amount < 0 && s > m.Amount) {
		return Money{}, ErrOverflow
	}
	return Money{Amount: s, Currency: m.Currency}, nil
}

// Sub returns m - o.
func (m Money) Sub(o Money) (Money, error) {
	if o.Amount == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return m.Add(o.Neg())
}

// Neg returns -m.
func (m Money) Neg() Money { return Money{Amount: -m.Amount, Currency: m.Currency} }

// Mul returns m × q, e.g. unit price × quantity.
func (m Money) Mul(q int64) (Money, error) {
	if q == 0 || m.Amount == 0 {
		return Money{Currency: m.Currency}, nil
	}
	p := m.Amount * q
	if p/q != m.Amount || (m.Amount == -1 && q == math.MinInt64) || (q == -1 && m.Amount == math.MinInt64) {
		return Money{}, ErrOverflow
	}
	return Money{Amount: p, Currency: m.Currency}, nil
}

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.Amount == 0 }

// Sum adds amounts of one currency; an empty list is an error because it has no currency.
func Sum(ms ...Money) (Money, error) {
	if len(ms) == 0 {
		return Money{}, fmt.Errorf("%w: empty sum", ErrCurrency)
	}
	total := Money{Currency: ms[0].Currency}
	for _, m := range ms {
		var err error
		if total, err = total.Add(m); err != nil {
			return Money{}, err
		}
	}
	return total, nil
}
