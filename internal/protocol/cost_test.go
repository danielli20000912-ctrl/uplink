package protocol

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"testing"
	"time"
)

type vectorFile struct {
	Cases []struct {
		Name          string       `json:"name"`
		Price         Price        `json:"price"`
		TimePricing   *TimePricing `json:"time_pricing"`
		CostPrecision int          `json:"cost_precision"`
		CreatedAt     string       `json:"created_at"`
		Tokens        Tokens       `json:"tokens"`
		Units         int64        `json:"units"`
		Expect        struct {
			Cost  string `json:"cost"`
			Error string `json:"error"`
		} `json:"expect"`
	} `json:"cases"`
}

func TestCostVectors(t *testing.T) {
	raw, err := os.ReadFile("../../spec/vectors/cost.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	if len(vf.Cases) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range vf.Cases {
		t.Run(c.Name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, c.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Cost(c.Price, c.TimePricing, CostInput{CreatedAt: at, Tokens: c.Tokens, Units: c.Units}, c.CostPrecision)
			if c.Expect.Error == "not_computable" {
				if !errors.Is(err, ErrNotComputable) {
					t.Fatalf("want ErrNotComputable, got %q, %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.Expect.Cost {
				t.Fatalf("want %s, got %s", c.Expect.Cost, got)
			}
		})
	}
}

func TestRoundHalfUp(t *testing.T) {
	cases := []struct {
		num, den int64
		prec     int
		want     string
	}{
		{25, 10, 0, "3"},
		{24, 10, 0, "2"},
		{1, 3, 4, "0.3333"},
		{2, 3, 4, "0.6667"},
		{5, 1000, 2, "0.01"},
		{0, 1, 6, "0.000000"},
		{123456, 1, 2, "123456.00"},
	}
	for _, c := range cases {
		if got := RoundHalfUp(ratOf(c.num, c.den), c.prec); got != c.want {
			t.Errorf("%d/%d @%d: want %s, got %s", c.num, c.den, c.prec, c.want, got)
		}
	}
}

func ratOf(n, d int64) *big.Rat { return big.NewRat(n, d) }
