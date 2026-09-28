package protocol

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	_ "time/tzdata" // make IANA zones available on any host
)

// ErrNotComputable is returned for custom pricing, which v0 cannot express.
var ErrNotComputable = errors.New("uplink: price is not computable")

var million = big.NewRat(1_000_000, 1)

// CostInput is everything the formula needs besides the price.
type CostInput struct {
	CreatedAt time.Time
	Tokens    Tokens
	Units     int64 // per_request count; 0 means 1
}

// Cost computes the cost of one request exactly and rounds half-up once, at the end,
// to precision decimal places. See spec/README.md, section "Cost".
func Cost(p Price, tp *TimePricing, in CostInput, precision int) (string, error) {
	var raw *big.Rat
	switch p.Unit {
	case UnitCustom:
		return "", ErrNotComputable
	case UnitPerRequest:
		price, err := dec(p.Price, "price")
		if err != nil {
			return "", err
		}
		units := in.Units
		if units == 0 {
			units = 1
		}
		raw = new(big.Rat).Mul(price, big.NewRat(units, 1))
	case UnitPer1MTokens:
		lv, err := selectLevel(p, in.Tokens.Context())
		if err != nil {
			return "", err
		}
		t := in.Tokens
		raw = new(big.Rat)
		for _, term := range []struct {
			n     int64
			price *big.Rat
		}{
			{t.Input, lv.input},
			{t.CachedInput, lv.cachedInput},
			{t.CacheWrite, lv.cacheWrite},
			{t.CacheWrite1h, lv.cacheWrite1h},
			{t.Output, lv.output},
		} {
			raw.Add(raw, new(big.Rat).Mul(big.NewRat(term.n, 1), term.price))
		}
		raw.Quo(raw, million)
	default:
		return "", fmt.Errorf("uplink: unknown price unit %q", p.Unit)
	}

	m, err := TimeMultiplier(tp, in.CreatedAt)
	if err != nil {
		return "", err
	}
	return RoundHalfUp(raw.Mul(raw, m), precision), nil
}

type level struct {
	input, cachedInput, cacheWrite, cacheWrite1h, output *big.Rat
}

// selectLevel picks the tier with the largest above_tokens strictly below context,
// or the base prices when none qualifies, then fills omitted fields.
func selectLevel(p Price, context int64) (level, error) {
	fields := [5]*string{p.Input, p.CachedInput, p.CacheWrite, p.CacheWrite1h, p.Output}
	var best *Tier
	for i := range p.Tiers {
		t := &p.Tiers[i]
		if context > t.AboveTokens && (best == nil || t.AboveTokens > best.AboveTokens) {
			best = t
		}
	}
	if best != nil {
		fields = [5]*string{best.Input, best.CachedInput, best.CacheWrite, best.CacheWrite1h, best.Output}
	}

	var lv level
	var err error
	if lv.input, err = dec(fields[0], "input"); err != nil {
		return lv, err
	}
	if lv.output, err = dec(fields[4], "output"); err != nil {
		return lv, err
	}
	lv.cachedInput = orDefault(fields[1], lv.input)
	lv.cacheWrite = orDefault(fields[2], lv.input)
	lv.cacheWrite1h = orDefault(fields[3], lv.cacheWrite)
	for _, r := range []*big.Rat{lv.cachedInput, lv.cacheWrite, lv.cacheWrite1h} {
		if r == nil {
			return lv, errors.New("uplink: invalid decimal in price level")
		}
	}
	return lv, nil
}

// TimeMultiplier returns the multiplier of the window containing createdAt, or 1.
func TimeMultiplier(tp *TimePricing, createdAt time.Time) (*big.Rat, error) {
	one := big.NewRat(1, 1)
	if tp == nil {
		return one, nil
	}
	loc, err := time.LoadLocation(tp.Timezone)
	if err != nil {
		return nil, fmt.Errorf("uplink: timezone %q: %w", tp.Timezone, err)
	}
	local := createdAt.In(loc)
	if tp.WeekdaysOnly && (local.Weekday() == time.Saturday || local.Weekday() == time.Sunday) {
		return one, nil
	}
	// Wall-clock time of day, not elapsed time since midnight: they differ on DST days.
	sinceMidnight := time.Duration(local.Hour())*time.Hour +
		time.Duration(local.Minute())*time.Minute +
		time.Duration(local.Second())*time.Second +
		time.Duration(local.Nanosecond())
	for _, w := range tp.Windows {
		start, err := clock(w.Start)
		if err != nil {
			return nil, err
		}
		end, err := clock(w.End)
		if err != nil {
			return nil, err
		}
		if start <= sinceMidnight && sinceMidnight < end {
			m, ok := new(big.Rat).SetString(w.Multiplier)
			if !ok {
				return nil, fmt.Errorf("uplink: invalid multiplier %q", w.Multiplier)
			}
			return m, nil
		}
	}
	return one, nil
}

// RoundHalfUp formats a non-negative r with exactly precision decimal places.
func RoundHalfUp(r *big.Rat, precision int) string {
	sign := ""
	if r.Sign() < 0 {
		// Negative values (margins) round half away from zero.
		sign, r = "-", new(big.Rat).Neg(r)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision)), nil)
	// floor((num*scale*2 + den) / (2*den)); num >= 0 here
	num := new(big.Int).Mul(r.Num(), scale)
	num.Mul(num, big.NewInt(2))
	num.Add(num, r.Denom())
	den := new(big.Int).Mul(r.Denom(), big.NewInt(2))
	q := new(big.Int).Quo(num, den)
	if q.Sign() == 0 {
		sign = ""
	}

	s := q.String()
	if precision == 0 {
		return sign + s
	}
	if len(s) <= precision {
		s = strings.Repeat("0", precision-len(s)+1) + s
	}
	return sign + s[:len(s)-precision] + "." + s[len(s)-precision:]
}

func dec(s *string, name string) (*big.Rat, error) {
	if s == nil {
		return nil, fmt.Errorf("uplink: missing %s", name)
	}
	r, ok := new(big.Rat).SetString(*s)
	if !ok {
		return nil, fmt.Errorf("uplink: invalid decimal %s=%q", name, *s)
	}
	return r, nil
}

func orDefault(s *string, def *big.Rat) *big.Rat {
	if s == nil {
		return def
	}
	r, ok := new(big.Rat).SetString(*s)
	if !ok {
		return nil
	}
	return r
}

func clock(hhmm string) (time.Duration, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("uplink: invalid clock time %q", hhmm)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}
