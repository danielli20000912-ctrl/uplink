// Package publish serves the uplink v0 endpoints from the export database.
// It is the internet-facing process: it can read only the export file, never
// the upstream platform's database, and holds no platform credentials.
package publish

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"uplink/internal/exportdb"
	"uplink/internal/protocol"
)

type Config struct {
	Listen              string        `yaml:"listen"`
	ExportDB            string        `yaml:"export_db"`
	APIBase             string        `yaml:"api_base"`
	Contact             string        `yaml:"contact"`
	UsageLagSeconds     int           `yaml:"usage_lag_seconds"`
	RateLimitPerMinute  int           `yaml:"rate_limit_per_minute"`
	SummaryTimezone     string        `yaml:"summary_timezone"`
	StaleAfter          time.Duration `yaml:"stale_after"`
	ClaimErrorsRedacted bool          `yaml:"claim_errors_redacted"`
}

func (c *Config) setDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8790"
	}
	if c.UsageLagSeconds == 0 {
		c.UsageLagSeconds = 120
	}
	if c.RateLimitPerMinute == 0 {
		c.RateLimitPerMinute = 60
	}
	if c.SummaryTimezone == "" {
		c.SummaryTimezone = "UTC"
	}
	if c.StaleAfter == 0 {
		c.StaleAfter = 5 * time.Minute
	}
}

const (
	pathDiscovery = "/.well-known/uplink.json"
	pathCatalog   = "/uplink/v0/catalog"
	pathAccount   = "/uplink/v0/account"
	pathUsage     = "/uplink/v0/usage"
	pathSummary   = "/uplink/v0/usage/summary"
	pathStatus    = "/uplink/v0/status"

	maxRange     = 31 * 24 * time.Hour
	defaultLimit = 100
	maxLimit     = 1000
)

type Server struct {
	cfg     Config
	db      *sql.DB
	summary *time.Location
	limiter *limiter
	now     func() time.Time
}

func New(cfg Config) (*Server, error) {
	cfg.setDefaults()
	if cfg.ExportDB == "" || cfg.APIBase == "" {
		return nil, fmt.Errorf("publish config: export_db and api_base are required")
	}
	loc, err := time.LoadLocation(cfg.SummaryTimezone)
	if err != nil {
		return nil, fmt.Errorf("publish config: summary_timezone: %w", err)
	}
	db, err := exportdb.Open(cfg.ExportDB, true)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, db: db, summary: loc, limiter: newLimiter(cfg.RateLimitPerMinute), now: time.Now}, nil
}

func (s *Server) Close() { s.db.Close() }

func (s *Server) ListenAndServe() error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("publish: listening on %s", s.cfg.Listen)
	return srv.ListenAndServe()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathDiscovery, s.discovery)
	mux.HandleFunc("GET "+pathCatalog, s.authed(s.catalog))
	mux.HandleFunc("GET "+pathAccount, s.authed(s.account))
	mux.HandleFunc("GET "+pathUsage, s.authed(s.usage))
	mux.HandleFunc("GET "+pathSummary, s.authed(s.usageSummary))
	mux.HandleFunc("GET "+pathStatus, s.authed(s.status))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return mux
}

// ---- shared state from the export ----

type exportMeta struct {
	currency   string
	precision  int
	quantum    *big.Rat
	exportedAt time.Time
	window     time.Duration
	errorLogs  bool // status is published only when failures are recorded
}

func (s *Server) meta() (*exportMeta, error) {
	get := func(k string) (string, error) { return exportdb.GetMeta(s.db, k) }
	var m exportMeta
	var err error
	if m.currency, err = get(exportdb.MetaCurrency); err != nil || m.currency == "" {
		return nil, fmt.Errorf("export not ready")
	}
	p, _ := get(exportdb.MetaCostPrecision)
	if m.precision, err = strconv.Atoi(p); err != nil {
		return nil, fmt.Errorf("export not ready")
	}
	q, _ := get(exportdb.MetaCostQuantum)
	if m.quantum, err = protocol.ParseDecimal(q); err != nil {
		return nil, fmt.Errorf("export not ready")
	}
	at, _ := get(exportdb.MetaExportedAt)
	if m.exportedAt, err = time.Parse(time.RFC3339, at); err != nil {
		return nil, fmt.Errorf("export not ready")
	}
	ws, _ := get(exportdb.MetaStatusWindowS)
	secs, _ := strconv.Atoi(ws)
	if secs <= 0 {
		secs = 900
	}
	m.window = time.Duration(secs) * time.Second
	el, _ := get(exportdb.MetaErrorLogs)
	m.errorLogs = el == "true"
	return &m, nil
}

func (m *exportMeta) money(quota int64) string {
	v := new(big.Rat).Mul(big.NewRat(quota, 1), m.quantum)
	if v.Sign() < 0 {
		v.SetInt64(0)
	}
	return protocol.RoundHalfUp(v, m.precision)
}

// ---- discovery ----

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	m, err := s.meta()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "not ready")
		return
	}
	caps := []string{"catalog", "account", "usage.records", "usage.summary"}
	endpoints := map[string]string{
		"catalog": pathCatalog, "account": pathAccount, "usage": pathUsage, "usage_summary": pathSummary,
	}
	if m.errorLogs {
		caps = append(caps, "status")
		endpoints["status"] = pathStatus
	}
	if s.cfg.ClaimErrorsRedacted {
		caps = append(caps, "errors.redacted")
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, protocol.Discovery{
		Protocol:        "uplink",
		Versions:        []string{"v0"},
		APIBase:         s.cfg.APIBase,
		Platform:        "new-api",
		Currency:        m.currency,
		CostPrecision:   m.precision,
		CostQuantum:     protocol.FormatDecimal(m.quantum),
		UsageLagSeconds: s.cfg.UsageLagSeconds,
		RequestIDHeader: "X-Oneapi-Request-Id",
		Endpoints:       endpoints,
		Capabilities:    caps,
		Contact:         s.cfg.Contact,
	})
}

// ---- authentication ----

type authedHandler func(w http.ResponseWriter, r *http.Request, key *exportdb.KeyRow, m *exportMeta)

// New API token and user status values (common/constants.go).
const (
	tokenEnabled   = 1
	tokenExhausted = 4
	userEnabled    = 1
)

func (s *Server) authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "missing or malformed Authorization header")
			return
		}
		hash := exportdb.HashKey(normalizeKey(raw))
		if !s.limiter.allow(hash, s.now()) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		m, err := s.meta()
		if err != nil || s.now().Sub(m.exportedAt) > s.cfg.StaleAfter {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "data temporarily unavailable")
			return
		}
		key, err := exportdb.LookupKey(s.db, hash)
		if err != nil {
			log.Printf("publish: key lookup: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if key == nil || !usable(key, s.now()) {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid, disabled or expired API key")
			return
		}
		h(w, r, key, m)
	}
}

func usable(k *exportdb.KeyRow, now time.Time) bool {
	if k.UserStatus != userEnabled {
		return false
	}
	if k.TokenStatus != tokenEnabled && k.TokenStatus != tokenExhausted {
		return false
	}
	return k.ExpiredTime == -1 || k.ExpiredTime > now.Unix()
}

func bearer(h string) (string, bool) {
	h = strings.TrimSpace(h)
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	k := strings.TrimSpace(h[7:])
	return k, k != ""
}

// normalizeKey mirrors New API's TokenAuth: strip "sk-", drop "-channel" suffixes.
func normalizeKey(k string) string {
	k = strings.TrimPrefix(k, "sk-")
	if i := strings.IndexByte(k, '-'); i >= 0 {
		k = k[:i]
	}
	return k
}

// ---- catalog ----

// usingGroup is the group a key's requests run in (New API: the token's group
// if set, else the owner's). For "auto" it falls back to the owner's group.
func usingGroup(key *exportdb.KeyRow) (string, bool) {
	using := key.TokenGroup
	if using == "" {
		using = key.UserGroup
	}
	if using == "auto" {
		return key.UserGroup, true
	}
	return using, false
}

func (s *Server) keyModels(key *exportdb.KeyRow) ([]protocol.Model, string, bool, error) {
	using, auto := usingGroup(key)
	js, generatedAt, ok, err := exportdb.LookupCatalog(s.db, key.UserGroup, using)
	if err != nil || !ok {
		return nil, "", ok, err
	}
	var models []protocol.Model
	if err := json.Unmarshal([]byte(js), &models); err != nil {
		return nil, "", false, err
	}
	if key.ModelLimitsEnabled {
		allowed := map[string]bool{}
		for _, m := range strings.Split(key.ModelLimits, ",") {
			allowed[strings.TrimSpace(m)] = true
		}
		kept := models[:0]
		for _, m := range models {
			if allowed[m.ID] {
				kept = append(kept, m)
			}
		}
		models = kept
	}
	if auto {
		for i := range models {
			models[i].Price = protocol.Price{Unit: protocol.UnitCustom, Description: "Price depends on automatic group selection."}
		}
	}
	return models, generatedAt, true, nil
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request, key *exportdb.KeyRow, m *exportMeta) {
	models, generatedAt, ok, err := s.keyModels(key)
	if err != nil {
		log.Printf("publish: catalog: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "permission_denied", "no catalog is published for this key")
		return
	}
	body, _ := json.Marshal(models)
	sum := sha256.Sum256(append(body, m.currency...))
	version := "sha256:" + hex.EncodeToString(sum[:8])
	cat := protocol.Catalog{CatalogVersion: version, GeneratedAt: generatedAt, Currency: m.currency, Models: models}
	if cat.Models == nil {
		cat.Models = []protocol.Model{}
	}
	etag := `"` + version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

// ---- account ----

func (s *Server) account(w http.ResponseWriter, r *http.Request, key *exportdb.KeyRow, m *exportMeta) {
	balance := key.UserQuota
	if !key.Unlimited && key.RemainQuota < balance {
		balance = key.RemainQuota
	}
	a := protocol.Account{Currency: m.currency, Unlimited: false, Balance: m.money(balance)}
	if key.ExpiredTime > 0 {
		a.ExpiresAt = time.Unix(key.ExpiredTime, 0).UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, a)
}

// ---- usage ----

type timeRange struct{ since, until int64 }

func parseRange(r *http.Request) (timeRange, string, string) {
	q := r.URL.Query()
	since, err1 := time.Parse(time.RFC3339, q.Get("since"))
	until, err2 := time.Parse(time.RFC3339, q.Get("until"))
	if err1 != nil || err2 != nil {
		return timeRange{}, "invalid_request", "since and until are required RFC 3339 timestamps"
	}
	if !until.After(since) {
		return timeRange{}, "invalid_request", "until must be after since"
	}
	if until.Sub(since) > maxRange {
		return timeRange{}, "range_too_large", "until - since must not exceed 31 days"
	}
	return timeRange{since.Unix(), until.Unix()}, "", ""
}

func encodeCursor(createdAt, logID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", createdAt, logID)))
}

func decodeCursor(c string) (int64, int64, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, 0, false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	a, err1 := strconv.ParseInt(parts[0], 10, 64)
	b2, err2 := strconv.ParseInt(parts[1], 10, 64)
	return a, b2, err1 == nil && err2 == nil
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request, key *exportdb.KeyRow, m *exportMeta) {
	tr, code, msg := parseRange(r)
	if code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	q := r.URL.Query()
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	afterAt, afterID := int64(-1), int64(-1)
	if c := q.Get("cursor"); c != "" {
		var ok bool
		if afterAt, afterID, ok = decodeCursor(c); !ok {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid cursor")
			return
		}
	}
	sqlq := `SELECT ` + exportdb.UsageColumns + ` FROM usage
WHERE token_id = ? AND status = 'success' AND created_at >= ? AND created_at < ?
  AND (created_at > ? OR (created_at = ? AND log_id > ?))`
	args := []any{key.TokenID, tr.since, tr.until, afterAt, afterAt, afterID}
	if model := q.Get("model"); model != "" {
		sqlq += ` AND model = ?`
		args = append(args, model)
	}
	sqlq += ` ORDER BY created_at, log_id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.Query(sqlq, args...)
	if err != nil {
		log.Printf("publish: usage: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	defer rows.Close()
	page := protocol.UsagePage{Records: []protocol.Record{}}
	var last exportdb.UsageRow
	for rows.Next() {
		u, err := exportdb.ScanUsage(rows)
		if err != nil {
			log.Printf("publish: usage scan: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if len(page.Records) == limit {
			c := encodeCursor(last.CreatedAt, last.LogID)
			page.NextCursor = &c
			break
		}
		page.Records = append(page.Records, toRecord(u, m))
		last = u
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, page)
}

func toRecord(u exportdb.UsageRow, m *exportMeta) protocol.Record {
	stream := u.Stream
	dur := u.DurationMS
	id := u.RequestID
	if id == "" {
		id = "newapi-log-" + strconv.FormatInt(u.LogID, 10)
	}
	rec := protocol.Record{
		RequestID: id,
		CreatedAt: time.Unix(u.CreatedAt, 0).UTC().Format(time.RFC3339),
		Model:     u.Model,
		Endpoint:  u.Endpoint,
		Stream:    &stream,
		Tokens: protocol.Tokens{Input: u.Input, CachedInput: u.CachedInput, CacheWrite: u.CacheWrite,
			CacheWrite1h: u.CacheWrite1, Output: u.Output},
		Cost:       m.money(u.Quota),
		Currency:   m.currency,
		Status:     u.Status,
		DurationMS: &dur,
	}
	if u.Unmodeled != "" {
		rec.XUnmodeled = strings.Split(u.Unmodeled, ",")
	}
	return rec
}

// ---- usage summary ----

func (s *Server) usageSummary(w http.ResponseWriter, r *http.Request, key *exportdb.KeyRow, m *exportMeta) {
	tr, code, msg := parseRange(r)
	if code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	loc := s.summary
	if tz := r.URL.Query().Get("timezone"); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown timezone")
			return
		}
		loc = l
	}
	rows, err := s.db.Query(`SELECT `+exportdb.UsageColumns+` FROM usage
WHERE token_id = ? AND status = 'success' AND created_at >= ? AND created_at < ? ORDER BY created_at, log_id`,
		key.TokenID, tr.since, tr.until)
	if err != nil {
		log.Printf("publish: summary: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	defer rows.Close()
	type agg struct {
		n     int64
		t     protocol.Tokens
		quota int64
		date  string
		model string
	}
	buckets := map[[2]string]*agg{}
	for rows.Next() {
		u, err := exportdb.ScanUsage(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		d := time.Unix(u.CreatedAt, 0).In(loc).Format("2006-01-02")
		k := [2]string{d, u.Model}
		a := buckets[k]
		if a == nil {
			a = &agg{date: d, model: u.Model}
			buckets[k] = a
		}
		a.n++
		a.quota += u.Quota
		a.t.Input += u.Input
		a.t.CachedInput += u.CachedInput
		a.t.CacheWrite += u.CacheWrite
		a.t.CacheWrite1h += u.CacheWrite1
		a.t.Output += u.Output
	}
	out := protocol.UsageSummary{Granularity: "day", Timezone: loc.String(), Currency: m.currency, Buckets: []protocol.SummaryBucket{}}
	for _, a := range buckets {
		out.Buckets = append(out.Buckets, protocol.SummaryBucket{Date: a.date, Model: a.model, Requests: a.n, Tokens: a.t, Cost: m.money(a.quota)})
	}
	sort.Slice(out.Buckets, func(i, j int) bool {
		if out.Buckets[i].Date != out.Buckets[j].Date {
			return out.Buckets[i].Date < out.Buckets[j].Date
		}
		return out.Buckets[i].Model < out.Buckets[j].Model
	})
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, out)
}

// ---- status ----

func (s *Server) status(w http.ResponseWriter, r *http.Request, key *exportdb.KeyRow, m *exportMeta) {
	if !m.errorLogs {
		writeError(w, http.StatusNotFound, "not_found", "status is not published: failed requests are not recorded")
		return
	}
	models, _, ok, err := s.keyModels(key)
	if err != nil || !ok {
		writeError(w, http.StatusForbidden, "permission_denied", "no catalog is published for this key")
		return
	}
	now := s.now()
	since := now.Add(-m.window).Unix()
	type counts struct {
		ok, bad int64
		frt     []int64
	}
	byModel := map[string]*counts{}
	// Only traffic in the key's own group: other groups may route to other
	// channels, and their volume is not this key's business.
	group, _ := usingGroup(key)
	rows, err := s.db.Query(`SELECT model, status, frt_ms FROM usage WHERE created_at >= ? AND grp = ?`, since, group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	for rows.Next() {
		var model, st string
		var frt int64
		if err := rows.Scan(&model, &st, &frt); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		c := byModel[model]
		if c == nil {
			c = &counts{}
			byModel[model] = c
		}
		if st == "success" {
			c.ok++
			if frt > 0 {
				c.frt = append(c.frt, frt)
			}
		} else {
			c.bad++
		}
	}
	rows.Close()

	out := protocol.Status{GeneratedAt: now.UTC().Format(time.RFC3339), WindowSeconds: int(m.window.Seconds()), Models: []protocol.ModelStatus{}}
	for _, mod := range models {
		ms := protocol.ModelStatus{ID: mod.ID, State: "ok"}
		if c := byModel[mod.ID]; c != nil {
			total := c.ok + c.bad
			ms.SampleSize = total
			if total > 0 {
				rate := new(big.Rat).SetFrac64(c.ok, total)
				ms.SuccessRate = protocol.RoundHalfUp(rate, 3)
				switch f, _ := rate.Float64(); {
				case f < 0.5:
					ms.State = "down"
				case f < 0.95:
					ms.State = "degraded"
				}
			}
			if len(c.frt) > 0 {
				sort.Slice(c.frt, func(i, j int) bool { return c.frt[i] < c.frt[j] })
				p50 := c.frt[len(c.frt)/2]
				ms.P50FirstTokenMS = &p50
			}
		}
		out.Models = append(out.Models, ms)
	}
	body, _ := json.Marshal(out.Models)
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, protocol.ErrorBody{Error: protocol.ErrorDetail{Code: errCode, Message: msg}})
}

// limiter is a fixed one-minute window per key hash.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window int64
	counts map[string]int
}

func newLimiter(perMinute int) *limiter {
	return &limiter{limit: perMinute, counts: map[string]int{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := now.Unix() / 60
	if w != l.window {
		l.window, l.counts = w, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.limit
}
