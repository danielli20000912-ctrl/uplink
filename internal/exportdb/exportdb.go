// Package exportdb is the small SQLite file that sits between the exporter,
// which can read the upstream platform, and the publisher, which faces the
// internet. It holds only what the publisher needs: key hashes (never keys),
// resolved catalogs, usage rows and health counters.
package exportdb

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS keys (
  key_hash             TEXT PRIMARY KEY,
  token_id             INTEGER NOT NULL,
  user_id              INTEGER NOT NULL,
  token_status         INTEGER NOT NULL,
  user_status          INTEGER NOT NULL,
  remain_quota         INTEGER NOT NULL,
  unlimited            INTEGER NOT NULL,
  expired_time         INTEGER NOT NULL,
  model_limits_enabled INTEGER NOT NULL,
  model_limits         TEXT NOT NULL,
  token_group          TEXT NOT NULL,
  user_group           TEXT NOT NULL,
  user_quota           INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS catalogs (
  user_group   TEXT NOT NULL,
  using_group  TEXT NOT NULL,
  models_json  TEXT NOT NULL,
  generated_at TEXT NOT NULL,
  PRIMARY KEY (user_group, using_group)
);
CREATE TABLE IF NOT EXISTS usage (
  log_id         INTEGER PRIMARY KEY,
  token_id       INTEGER NOT NULL,
  created_at     INTEGER NOT NULL,
  model          TEXT NOT NULL,
  endpoint       TEXT NOT NULL,
  stream         INTEGER NOT NULL,
  input          INTEGER NOT NULL,
  cached_input   INTEGER NOT NULL,
  cache_write    INTEGER NOT NULL,
  cache_write_1h INTEGER NOT NULL,
  output         INTEGER NOT NULL,
  quota          INTEGER NOT NULL,
  status         TEXT NOT NULL,
  duration_ms    INTEGER NOT NULL,
  frt_ms         INTEGER NOT NULL,
  request_id     TEXT NOT NULL,
  unmodeled      TEXT NOT NULL,
  grp            TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_token_time ON usage (token_id, created_at, log_id);
CREATE INDEX IF NOT EXISTS usage_time ON usage (created_at, grp);
`

// schemaVersion is stored in PRAGMA user_version; a mismatch means the file was
// written by another uplink version and must be deleted and rebuilt.
const schemaVersion = 2

// Meta keys.
const (
	MetaCurrency      = "currency"
	MetaCostPrecision = "cost_precision"
	MetaCostQuantum   = "cost_quantum"
	MetaLastLogID     = "last_log_id"
	MetaExportedAt    = "exported_at"
	MetaPlatform      = "platform"
	MetaLogConsumeOff = "log_consume_disabled"
	MetaStatusWindowS = "status_window_seconds"
	MetaErrorLogs     = "error_logs" // "true" if New API records failed requests
)

// Open opens (and for writers, creates) the export database.
func Open(path string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
	} else {
		// Rollback journal, not WAL: the publisher reads through a read-only
		// directory mount, where WAL readers cannot create the -shm file.
		q.Add("_pragma", "journal_mode(DELETE)")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("exportdb: open %s: %w", path, err)
	}
	if !readOnly {
		db.SetMaxOpenConns(1)
		if version == 0 {
			if _, err := db.Exec(schema); err != nil {
				db.Close()
				return nil, fmt.Errorf("exportdb: create schema: %w", err)
			}
			if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
				db.Close()
				return nil, err
			}
			version = schemaVersion
		}
	}
	if version != schemaVersion {
		db.Close()
		return nil, fmt.Errorf("exportdb: %s has schema version %d, want %d; delete it and let the exporter rebuild it", path, version, schemaVersion)
	}
	return db, nil
}

// HashKey is how keys are stored: hex SHA-256 of the key as the platform stores it.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// GetMeta returns a meta value, or "" if absent.
func GetMeta(q interface {
	QueryRow(string, ...any) *sql.Row
}, k string) (string, error) {
	var v string
	err := q.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetMeta upserts a meta value.
func SetMeta(tx *sql.Tx, k, v string) error {
	_, err := tx.Exec(`INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// KeyRow is one API key as the publisher needs it.
type KeyRow struct {
	KeyHash            string
	TokenID, UserID    int64
	TokenStatus        int
	UserStatus         int
	RemainQuota        int64
	Unlimited          bool
	ExpiredTime        int64
	ModelLimitsEnabled bool
	ModelLimits        string
	TokenGroup         string
	UserGroup          string
	UserQuota          int64
}

// UsageRow is one request, normalized to disjoint token buckets.
type UsageRow struct {
	LogID, TokenID, CreatedAt                   int64
	Model, Endpoint                             string
	Stream                                      bool
	Input, CachedInput, CacheWrite, CacheWrite1 int64
	Output, Quota                               int64
	Status                                      string // success | error
	DurationMS, FirstTokenMS                    int64
	RequestID                                   string
	Unmodeled                                   string // comma-separated reasons the cost may not follow the catalog
	Group                                       string // the group the request ran in
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ReplaceKeys swaps the whole key table.
func ReplaceKeys(tx *sql.Tx, rows []KeyRow) error {
	if _, err := tx.Exec(`DELETE FROM keys`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO keys VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(r.KeyHash, r.TokenID, r.UserID, r.TokenStatus, r.UserStatus, r.RemainQuota,
			b2i(r.Unlimited), r.ExpiredTime, b2i(r.ModelLimitsEnabled), r.ModelLimits, r.TokenGroup, r.UserGroup, r.UserQuota); err != nil {
			return err
		}
	}
	return nil
}

// LookupKey returns the key row for a hash, or nil.
func LookupKey(db *sql.DB, hash string) (*KeyRow, error) {
	r := KeyRow{KeyHash: hash}
	var unlimited, mle int
	err := db.QueryRow(`SELECT token_id, user_id, token_status, user_status, remain_quota, unlimited, expired_time,
		model_limits_enabled, model_limits, token_group, user_group, user_quota FROM keys WHERE key_hash = ?`, hash).
		Scan(&r.TokenID, &r.UserID, &r.TokenStatus, &r.UserStatus, &r.RemainQuota, &unlimited, &r.ExpiredTime,
			&mle, &r.ModelLimits, &r.TokenGroup, &r.UserGroup, &r.UserQuota)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Unlimited, r.ModelLimitsEnabled = unlimited == 1, mle == 1
	return &r, nil
}

// ReplaceCatalogs replaces the catalogs of the refreshed user groups, keeps
// those of configured groups whose refresh failed, and drops the rest.
func ReplaceCatalogs(tx *sql.Tx, generatedAt string, catalogs map[[2]string]string, refreshed, configured []string) error {
	keep := map[string]bool{}
	for _, g := range configured {
		keep[g] = true
	}
	for _, g := range refreshed {
		keep[g] = false // replaced below
	}
	rows, err := tx.Query(`SELECT DISTINCT user_group FROM catalogs`)
	if err != nil {
		return err
	}
	var drop []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			return err
		}
		if !keep[g] {
			drop = append(drop, g)
		}
	}
	rows.Close()
	for _, g := range drop {
		if _, err := tx.Exec(`DELETE FROM catalogs WHERE user_group = ?`, g); err != nil {
			return err
		}
	}
	for k, js := range catalogs {
		if _, err := tx.Exec(`INSERT INTO catalogs VALUES (?,?,?,?)`, k[0], k[1], js, generatedAt); err != nil {
			return err
		}
	}
	return nil
}

// LookupCatalog returns the models JSON for a (user group, using group) pair.
func LookupCatalog(db *sql.DB, userGroup, usingGroup string) (modelsJSON, generatedAt string, ok bool, err error) {
	err = db.QueryRow(`SELECT models_json, generated_at FROM catalogs WHERE user_group = ? AND using_group = ?`,
		userGroup, usingGroup).Scan(&modelsJSON, &generatedAt)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return modelsJSON, generatedAt, err == nil, err
}

// InsertUsage appends usage rows; rows already present are ignored.
func InsertUsage(tx *sql.Tx, rows []UsageRow) error {
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO usage VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(r.LogID, r.TokenID, r.CreatedAt, r.Model, r.Endpoint, b2i(r.Stream), r.Input, r.CachedInput,
			r.CacheWrite, r.CacheWrite1, r.Output, r.Quota, r.Status, r.DurationMS, r.FirstTokenMS, r.RequestID, r.Unmodeled, r.Group); err != nil {
			return err
		}
	}
	return nil
}

// ScanUsage reads a usage row from a query selecting usageColumns.
const UsageColumns = `log_id, token_id, created_at, model, endpoint, stream, input, cached_input, cache_write,
  cache_write_1h, output, quota, status, duration_ms, frt_ms, request_id, unmodeled, grp`

func ScanUsage(rows *sql.Rows) (UsageRow, error) {
	var r UsageRow
	var stream int
	err := rows.Scan(&r.LogID, &r.TokenID, &r.CreatedAt, &r.Model, &r.Endpoint, &stream, &r.Input, &r.CachedInput,
		&r.CacheWrite, &r.CacheWrite1, &r.Output, &r.Quota, &r.Status, &r.DurationMS, &r.FirstTokenMS, &r.RequestID, &r.Unmodeled, &r.Group)
	r.Stream = stream == 1
	return r, err
}

// Itoa is a small helper for meta values.
func Itoa(v int64) string { return strconv.FormatInt(v, 10) }
