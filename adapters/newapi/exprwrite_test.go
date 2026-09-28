package newapi

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"uplink/internal/protocol"
)

var identity = func(v *big.Rat) *big.Rat { return v }

// Every price in the spec's vectors and examples must survive
// write -> parse with the same levels and coefficients.
func TestWriteBillingExprRoundTrip(t *testing.T) {
	var prices []protocol.Price
	var vf struct {
		Cases []struct {
			Price protocol.Price `json:"price"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile("../../spec/vectors/cost.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	for _, c := range vf.Cases {
		prices = append(prices, c.Price)
	}
	var cat protocol.Catalog
	raw, _ = os.ReadFile("../../spec/examples/catalog.json")
	if err := json.Unmarshal(raw, &cat); err != nil {
		t.Fatal(err)
	}
	for _, m := range cat.Models {
		prices = append(prices, m.Price)
	}

	tested := 0
	for _, p := range prices {
		expr, err := WriteBillingExpr(p, identity, big.NewRat(1, 1_000_000_000_000))
		if p.Unit == protocol.UnitCustom {
			if err == nil {
				t.Error("custom price written")
			}
			continue
		}
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		levels, err := parseBillingExpr(expr)
		if err != nil {
			t.Fatalf("written expression does not parse: %s: %v", expr, err)
		}
		if p.Unit == protocol.UnitPerRequest {
			want, _ := protocol.ParseDecimal(*p.Price)
			if len(levels) != 1 || levels[0].leaf.fixed == nil || levels[0].leaf.fixed.Cmp(want) != 0 {
				t.Errorf("per-request round trip: %s", expr)
			}
			tested++
			continue
		}
		back, err := translateExpr(expr, big.NewRat(1, 1), Money{QuotaPerUnit: big.NewRat(1, 1), PricePerUSD: big.NewRat(1, 1)})
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		// Compare costs on a spread of requests, which covers defaults and tiers.
		for _, tk := range []protocol.Tokens{
			{Input: 1000, CachedInput: 2000, CacheWrite: 300, CacheWrite1h: 400, Output: 500},
			{Input: 272001, Output: 10},
			{Input: 272000, CachedInput: 1, Output: 10},
		} {
			in := protocol.CostInput{Tokens: tk}
			a, err1 := protocol.Cost(p, nil, in, 9)
			b, err2 := protocol.Cost(back, nil, in, 9)
			if err1 != nil || err2 != nil || a != b {
				t.Errorf("round trip changed cost for %+v: %s vs %s (%v %v)\n%s", tk, a, b, err1, err2, expr)
			}
		}
		tested++
	}
	if tested < 10 {
		t.Fatalf("only %d prices tested", tested)
	}
}

func TestWriteBillingExprScalesAndRoundsUp(t *testing.T) {
	in, out := "1", "3"
	p := protocol.Price{Unit: protocol.UnitPer1MTokens, Input: &in, Output: &out}
	markup := func(v *big.Rat) *big.Rat { return new(big.Rat).Mul(v, big.NewRat(13, 30)) } // 1.3 / 3
	expr, err := WriteBillingExpr(p, markup, big.NewRat(1, 1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	// 1 * 1.3 / 3 = 0.4333... -> 0.433334; 3 * 1.3 / 3 = 1.3
	want := `tier("base", p * 0.433334 + c * 1.3 + cr * 0.433334 + cc * 0.433334 + cc1h * 0.433334)`
	if expr != want {
		t.Errorf("got  %s\nwant %s", expr, want)
	}
}
