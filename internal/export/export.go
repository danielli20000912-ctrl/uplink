// Package export runs the exporter: the only uplink process that can read the
// upstream platform. It has no network listener; each cycle it copies the
// minimum the publisher needs into the export database.
package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"sort"
	"strconv"
	"time"

	"uplink/adapters/newapi"
	"uplink/internal/exportdb"
	"uplink/internal/protocol"
)

type Probe struct {
	Group          string `yaml:"group"`
	AccessTokenEnv string `yaml:"access_token_env"`
}

type Config struct {
	NewAPI struct {
		BaseURL   string  `yaml:"base_url"`
		DBPath    string  `yaml:"db_path"`
		LogDBPath string  `yaml:"log_db_path"`
		Probes    []Probe `yaml:"probes"`
		// ErrorLogs must match New API's ERROR_LOG_ENABLED (default false).
		// Without error logs, failed requests leave no row, so a success
		// rate would always read 100%; the status endpoint is then not
		// published.
		ErrorLogs bool `yaml:"error_logs"`
	} `yaml:"newapi"`
	Currency      string        `yaml:"currency"`
	PricePerUSD   string        `yaml:"price_per_usd"`
	ExportDB      string        `yaml:"export_db"`
	Interval      time.Duration `yaml:"interval"`
	RetentionDays int           `yaml:"retention_days"`
	StatusWindow  time.Duration `yaml:"status_window"`
	BatchSize     int           `yaml:"batch_size"`
}

func (c *Config) setDefaults() {
	if c.Interval == 0 {
		c.Interval = 30 * time.Second
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 62
	}
	if c.StatusWindow == 0 {
		c.StatusWindow = 15 * time.Minute
	}
	if c.BatchSize == 0 {
		c.BatchSize = 5000
	}
}

func (c *Config) validate() error {
	switch {
	case c.NewAPI.DBPath == "":
		return fmt.Errorf("newapi.db_path is required")
	case c.NewAPI.BaseURL == "":
		return fmt.Errorf("newapi.base_url is required")
	case len(c.Currency) != 3:
		return fmt.Errorf("currency must be an ISO 4217 code")
	case c.ExportDB == "":
		return fmt.Errorf("export_db is required")
	}
	if r, ok := new(big.Rat).SetString(c.PricePerUSD); !ok || r.Sign() <= 0 {
		return fmt.Errorf("price_per_usd must be a positive decimal")
	}
	return nil
}

type Exporter struct {
	cfg    Config
	src    *newapi.Source
	out    *sql.DB
	tokens map[string]string // group -> probe access token
}

func New(cfg Config) (*Exporter, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("export config: %w", err)
	}
	tokens := map[string]string{}
	for _, p := range cfg.NewAPI.Probes {
		t := os.Getenv(p.AccessTokenEnv)
		if t == "" {
			return nil, fmt.Errorf("export config: probe for a group has empty %s", p.AccessTokenEnv)
		}
		tokens[p.Group] = t
	}
	src, err := newapi.OpenSQLite(cfg.NewAPI.DBPath, cfg.NewAPI.LogDBPath, cfg.NewAPI.BaseURL)
	if err != nil {
		return nil, err
	}
	out, err := exportdb.Open(cfg.ExportDB, false)
	if err != nil {
		src.Close()
		return nil, err
	}
	return &Exporter{cfg: cfg, src: src, out: out, tokens: tokens}, nil
}

func (e *Exporter) Close() {
	e.src.Close()
	e.out.Close()
}

// Run exports every interval until ctx is done.
func (e *Exporter) Run(ctx context.Context) error {
	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()
	for {
		if err := e.Once(ctx); err != nil {
			log.Printf("export: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Once performs one export cycle.
func (e *Exporter) Once(ctx context.Context) error {
	now := time.Now()
	opts, err := e.src.ReadOptions(ctx)
	if err != nil {
		return err
	}
	pricePerUSD, _ := new(big.Rat).SetString(e.cfg.PricePerUSD)
	money := newapi.Money{QuotaPerUnit: opts.QuotaPerUnit, PricePerUSD: pricePerUSD}
	quantum := money.Quantum()
	precision := protocol.DecimalPlaces(quantum)
	if precision < 0 {
		return fmt.Errorf("price_per_usd / QuotaPerUnit needs more than %d decimal places", protocol.MaxDecimalPlaces)
	}

	keys, err := e.src.ReadKeys(ctx)
	if err != nil {
		return err
	}
	catalogs, refreshed, err := e.buildCatalogs(ctx, money)
	if err != nil {
		return err
	}

	tx, err := e.out.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	lastID := int64(0)
	if v, err := exportdb.GetMeta(tx, exportdb.MetaLastLogID); err != nil {
		return err
	} else if v != "" {
		lastID, _ = strconv.ParseInt(v, 10, 64)
	}
	cutoff := now.AddDate(0, 0, -e.cfg.RetentionDays).Unix()
	if lastID == 0 {
		if lastID, err = e.src.LastLogIDBefore(ctx, cutoff); err != nil {
			return err
		}
	}
	for {
		rows, err := e.src.ReadLogs(ctx, lastID, e.cfg.BatchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		if err := exportdb.InsertUsage(tx, rows); err != nil {
			return err
		}
		lastID = rows[len(rows)-1].LogID
		if len(rows) < e.cfg.BatchSize {
			break
		}
	}

	if _, err := tx.Exec(`DELETE FROM usage WHERE created_at < ?`, cutoff); err != nil {
		return err
	}
	if err := exportdb.ReplaceKeys(tx, keys); err != nil {
		return err
	}
	if err := exportdb.ReplaceCatalogs(tx, now.UTC().Format(time.RFC3339), catalogs, refreshed, e.probeGroups()); err != nil {
		return err
	}
	meta := map[string]string{
		exportdb.MetaCurrency:      e.cfg.Currency,
		exportdb.MetaCostPrecision: strconv.Itoa(precision),
		exportdb.MetaCostQuantum:   protocol.FormatDecimal(quantum),
		exportdb.MetaLastLogID:     exportdb.Itoa(lastID),
		exportdb.MetaExportedAt:    now.UTC().Format(time.RFC3339),
		exportdb.MetaPlatform:      "new-api",
		exportdb.MetaLogConsumeOff: strconv.FormatBool(!opts.LogConsumeEnabled),
		exportdb.MetaStatusWindowS: strconv.Itoa(int(e.cfg.StatusWindow.Seconds())),
		exportdb.MetaErrorLogs:     strconv.FormatBool(e.cfg.NewAPI.ErrorLogs),
	}
	for k, v := range meta {
		if err := exportdb.SetMeta(tx, k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !opts.LogConsumeEnabled {
		log.Printf("export: New API consume logging is off; usage records will be empty")
	}
	return nil
}

// buildCatalogs resolves one catalog per (probe group, using group) pair. A
// probe that fails is logged and skipped, so its previous catalogs are kept and
// keys and usage still export.
func (e *Exporter) buildCatalogs(ctx context.Context, money newapi.Money) (map[[2]string]string, []string, error) {
	out := map[[2]string]string{}
	groups := e.probeGroups()
	var refreshed []string
	for i, g := range groups {
		resp, err := e.src.FetchPricing(ctx, e.tokens[g])
		if err != nil {
			log.Printf("export: probe #%d failed, keeping its previous catalogs: %v", i+1, err)
			continue
		}
		using := map[string]bool{g: true}
		for u := range resp.UsableGroup {
			using[u] = true
		}
		for u := range using {
			if u == "auto" {
				continue
			}
			models, err := newapi.BuildModels(resp, u, money)
			if err != nil {
				continue // group ratio not visible to this probe: no catalog for that pair
			}
			js, err := json.Marshal(models)
			if err != nil {
				return nil, nil, err
			}
			out[[2]string{g, u}] = string(js)
		}
		refreshed = append(refreshed, g)
	}
	return out, refreshed, nil
}

func (e *Exporter) probeGroups() []string {
	groups := make([]string, 0, len(e.tokens))
	for g := range e.tokens {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return groups
}
