package protocol

import (
	"fmt"
	"math/big"
	"strconv"
)

// MaxDecimalPlaces is the most fractional digits a wire decimal may carry.
const MaxDecimalPlaces = 12

// ParseDecimal parses a wire decimal string exactly.
func ParseDecimal(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("uplink: invalid decimal %q", s)
	}
	return r, nil
}

// RatFromFloat converts a float64 through its shortest decimal representation,
// matching how shopspring/decimal.NewFromFloat reads configuration ratios.
func RatFromFloat(f float64) *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	return r
}

// FormatDecimal renders a non-negative r with the fewest fractional digits that
// represent it exactly, up to MaxDecimalPlaces; beyond that it rounds half-up.
func FormatDecimal(r *big.Rat) string {
	for places := 0; places <= MaxDecimalPlaces; places++ {
		scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow10(places)))
		if scaled.IsInt() {
			return RoundHalfUp(r, places)
		}
	}
	return trimZeros(RoundHalfUp(r, MaxDecimalPlaces))
}

// DecimalPlaces returns how many fractional digits represent r exactly, or -1
// if more than MaxDecimalPlaces would be needed.
func DecimalPlaces(r *big.Rat) int {
	for places := 0; places <= MaxDecimalPlaces; places++ {
		if new(big.Rat).Mul(r, new(big.Rat).SetInt(pow10(places))).IsInt() {
			return places
		}
	}
	return -1
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func trimZeros(s string) string {
	i := len(s)
	for i > 0 && s[i-1] == '0' {
		i--
	}
	if i > 0 && s[i-1] == '.' {
		i--
	}
	return s[:i]
}
