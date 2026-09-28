package protocol

import (
	"encoding/json"
	"os"
	"testing"
)

func sp(s string) *string { return &s }

func TestValidateRejects(t *testing.T) {
	base := func() Price {
		return Price{Unit: UnitPer1MTokens, Input: sp("1"), Output: sp("2")}
	}
	dup := base()
	dup.Tiers = []Tier{{AboveTokens: 100, Input: sp("2"), Output: sp("3")}, {AboveTokens: 100, Input: sp("3"), Output: sp("4")}}
	desc := base()
	desc.Tiers = []Tier{{AboveTokens: 200, Input: sp("2"), Output: sp("3")}, {AboveTokens: 100, Input: sp("3"), Output: sp("4")}}
	noOut := Price{Unit: UnitPer1MTokens, Input: sp("1")}
	for name, p := range map[string]Price{"duplicate tiers": dup, "descending tiers": desc, "missing output": noOut, "unknown unit": {Unit: "per_hour"}} {
		if ValidatePrice(p) == nil {
			t.Errorf("%s: want error", name)
		}
	}

	for name, tp := range map[string]*TimePricing{
		"overlap":       {Timezone: "UTC", Windows: []TimeWindow{{"09:00", "12:00", "1.5"}, {"11:00", "13:00", "2"}}},
		"crosses night": {Timezone: "UTC", Windows: []TimeWindow{{"22:00", "02:00", "1.5"}}},
		"bad zone":      {Timezone: "Mars/Olympus", Windows: nil},
	} {
		if ValidateTimePricing(tp) == nil {
			t.Errorf("%s: want error", name)
		}
	}
	adjacent := &TimePricing{Timezone: "UTC", Windows: []TimeWindow{{"09:00", "12:00", "1.5"}, {"12:00", "13:00", "2"}}}
	if err := ValidateTimePricing(adjacent); err != nil {
		t.Errorf("adjacent windows: %v", err)
	}
}

// Every price shipped in the spec must pass semantic validation.
func TestSpecPricesValid(t *testing.T) {
	var vf vectorFile
	mustLoad(t, "../../spec/vectors/cost.json", &vf)
	for _, c := range vf.Cases {
		if err := ValidatePrice(c.Price); err != nil {
			t.Errorf("vector %q: %v", c.Name, err)
		}
		if err := ValidateTimePricing(c.TimePricing); err != nil {
			t.Errorf("vector %q: %v", c.Name, err)
		}
	}

	var cat struct {
		Models []struct {
			ID          string       `json:"id"`
			Price       Price        `json:"price"`
			TimePricing *TimePricing `json:"time_pricing"`
		} `json:"models"`
		PendingChanges []struct {
			Model       string       `json:"model"`
			Price       Price        `json:"price"`
			TimePricing *TimePricing `json:"time_pricing"`
		} `json:"pending_changes"`
	}
	mustLoad(t, "../../spec/examples/catalog.json", &cat)
	for _, m := range cat.Models {
		if err := ValidatePrice(m.Price); err != nil {
			t.Errorf("catalog %s: %v", m.ID, err)
		}
		if err := ValidateTimePricing(m.TimePricing); err != nil {
			t.Errorf("catalog %s: %v", m.ID, err)
		}
	}
	for _, pc := range cat.PendingChanges {
		if err := ValidatePrice(pc.Price); err != nil {
			t.Errorf("pending %s: %v", pc.Model, err)
		}
	}
}

func mustLoad(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}
