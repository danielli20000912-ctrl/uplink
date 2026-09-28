package newapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"uplink/internal/exportdb"
)

// Source reads a New API instance: its SQLite database, read-only, for keys,
// options and logs; and its HTTP API, as probe users, for resolved prices.
//
// Only the columns listed in the queries below are read. Nothing from the
// options table other than the named keys, and nothing from logs.other other
// than the named fields, leaves this package.
type Source struct {
	db      *sql.DB
	logDB   *sql.DB
	baseURL string
	client  *http.Client
}

// OpenSQLite opens New API's SQLite database (and optional separate log
// database) read-only. The files must be on a read-write mount so SQLite can
// use the WAL shared-memory file; the connection itself never writes.
func OpenSQLite(dbPath, logDBPath, baseURL string) (*Source, error) {
	open := func(p string) (*sql.DB, error) {
		q := url.Values{}
		q.Set("mode", "ro")
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "query_only(1)")
		db, err := sql.Open("sqlite", "file:"+p+"?"+q.Encode())
		if err != nil {
			return nil, err
		}
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}
	db, err := open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("newapi: open %s: %w", dbPath, err)
	}
	logDB := db
	if logDBPath != "" && logDBPath != dbPath {
		if logDB, err = open(logDBPath); err != nil {
			db.Close()
			return nil, fmt.Errorf("newapi: open log db: %w", err)
		}
	}
	return &Source{db: db, logDB: logDB, baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (s *Source) Close() {
	if s.logDB != s.db {
		s.logDB.Close()
	}
	s.db.Close()
}

// Options holds the New API options uplink depends on.
type Options struct {
	QuotaPerUnit      *big.Rat
	LogConsumeEnabled bool
}

// ReadOptions reads only QuotaPerUnit and LogConsumeEnabled.
func (s *Source) ReadOptions(ctx context.Context) (Options, error) {
	o := Options{QuotaPerUnit: big.NewRat(500000, 1), LogConsumeEnabled: true} // New API defaults
	rows, err := s.db.QueryContext(ctx, `SELECT "key", value FROM options WHERE "key" IN ('QuotaPerUnit', 'LogConsumeEnabled')`)
	if err != nil {
		return o, fmt.Errorf("newapi: read options: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return o, err
		}
		switch k {
		case "QuotaPerUnit":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				return o, fmt.Errorf("newapi: bad QuotaPerUnit %q", v)
			}
			o.QuotaPerUnit, _ = new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
		case "LogConsumeEnabled":
			o.LogConsumeEnabled = v == "true"
		}
	}
	return o, rows.Err()
}

// ReadKeys returns every live token with its owner's group, status and quota.
// Keys are hashed here; the plaintext never leaves this function.
func (s *Source) ReadKeys(ctx context.Context) ([]exportdb.KeyRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, t."key", t.user_id, t.status, t.remain_quota, t.unlimited_quota, t.expired_time,
       COALESCE(t.model_limits_enabled, 0), COALESCE(t.model_limits, ''), COALESCE(t."group", ''),
       COALESCE(u."group", 'default'), u.status, u.quota
FROM tokens t JOIN users u ON u.id = t.user_id
WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("newapi: read tokens: %w", err)
	}
	defer rows.Close()
	var out []exportdb.KeyRow
	for rows.Next() {
		var r exportdb.KeyRow
		var key string
		if err := rows.Scan(&r.TokenID, &key, &r.UserID, &r.TokenStatus, &r.RemainQuota, &r.Unlimited, &r.ExpiredTime,
			&r.ModelLimitsEnabled, &r.ModelLimits, &r.TokenGroup, &r.UserGroup, &r.UserStatus, &r.UserQuota); err != nil {
			return nil, err
		}
		r.KeyHash = exportdb.HashKey(key)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Log types (model/log.go).
const (
	logTypeConsume = 2
	logTypeError   = 5
)

// logOther lists the only fields read from logs.other.
type logOther struct {
	CacheTokens           int64    `json:"cache_tokens"`
	CacheCreationTokens   int64    `json:"cache_creation_tokens"`
	CacheCreationTokens5m int64    `json:"cache_creation_tokens_5m"`
	CacheCreationTokens1h int64    `json:"cache_creation_tokens_1h"`
	UsageSemantic         string   `json:"usage_semantic"`
	RequestPath           string   `json:"request_path"`
	FRT                   float64  `json:"frt"`
	Image                 bool     `json:"image"`
	AudioSeparate         bool     `json:"audio_input_seperate_price"`
	ToolSurcharges        []any    `json:"tool_surcharges"`
	RequestRules          []any    `json:"request_rules"`
	ImageCount            *float64 `json:"image_count"`
}

// ReadLogs returns consume and error logs with id > afterID, oldest first.
func (s *Source) ReadLogs(ctx context.Context, afterID int64, limit int) ([]exportdb.UsageRow, error) {
	rows, err := s.logDB.QueryContext(ctx, `
SELECT id, type, created_at, COALESCE(model_name, ''), quota, prompt_tokens, completion_tokens, use_time,
       is_stream, token_id, COALESCE(request_id, ''), COALESCE(other, ''), COALESCE("group", '')
FROM logs WHERE id > ? AND type IN (?, ?) ORDER BY id LIMIT ?`, afterID, logTypeConsume, logTypeError, limit)
	if err != nil {
		return nil, fmt.Errorf("newapi: read logs: %w", err)
	}
	defer rows.Close()
	var out []exportdb.UsageRow
	for rows.Next() {
		var (
			id, typ, createdAt, quota, prompt, completion, useTime, tokenID int64
			stream                                                          bool
			model, requestID, otherJSON, group                              string
		)
		if err := rows.Scan(&id, &typ, &createdAt, &model, &quota, &prompt, &completion, &useTime, &stream, &tokenID, &requestID, &otherJSON, &group); err != nil {
			return nil, err
		}
		var o logOther
		if otherJSON != "" {
			_ = json.Unmarshal([]byte(otherJSON), &o) // malformed: treat as no details
		}
		r := exportdb.UsageRow{
			LogID: id, TokenID: tokenID, CreatedAt: createdAt, Model: model, Stream: stream,
			Output: completion, Quota: quota, DurationMS: useTime * 1000, FirstTokenMS: int64(o.FRT),
			RequestID: requestID, Endpoint: EndpointFromPath(o.RequestPath), Status: "success", Group: group,
		}
		if typ == logTypeError {
			r.Status, r.Quota = "error", 0
		}
		normalizeBuckets(&r, prompt, o)
		r.Unmodeled = unmodeled(o)
		out = append(out, r)
	}
	return out, rows.Err()
}

// normalizeBuckets maps New API usage to uplink's disjoint buckets, following
// service/text_quota.go: OpenAI-semantics prompt tokens include cache reads and
// writes; Anthropic-semantics prompt tokens exclude them.
func normalizeBuckets(r *exportdb.UsageRow, prompt int64, o logOther) {
	r.CachedInput = o.CacheTokens
	if o.UsageSemantic == "anthropic" {
		r.Input = prompt
		rest := o.CacheCreationTokens - o.CacheCreationTokens5m - o.CacheCreationTokens1h
		if rest < 0 {
			rest = 0
		}
		r.CacheWrite = rest + o.CacheCreationTokens5m
		r.CacheWrite1 = o.CacheCreationTokens1h
		return
	}
	r.CacheWrite = o.CacheCreationTokens
	r.Input = prompt - o.CacheTokens - o.CacheCreationTokens
	if r.Input < 0 {
		r.Input = 0
	}
}

// unmodeled lists charge components uplink v0 prices do not describe.
func unmodeled(o logOther) string {
	var reasons []string
	if len(o.ToolSurcharges) > 0 {
		reasons = append(reasons, "tool_surcharge")
	}
	if o.Image {
		reasons = append(reasons, "image_tokens")
	}
	if o.AudioSeparate {
		reasons = append(reasons, "audio_tokens")
	}
	if len(o.RequestRules) > 0 {
		reasons = append(reasons, "request_rules")
	}
	return strings.Join(reasons, ",")
}

// LastLogIDBefore returns the largest log id created before unix time t, so a
// first export can skip history older than the retention window.
func (s *Source) LastLogIDBefore(ctx context.Context, t int64) (int64, error) {
	var id int64
	err := s.logDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM logs WHERE created_at < ?`, t).Scan(&id)
	return id, err
}

// FetchPricing calls GET /api/pricing as the probe user that owns accessToken.
func (s *Source) FetchPricing(ctx context.Context, accessToken string) (*PricingResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/api/pricing", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("newapi: pricing: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("newapi: pricing: HTTP %d", resp.StatusCode)
	}
	var pr PricingResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("newapi: pricing: %w", err)
	}
	if !pr.Success {
		return nil, fmt.Errorf("newapi: pricing refused for probe token")
	}
	return &pr, nil
}
