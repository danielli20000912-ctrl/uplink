// Package protocol holds the uplink v0 wire types and the reference cost formula.
// spec/schemas is the source of truth; these types mirror it.
package protocol

// Price units.
const (
	UnitPer1MTokens = "per_1m_tokens"
	UnitPerRequest  = "per_request"
	UnitCustom      = "custom"
)

// Price is the union of the three v0 price shapes, discriminated by Unit.
// Decimal fields are strings on the wire; nil means the field was omitted.
type Price struct {
	Unit string `json:"unit"`

	// per_1m_tokens
	Input        *string `json:"input,omitempty"`
	CachedInput  *string `json:"cached_input,omitempty"`
	CacheWrite   *string `json:"cache_write,omitempty"`
	CacheWrite1h *string `json:"cache_write_1h,omitempty"`
	Output       *string `json:"output,omitempty"`
	Basis        string  `json:"basis,omitempty"`
	Tiers        []Tier  `json:"tiers,omitempty"`

	// per_request
	Price *string `json:"price,omitempty"`

	// custom
	Description string `json:"description,omitempty"`
}

// Tier applies when context tokens are strictly greater than AboveTokens.
type Tier struct {
	AboveTokens  int64   `json:"above_tokens"`
	Input        *string `json:"input,omitempty"`
	CachedInput  *string `json:"cached_input,omitempty"`
	CacheWrite   *string `json:"cache_write,omitempty"`
	CacheWrite1h *string `json:"cache_write_1h,omitempty"`
	Output       *string `json:"output,omitempty"`
}

// TimePricing multiplies the whole cost of requests created inside a window.
type TimePricing struct {
	Timezone     string       `json:"timezone"`
	WeekdaysOnly bool         `json:"weekdays_only,omitempty"`
	Windows      []TimeWindow `json:"windows"`
}

// TimeWindow is [Start, End) in local wall-clock time, HH:MM.
type TimeWindow struct {
	Start      string `json:"start"`
	End        string `json:"end"`
	Multiplier string `json:"multiplier"`
}

// Tokens are disjoint billed buckets. Reasoning is a subset of Output and is not billed separately.
type Tokens struct {
	Input        int64 `json:"input"`
	CachedInput  int64 `json:"cached_input"`
	CacheWrite   int64 `json:"cache_write,omitempty"`
	CacheWrite1h int64 `json:"cache_write_1h,omitempty"`
	Output       int64 `json:"output"`
	Reasoning    int64 `json:"reasoning,omitempty"`
}

// Context is the token count used for tier selection.
func (t Tokens) Context() int64 {
	return t.Input + t.CachedInput + t.CacheWrite + t.CacheWrite1h
}
