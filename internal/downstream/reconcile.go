package downstream

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"uplink/adapters/newapi"
	"uplink/internal/protocol"
)

// ReconcileReport summarizes one upstream over one window.
type ReconcileReport struct {
	Upstream       string
	Since, Until   time.Time
	Matched        int
	TokenMismatch  int
	UpstreamOnly   int // charged upstream, no downstream log: paid but not billed
	DownstreamOnly int // billed downstream, no upstream record yet (lag) or never charged
	Unmodeled      int
	Charged        *big.Rat // downstream currency, matched records
	Cost           *big.Rat // downstream currency, matched records
	MinMarginPct   *big.Rat // lowest per-request margin among matched, modeled records
	LowMargin      int      // records below the alert threshold by more than rounding
}

func (r *ReconcileReport) String() string {
	margin := "n/a"
	if r.Cost.Sign() > 0 {
		m := new(big.Rat).Quo(new(big.Rat).Sub(r.Charged, r.Cost), r.Cost)
		margin = protocol.RoundHalfUp(m.Mul(m, big.NewRat(100, 1)), 2) + "%"
	}
	minM := "n/a"
	if r.MinMarginPct != nil {
		minM = protocol.RoundHalfUp(r.MinMarginPct, 2) + "%"
	}
	return fmt.Sprintf("%s %s..%s: matched %d (charged %s, cost %s, margin %s, lowest %s), token mismatch %d, low margin %d, upstream-only %d, downstream-only %d, unmodeled %d",
		r.Upstream, r.Since.UTC().Format(time.RFC3339), r.Until.UTC().Format(time.RFC3339), r.Matched,
		protocol.FormatDecimal(r.Charged), protocol.FormatDecimal(r.Cost), margin, minM,
		r.TokenMismatch, r.LowMargin, r.UpstreamOnly, r.DownstreamOnly, r.Unmodeled)
}

// reconcile joins downstream consume logs to upstream usage records on the
// upstream request id and measures what each request actually earned.
func (s *Syncer) reconcile(ctx context.Context, u *upstream, now time.Time, alerts *[]Issue) (*ReconcileReport, error) {
	disc, err := u.client.Discovery(ctx)
	if err != nil {
		return nil, err
	}
	lag := time.Duration(disc.UsageLagSeconds) * time.Second
	if lag < time.Minute {
		lag = time.Minute
	}
	until := now.Add(-lag)
	since := until.Add(-s.cfg.Reconcile.Lookback)
	pad := 5 * time.Minute

	records, err := u.client.Usage(ctx, since.Add(-pad), until)
	if err != nil {
		return nil, fmt.Errorf("upstream usage: %w", err)
	}
	var logs []newapi.DownstreamLog
	end := until.Add(pad)
	if end.After(now) {
		end = now
	}
	for _, ch := range u.cfg.ChannelIDs {
		l, err := s.admin.ConsumeLogs(ctx, ch, since.Add(-pad).Unix(), end.Unix())
		if err != nil {
			return nil, fmt.Errorf("downstream logs: %w", err)
		}
		logs = append(logs, l...)
	}

	fx := big.NewRat(1, 1)
	if disc.Currency != s.cfg.Downstream.Currency {
		fx = s.fx[disc.Currency]
		if fx == nil {
			return nil, fmt.Errorf("no fx rate from %s", disc.Currency)
		}
	}
	threshold := s.marginAlert
	// Rounding each side can explain: one downstream quota unit, plus one
	// upstream cost step (the spec's match tolerance) converted.
	upStep := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(disc.CostPrecision)), nil))
	if q, err := protocol.ParseDecimal(disc.CostQuantum); disc.CostQuantum != "" && err == nil && q.Cmp(upStep) > 0 {
		upStep = q
	}
	upStep.Mul(upStep, fx)
	want := new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(threshold, big.NewRat(100, 1)))
	slack := new(big.Rat).Add(s.quantum, new(big.Rat).Mul(upStep, want))

	rep := &ReconcileReport{Upstream: u.cfg.Name, Since: since, Until: until, Charged: new(big.Rat), Cost: new(big.Rat)}
	byID := map[string]newapi.DownstreamLog{}
	for _, l := range logs {
		if l.UpstreamRequestID != "" {
			byID[l.UpstreamRequestID] = l
		}
	}
	inWindow := func(t time.Time) bool { return !t.Before(since) && t.Before(until) }
	matchedIDs := map[string]bool{}
	for _, r := range records {
		at, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			continue
		}
		l, ok := byID[r.RequestID]
		if !ok {
			if inWindow(at) {
				rep.UpstreamOnly++
				if first, _ := s.state.FirstSeen(r.RequestID, "upstream_only"); first {
					*alerts = append(*alerts, Issue{"alert", r.Model, fmt.Sprintf("upstream %s charged request %s (%s %s) that has no downstream log", u.cfg.Name, r.RequestID, r.Cost, r.Currency)})
				}
			}
			continue
		}
		matchedIDs[r.RequestID] = true
		if !inWindow(time.Unix(l.CreatedAt, 0)) {
			continue
		}
		rep.Matched++
		t := r.Tokens
		if l.Buckets != (newapi.Buckets{Input: t.Input, CachedInput: t.CachedInput, CacheWrite: t.CacheWrite, CacheWrite1h: t.CacheWrite1h, Output: t.Output}) {
			rep.TokenMismatch++
			if first, _ := s.state.FirstSeen(r.RequestID, "tokens"); first {
				*alerts = append(*alerts, Issue{"warn", r.Model, fmt.Sprintf("request %s: downstream tokens %+v differ from upstream %+v", r.RequestID, l.Buckets, t)})
			}
		}
		cost, err := protocol.ParseDecimal(r.Cost)
		if err != nil {
			continue
		}
		cost.Mul(cost, fx)
		charged := new(big.Rat).Mul(big.NewRat(l.Quota, 1), s.quantum)
		rep.Charged.Add(rep.Charged, charged)
		rep.Cost.Add(rep.Cost, cost)
		if len(r.XUnmodeled) > 0 || l.Unmodeled != "" {
			rep.Unmodeled++
			continue
		}
		if cost.Sign() == 0 {
			continue
		}
		m := new(big.Rat).Quo(new(big.Rat).Sub(charged, cost), cost)
		m.Mul(m, big.NewRat(100, 1))
		if rep.MinMarginPct == nil || m.Cmp(rep.MinMarginPct) < 0 {
			rep.MinMarginPct = m
		}
		// Low margin only when the shortfall exceeds what rounding explains.
		short := new(big.Rat).Sub(new(big.Rat).Mul(cost, want), charged)
		if short.Cmp(slack) > 0 {
			rep.LowMargin++
			if first, _ := s.state.FirstSeen(r.RequestID, "low_margin"); first {
				*alerts = append(*alerts, Issue{"alert", l.Model, fmt.Sprintf("request %s earned %s%% (charged %s, cost %s %s)",
					r.RequestID, protocol.RoundHalfUp(m, 2), protocol.FormatDecimal(charged), protocol.FormatDecimal(cost), s.cfg.Downstream.Currency)})
				_ = s.state.AddHold("low_margin", fmt.Sprintf("request %s on %s", r.RequestID, l.Model), now.Unix())
			}
		}
	}
	for _, l := range logs {
		if inWindow(time.Unix(l.CreatedAt, 0)) && !matchedIDs[l.UpstreamRequestID] {
			rep.DownstreamOnly++
		}
	}
	return rep, nil
}
