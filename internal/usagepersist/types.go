// Package usagepersist stores secret-free copies of the built-in usage provider's output.
package usagepersist

import (
	"encoding/json"
	"time"
)

// Event intentionally excludes API keys, auth files, URLs, headers and request/response bodies.
// Input includes cache reads/writes; output includes reasoning, using SDK canonical accounting.
type Event struct {
	ID                 string    `json:"id"`
	RequestedAt        time.Time `json:"requested_at"`
	Provider           string    `json:"provider"`
	Model              string    `json:"model"`
	Alias              string    `json:"alias,omitempty"`
	AuthIndex          string    `json:"auth_index,omitempty"`
	KeyID              string    `json:"key_id,omitempty"`
	InputTokens        int64     `json:"input_tokens"`
	OutputTokens       int64     `json:"output_tokens"`
	ReasoningTokens    int64     `json:"reasoning_tokens"`
	CacheReadTokens    int64     `json:"cache_read_tokens"`
	CacheWriteTokens   int64     `json:"cache_write_tokens"`
	TotalTokens        int64     `json:"total_tokens"`
	UnclassifiedTokens int64     `json:"unclassified_tokens"`
	AccountingQuality  string    `json:"accounting_quality"`
	LatencyMS          float64   `json:"latency_ms"`
	TTFTMS             float64   `json:"ttft_ms"`
	Failed             bool      `json:"failed"`
	StatusCode         int       `json:"status_code"`
	Stream             bool      `json:"stream"`
	CostUSD            float64   `json:"cost_usd"`
	Priced             bool      `json:"priced"`
}

type Filter struct {
	From, To                                  time.Time
	Model, Provider, AuthIndex, KeyID, Status string
}

func (f Filter) matches(e Event) bool {
	return (f.From.IsZero() || !e.RequestedAt.Before(f.From)) &&
		(f.To.IsZero() || e.RequestedAt.Before(f.To)) &&
		(f.Model == "" || e.Model == f.Model) && (f.Provider == "" || e.Provider == f.Provider) &&
		(f.AuthIndex == "" || e.AuthIndex == f.AuthIndex) && (f.KeyID == "" || e.KeyID == f.KeyID) &&
		(f.Status == "" || f.Status == "failed" && e.Failed || f.Status == "success" && !e.Failed)
}

type Price struct {
	Model                string    `json:"model"`
	InputPerMillion      float64   `json:"input_per_million"`
	OutputPerMillion     float64   `json:"output_per_million"`
	CacheReadPerMillion  float64   `json:"cache_read_per_million"`
	CacheWritePerMillion float64   `json:"cache_write_per_million"`
	CacheReadAvailable   bool      `json:"cache_read_available"`
	CacheWriteAvailable  bool      `json:"cache_write_available"`
	Source               string    `json:"source"`
	Manual               bool      `json:"manual"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type Summary struct {
	Requests         int64   `json:"requests"`
	Successes        int64   `json:"successes"`
	Failures         int64   `json:"failures"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	PricedRequests   int64   `json:"priced_requests"`
	UnpricedRequests int64   `json:"unpriced_requests"`
	AverageLatencyMS float64 `json:"average_latency_ms"`
}

func (s *Summary) add(e Event) {
	s.Requests++
	if e.Failed {
		s.Failures++
	} else {
		s.Successes++
	}
	s.InputTokens += e.InputTokens
	s.OutputTokens += e.OutputTokens
	s.ReasoningTokens += e.ReasoningTokens
	s.CacheReadTokens += e.CacheReadTokens
	s.CacheWriteTokens += e.CacheWriteTokens
	s.TotalTokens += e.TotalTokens
	s.CostUSD += e.CostUSD
	if e.Priced {
		s.PricedRequests++
	} else {
		s.UnpricedRequests++
	}
	s.AverageLatencyMS += (e.LatencyMS - s.AverageLatencyMS) / float64(s.Requests)
}

type SeriesPoint struct {
	Time time.Time `json:"time"`
	Summary
}
type NamedSummary struct {
	Name string `json:"name"`
	Summary
}
type Analysis struct {
	Summary     Summary        `json:"summary"`
	Series      []SeriesPoint  `json:"series"`
	Models      []NamedSummary `json:"models"`
	Providers   []NamedSummary `json:"providers"`
	GeneratedAt time.Time      `json:"generated_at"`
}
type EventPage struct {
	Events      []Event   `json:"events"`
	Total       int64     `json:"total"`
	GeneratedAt time.Time `json:"generated_at"`
}

type cacheUpdate struct {
	Namespace string          `json:"namespace"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value,omitempty"`
	Delete    bool            `json:"delete,omitempty"`
}
