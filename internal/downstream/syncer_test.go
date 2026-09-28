package downstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"uplink/internal/protocol"
)

// fakeNewAPI is the part of New API's admin API that sync uses. Versions
// are a hash of the stored pricing, as in New API.
type fakeNewAPI struct {
	mu       sync.Mutex
	pricing  map[string]map[string]any
	channels []map[string]any
	logs     []map[string]any
	patches  int
	// afterPatch runs after a successful write, as if the operator saved
	// pricing in the admin UI right after sync did.
	afterPatch func(pricing map[string]map[string]any)
}

func version(v map[string]any) string {
	if v == nil {
		v = map[string]any{}
	}
	b, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func ok(w http.ResponseWriter, data any) {
	json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (f *fakeNewAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/status":
		ok(w, map[string]any{"quota_per_unit": 500000, "price": 7.3})
	case r.URL.Path == "/api/channel/":
		ok(w, map[string]any{"items": f.channels, "total": len(f.channels)})
	case r.URL.Path == "/api/log/":
		var items []map[string]any
		for _, l := range f.logs {
			if fmt.Sprint(l["channel"]) == r.URL.Query().Get("channel") {
				items = append(items, l)
			}
		}
		ok(w, map[string]any{"items": items, "total": len(items)})
	case r.URL.Path == "/api/option/model_pricing" && r.Method == http.MethodGet:
		var entries []map[string]any
		for _, m := range r.URL.Query()["model"] {
			if p, found := f.pricing[m]; found {
				entries = append(entries, map[string]any{"model_name": m, "version": version(p), "configured": p})
			}
		}
		ok(w, map[string]any{"entries": entries, "empty_version": version(nil)})
	case r.URL.Path == "/api/option/model_pricing" && r.Method == http.MethodPatch:
		var req struct {
			Changes []struct {
				ModelName       string         `json:"model_name"`
				ExpectedVersion string         `json:"expected_version"`
				Pricing         map[string]any `json:"pricing"`
			} `json:"changes"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		for _, c := range req.Changes {
			if version(f.pricing[c.ModelName]) != c.ExpectedVersion {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "conflict"})
				return
			}
		}
		for _, c := range req.Changes {
			f.pricing[c.ModelName] = c.Pricing
		}
		f.patches++
		if f.afterPatch != nil {
			f.afterPatch(f.pricing)
			f.afterPatch = nil
		}
		ok(w, nil)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeNewAPI) expr(model string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, _ := f.pricing[model]["billing_setting.billing_expr"].(string)
	return s
}

// fakeUpstream is an uplink publisher with a mutable catalog and usage.
type fakeUpstream struct {
	mu      sync.Mutex
	input   string // price of up-model, CNY per 1M input tokens
	records []protocol.Record
}

func (u *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/uplink.json":
		json.NewEncoder(w).Encode(protocol.Discovery{Protocol: "uplink", Versions: []string{"v0"}, Currency: "CNY", CostPrecision: 6,
			UsageLagSeconds: 0, Endpoints: map[string]string{"catalog": "/c", "account": "/a", "usage": "/u", "status": "/s"}})
	case "/c":
		json.NewEncoder(w).Encode(protocol.Catalog{CatalogVersion: u.input, Currency: "CNY", Models: []protocol.Model{{ID: "up-model",
			Endpoints: []string{"chat.completions"}, Status: "available",
			Price: protocol.Price{Unit: protocol.UnitPer1MTokens, Input: sp(u.input), Output: sp("8")}}}})
	case "/a":
		json.NewEncoder(w).Encode(protocol.Account{Currency: "CNY", Unlimited: true})
	case "/u":
		json.NewEncoder(w).Encode(protocol.UsagePage{Records: u.records})
	default:
		http.NotFound(w, r)
	}
}

func (u *fakeUpstream) setInput(p string) {
	u.mu.Lock()
	u.input = p
	u.mu.Unlock()
}

type rig struct {
	t    *testing.T
	na   *fakeNewAPI
	up   *fakeUpstream
	s    *Syncer
	out  *bytes.Buffer
	orig map[string]any
}

func newRig(t *testing.T, apply string) *rig {
	orig := map[string]any{"ModelRatio": 1.5, "CompletionRatio": 4.0}
	na := &fakeNewAPI{
		pricing: map[string]map[string]any{"sold": orig},
		channels: []map[string]any{{"id": 7, "name": "uplink", "status": 1, "models": "sold", "group": "default",
			"model_mapping": `{"sold":"up-model"}`}},
	}
	up := &fakeUpstream{input: "2"}
	nsrv := httptest.NewServer(na)
	usrv := httptest.NewServer(up)
	t.Cleanup(nsrv.Close)
	t.Cleanup(usrv.Close)
	t.Setenv("T_ADMIN", "admin-token")
	t.Setenv("T_KEY", "sk-test")

	var cfg Config
	cfg.Downstream.BaseURL = nsrv.URL
	cfg.Downstream.AdminTokenEnv = "T_ADMIN"
	cfg.Downstream.Currency = "CNY"
	cfg.Downstream.PricePerQuotaUSD = "7.3"
	cfg.Upstreams = []UpstreamConfig{{Name: "up", BaseURL: usrv.URL, KeyEnv: "T_KEY", ChannelIDs: []int{7}, Markup: "1.3"}}
	cfg.Pricing.Apply = apply
	cfg.Reconcile.MarginAlertPct = "10"
	cfg.StateDB = filepath.Join(t.TempDir(), "state.db")
	out := &bytes.Buffer{}
	s, err := New(cfg, out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return &rig{t: t, na: na, up: up, s: s, out: out, orig: orig}
}

func (r *rig) once(act Actions) string {
	r.t.Helper()
	r.out.Reset()
	if err := r.s.Once(context.Background(), act); err != nil {
		r.t.Fatal(err)
	}
	return r.out.String()
}

func TestSyncNeedsExplicitAdoption(t *testing.T) {
	r := newRig(t, "auto")
	out := r.once(Actions{})
	if !strings.Contains(out, "TODO  sold") || r.na.patches != 0 {
		t.Fatalf("wrote without -adopt (patches %d):\n%s", r.na.patches, out)
	}
	out = r.once(Actions{Adopt: []string{"all"}})
	if !strings.Contains(out, "DONE  sold adopted") || r.na.expr("sold") == "" {
		t.Fatalf("adopt did not write:\n%s", out)
	}
	m, _ := r.s.state.AllManaged()
	if m["sold"] == nil || m["sold"].Original["ModelRatio"] != 1.5 || m["sold"].WrittenVersion != version(r.na.pricing["sold"]) {
		t.Fatalf("state after adopt: %+v", m["sold"])
	}
	// Same upstream price: nothing to write.
	before := r.na.patches
	r.once(Actions{})
	if r.na.patches != before {
		t.Fatal("rewrote an unchanged price")
	}
}

func TestSyncAutoAppliesSmallChangesAndHoldsLargeOnes(t *testing.T) {
	r := newRig(t, "auto")
	r.once(Actions{Adopt: []string{"sold"}})
	e0 := r.na.expr("sold")

	r.up.setInput("2.2") // +10% on input, under the 20% default
	out := r.once(Actions{})
	e1 := r.na.expr("sold")
	if e1 == e0 || !strings.Contains(out, "DONE  sold updated") {
		t.Fatalf("small change not applied:\n%s", out)
	}

	r.up.setInput("4") // +82%
	out = r.once(Actions{})
	if r.na.expr("sold") != e1 || !strings.Contains(out, "HELD  sold") {
		t.Fatalf("large change applied without approval:\n%s", out)
	}
	// The upstream moves again before the operator approves: the approval
	// was for a price that is no longer the plan, so nothing is written.
	r.up.setInput("5")
	out = r.once(Actions{Approve: []string{"sold"}})
	if r.na.expr("sold") != e1 || !strings.Contains(out, "changed since it was held") {
		t.Fatalf("approve wrote a price the operator never saw:\n%s", out)
	}
	out = r.once(Actions{Approve: []string{"sold"}})
	if r.na.expr("sold") == e1 || !strings.Contains(out, "DONE  sold updated") {
		t.Fatalf("approve did not apply:\n%s", out)
	}
}

func TestSyncApproveWithoutHeldChangeDoesNothing(t *testing.T) {
	r := newRig(t, "preview")
	r.once(Actions{Adopt: []string{"sold"}})
	e0 := r.na.expr("sold")
	// A change appears and is approved in the same run, before anyone saw it.
	r.up.setInput("3")
	out := r.once(Actions{Approve: []string{"all"}})
	if r.na.expr("sold") != e0 || !strings.Contains(out, "HELD  sold") {
		t.Fatalf("approve applied a change nobody had seen:\n%s", out)
	}
}

func TestSyncPreviewNeverWritesByItself(t *testing.T) {
	r := newRig(t, "preview")
	r.once(Actions{Adopt: []string{"sold"}})
	e0 := r.na.expr("sold")
	r.up.setInput("2.1")
	out := r.once(Actions{})
	if r.na.expr("sold") != e0 || !strings.Contains(out, "HELD  sold") {
		t.Fatalf("preview mode wrote a change:\n%s", out)
	}
}

func TestSyncStopsManagingAfterOutsideEdit(t *testing.T) {
	r := newRig(t, "auto")
	r.once(Actions{Adopt: []string{"sold"}})
	r.na.mu.Lock()
	r.na.pricing["sold"] = map[string]any{"ModelRatio": 9.0}
	r.na.mu.Unlock()

	out := r.once(Actions{})
	if !strings.Contains(out, "ALERT sold pricing was edited outside uplink") {
		t.Fatalf("no conflict alert:\n%s", out)
	}
	r.up.setInput("2.1")
	r.once(Actions{})
	if r.na.pricing["sold"]["ModelRatio"] != 9.0 {
		t.Fatal("overwrote the operator's edit")
	}
	// Release of a conflicted model forgets it without touching pricing.
	out = r.once(Actions{Release: []string{"sold"}})
	if r.na.pricing["sold"]["ModelRatio"] != 9.0 || !strings.Contains(out, "left as is") {
		t.Fatalf("release touched an edited model:\n%s", out)
	}
	if m, _ := r.s.state.AllManaged(); len(m) != 0 {
		t.Fatalf("still managed: %+v", m)
	}
}

func TestSyncReleaseRestoresOriginal(t *testing.T) {
	r := newRig(t, "auto")
	r.once(Actions{Adopt: []string{"sold"}})
	out := r.once(Actions{Release: []string{"sold"}})
	if !strings.Contains(out, "previous pricing restored") {
		t.Fatalf("release:\n%s", out)
	}
	if version(r.na.pricing["sold"]) != version(r.orig) {
		t.Fatalf("pricing after release %+v, want %+v", r.na.pricing["sold"], r.orig)
	}
	// Not re-adopted automatically.
	out = r.once(Actions{})
	if !strings.Contains(out, "TODO  sold") {
		t.Fatalf("released model was re-adopted:\n%s", out)
	}
}

func TestSyncOutsideEditDuringAdoptionKeepsOriginal(t *testing.T) {
	r := newRig(t, "auto")
	r.na.afterPatch = func(p map[string]map[string]any) { p["sold"] = map[string]any{"ModelRatio": 2.0} }
	out := r.once(Actions{Adopt: []string{"sold"}})
	if strings.Contains(out, "DONE  sold adopted") || !strings.Contains(out, "ALERT sold adopt") {
		t.Fatalf("adoption reported success over an outside edit:\n%s", out)
	}
	out = r.once(Actions{})
	if !strings.Contains(out, "ALERT sold pricing was edited outside uplink") || r.na.pricing["sold"]["ModelRatio"] != 2.0 {
		t.Fatalf("outside edit not detected:\n%s", out)
	}
	m, _ := r.s.state.AllManaged()
	if m["sold"] == nil || m["sold"].Original["ModelRatio"] != 1.5 {
		t.Fatalf("original pricing lost: %+v", m["sold"])
	}
}

func TestReconcileMatchesAndHoldsOnLowMargin(t *testing.T) {
	r := newRig(t, "auto")
	r.once(Actions{Adopt: []string{"sold"}})
	now := time.Now()
	at := now.Add(-2 * time.Minute)
	rec := func(id, cost string, in, out int64) protocol.Record {
		return protocol.Record{RequestID: id, CreatedAt: at.UTC().Format(time.RFC3339), Model: "up-model", Endpoint: "chat.completions",
			Tokens: protocol.Tokens{Input: in, Output: out}, Cost: cost, Currency: "CNY", Status: "success"}
	}
	log := func(id string, quota, in, out int64) map[string]any {
		return map[string]any{"id": 1, "created_at": at.Unix(), "model_name": "sold", "quota": quota, "prompt_tokens": in,
			"completion_tokens": out, "upstream_request_id": id, "channel": 7, "other": "{}"}
	}
	// One quota unit is 7.3/500000 CNY. Request a: cost 0.001 CNY, charged
	// 100 quota = 0.00146 CNY (+46%). Request b: cost 0.001, charged 70 quota
	// = 0.001022 (+2.2%, under the 10% alert). c: upstream only. d: tiny,
	// cost 0.00004 = 2.74 quota units, charged 2 (-27%): whole-unit rounding
	// explains it, so no alert.
	r.up.mu.Lock()
	r.up.records = []protocol.Record{rec("a", "0.001", 100, 10), rec("b", "0.001", 100, 10), rec("c", "0.002", 1, 1), rec("d", "0.00004", 3, 1)}
	r.up.mu.Unlock()
	r.na.mu.Lock()
	r.na.logs = []map[string]any{log("a", 100, 100, 10), log("b", 70, 100, 11), log("d", 2, 3, 1)}
	r.na.mu.Unlock()

	out := r.once(Actions{})
	for _, want := range []string{"matched 3", "token mismatch 1", "low margin 1", "upstream-only 1", "lowest -27.00%",
		"ALERT sold request b earned 2.20%", "ALERT up-model upstream up charged request c", "HOLD  low_margin"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "request d earned") {
		t.Errorf("rounding on a tiny request raised an alert:\n%s", out)
	}
	// Alerts fire once per request.
	out = r.once(Actions{})
	if strings.Contains(out, "ALERT sold request b") {
		t.Errorf("alert repeated:\n%s", out)
	}
	// A hold blocks automatic changes, even small ones, until -ack.
	e := r.na.expr("sold")
	r.up.setInput("2.1")
	out = r.once(Actions{})
	if r.na.expr("sold") != e || !strings.Contains(out, "HELD  sold") {
		t.Fatalf("change applied during hold:\n%s", out)
	}
	r.once(Actions{Ack: true})
	if r.na.expr("sold") == e {
		t.Fatal("change not applied after -ack")
	}
}

func TestExampleConfigParses(t *testing.T) {
	b, err := os.ReadFile("../../deploy/sync.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Interval != 5*time.Minute || cfg.Reconcile.Lookback != 24*time.Hour || cfg.FX["USD"] != "7.10" ||
		len(cfg.Upstreams) != 1 || len(cfg.Upstreams[0].ChannelIDs) != 2 || cfg.Pricing.Apply != "preview" {
		t.Fatalf("example config: %+v", cfg)
	}
}
