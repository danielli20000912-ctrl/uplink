// Package downstream is `uplink sync`: it runs beside a reseller's New API,
// prices the models it resells from each upstream's uplink catalog, and
// reconciles every request it billed against what the upstream charged.
//
// It holds a root access token for the downstream New API (model pricing is
// root-only there) and has no network listener.
package downstream

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"sort"
	"strings"
	"time"

	"uplink/adapters/newapi"
	"uplink/internal/client"
	"uplink/internal/protocol"
)

type UpstreamConfig struct {
	Name       string `yaml:"name"`
	BaseURL    string `yaml:"base_url"`
	KeyEnv     string `yaml:"key_env"`
	ChannelIDs []int  `yaml:"channel_ids"`
	Markup     string `yaml:"markup"`
}

type Config struct {
	Downstream struct {
		BaseURL          string `yaml:"base_url"`
		AdminTokenEnv    string `yaml:"admin_token_env"`
		Currency         string `yaml:"currency"`
		PricePerQuotaUSD string `yaml:"price_per_quota_usd"`
	} `yaml:"downstream"`
	Upstreams []UpstreamConfig  `yaml:"upstreams"`
	FX        map[string]string `yaml:"fx"`
	Pricing   struct {
		Apply            string `yaml:"apply"` // preview | auto
		MaxAutoChangePct string `yaml:"max_auto_change_pct"`
		MinGroupRatio    string `yaml:"min_group_ratio"`
		MinMarginPct     string `yaml:"min_margin_pct"`
		RoundTo          string `yaml:"round_to"`
	} `yaml:"pricing"`
	Reconcile struct {
		Lookback       time.Duration `yaml:"lookback"`
		MarginAlertPct string        `yaml:"margin_alert_pct"`
	} `yaml:"reconcile"`
	StateDB  string        `yaml:"state_db"`
	Interval time.Duration `yaml:"interval"`
}

type upstream struct {
	cfg    UpstreamConfig
	client *client.Client
	markup *big.Rat
}

type Syncer struct {
	cfg         Config
	admin       *newapi.Admin
	state       *State
	ups         []*upstream
	fx          map[string]*big.Rat
	pppu        *big.Rat
	quantum     *big.Rat // downstream currency per quota unit
	maxChange   *big.Rat
	minGroup    *big.Rat
	minMargin   *big.Rat
	marginAlert *big.Rat
	roundTo     *big.Rat
	out         io.Writer
	now         func() time.Time
}

func dec(name, s, def string) (*big.Rat, error) {
	if s == "" {
		s = def
	}
	r, err := protocol.ParseDecimal(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return r, nil
}

func New(cfg Config, out io.Writer) (*Syncer, error) {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Reconcile.Lookback == 0 {
		cfg.Reconcile.Lookback = 24 * time.Hour
	}
	if cfg.Pricing.Apply == "" {
		cfg.Pricing.Apply = "preview"
	}
	if cfg.Pricing.Apply != "preview" && cfg.Pricing.Apply != "auto" {
		return nil, fmt.Errorf("pricing.apply must be preview or auto")
	}
	if cfg.StateDB == "" || cfg.Downstream.BaseURL == "" || len(cfg.Downstream.Currency) != 3 {
		return nil, fmt.Errorf("state_db, downstream.base_url and downstream.currency are required")
	}
	s := &Syncer{cfg: cfg, out: out, now: time.Now, fx: map[string]*big.Rat{}}
	var err error
	if s.pppu, err = dec("downstream.price_per_quota_usd", cfg.Downstream.PricePerQuotaUSD, ""); err != nil {
		return nil, err
	}
	if s.pppu.Sign() <= 0 {
		return nil, fmt.Errorf("downstream.price_per_quota_usd must be positive")
	}
	if s.maxChange, err = dec("pricing.max_auto_change_pct", cfg.Pricing.MaxAutoChangePct, "20"); err != nil {
		return nil, err
	}
	if s.minGroup, err = dec("pricing.min_group_ratio", cfg.Pricing.MinGroupRatio, "1"); err != nil {
		return nil, err
	}
	if s.minMargin, err = dec("pricing.min_margin_pct", cfg.Pricing.MinMarginPct, "5"); err != nil {
		return nil, err
	}
	if s.roundTo, err = dec("pricing.round_to", cfg.Pricing.RoundTo, "0.000001"); err != nil {
		return nil, err
	}
	if s.marginAlert, err = dec("reconcile.margin_alert_pct", cfg.Reconcile.MarginAlertPct, "0"); err != nil {
		return nil, err
	}
	for cur, v := range cfg.FX {
		if s.fx[cur], err = dec("fx."+cur, v, ""); err != nil {
			return nil, err
		}
	}
	token := os.Getenv(cfg.Downstream.AdminTokenEnv)
	if token == "" {
		return nil, fmt.Errorf("downstream admin token: $%s is empty", cfg.Downstream.AdminTokenEnv)
	}
	s.admin = newapi.NewAdmin(cfg.Downstream.BaseURL, token)
	for _, u := range cfg.Upstreams {
		key := os.Getenv(u.KeyEnv)
		if key == "" {
			return nil, fmt.Errorf("upstream %s: $%s is empty", u.Name, u.KeyEnv)
		}
		m, err := dec("upstreams."+u.Name+".markup", u.Markup, "")
		if err != nil {
			return nil, err
		}
		s.ups = append(s.ups, &upstream{cfg: u, client: client.New(u.BaseURL, key), markup: m})
	}
	if s.state, err = OpenState(cfg.StateDB); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Syncer) Close() { s.state.Close() }

// Actions are explicit operator decisions for one run.
type Actions struct {
	Adopt   []string // model names, or "all"
	Approve []string
	Release []string
	Ack     bool
}

func has(list []string, m string) bool {
	for _, v := range list {
		if v == m || v == "all" {
			return true
		}
	}
	return false
}

func (s *Syncer) printf(format string, a ...any) { fmt.Fprintf(s.out, format+"\n", a...) }

// Run repeats Once every interval until ctx is done.
func (s *Syncer) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if err := s.Once(ctx, Actions{}); err != nil {
			log.Printf("sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Once runs one cycle: plan prices, apply what is allowed, reconcile.
func (s *Syncer) Once(ctx context.Context, act Actions) error {
	now := s.now()
	if act.Ack {
		if err := s.state.ClearHolds(); err != nil {
			return err
		}
		s.printf("holds cleared")
	}
	st, err := s.admin.Status(ctx)
	if err != nil {
		return err
	}
	s.quantum = new(big.Rat).Quo(s.pppu, st.QuotaPerUnitRat())
	if st.Price > 0 && protocol.RatFromFloat(st.Price).Cmp(s.pppu) != 0 {
		s.printf("WARN  downstream top-up price is %v per USD but price_per_quota_usd is %s", st.Price, protocol.FormatDecimal(s.pppu))
	}

	in := PlanInput{Currency: s.cfg.Downstream.Currency, PricePerQuotaUSD: s.pppu, FX: s.fx, RoundTo: s.roundTo,
		MinGroupRatio: s.minGroup, MinMarginPct: s.minMargin}
	for _, u := range s.ups {
		disc, err := u.client.Discovery(ctx)
		if err != nil {
			return fmt.Errorf("upstream %s: %w", u.cfg.Name, err)
		}
		cat, err := u.client.Catalog(ctx)
		if err != nil {
			return fmt.Errorf("upstream %s catalog: %w", u.cfg.Name, err)
		}
		m := map[string]protocol.Model{}
		for _, mod := range cat.Models {
			m[mod.ID] = mod
		}
		for _, pc := range cat.PendingChanges {
			s.printf("INFO  upstream %s announced a price change for %s effective %s", u.cfg.Name, pc.Model, pc.EffectiveAt)
		}
		in.Upstreams = append(in.Upstreams, UpstreamView{Name: u.cfg.Name, Currency: disc.Currency, Catalog: m,
			ChannelIDs: u.cfg.ChannelIDs, Markup: u.markup})
		if acct, err := u.client.Account(ctx); err == nil && !acct.Unlimited {
			s.printf("INFO  upstream %s balance %s %s", u.cfg.Name, acct.Balance, acct.Currency)
		}
	}
	if in.Channels, err = s.admin.Channels(ctx); err != nil {
		return err
	}
	plan := BuildPlan(in)
	for _, i := range plan.Issues {
		s.printf("%-5s %s %s", strings.ToUpper(i.Level), i.Model, i.Message)
	}

	if err := s.applyPrices(ctx, plan, act, now); err != nil {
		return err
	}

	var alerts []Issue
	for _, u := range s.ups {
		rep, err := s.reconcile(ctx, u, now, &alerts)
		if err != nil {
			s.printf("WARN  reconcile %s: %v", u.cfg.Name, err)
			continue
		}
		s.printf("RECON %s", rep)
	}
	for _, a := range alerts {
		s.printf("%-5s %s %s", strings.ToUpper(a.Level), a.Model, a.Message)
	}
	holds, _ := s.state.Holds()
	for _, h := range holds {
		s.printf("HOLD  %s (automatic price changes paused; run with -ack after checking)", h)
	}
	return nil
}

// applyPrices writes prices under the ownership rules:
//   - a model is written for the first time only with -adopt; its previous
//     pricing is saved for -release
//   - a managed model whose New API version differs from the one uplink last
//     wrote was edited by someone else; uplink stops managing it
//   - changes larger than max_auto_change_pct, or any change while a hold is
//     active, wait for -approve; in preview mode nothing is written automatically
func (s *Syncer) applyPrices(ctx context.Context, plan *Plan, act Actions, now time.Time) error {
	managed, err := s.state.AllManaged()
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, t := range plan.Targets {
		names[t.Model] = true
	}
	for m := range managed {
		names[m] = true
	}
	for _, m := range act.Release {
		if m != "all" {
			names[m] = true
		}
	}
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	if len(list) == 0 {
		return nil
	}
	stored, err := s.admin.Pricing(ctx, list)
	if err != nil {
		return err
	}
	holds, err := s.state.Holds()
	if err != nil {
		return err
	}

	// Release first.
	for _, m := range list {
		row := managed[m]
		if row == nil || !has(act.Release, m) {
			continue
		}
		if row.State == "managed" && stored[m].Version == row.WrittenVersion {
			if err := s.write(ctx, m, stored[m].Version, row.Original); err != nil {
				s.printf("ALERT %s release failed: %v", m, err)
				continue
			}
			s.printf("DONE  %s released; previous pricing restored", m)
		} else {
			s.printf("DONE  %s released; pricing was edited outside uplink, left as is", m)
		}
		if err := s.state.DeleteManaged(m); err != nil {
			return err
		}
		_ = s.state.ClearPending(m)
		delete(managed, m)
	}

	for _, t := range plan.Targets {
		cur := stored[t.Model]
		row := managed[t.Model]
		switch {
		case row == nil:
			if !has(act.Adopt, t.Model) {
				s.printf("TODO  %s is not managed yet; run with -adopt %s to take over its price", t.Model, t.Model)
				continue
			}
			orig := cur.Configured
			if orig == nil {
				orig = map[string]any{}
			}
			// Save the previous pricing before touching it. WrittenVersion
			// stays empty until the write is confirmed, so an interrupted
			// adoption shows up as a conflict next cycle, original kept.
			row = &Managed{Model: t.Model, AdoptedAt: now.Unix(), Original: orig, WrittenExpr: t.Expr, WrittenSell: t.Sell, State: "managed"}
			if err := s.state.PutManaged(row); err != nil {
				return err
			}
			want := newapi.ExprPricing(t.Expr)
			if err := s.write(ctx, t.Model, cur.Version, want); err != nil {
				s.printf("ALERT %s adopt failed: %v", t.Model, err)
				// Forget the adoption only if the pricing is provably untouched.
				if e, rerr := s.admin.Pricing(ctx, []string{t.Model}); rerr == nil && e[t.Model].Version == cur.Version {
					if err := s.state.DeleteManaged(t.Model); err != nil {
						return err
					}
				}
				continue
			}
			v, err := s.written(ctx, t.Model, want)
			if err != nil {
				s.printf("ALERT %s adopt: %v", t.Model, err)
				continue
			}
			row.WrittenVersion = v
			if err := s.state.PutManaged(row); err != nil {
				return err
			}
			s.printf("DONE  %s adopted: %s", t.Model, t.Expr)
		case row.State == "conflict":
			s.printf("WARN  %s not managed since its pricing was edited outside uplink; -release %s to forget it", t.Model, t.Model)
		case cur.Version != row.WrittenVersion:
			row.State = "conflict"
			if err := s.state.PutManaged(row); err != nil {
				return err
			}
			_ = s.state.ClearPending(t.Model)
			s.printf("ALERT %s pricing was edited outside uplink; no longer managed (use -release %s to forget it)", t.Model, t.Model)
		case t.Expr == row.WrittenExpr:
			_ = s.state.ClearPending(t.Model)
		default:
			pct := MaxChangePct(row.WrittenSell, t.Sell)
			auto := s.cfg.Pricing.Apply == "auto" && pct.Cmp(s.maxChange) <= 0 && len(holds) == 0
			if !auto {
				// -approve applies only the exact price the operator was
				// shown; if the upstream moved again, show the new one.
				held, found, err := s.state.Pending(t.Model)
				if err != nil {
					return err
				}
				approved := has(act.Approve, t.Model) && found && held == t.Expr
				if !approved {
					if err := s.state.SetPending(t.Model, t.Expr, protocol.RoundHalfUp(pct, 2), now.Unix()); err != nil {
						return err
					}
					again := ""
					if has(act.Approve, t.Model) {
						again = " (changed since it was held; check the new price and approve again)"
					}
					s.printf("HELD  %s price change of %s%% waits for -approve %s%s: %s", t.Model, protocol.RoundHalfUp(pct, 2), t.Model, again, t.Expr)
					continue
				}
			}
			want := newapi.ExprPricing(t.Expr)
			if err := s.write(ctx, t.Model, cur.Version, want); err != nil {
				s.printf("ALERT %s update failed: %v", t.Model, err)
				continue
			}
			v, err := s.written(ctx, t.Model, want)
			if err != nil {
				// Leave WrittenVersion as it was: the next cycle sees a
				// version it did not write and stops managing the model.
				s.printf("ALERT %s update: %v", t.Model, err)
				continue
			}
			row.WrittenVersion, row.WrittenExpr, row.WrittenSell = v, t.Expr, t.Sell
			if err := s.state.PutManaged(row); err != nil {
				return err
			}
			_ = s.state.ClearPending(t.Model)
			s.printf("DONE  %s updated (%s%%): %s", t.Model, protocol.RoundHalfUp(pct, 2), t.Expr)
		}
	}
	for m, row := range managed {
		if !hasTarget(plan, m) && row.State == "managed" {
			s.printf("WARN  %s is managed but no longer priced from an upstream; price left as is", m)
		}
	}
	return nil
}

func hasTarget(p *Plan, m string) bool {
	for _, t := range p.Targets {
		if t.Model == m {
			return true
		}
	}
	return false
}

func (s *Syncer) write(ctx context.Context, model, version string, pricing map[string]any) error {
	return s.admin.SetPricing(ctx, []newapi.PricingChange{{ModelName: model, ExpectedVersion: version, Pricing: pricing}})
}

// written re-reads a model after a write and returns its new version. New API
// versions are a hash of the stored pricing and the write response carries no
// version, so the stored expression is checked to be the one just written; if
// someone else wrote in between, that is reported as a conflict.
func (s *Syncer) written(ctx context.Context, model string, want map[string]any) (string, error) {
	e, err := s.admin.Pricing(ctx, []string{model})
	if err != nil {
		return "", err
	}
	got := e[model]
	for k, v := range want {
		if fmt.Sprint(got.Configured[k]) != fmt.Sprint(v) {
			return "", newapi.ErrPricingConflict
		}
	}
	return got.Version, nil
}
