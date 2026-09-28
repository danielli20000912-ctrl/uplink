package newapi

import (
	"testing"

	"uplink/internal/exportdb"
)

func TestNormalizeBuckets(t *testing.T) {
	cases := []struct {
		name                       string
		prompt                     int64
		o                          logOther
		in, cached, write, write1h int64
	}{
		{"openai: prompt includes cache", 10432, logOther{CacheTokens: 9984}, 448, 9984, 0, 0},
		{"openai: cache writes subtracted", 5000, logOther{CacheTokens: 1000, CacheCreationTokens: 2000}, 2000, 1000, 2000, 0},
		{"openai: overlap clamps at zero", 100, logOther{CacheTokens: 80, CacheCreationTokens: 50}, 0, 80, 50, 0},
		{"anthropic: prompt excludes cache", 1200,
			logOther{UsageSemantic: "anthropic", CacheTokens: 30000, CacheCreationTokens: 6000, CacheCreationTokens5m: 4000, CacheCreationTokens1h: 2000},
			1200, 30000, 4000, 2000},
		{"anthropic: unsplit remainder counts as default write", 10,
			logOther{UsageSemantic: "anthropic", CacheCreationTokens: 700, CacheCreationTokens1h: 200},
			10, 0, 500, 200},
	}
	for _, c := range cases {
		var r exportdb.UsageRow
		normalizeBuckets(&r, c.prompt, c.o)
		if r.Input != c.in || r.CachedInput != c.cached || r.CacheWrite != c.write || r.CacheWrite1 != c.write1h {
			t.Errorf("%s: got in=%d cached=%d cw=%d cw1h=%d", c.name, r.Input, r.CachedInput, r.CacheWrite, r.CacheWrite1)
		}
	}
}

func TestUnmodeled(t *testing.T) {
	if got := unmodeled(logOther{ToolSurcharges: []any{1}, Image: true}); got != "tool_surcharge,image_tokens" {
		t.Errorf("got %q", got)
	}
	if unmodeled(logOther{}) != "" {
		t.Error("plain request flagged")
	}
}

func TestEndpointFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/chat/completions":   "chat.completions",
		"/v1/responses":          "responses",
		"/v1/responses/compact":  "responses",
		"/v1/messages":           "messages",
		"/v1/embeddings":         "embeddings",
		"/v1/images/generations": "images",
		"/v1/audio/speech":       "other",
		"":                       "other",
	} {
		if got := EndpointFromPath(path); got != want {
			t.Errorf("%q: got %s, want %s", path, got, want)
		}
	}
}
