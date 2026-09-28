// Package check verifies a live uplink publisher: it fetches every endpoint,
// validates prices, and recomputes each usage record from the catalog.
package check

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"uplink/internal/protocol"
)

type Options struct {
	BaseURL string
	Key     string
	Since   time.Time
	Until   time.Time
	SaveDir string // if set, raw responses are written here
	Out     io.Writer
}

type Report struct {
	Failures   []string
	Records    int
	Matched    int
	Unmodeled  map[string]int
	Mismatches []string
	NotPriced  map[string]int
}

func (r *Report) fail(format string, a ...any) {
	r.Failures = append(r.Failures, fmt.Sprintf(format, a...))
}

// OK reports whether nothing failed and every modeled record matched.
func (r *Report) OK() bool { return len(r.Failures) == 0 && len(r.Mismatches) == 0 }

func Run(o Options) (*Report, error) {
	rep := &Report{Unmodeled: map[string]int{}, NotPriced: map[string]int{}}
	c := &client{base: strings.TrimRight(o.BaseURL, "/"), key: o.Key, save: o.SaveDir, http: &http.Client{Timeout: 30 * time.Second}}

	var disc protocol.Discovery
	if err := c.get("/.well-known/uplink.json", false, "discovery", &disc); err != nil {
		return nil, err
	}
	if disc.Protocol != "uplink" {
		rep.fail("discovery: protocol is %q", disc.Protocol)
	}
	tolerance := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(disc.CostPrecision)), nil))
	if disc.CostQuantum != "" {
		q, err := protocol.ParseDecimal(disc.CostQuantum)
		if err != nil {
			rep.fail("discovery: bad cost_quantum")
		} else if q.Cmp(tolerance) > 0 {
			tolerance = q
		}
	}

	// Unauthenticated calls must be refused.
	if code, err := c.status(disc.Endpoints["catalog"], false); err != nil || code != http.StatusUnauthorized {
		rep.fail("catalog without a key: want 401, got %d (%v)", code, err)
	}

	var cat protocol.Catalog
	if err := c.get(disc.Endpoints["catalog"], true, "catalog", &cat); err != nil {
		return nil, err
	}
	if cat.Currency != disc.Currency {
		rep.fail("catalog currency %s differs from discovery %s", cat.Currency, disc.Currency)
	}
	prices := map[string]protocol.Model{}
	for _, m := range cat.Models {
		if err := protocol.ValidatePrice(m.Price); err != nil {
			rep.fail("catalog %s: %v", m.ID, err)
		}
		if err := protocol.ValidateTimePricing(m.TimePricing); err != nil {
			rep.fail("catalog %s: %v", m.ID, err)
		}
		prices[m.ID] = m
	}

	var acct protocol.Account
	if err := c.get(disc.Endpoints["account"], true, "account", &acct); err != nil {
		return nil, err
	}
	if !acct.Unlimited && acct.Balance == "" {
		rep.fail("account: balance missing for a limited key")
	}

	var st protocol.Status
	if err := c.get(disc.Endpoints["status"], true, "status", &st); err != nil {
		return nil, err
	}

	// Usage: page through the window and recompute every record.
	cursor := ""
	for page := 0; ; page++ {
		q := url.Values{}
		q.Set("since", o.Since.UTC().Format(time.RFC3339))
		q.Set("until", o.Until.UTC().Format(time.RFC3339))
		q.Set("limit", "500")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var up protocol.UsagePage
		if err := c.get(disc.Endpoints["usage"]+"?"+q.Encode(), true, fmt.Sprintf("usage-%03d", page), &up); err != nil {
			return nil, err
		}
		for _, rec := range up.Records {
			rep.Records++
			checkRecord(rep, rec, prices, disc.CostPrecision, tolerance)
		}
		if up.NextCursor == nil {
			break
		}
		cursor = *up.NextCursor
	}
	return rep, nil
}

func checkRecord(rep *Report, rec protocol.Record, prices map[string]protocol.Model, precision int, tol *big.Rat) {
	if len(rec.XUnmodeled) > 0 {
		for _, reason := range rec.XUnmodeled {
			rep.Unmodeled[reason]++
		}
		return
	}
	m, ok := prices[rec.Model]
	if !ok {
		rep.NotPriced["not in catalog"]++
		return
	}
	at, err := time.Parse(time.RFC3339, rec.CreatedAt)
	if err != nil {
		rep.fail("record %s: bad created_at", rec.RequestID)
		return
	}
	want, err := protocol.Cost(m.Price, m.TimePricing, protocol.CostInput{CreatedAt: at, Tokens: rec.Tokens, Units: rec.Units}, precision)
	if err != nil {
		rep.NotPriced[m.Price.Unit]++
		return
	}
	got, err1 := protocol.ParseDecimal(rec.Cost)
	exp, err2 := protocol.ParseDecimal(want)
	if err1 != nil || err2 != nil {
		rep.fail("record %s: bad cost", rec.RequestID)
		return
	}
	diff := new(big.Rat).Sub(got, exp)
	if diff.Sign() < 0 {
		diff.Neg(diff)
	}
	if diff.Cmp(tol) <= 0 {
		rep.Matched++
		return
	}
	t := rec.Tokens
	rep.Mismatches = append(rep.Mismatches, fmt.Sprintf("%s %s in=%d cached=%d cw=%d cw1h=%d out=%d: recorded %s, catalog gives %s",
		rec.RequestID, rec.Model, t.Input, t.CachedInput, t.CacheWrite, t.CacheWrite1h, t.Output, rec.Cost, want))
}

// Print writes a human-readable report.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "records: %d, matched: %d, mismatched: %d\n", r.Records, r.Matched, len(r.Mismatches))
	printCounts := func(title string, m map[string]int) {
		if len(m) == 0 {
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "%s:\n", title)
		for _, k := range keys {
			fmt.Fprintf(w, "  %s: %d\n", k, m[k])
		}
	}
	printCounts("skipped, cost includes components the catalog does not describe", r.Unmodeled)
	printCounts("skipped, price not computable", r.NotPriced)
	for _, m := range r.Mismatches {
		fmt.Fprintf(w, "MISMATCH %s\n", m)
	}
	for _, f := range r.Failures {
		fmt.Fprintf(w, "FAIL %s\n", f)
	}
	if r.OK() {
		fmt.Fprintln(w, "OK")
	}
}

type client struct {
	base, key, save string
	http            *http.Client
}

func (c *client) request(path string, auth bool) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	return c.http.Do(req)
}

func (c *client) status(path string, auth bool) (int, error) {
	resp, err := c.request(path, auth)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (c *client) get(path string, auth bool, name string, v any) error {
	resp, err := c.request(path, auth)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if c.save != "" {
		if err := os.MkdirAll(c.save, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(c.save, name+".json"), body, 0o644); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
