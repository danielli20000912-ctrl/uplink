package newapi

// Admin is a client for the downstream New API's admin API, used by
// `uplink sync`. It needs a root user's access token, because model pricing
// is root-only in New API. It reads channels without keys (New API omits the
// key column from channel lists), per-model pricing, and consume logs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrPricingConflict means the model's pricing changed since it was read.
var ErrPricingConflict = errors.New("newapi: model pricing changed since it was read")

type Admin struct {
	base   string
	token  string
	client *http.Client
}

func NewAdmin(baseURL, accessToken string) *Admin {
	return &Admin{base: strings.TrimRight(baseURL, "/"), token: accessToken, client: &http.Client{Timeout: 30 * time.Second}}
}

type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (a *Admin) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("newapi admin %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return resp.StatusCode, fmt.Errorf("newapi admin %s %s: HTTP %d, not JSON", method, path, resp.StatusCode)
	}
	if !env.Success {
		return resp.StatusCode, fmt.Errorf("newapi admin %s %s: HTTP %d: %s", method, path, resp.StatusCode, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("newapi admin %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// SiteStatus is the public GET /api/status subset uplink needs.
type SiteStatus struct {
	QuotaPerUnit float64 `json:"quota_per_unit"`
	Price        float64 `json:"price"`
}

func (a *Admin) Status(ctx context.Context) (SiteStatus, error) {
	var s SiteStatus
	_, err := a.do(ctx, http.MethodGet, "/api/status", nil, &s)
	if err == nil && s.QuotaPerUnit <= 0 {
		err = fmt.Errorf("newapi admin: status has no quota_per_unit")
	}
	return s, err
}

// QuotaPerUnit as an exact rational.
func (s SiteStatus) QuotaPerUnitRat() *big.Rat {
	r, _ := new(big.Rat).SetString(strconv.FormatFloat(s.QuotaPerUnit, 'f', -1, 64))
	return r
}

// Channel holds only the channel fields sync uses.
type Channel struct {
	ID           int
	Name         string
	Status       int
	Models       []string
	Groups       []string
	ModelMapping map[string]string // downstream name -> upstream name
}

// Enabled reports New API channel status 1.
func (c Channel) Enabled() bool { return c.Status == 1 }

// Channels lists every channel. Keys are never requested or kept.
func (a *Admin) Channels(ctx context.Context) ([]Channel, error) {
	var out []Channel
	for page := 1; ; page++ {
		var data struct {
			Items []struct {
				ID           int     `json:"id"`
				Name         string  `json:"name"`
				Status       int     `json:"status"`
				Models       string  `json:"models"`
				Group        string  `json:"group"`
				ModelMapping *string `json:"model_mapping"`
			} `json:"items"`
			Total int `json:"total"`
		}
		if _, err := a.do(ctx, http.MethodGet, fmt.Sprintf("/api/channel/?p=%d&page_size=100", page), nil, &data); err != nil {
			return nil, err
		}
		for _, it := range data.Items {
			c := Channel{ID: it.ID, Name: it.Name, Status: it.Status, Models: splitList(it.Models), Groups: splitList(it.Group)}
			if it.ModelMapping != nil && strings.TrimSpace(*it.ModelMapping) != "" {
				if err := json.Unmarshal([]byte(*it.ModelMapping), &c.ModelMapping); err != nil {
					return nil, fmt.Errorf("newapi admin: channel %d has invalid model_mapping", it.ID)
				}
			}
			out = append(out, c)
		}
		if len(data.Items) == 0 || len(out) >= data.Total {
			return out, nil
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// StoredPricing is one model's pricing as stored by New API.
type StoredPricing struct {
	ModelName  string         `json:"model_name"`
	Version    string         `json:"version"`
	Configured map[string]any `json:"configured"`
}

// Pricing returns the stored pricing of the named models.
func (a *Admin) Pricing(ctx context.Context, models []string) (map[string]StoredPricing, error) {
	q := url.Values{}
	for _, m := range models {
		q.Add("model", m)
	}
	var snap struct {
		Entries      []StoredPricing `json:"entries"`
		EmptyVersion string          `json:"empty_version"`
	}
	if _, err := a.do(ctx, http.MethodGet, "/api/option/model_pricing?"+q.Encode(), nil, &snap); err != nil {
		return nil, err
	}
	out := map[string]StoredPricing{}
	for _, e := range snap.Entries {
		out[e.ModelName] = e
	}
	for _, m := range models {
		if _, ok := out[m]; !ok {
			out[m] = StoredPricing{ModelName: m, Version: snap.EmptyVersion, Configured: map[string]any{}}
		}
	}
	return out, nil
}

// PricingChange replaces one model's pricing if its version still matches.
type PricingChange struct {
	ModelName       string         `json:"model_name"`
	ExpectedVersion string         `json:"expected_version"`
	Pricing         map[string]any `json:"pricing"`
}

// ExprPricing is the pricing value that selects a billing expression.
func ExprPricing(expr string) map[string]any {
	return map[string]any{"billing_setting.billing_mode": "tiered_expr", "billing_setting.billing_expr": expr}
}

// SetPricing applies changes atomically; ErrPricingConflict if any version moved.
func (a *Admin) SetPricing(ctx context.Context, changes []PricingChange) error {
	code, err := a.do(ctx, http.MethodPatch, "/api/option/model_pricing", map[string]any{"changes": changes}, nil)
	if code == http.StatusConflict {
		return ErrPricingConflict
	}
	return err
}

// DownstreamLog is one consume log with tokens normalized to uplink buckets.
type DownstreamLog struct {
	ID                int64
	CreatedAt         int64
	Model             string
	Quota             int64
	UpstreamRequestID string
	Buckets           Buckets
	Unmodeled         string
}

// Buckets are uplink's disjoint token buckets.
type Buckets struct{ Input, CachedInput, CacheWrite, CacheWrite1h, Output int64 }

// ConsumeLogs returns consume logs of one channel with start <= created_at <= end.
func (a *Admin) ConsumeLogs(ctx context.Context, channelID int, start, end int64) ([]DownstreamLog, error) {
	var out []DownstreamLog
	for page := 1; ; page++ {
		var data struct {
			Items []struct {
				ID                int64  `json:"id"`
				CreatedAt         int64  `json:"created_at"`
				ModelName         string `json:"model_name"`
				Quota             int64  `json:"quota"`
				PromptTokens      int64  `json:"prompt_tokens"`
				CompletionTokens  int64  `json:"completion_tokens"`
				UpstreamRequestID string `json:"upstream_request_id"`
				Other             any    `json:"other"`
			} `json:"items"`
			Total int `json:"total"`
		}
		path := fmt.Sprintf("/api/log/?type=%d&channel=%d&start_timestamp=%d&end_timestamp=%d&p=%d&page_size=100",
			logTypeConsume, channelID, start, end, page)
		if _, err := a.do(ctx, http.MethodGet, path, nil, &data); err != nil {
			return nil, err
		}
		for _, it := range data.Items {
			o := parseOther(it.Other)
			in, cached, cw, cw1h := normalize(it.PromptTokens, o)
			out = append(out, DownstreamLog{
				ID: it.ID, CreatedAt: it.CreatedAt, Model: it.ModelName, Quota: it.Quota,
				UpstreamRequestID: it.UpstreamRequestID, Unmodeled: unmodeled(o),
				Buckets: Buckets{in, cached, cw, cw1h, it.CompletionTokens},
			})
		}
		if len(data.Items) == 0 || len(out) >= data.Total {
			return out, nil
		}
	}
}

// parseOther reads only the fields uplink needs from a log's other field,
// which New API may return as a JSON string or an object.
func parseOther(v any) logOther {
	var o logOther
	var raw []byte
	switch t := v.(type) {
	case string:
		raw = []byte(t)
	case nil:
		return o
	default:
		raw, _ = json.Marshal(t)
	}
	_ = json.Unmarshal(raw, &o)
	return o
}
