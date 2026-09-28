package protocol

// Wire types for uplink v0 responses. spec/schemas is authoritative.

type Discovery struct {
	Protocol        string            `json:"protocol"`
	Versions        []string          `json:"versions"`
	APIBase         string            `json:"api_base"`
	Platform        string            `json:"platform,omitempty"`
	Currency        string            `json:"currency"`
	CostPrecision   int               `json:"cost_precision"`
	CostQuantum     string            `json:"cost_quantum,omitempty"`
	UsageLagSeconds int               `json:"usage_lag_seconds"`
	RequestIDHeader string            `json:"request_id_header,omitempty"`
	Endpoints       map[string]string `json:"endpoints"`
	Capabilities    []string          `json:"capabilities"`
	Contact         string            `json:"contact,omitempty"`
}

type Catalog struct {
	CatalogVersion string          `json:"catalog_version"`
	GeneratedAt    string          `json:"generated_at"`
	Currency       string          `json:"currency"`
	Models         []Model         `json:"models"`
	PendingChanges []PendingChange `json:"pending_changes,omitempty"`
}

type Model struct {
	ID            string             `json:"id"`
	Endpoints     []string           `json:"endpoints"`
	ContextWindow int64              `json:"context_window,omitempty"`
	Price         Price              `json:"price"`
	TimePricing   *TimePricing       `json:"time_pricing,omitempty"`
	Capabilities  *ModelCapabilities `json:"capabilities,omitempty"`
	Status        string             `json:"status"`
}

type ModelCapabilities struct {
	CacheAffinity string `json:"cache_affinity,omitempty"`
	Streaming     *bool  `json:"streaming,omitempty"`
}

type PendingChange struct {
	Model       string       `json:"model"`
	EffectiveAt string       `json:"effective_at"`
	Price       Price        `json:"price"`
	TimePricing *TimePricing `json:"time_pricing,omitempty"`
}

type Account struct {
	Currency         string      `json:"currency"`
	Unlimited        bool        `json:"unlimited"`
	Balance          string      `json:"balance,omitempty"`
	ExpiresAt        string      `json:"expires_at,omitempty"`
	RateLimits       []RateLimit `json:"rate_limits,omitempty"`
	ConcurrencyLimit int         `json:"concurrency_limit,omitempty"`
}

type RateLimit struct {
	Scope     string `json:"scope"`
	Window    string `json:"window"`
	Limit     string `json:"limit"`
	Remaining string `json:"remaining,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
}

type UsagePage struct {
	Records    []Record `json:"records"`
	NextCursor *string  `json:"next_cursor"`
}

type Record struct {
	RequestID      string `json:"request_id"`
	CreatedAt      string `json:"created_at"`
	Model          string `json:"model"`
	Endpoint       string `json:"endpoint"`
	Stream         *bool  `json:"stream,omitempty"`
	Tokens         Tokens `json:"tokens"`
	Units          int64  `json:"units,omitempty"`
	Cost           string `json:"cost"`
	Currency       string `json:"currency"`
	Status         string `json:"status"`
	DurationMS     *int64 `json:"duration_ms,omitempty"`
	CatalogVersion string `json:"catalog_version,omitempty"`
	// XUnmodeled lists charge components the catalog does not describe
	// (e.g. tool_surcharge), so a recomputed cost is not expected to match.
	XUnmodeled []string `json:"x-unmodeled,omitempty"`
}

type UsageSummary struct {
	Granularity string          `json:"granularity"`
	Timezone    string          `json:"timezone"`
	Currency    string          `json:"currency"`
	Buckets     []SummaryBucket `json:"buckets"`
}

type SummaryBucket struct {
	Date     string `json:"date"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   Tokens `json:"tokens"`
	Cost     string `json:"cost"`
}

type Status struct {
	GeneratedAt   string        `json:"generated_at"`
	WindowSeconds int           `json:"window_seconds"`
	Models        []ModelStatus `json:"models"`
	Notices       []Notice      `json:"notices,omitempty"`
}

type ModelStatus struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	SuccessRate     string `json:"success_rate,omitempty"`
	SampleSize      int64  `json:"sample_size"`
	P50FirstTokenMS *int64 `json:"p50_first_token_ms,omitempty"`
}

type Notice struct {
	At    string `json:"at"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Endpoint names.
const (
	EndpointChat       = "chat.completions"
	EndpointResponses  = "responses"
	EndpointMessages   = "messages"
	EndpointEmbeddings = "embeddings"
	EndpointImages     = "images"
	EndpointOther      = "other"
)
