package newapi

import (
	"math/big"
	"testing"
)

func TestParseBillingExprLevels(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		aboves []int64
		pIn    []string // p coefficient per level
	}{
		{"flat", `tier("base", p * 2.5 + c * 15 + cr * 0.25)`, []int64{-1}, []string{"5/2"}},
		{"two tiers", `len <= 200000
  ? tier("standard", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6)
  : tier("long_context", p * 6 + c * 22.5 + cr * 0.6 + cc * 7.5 + cc1h * 12)`, []int64{-1, 200000}, []string{"3", "6"}},
		{"strict less and version prefix", `v1:len < 272001 ? tier("a", p*1 + c*2) : tier("b", 2*p + 4*c)`, []int64{-1, 272000}, []string{"1", "2"}},
		{"swapped greater", `len > 200000 ? tier("long", p*6 + c*22.5) : tier("std", p*3 + c*15)`, []int64{-1, 200000}, []string{"3", "6"}},
		{"three levels nested", `len <= 32000 ? tier("a", p*1+c*2) : (len <= 128000 ? tier("b", p*2+c*4) : tier("c", p*3+c*6))`, []int64{-1, 32000, 128000}, []string{"1", "2", "3"}},
		{"equal adjacent leaves merge", `len <= 1000 ? tier("a", p*1+c*2) : tier("b", p*1+c*2)`, []int64{-1}, []string{"1"}},
		{"bare variable", `tier("base", p + c * 4)`, []int64{-1}, []string{"1"}},
		{"fixed", `tier("request", fixed(0.01))`, []int64{-1}, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			levels, err := parseBillingExpr(c.src)
			if err != nil {
				t.Fatal(err)
			}
			if len(levels) != len(c.aboves) {
				t.Fatalf("want %d levels, got %d", len(c.aboves), len(levels))
			}
			for i, lv := range levels {
				if lv.above != c.aboves[i] {
					t.Errorf("level %d: above %d, want %d", i, lv.above, c.aboves[i])
				}
				if c.pIn[i] == "" {
					continue
				}
				want, _ := new(big.Rat).SetString(c.pIn[i])
				if lv.leaf.coef["p"].Cmp(want) != 0 {
					t.Errorf("level %d: p=%v, want %v", i, lv.leaf.coef["p"], want)
				}
			}
		})
	}
}

func TestParseBillingExprUnsupported(t *testing.T) {
	for _, src := range []string{
		`tier("base", p * 2 + c * 8 + img * 2.5)`,
		`tier("base", p * 5 + c * 25)|||when(header("anthropic-beta") has "fast-mode") * 6`,
		`hour("Asia/Shanghai") < 8 ? tier("night", p*1+c*2) : tier("day", p*2+c*4)`,
		`param("service_tier") == "flex" ? tier("flex", p*1+c*2) : tier("std", p*2+c*4)`,
		`tier("base", p * 2 * 1.5 + c * 8)`,
		`tier("base", p * 2 + c * 8) * 1.1`,
		`tier("a", p*1+c*2) + tier("b", p*1+c*2)`,
		`tier("base", c * 8)`,
		`v2:tier("base", p * 2 + c * 8)`,
		`tier("base", p * 1e-3 + c * 8)`,
	} {
		if _, err := parseBillingExpr(src); err == nil || !IsUnsupported(err) {
			t.Errorf("%s: want unsupported, got %v", src, err)
		}
	}
}
