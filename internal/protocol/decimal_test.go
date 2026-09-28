package protocol

import (
	"math/big"
	"testing"
)

func TestFormatDecimal(t *testing.T) {
	cases := []struct {
		r    *big.Rat
		want string
	}{
		{big.NewRat(5, 2), "2.5"},
		{big.NewRat(3, 1), "3"},
		{big.NewRat(1, 500000), "0.000002"},
		{big.NewRat(73, 5000000), "0.0000146"},
		{big.NewRat(1, 3), "0.333333333333"},
		{RatFromFloat(0.1), "0.1"},
		{RatFromFloat(1.25), "1.25"},
	}
	for _, c := range cases {
		if got := FormatDecimal(c.r); got != c.want {
			t.Errorf("%v: want %s, got %s", c.r, c.want, got)
		}
	}
	if DecimalPlaces(big.NewRat(73, 5000000)) != 7 || DecimalPlaces(big.NewRat(1, 3)) != -1 {
		t.Error("DecimalPlaces")
	}
}
