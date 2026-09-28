package downstream

import (
	"fmt"
	"math/big"
	"sort"

	"uplink/adapters/newapi"
	"uplink/internal/protocol"
)

// UpstreamView is one upstream as the planner sees it.
type UpstreamView struct {
	Name       string
	Currency   string
	Catalog    map[string]protocol.Model // by upstream model id
	ChannelIDs []int
	Markup     *big.Rat
}

type PlanInput struct {
	Upstreams        []UpstreamView
	Channels         []newapi.Channel
	Currency         string              // downstream currency
	PricePerQuotaUSD *big.Rat            // what downstream customers pay for one USD of quota
	FX               map[string]*big.Rat // upstream currency -> downstream currency per unit
	RoundTo          *big.Rat
	MinGroupRatio    *big.Rat
	MinMarginPct     *big.Rat
}

// Source is one route a downstream model name takes.
type Source struct {
	Upstream      string
	UpstreamModel string
	ChannelID     int
}

// Target is the price uplink wants on one downstream model name.
type Target struct {
	Model   string
	Sources []Source
	Unit    string
	Cost    []level // downstream currency, per 1M tokens or per request
	Sell    []level // quota-USD coefficients, rounded up
	Expr    string
}

// level holds one pricing level: resolved prices in bucket order, and the
// tier threshold (-1 for the base level).
type level struct {
	Above int64
	P     [5]*big.Rat // input, cached_input, cache_write, cache_write_1h, output; per_request uses P[0]
}

type Issue struct {
	Level   string // info | warn | alert
	Model   string
	Message string
}

type Plan struct {
	Targets []Target
	Issues  []Issue
}

func (p *Plan) issue(lvl, model, format string, a ...any) {
	p.Issues = append(p.Issues, Issue{Level: lvl, Model: model, Message: fmt.Sprintf(format, a...)})
}

// BuildPlan decides a price for every downstream model name served only by
// uplink upstreams. Names also served by any other enabled channel are left
// alone, because a New API model name has one price for the whole station.
func BuildPlan(in PlanInput) *Plan {
	plan := &Plan{}
	byChannel := map[int]int{}
	for i, u := range in.Upstreams {
		for _, id := range u.ChannelIDs {
			byChannel[id] = i
		}
	}

	type candidate struct {
		src   Source
		model protocol.Model
		up    int
	}
	candidates := map[string][]candidate{}
	foreign := map[string][]int{}
	seen := map[int]bool{}
	for _, c := range in.Channels {
		ui, isUplink := byChannel[c.ID]
		if !c.Enabled() {
			if isUplink {
				plan.issue("warn", "", "channel #%d of upstream %s is disabled", c.ID, in.Upstreams[ui].Name)
				seen[c.ID] = true
			}
			continue
		}
		if !isUplink {
			for _, n := range c.Models {
				foreign[n] = append(foreign[n], c.ID)
			}
			continue
		}
		seen[c.ID] = true
		u := in.Upstreams[ui]
		for _, n := range c.Models {
			m := n
			if mapped, ok := c.ModelMapping[n]; ok && mapped != "" {
				m = mapped
			}
			model, ok := u.Catalog[m]
			if !ok {
				plan.issue("warn", n, "channel #%d offers %s, but upstream %s does not list %s", c.ID, n, u.Name, m)
				continue
			}
			candidates[n] = append(candidates[n], candidate{Source{u.Name, m, c.ID}, model, ui})
		}
	}
	for id := range byChannel {
		if !seen[id] {
			plan.issue("alert", "", "channel #%d is configured for an upstream but does not exist", id)
		}
	}

	names := make([]string, 0, len(candidates))
	for n := range candidates {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		cs := candidates[n]
		if ids := foreign[n]; len(ids) > 0 {
			plan.issue("warn", n, "also served by non-uplink channel(s) %v; its price is not managed", ids)
			continue
		}
		t := Target{Model: n}
		ok := true
		for _, c := range cs {
			t.Sources = append(t.Sources, c.src)
			cost, sell, unit, err := convert(c.model, in.Upstreams[c.up], in)
			if err != nil {
				plan.issue("warn", n, "not priced: %v", err)
				ok = false
				break
			}
			if t.Unit == "" {
				t.Unit, t.Cost, t.Sell = unit, cost, sell
				continue
			}
			if t.Unit != unit || !sameShape(t.Cost, cost) {
				plan.issue("warn", n, "not priced: upstreams price it in different shapes")
				ok = false
				break
			}
			t.Cost, t.Sell = maxLevels(t.Cost, cost), maxLevels(t.Sell, sell)
		}
		if !ok {
			continue
		}
		if msg := marginViolation(t, in); msg != "" {
			plan.issue("alert", n, "not priced: %s", msg)
			continue
		}
		expr, err := newapi.WriteBillingExpr(toPrice(t.Unit, t.Sell), func(v *big.Rat) *big.Rat { return v }, in.RoundTo)
		if err != nil {
			plan.issue("warn", n, "not priced: %v", err)
			continue
		}
		t.Expr = expr
		plan.Targets = append(plan.Targets, t)
	}
	return plan
}

// convert returns the cost in downstream currency and the sell coefficients
// in quota-USD (rounded up), for one upstream model.
func convert(m protocol.Model, u UpstreamView, in PlanInput) (cost, sell []level, unit string, err error) {
	if m.TimePricing != nil {
		return nil, nil, "", fmt.Errorf("time-of-day pricing is not supported by sync v0")
	}
	if err := protocol.ValidatePrice(m.Price); err != nil {
		return nil, nil, "", err
	}
	fx := big.NewRat(1, 1)
	if u.Currency != in.Currency {
		r, ok := in.FX[u.Currency]
		if !ok {
			return nil, nil, "", fmt.Errorf("no fx rate from %s to %s", u.Currency, in.Currency)
		}
		fx = r
	}
	switch m.Price.Unit {
	case protocol.UnitPerRequest:
		v, err := protocol.ParseDecimal(*m.Price.Price)
		if err != nil {
			return nil, nil, "", err
		}
		cost = []level{{Above: -1, P: [5]*big.Rat{mul(v, fx)}}}
	case protocol.UnitPer1MTokens:
		base, err := resolve(m.Price.Input, m.Price.CachedInput, m.Price.CacheWrite, m.Price.CacheWrite1h, m.Price.Output)
		if err != nil {
			return nil, nil, "", err
		}
		cost = append(cost, level{Above: -1, P: scaleAll(base, fx)})
		for _, t := range m.Price.Tiers {
			lv, err := resolve(t.Input, t.CachedInput, t.CacheWrite, t.CacheWrite1h, t.Output)
			if err != nil {
				return nil, nil, "", err
			}
			cost = append(cost, level{Above: t.AboveTokens, P: scaleAll(lv, fx)})
		}
	default:
		return nil, nil, "", fmt.Errorf("%s pricing cannot be synced", m.Price.Unit)
	}
	factor := new(big.Rat).Quo(u.Markup, in.PricePerQuotaUSD)
	for _, lv := range cost {
		s := level{Above: lv.Above}
		for i, v := range lv.P {
			if v != nil {
				s.P[i] = ceil(mul(v, factor), in.RoundTo)
			}
		}
		sell = append(sell, s)
	}
	return cost, sell, m.Price.Unit, nil
}

// resolve applies uplink's defaults for omitted prices.
func resolve(in, cached, cw, cw1h, out *string) ([5]*big.Rat, error) {
	var r [5]*big.Rat
	var err error
	if r[0], err = protocol.ParseDecimal(*in); err != nil {
		return r, err
	}
	if r[4], err = protocol.ParseDecimal(*out); err != nil {
		return r, err
	}
	pick := func(s *string, def *big.Rat) (*big.Rat, error) {
		if s == nil {
			return def, nil
		}
		return protocol.ParseDecimal(*s)
	}
	if r[1], err = pick(cached, r[0]); err != nil {
		return r, err
	}
	if r[2], err = pick(cw, r[0]); err != nil {
		return r, err
	}
	r[3], err = pick(cw1h, r[2])
	return r, err
}

func marginViolation(t Target, in PlanInput) string {
	need := new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(in.MinMarginPct, big.NewRat(100, 1)))
	for i := range t.Cost {
		for j, c := range t.Cost[i].P {
			if c == nil || c.Sign() == 0 {
				continue
			}
			// Customer pays sell * group ratio * price per quota USD.
			paid := mul(mul(t.Sell[i].P[j], in.MinGroupRatio), in.PricePerQuotaUSD)
			if paid.Cmp(mul(c, need)) < 0 {
				return fmt.Sprintf("at the lowest group ratio %s the price is below cost plus %s%% margin",
					protocol.FormatDecimal(in.MinGroupRatio), protocol.FormatDecimal(in.MinMarginPct))
			}
		}
	}
	return ""
}

func toPrice(unit string, lv []level) protocol.Price {
	s := func(r *big.Rat) *string { v := protocol.FormatDecimal(r); return &v }
	if unit == protocol.UnitPerRequest {
		return protocol.Price{Unit: unit, Price: s(lv[0].P[0])}
	}
	p := protocol.Price{Unit: unit, Input: s(lv[0].P[0]), CachedInput: s(lv[0].P[1]), CacheWrite: s(lv[0].P[2]),
		CacheWrite1h: s(lv[0].P[3]), Output: s(lv[0].P[4])}
	for _, l := range lv[1:] {
		p.Tiers = append(p.Tiers, protocol.Tier{AboveTokens: l.Above, Input: s(l.P[0]), CachedInput: s(l.P[1]),
			CacheWrite: s(l.P[2]), CacheWrite1h: s(l.P[3]), Output: s(l.P[4])})
	}
	if len(p.Tiers) > 0 {
		p.Basis = "whole_request"
	}
	return p
}

func sameShape(a, b []level) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Above != b[i].Above {
			return false
		}
	}
	return true
}

func maxLevels(a, b []level) []level {
	out := make([]level, len(a))
	for i := range a {
		out[i].Above = a[i].Above
		for j := range a[i].P {
			switch {
			case a[i].P[j] == nil:
				out[i].P[j] = b[i].P[j]
			case b[i].P[j] == nil || a[i].P[j].Cmp(b[i].P[j]) >= 0:
				out[i].P[j] = a[i].P[j]
			default:
				out[i].P[j] = b[i].P[j]
			}
		}
	}
	return out
}

// MaxChangePct is the largest relative change between two sell price sets, in
// percent; 100 if their shapes differ.
func MaxChangePct(old, cur []level) *big.Rat {
	if !sameShape(old, cur) {
		return big.NewRat(100, 1)
	}
	max := new0()
	for i := range old {
		for j := range old[i].P {
			o, n := old[i].P[j], cur[i].P[j]
			if o == nil || n == nil {
				continue
			}
			if o.Sign() == 0 {
				if n.Sign() != 0 {
					return big.NewRat(100, 1)
				}
				continue
			}
			d := new(big.Rat).Sub(n, o)
			d.Abs(d)
			d.Quo(d, o)
			d.Mul(d, big.NewRat(100, 1))
			if d.Cmp(max) > 0 {
				max = d
			}
		}
	}
	return max
}

func new0() *big.Rat { return new(big.Rat) }

func mul(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }

func scaleAll(p [5]*big.Rat, f *big.Rat) [5]*big.Rat {
	var out [5]*big.Rat
	for i, v := range p {
		if v != nil {
			out[i] = mul(v, f)
		}
	}
	return out
}

// ceil rounds v up to a multiple of step.
func ceil(v, step *big.Rat) *big.Rat {
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
