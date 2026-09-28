package newapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"uplink/internal/protocol"
)

// PricingResponse is the subset of New API's GET /api/pricing that uplink reads.
// Called with a probe user's access token, it is already resolved for that
// user's group: models are filtered to its usable groups and group_ratio holds
// the ratios that group pays, including group-to-group overrides.
type PricingResponse struct {
	Success         bool                          `json:"success"`
	Message         string                        `json:"message"`
	Data            []PricingEntry                `json:"data"`
	GroupRatio      map[string]float64            `json:"group_ratio"`
	ModelGroupRatio map[string]map[string]float64 `json:"model_group_ratio"` // some forks: per-group, per-model override
	UsableGroup     map[string]string             `json:"usable_group"`
}

type PricingEntry struct {
	ModelName              string          `json:"model_name"`
	QuotaType              int             `json:"quota_type"`
	ModelRatio             float64         `json:"model_ratio"`
	ModelPrice             float64         `json:"model_price"`
	CompletionRatio        float64         `json:"completion_ratio"`
	CacheRatio             *float64        `json:"cache_ratio"`
	CreateCacheRatio       *float64        `json:"create_cache_ratio"`
	ImageRatio             *float64        `json:"image_ratio"`
	AudioRatio             *float64        `json:"audio_ratio"`
	EnableGroup            []string        `json:"enable_groups"`
	SupportedEndpointTypes []string        `json:"supported_endpoint_types"`
	BillingMode            string          `json:"billing_mode"`
	BillingExpr            string          `json:"billing_expr"`
	BillingPluginVariants  json.RawMessage `json:"billing_plugin_variants"`
}

// New API runtime defaults (setting/ratio_setting/cache_ratio.go) and the fixed
// 1-hour cache-write multiplier (relay/helper/price.go).
var (
	defaultCacheRatio       = big.NewRat(1, 1)
	defaultCreateCacheRatio = big.NewRat(5, 4)
	cacheWrite1hMultiplier  = big.NewRat(600, 375) // 6 / 3.75
	million                 = big.NewRat(1_000_000, 1)
)

// Money converts New API amounts to the publisher's currency.
type Money struct {
	QuotaPerUnit *big.Rat // New API QuotaPerUnit option: quota per USD
	PricePerUSD  *big.Rat // what the operator charges for one USD of quota
}

// usd converts a USD amount into the publisher's currency.
func (m Money) usd(v *big.Rat) *big.Rat { return new(big.Rat).Mul(v, m.PricePerUSD) }

// Quantum is the currency value of one quota unit.
func (m Money) Quantum() *big.Rat { return new(big.Rat).Quo(m.PricePerUSD, m.QuotaPerUnit) }

// BuildModels translates a pricing response into the catalog of a key whose
// requests run in usingGroup. Models not enabled for usingGroup are left out.
func BuildModels(resp *PricingResponse, usingGroup string, money Money) ([]protocol.Model, error) {
	gr, ok := resp.GroupRatio[usingGroup]
	if !ok {
		return nil, fmt.Errorf("newapi: group ratio not visible to the probe user")
	}
	models := make([]protocol.Model, 0, len(resp.Data))
	for _, e := range resp.Data {
		if !contains(e.EnableGroup, usingGroup) {
			continue
		}
		ratio := gr
		if over, ok := resp.ModelGroupRatio[usingGroup][e.ModelName]; ok {
			ratio = over
		}
		price := translateEntry(e, protocol.RatFromFloat(ratio), money)
		models = append(models, protocol.Model{
			ID:        e.ModelName,
			Endpoints: mapEndpoints(e.SupportedEndpointTypes),
			Price:     price,
			Status:    "available",
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func translateEntry(e PricingEntry, groupRatio *big.Rat, money Money) protocol.Price {
	if len(e.BillingPluginVariants) > 0 && string(e.BillingPluginVariants) != "null" {
		return custom("Billed per provider plugin variant.")
	}
	if e.BillingMode == "tiered_expr" && strings.TrimSpace(e.BillingExpr) != "" {
		p, err := translateExpr(e.BillingExpr, groupRatio, money)
		if err != nil {
			return custom("Billed by an expression that uplink v0 cannot express.")
		}
		return p
	}
	if e.QuotaType == 1 {
		// Fixed USD price per request, times the group ratio.
		v := new(big.Rat).Mul(protocol.RatFromFloat(e.ModelPrice), groupRatio)
		return protocol.Price{Unit: protocol.UnitPerRequest, Price: dstr(money.usd(v))}
	}
	return translateRatios(e, groupRatio, money)
}

// translateRatios: legacy ratio billing (service/text_quota.go).
// input USD per 1M = model_ratio * group_ratio * 1e6 / QuotaPerUnit.
func translateRatios(e PricingEntry, groupRatio *big.Rat, money Money) protocol.Price {
	input := new(big.Rat).Mul(protocol.RatFromFloat(e.ModelRatio), groupRatio)
	input.Mul(input, million)
	input.Quo(input, money.QuotaPerUnit)
	input = money.usd(input)

	cacheRatio := defaultCacheRatio
	if e.CacheRatio != nil {
		cacheRatio = protocol.RatFromFloat(*e.CacheRatio)
	}
	createRatio := defaultCreateCacheRatio
	if e.CreateCacheRatio != nil {
		createRatio = protocol.RatFromFloat(*e.CreateCacheRatio)
	}
	cacheWrite := new(big.Rat).Mul(input, createRatio)
	return protocol.Price{
		Unit:         protocol.UnitPer1MTokens,
		Input:        dstr(input),
		CachedInput:  dstr(new(big.Rat).Mul(input, cacheRatio)),
		CacheWrite:   dstr(cacheWrite),
		CacheWrite1h: dstr(new(big.Rat).Mul(cacheWrite, cacheWrite1hMultiplier)),
		Output:       dstr(new(big.Rat).Mul(input, protocol.RatFromFloat(e.CompletionRatio))),
	}
}

// translateExpr: expression coefficients are USD per 1M tokens (or USD per
// request for fixed), multiplied by the group ratio.
func translateExpr(src string, groupRatio *big.Rat, money Money) (protocol.Price, error) {
	levels, err := parseBillingExpr(src)
	if err != nil {
		return protocol.Price{}, err
	}
	scale := func(v *big.Rat) *big.Rat { return money.usd(new(big.Rat).Mul(v, groupRatio)) }

	if levels[0].leaf.fixed != nil {
		if len(levels) != 1 {
			return protocol.Price{}, fmt.Errorf("%w: length-conditional fixed price", errUnsupported)
		}
		return protocol.Price{Unit: protocol.UnitPerRequest, Price: dstr(scale(levels[0].leaf.fixed))}, nil
	}

	price := protocol.Price{Unit: protocol.UnitPer1MTokens, Basis: "whole_request"}
	for i, lv := range levels {
		if lv.leaf.fixed != nil {
			return protocol.Price{}, fmt.Errorf("%w: mixed fixed and token leaves", errUnsupported)
		}
		c := lv.leaf.coef
		in := scale(c["p"])
		// Omitted cr: cache reads stay in p (both usage semantics).
		// Omitted cc: OpenAI-semantics cache writes stay in p. (Anthropic-semantics
		// writes are then free; see docs/adapters/newapi.md.)
		// Omitted cc1h: 1-hour writes only exist with Anthropic semantics, where an
		// unreferenced cc1h is not billed, so publish 0.
		cached, write, write1h := in, in, new(big.Rat)
		if v := c["cr"]; v != nil {
			cached = scale(v)
		}
		if v := c["cc"]; v != nil {
			write = scale(v)
		}
		if v := c["cc1h"]; v != nil {
			write1h = scale(v)
		}
		fields := [5]*string{dstr(in), dstr(cached), dstr(write), dstr(write1h), dstr(scale(c["c"]))}
		if i == 0 {
			price.Input, price.CachedInput, price.CacheWrite, price.CacheWrite1h, price.Output = fields[0], fields[1], fields[2], fields[3], fields[4]
			continue
		}
		price.Tiers = append(price.Tiers, protocol.Tier{
			AboveTokens: lv.above,
			Input:       fields[0], CachedInput: fields[1], CacheWrite: fields[2], CacheWrite1h: fields[3], Output: fields[4],
		})
	}
	if len(price.Tiers) == 0 {
		price.Basis = ""
	}
	return price, nil
}

func custom(desc string) protocol.Price {
	return protocol.Price{Unit: protocol.UnitCustom, Description: desc}
}

func dstr(r *big.Rat) *string {
	s := protocol.FormatDecimal(r)
	return &s
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var endpointMap = map[string]string{
	"openai":                  protocol.EndpointChat,
	"openai-response":         protocol.EndpointResponses,
	"openai-response-compact": protocol.EndpointResponses,
	"anthropic":               protocol.EndpointMessages,
	"embeddings":              protocol.EndpointEmbeddings,
	"image-generation":        protocol.EndpointImages,
}

func mapEndpoints(types []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range types {
		e, ok := endpointMap[t]
		if !ok {
			e = protocol.EndpointOther
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		out = []string{protocol.EndpointChat}
	}
	sort.Strings(out)
	return out
}

// EndpointFromPath maps a request path recorded in New API logs to an uplink endpoint.
func EndpointFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return protocol.EndpointChat
	case strings.Contains(path, "/responses"):
		return protocol.EndpointResponses
	case strings.HasSuffix(path, "/messages"):
		return protocol.EndpointMessages
	case strings.HasSuffix(path, "/embeddings"):
		return protocol.EndpointEmbeddings
	case strings.Contains(path, "/images/"):
		return protocol.EndpointImages
	}
	return protocol.EndpointOther
}

// IsUnsupported reports whether err means the expression is outside the translated subset.
func IsUnsupported(err error) bool { return errors.Is(err, errUnsupported) }
