package downstream

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"

	_ "modernc.org/sqlite"
)

// State is sync's own record of which models it manages, what it wrote, and
// what the operator had configured before it took over.
type State struct{ db *sql.DB }

const stateSchema = `
CREATE TABLE IF NOT EXISTS managed (
  model           TEXT PRIMARY KEY,
  adopted_at      INTEGER NOT NULL,
  original_json   TEXT NOT NULL,   -- New API "configured" pricing before adoption
  written_version TEXT NOT NULL,   -- New API pricing version right after our last write
  written_expr    TEXT NOT NULL,
  written_sell    TEXT NOT NULL,   -- sell levels, for change size
  state           TEXT NOT NULL    -- managed | conflict
);
CREATE TABLE IF NOT EXISTS pending (
  model      TEXT PRIMARY KEY,
  expr       TEXT NOT NULL,
  change_pct TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS holds (
  reason     TEXT PRIMARY KEY,
  detail     TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS recon_seen (
  request_id TEXT NOT NULL,
  kind       TEXT NOT NULL,
  PRIMARY KEY (request_id, kind)
);
`

func OpenState(path string) (*State, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(stateSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sync state: %w", err)
	}
	return &State{db: db}, nil
}

func (s *State) Close() { s.db.Close() }

type Managed struct {
	Model          string
	AdoptedAt      int64
	Original       map[string]any
	WrittenVersion string
	WrittenExpr    string
	WrittenSell    []level
	State          string
}

func encodeLevels(lv []level) string {
	type row struct {
		Above int64     `json:"above"`
		P     [5]string `json:"p"`
	}
	rows := make([]row, len(lv))
	for i, l := range lv {
		rows[i].Above = l.Above
		for j, v := range l.P {
			if v != nil {
				rows[i].P[j] = v.RatString()
			}
		}
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

func decodeLevels(s string) []level {
	var rows []struct {
		Above int64     `json:"above"`
		P     [5]string `json:"p"`
	}
	_ = json.Unmarshal([]byte(s), &rows)
	out := make([]level, len(rows))
	for i, r := range rows {
		out[i].Above = r.Above
		for j, v := range r.P {
			if v != "" {
				out[i].P[j], _ = new(big.Rat).SetString(v)
			}
		}
	}
	return out
}

func (s *State) AllManaged() (map[string]*Managed, error) {
	rows, err := s.db.Query(`SELECT model, adopted_at, original_json, written_version, written_expr, written_sell, state FROM managed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Managed{}
	for rows.Next() {
		var m Managed
		var orig, sell string
		if err := rows.Scan(&m.Model, &m.AdoptedAt, &orig, &m.WrittenVersion, &m.WrittenExpr, &sell, &m.State); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(orig), &m.Original)
		m.WrittenSell = decodeLevels(sell)
		out[m.Model] = &m
	}
	return out, rows.Err()
}

func (s *State) PutManaged(m *Managed) error {
	orig, _ := json.Marshal(m.Original)
	_, err := s.db.Exec(`INSERT INTO managed VALUES (?,?,?,?,?,?,?)
ON CONFLICT(model) DO UPDATE SET written_version = excluded.written_version, written_expr = excluded.written_expr,
  written_sell = excluded.written_sell, state = excluded.state`,
		m.Model, m.AdoptedAt, string(orig), m.WrittenVersion, m.WrittenExpr, encodeLevels(m.WrittenSell), m.State)
	return err
}

func (s *State) DeleteManaged(model string) error {
	_, err := s.db.Exec(`DELETE FROM managed WHERE model = ?`, model)
	return err
}

func (s *State) SetPending(model, expr, pct string, now int64) error {
	_, err := s.db.Exec(`INSERT INTO pending VALUES (?,?,?,?) ON CONFLICT(model) DO UPDATE SET expr = excluded.expr,
  change_pct = excluded.change_pct`, model, expr, pct, now)
	return err
}

// Pending returns the held expression for a model, if any.
func (s *State) Pending(model string) (string, bool, error) {
	var expr string
	err := s.db.QueryRow(`SELECT expr FROM pending WHERE model = ?`, model).Scan(&expr)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return expr, err == nil, err
}

func (s *State) ClearPending(model string) error {
	_, err := s.db.Exec(`DELETE FROM pending WHERE model = ?`, model)
	return err
}

func (s *State) AddHold(reason, detail string, now int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO holds VALUES (?,?,?)`, reason, detail, now)
	return err
}

func (s *State) Holds() ([]string, error) {
	rows, err := s.db.Query(`SELECT reason || ': ' || detail FROM holds ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *State) ClearHolds() error {
	_, err := s.db.Exec(`DELETE FROM holds`)
	return err
}

// FirstSeen records (id, kind) and reports whether it was new.
func (s *State) FirstSeen(id, kind string) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO recon_seen VALUES (?, ?)`, id, kind)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
