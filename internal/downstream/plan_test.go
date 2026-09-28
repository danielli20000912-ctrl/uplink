package downstream

import (
	"math/big"
	"strings"
	"testing"

	"uplink/adapters/newapi"
	"uplink/internal/protocol"
)

func sp(s string) *string { return &s }

func catalog() map[string]protocol.Model {
	return map[string]protocol.Model{
		"m-ratio": {ID: "m-ratio", Price: protocol.Price{Unit: protocol.UnitPer1MTokens, Input: sp("1.25"), CachedInput: sp("0.125"), Output: sp("10")}},
		"m-tier": {ID: "m-tier", Price: protocol.Price{Unit: protocol.UnitPer1MTokens, Input: sp("1.5"), Output: sp("7.5"),
			Basis: "whole_request", Tiers: []protocol.Tier{{AboveTokens: 200000, Input: sp("3"), Output: sp("11.25")}}}},
		"m-fixed":  {ID: "m-fixed", Price: protocol.Price{Unit: protocol.UnitPerRequest, Price: sp("0.01")}},
		"m-custom": {ID: "m-custom", Price: protocol.Price{Unit: protocol.UnitCustom, Description: "x"}},
	}
}

func baseInput() PlanInput {
	return PlanInput{
		Upstreams: []UpstreamView{{Name: "up", Currency: "CNY", Catalog: catalog(), ChannelIDs: []int{1}, Markup: big.NewRat(13, 10)}},
		Channels: []newapi.Channel{{ID: 1, Status: 1, Models: []string{"gpt-x", "m-tier", "m-fixed", "m-custom", "m-missing"},
			ModelMapping: map[string]string{"gpt-x": "m-ratio"}}},
		Currency: "CNY", PricePerQuotaUSD: big.NewRat(1, 1), RoundTo: big.NewRat(1, 1_000_000),
		MinGroupRatio: big.NewRat(1, 1), MinMarginPct: big.NewRat(5, 1),
	}
}

func targets(p *Plan) map[string]Target {
	out := map[string]Target{}
	for _, t := range p.Targets {
		out[t.Model] = t
	}
	return out
}

func hasIssue(p *Plan, model, substr string) bool {
	for _, i := range p.Issues {
		if i.Model == model && strings.Contains(i.Message, substr) {
			return true
		}
	}
	return false
}

func TestPlanPricesMappedNames(t *testing.T) {
	p := BuildPlan(baseInput())
	ts := targets(p)
	x, ok := ts["gpt-x"]
	if !ok {
		t.Fatalf("mapped name not priced; issues %+v", p.Issues)
	}
	if x.Sources[0].UpstreamModel != "m-ratio" {
		t.Errorf("source %+v", x.Sources)
	}
	// 1.25*1.3 = 1.625; 0.125*1.3 = 0.1625; 10*1.3 = 13; omitted cache writes default to input.
	want := `tier("base", p * 1.625 + c * 13 + cr * 0.1625 + cc * 1.625 + cc1h * 1.625)`
	if x.Expr != want {
		t.Errorf("expr\n got %s\nwant %s", x.Expr, want)
	}
	if !strings.HasPrefix(ts["m-tier"].Expr, "len <= 200000 ? ") {
		t.Errorf("tier expr %s", ts["m-tier"].Expr)
	}
	if ts["m-fixed"].Expr != `tier("request", fixed(0.013))` {
		t.Errorf("fixed expr %s", ts["m-fixed"].Expr)
	}
	if _, ok := ts["m-custom"]; ok || !hasIssue(p, "m-custom", "cannot be synced") {
		t.Error("custom price must be skipped with an issue")
	}
	if !hasIssue(p, "m-missing", "does not list") {
		t.Error("missing upstream model must be reported")
	}
}

func TestPlanRefusesSharedNames(t *testing.T) {
	in := baseInput()
	in.Channels = append(in.Channels, newapi.Channel{ID: 2, Status: 1, Models: []string{"gpt-x"}})
	p := BuildPlan(in)
	if _, ok := targets(p)["gpt-x"]; ok || !hasIssue(p, "gpt-x", "non-uplink") {
		t.Error("name shared with a non-uplink channel must not be managed")
	}
	// A disabled foreign channel does not block.
	in.Channels[1].Status = 2
	if _, ok := targets(BuildPlan(in))["gpt-x"]; !ok {
		t.Error("disabled foreign channel should not block")
	}
}

func TestPlanMarginGuard(t *testing.T) {
	in := baseInput()
	in.MinGroupRatio = big.NewRat(1, 2) // 1.3 * 0.5 < 1.05
	p := BuildPlan(in)
	if len(p.Targets) != 0 || !hasIssue(p, "gpt-x", "below cost") {
		t.Errorf("margin guard: %d targets, issues %+v", len(p.Targets), p.Issues)
	}
}

func TestPlanCurrencyAndQuotaPrice(t *testing.T) {
	in := baseInput()
	in.Upstreams[0].Currency = "USD"
	if p := BuildPlan(in); len(p.Targets) != 0 || !hasIssue(p, "gpt-x", "no fx rate") {
		t.Error("missing fx must block")
	}
	in.FX = map[string]*big.Rat{"USD": big.NewRat(71, 10)}
	in.PricePerQuotaUSD = big.NewRat(2, 1) // customers pay 2 CNY per USD of quota
	x := targets(BuildPlan(in))["gpt-x"]
	// input 1.25 USD * 7.1 = 8.875 CNY cost; *1.3 / 2 = 5.76875 quota-USD
	if got := protocol.FormatDecimal(x.Sell[0].P[0]); got != "5.76875" {
		t.Errorf("sell input %s", got)
	}
}

func TestPlanTwoUpstreamsTakesHighest(t *testing.T) {
	in := baseInput()
	cat2 := catalog()
	m := cat2["m-ratio"]
	m.Price.Input = sp("2")
	cat2["m-ratio"] = m
	in.Upstreams = append(in.Upstreams, UpstreamView{Name: "up2", Currency: "CNY", Catalog: cat2, ChannelIDs: []int{3}, Markup: big.NewRat(11, 10)})
	in.Channels = append(in.Channels, newapi.Channel{ID: 3, Status: 1, Models: []string{"gpt-x"}, ModelMapping: map[string]string{"gpt-x": "m-ratio"}})
	x := targets(BuildPlan(in))["gpt-x"]
	// input: max(1.25*1.3=1.625, 2*1.1=2.2) = 2.2; output: max(13, 11) = 13
	if protocol.FormatDecimal(x.Sell[0].P[0]) != "2.2" || protocol.FormatDecimal(x.Sell[0].P[4]) != "13" || len(x.Sources) != 2 {
		t.Errorf("sell %v %v sources %d", x.Sell[0].P[0], x.Sell[0].P[4], len(x.Sources))
	}
}

func TestMaxChangePct(t *testing.T) {
	a := []level{{Above: -1, P: [5]*big.Rat{big.NewRat(1, 1), nil, nil, nil, big.NewRat(4, 1)}}}
	b := []level{{Above: -1, P: [5]*big.Rat{big.NewRat(11, 10), nil, nil, nil, big.NewRat(4, 1)}}}
	if got := MaxChangePct(a, b); got.Cmp(big.NewRat(10, 1)) != 0 {
		t.Errorf("got %v", got)
	}
	c := []level{{Above: -1}, {Above: 100}}
	if MaxChangePct(a, c).Cmp(big.NewRat(100, 1)) != 0 {
		t.Error("shape change must count as 100%")
	}
}
