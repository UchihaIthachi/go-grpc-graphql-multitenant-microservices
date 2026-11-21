package money

import (
	"errors"
	"math"
	"testing"
)

func TestParseAndString(t *testing.T) {
	cases := []struct {
		in, cur string
		minor   int64
		out     string
	}{
		{"12.34", "USD", 1234, "12.34 USD"},
		{"0.1", "USD", 10, "0.10 USD"},
		{"-0.05", "EUR", -5, "-0.05 EUR"},
		{"7", "USD", 700, "7.00 USD"},
		{"1500", "JPY", 1500, "1500 JPY"},
		{"1.234", "BHD", 1234, "1.234 BHD"},
		{"+3.5", "LKR", 350, "3.50 LKR"},
	}
	for _, c := range cases {
		m, err := Parse(c.in, c.cur)
		if err != nil {
			t.Fatalf("Parse(%q,%s): %v", c.in, c.cur, err)
		}
		if m.Amount != c.minor || m.String() != c.out {
			t.Errorf("Parse(%q,%s) = %d %q, want %d %q", c.in, c.cur, m.Amount, m.String(), c.minor, c.out)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "1.", ".5", "1.234", "abc", "1 000", "1_000", "--1"} {
		if _, err := Parse(in, "USD"); err == nil {
			t.Errorf("Parse(%q) accepted", in)
		}
	}
	if _, err := Parse("1.5", "JPY"); err == nil {
		t.Error("JPY has no minor unit; 1.5 must be rejected, not rounded")
	}
	if _, err := Parse("1", "usd"); !errors.Is(err, ErrCurrency) {
		t.Errorf("lower-case currency: %v", err)
	}
	if _, err := Parse("92233720368547758.08", "USD"); !errors.Is(err, ErrOverflow) {
		t.Errorf("overflow: %v", err)
	}
}

func TestNoFloatDrift(t *testing.T) {
	a, b := MustNew(10, "USD"), MustNew(20, "USD")
	s, _ := a.Add(b)
	if s.Amount != 30 {
		t.Fatalf("0.10 + 0.20 = %v", s)
	}
}

func TestArithmeticGuards(t *testing.T) {
	if _, err := MustNew(1, "USD").Add(MustNew(1, "EUR")); !errors.Is(err, ErrCurrencyMismatch) {
		t.Error("mixed currencies must fail")
	}
	if _, err := MustNew(math.MaxInt64, "USD").Add(MustNew(1, "USD")); !errors.Is(err, ErrOverflow) {
		t.Error("add overflow not detected")
	}
	if _, err := MustNew(math.MaxInt64/2+1, "USD").Mul(2); !errors.Is(err, ErrOverflow) {
		t.Error("mul overflow not detected")
	}
	if _, err := MustNew(0, "USD").Sub(MustNew(math.MinInt64, "USD")); !errors.Is(err, ErrOverflow) {
		t.Error("sub of MinInt64 not detected")
	}
	line, _ := MustNew(1999, "USD").Mul(3)
	if line.Amount != 5997 {
		t.Errorf("19.99 × 3 = %v", line)
	}
	total, err := Sum(MustNew(5997, "USD"), MustNew(-500, "USD"), MustNew(3, "USD"))
	if err != nil || total.Amount != 5500 {
		t.Errorf("sum = %v, %v", total, err)
	}
	if _, err := Sum(); err == nil {
		t.Error("empty sum must fail")
	}
}
