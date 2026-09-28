package protocol

import (
	"fmt"
	"math/big"
	"sort"
	"time"
)

// ValidatePrice checks the rules in spec/README.md that JSON Schema cannot express.
func ValidatePrice(p Price) error {
	switch p.Unit {
	case UnitCustom:
		if p.Description == "" {
			return fmt.Errorf("uplink: custom price needs a description")
		}
	case UnitPerRequest:
		if _, err := dec(p.Price, "price"); err != nil {
			return err
		}
	case UnitPer1MTokens:
		if p.Basis != "" && p.Basis != "whole_request" {
			return fmt.Errorf("uplink: unknown basis %q", p.Basis)
		}
		if _, err := selectLevel(p, 0); err != nil {
			return err
		}
		for i, t := range p.Tiers {
			if i > 0 && t.AboveTokens <= p.Tiers[i-1].AboveTokens {
				return fmt.Errorf("uplink: tiers must have strictly increasing above_tokens (%d after %d)", t.AboveTokens, p.Tiers[i-1].AboveTokens)
			}
			if _, err := selectLevel(p, t.AboveTokens+1); err != nil {
				return fmt.Errorf("uplink: tier above %d: %w", t.AboveTokens, err)
			}
		}
	default:
		return fmt.Errorf("uplink: unknown price unit %q", p.Unit)
	}
	return nil
}

// ValidateTimePricing checks that the zone exists, every window has start < end,
// multipliers parse, and windows do not overlap.
func ValidateTimePricing(tp *TimePricing) error {
	if tp == nil {
		return nil
	}
	if _, err := time.LoadLocation(tp.Timezone); err != nil {
		return fmt.Errorf("uplink: timezone %q: %w", tp.Timezone, err)
	}
	type span struct{ start, end time.Duration }
	spans := make([]span, 0, len(tp.Windows))
	for _, w := range tp.Windows {
		start, err := clock(w.Start)
		if err != nil {
			return err
		}
		end, err := clock(w.End)
		if err != nil {
			return err
		}
		if start >= end {
			return fmt.Errorf("uplink: window %s-%s must have start < end; split windows that cross midnight", w.Start, w.End)
		}
		if _, ok := new(big.Rat).SetString(w.Multiplier); !ok {
			return fmt.Errorf("uplink: invalid multiplier %q", w.Multiplier)
		}
		spans = append(spans, span{start, end})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return fmt.Errorf("uplink: time windows overlap")
		}
	}
	return nil
}
