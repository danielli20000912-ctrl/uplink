package newapi

import (
	"fmt"
	"math/big"
	"strings"

	"uplink/internal/protocol"
)

// WriteBillingExpr renders an uplink price as a New API billing expression in
// the canonical shape parseBillingExpr accepts. Every coefficient is passed
// through scale (currency and markup conversion into quota-USD) and then
// rounded up to a multiple of roundTo, so the result never undercharges.
// Every token variable is written explicitly, so New API's automatic
// exclusion rules cannot move tokens between buckets.
func WriteBillingExpr(p protocol.Price, scale func(*big.Rat) *big.Rat, roundTo *big.Rat) (string, error) {
	coef := func(s *string) (string, error) {
		v, err := protocol.ParseDecimal(*s)
		if err != nil {
			return "", err
		}
		return protocol.FormatDecimal(ceilTo(scale(v), roundTo)), nil
	}
	switch p.Unit {
	case protocol.UnitPerRequest:
		c, err := coef(p.Price)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`tier("request", fixed(%s))`, c), nil
	case protocol.UnitPer1MTokens:
	default:
		return "", fmt.Errorf("%w: price unit %q", errUnsupported, p.Unit)
	}
	if err := protocol.ValidatePrice(p); err != nil {
		return "", err
	}

	leaf := func(name string, in, cached, cw, cw1h, out *string) (string, error) {
		// Resolve omitted fields with the uplink defaults before writing them out.
		if cached == nil {
			cached = in
		}
		if cw == nil {
			cw = in
		}
		if cw1h == nil {
			cw1h = cw
		}
		var parts []string
		for _, t := range []struct {
			v   string
			val *string
		}{{"p", in}, {"c", out}, {"cr", cached}, {"cc", cw}, {"cc1h", cw1h}} {
			c, err := coef(t.val)
			if err != nil {
				return "", err
			}
			parts = append(parts, t.v+" * "+c)
		}
		return fmt.Sprintf(`tier(%q, %s)`, name, strings.Join(parts, " + ")), nil
	}

	levels := []string{}
	base, err := leaf("base", p.Input, p.CachedInput, p.CacheWrite, p.CacheWrite1h, p.Output)
	if err != nil {
		return "", err
	}
	levels = append(levels, base)
	for i, t := range p.Tiers {
		l, err := leaf(fmt.Sprintf("above_%d", t.AboveTokens), t.Input, t.CachedInput, t.CacheWrite, t.CacheWrite1h, t.Output)
		if err != nil {
			return "", err
		}
		_ = i
		levels = append(levels, l)
	}
	// len <= a1 ? L0 : (len <= a2 ? L1 : L2)
	expr := levels[len(levels)-1]
	for i := len(p.Tiers) - 1; i >= 0; i-- {
		expr = fmt.Sprintf("len <= %d ? %s : %s", p.Tiers[i].AboveTokens, levels[i], wrap(expr, i < len(p.Tiers)-1))
	}
	return expr, nil
}

func wrap(s string, nested bool) string {
	if nested {
		return "(" + s + ")"
	}
	return s
}

// ceilTo rounds v up to a multiple of step (step > 0).
func ceilTo(v, step *big.Rat) *big.Rat {
	if step == nil || step.Sign() <= 0 {
		return v
	}
	q := new(big.Rat).Quo(v, step)
	n := new(big.Int).Quo(q.Num(), q.Denom())
	if new(big.Rat).SetInt(n).Cmp(q) < 0 {
		n.Add(n, big.NewInt(1))
	}
	return new(big.Rat).Mul(new(big.Rat).SetInt(n), step)
}
