package newapi

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"uplink/internal/protocol"
)

var testMoney = Money{QuotaPerUnit: big.NewRat(500000, 1), PricePerUSD: big.NewRat(1, 1)}

const pricingFixture = `{
  "success": true,
  "data": [
    {"model_name": "ratio-model", "quota_type": 0, "model_ratio": 1.25, "completion_ratio": 8, "cache_ratio": 0.1,
     "enable_groups": ["partner", "default"], "supported_endpoint_types": ["openai", "openai-response"]},
    {"model_name": "price-model", "quota_type": 1, "model_price": 0.04, "enable_groups": ["partner"],
     "supported_endpoint_types": ["image-generation"]},
    {"model_name": "expr-model", "quota_type": 0, "model_ratio": 37.5, "completion_ratio": 1,
     "billing_mode": "tiered_expr",
     "billing_expr": "len <= 200000 ? tier(\"standard\", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6) : tier(\"long_context\", p * 6 + c * 22.5 + cr * 0.6 + cc * 7.5 + cc1h * 12)",
     "enable_groups": ["partner"], "supported_endpoint_types": ["anthropic"]},
    {"model_name": "rules-model", "quota_type": 0, "model_ratio": 1, "completion_ratio": 1, "billing_mode": "tiered_expr",
     "billing_expr": "tier(\"base\", p * 5 + c * 25)|||when(header(\"x\") has \"y\") * 6",
     "enable_groups": ["partner"], "supported_endpoint_types": ["openai"]},
    {"model_name": "hidden-model", "quota_type": 0, "model_ratio": 1, "completion_ratio": 1,
     "enable_groups": ["vip"], "supported_endpoint_types": ["openai"]},
    {"model_name": "override-model", "quota_type": 0, "model_ratio": 1, "completion_ratio": 2,
     "enable_groups": ["partner"], "supported_endpoint_types": ["openai"]}
  ],
  "group_ratio": {"partner": 0.5, "default": 1},
  "model_group_ratio": {"partner": {"override-model": 0.75}},
  "usable_group": {"partner": "", "default": ""}
}`

func buildFixture(t *testing.T) map[string]protocol.Model {
	t.Helper()
	var resp PricingResponse
	if err := json.Unmarshal([]byte(pricingFixture), &resp); err != nil {
		t.Fatal(err)
	}
	models, err := BuildModels(&resp, "partner", testMoney)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]protocol.Model{}
	for _, m := range models {
		if err := protocol.ValidatePrice(m.Price); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
		out[m.ID] = m
	}
	return out
}

func s(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestBuildModelsRatio(t *testing.T) {
	m := buildFixture(t)
	if _, ok := m["hidden-model"]; ok {
		t.Error("model outside the using group must be left out")
	}
	p := m["ratio-model"].Price
	// 1.25 * 0.5 * 1e6 / 500000 = 1.25 USD/1M; completion 8; cache 0.1; create default 1.25; 1h x1.6
	got := []string{s(p.Input), s(p.Output), s(p.CachedInput), s(p.CacheWrite), s(p.CacheWrite1h)}
	want := []string{"1.25", "10", "0.125", "1.5625", "2.5"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ratio-model field %d: got %s, want %s", i, got[i], want[i])
		}
	}
	if eps := m["ratio-model"].Endpoints; len(eps) != 2 || eps[0] != "chat.completions" || eps[1] != "responses" {
		t.Errorf("endpoints %v", eps)
	}
	if o := m["override-model"].Price; s(o.Input) != "1.5" || s(o.Output) != "3" {
		t.Errorf("per-model group override: input %s output %s", s(o.Input), s(o.Output))
	}
	if pp := m["price-model"].Price; pp.Unit != protocol.UnitPerRequest || s(pp.Price) != "0.02" {
		t.Errorf("price-model: %+v", pp)
	}
	if m["rules-model"].Price.Unit != protocol.UnitCustom {
		t.Error("request-rule expression must be custom")
	}
}

func TestBuildModelsExprCost(t *testing.T) {
	m := buildFixture(t)
	p := m["expr-model"].Price
	if len(p.Tiers) != 1 || p.Tiers[0].AboveTokens != 200000 {
		t.Fatalf("tiers %+v", p.Tiers)
	}
	// Context 250,000 > 200,000 -> long tier:
	// (10000*6 + 200000*0.6 + 40000*7.5 + 1000*22.5)/1e6 = 0.5025 USD, x group 0.5 = 0.25125
	cost, err := protocol.Cost(p, nil, protocol.CostInput{
		CreatedAt: time.Now(),
		Tokens:    protocol.Tokens{Input: 10000, CachedInput: 200000, CacheWrite: 40000, Output: 1000},
	}, 6)
	if err != nil {
		t.Fatal(err)
	}
	if cost != "0.251250" {
		t.Errorf("cost %s, want 0.251250", cost)
	}
}
