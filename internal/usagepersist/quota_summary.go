package usagepersist

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	quotaSummaryDefaultRange = 7 * 24 * time.Hour
	quotaSummaryMaxRange     = 90 * 24 * time.Hour
)

// quotaSummaryQuery is the validated /quota/summary request. Provider is already
// trimmed and lowercased; times are UTC.
type quotaSummaryQuery struct {
	Provider string
	Account  string
	Window   string
	From     time.Time
	To       time.Time
}

type quotaSummaryRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// quotaSummaryObservation is one plotted point. Only the window id and its
// percentage leave storage, so no secret can reach the response.
type quotaSummaryObservation struct {
	ObservedAt  time.Time `json:"observed_at"`
	Source      string    `json:"source"`
	UsedPercent *float64  `json:"used_percent"`
}

// quotaSummaryUsage is the persisted usage aggregate for the same credential and
// range. It deliberately exposes a fixed, secret-free set of fields.
type quotaSummaryUsage struct {
	Requests         int64   `json:"requests"`
	PricedRequests   int64   `json:"priced_requests"`
	UnpricedRequests int64   `json:"unpriced_requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// quotaSummaryEstimate extrapolates a full window cost from the priced usage
// observed so far and the latest reported utilization. Every money field stays
// null when it cannot be derived from real prices.
type quotaSummaryEstimate struct {
	UsedPercent *float64 `json:"used_percent"`
	UsedUSD     *float64 `json:"used_usd"`
	FullUSD     *float64 `json:"full_usd"`
	UnusedUSD   *float64 `json:"unused_usd"`
}

type quotaSummaryResponse struct {
	Provider     string                    `json:"provider"`
	Account      string                    `json:"account"`
	Window       string                    `json:"window"`
	Range        quotaSummaryRange         `json:"range"`
	Observations []quotaSummaryObservation `json:"observations"`
	Latest       *quotaSummaryObservation  `json:"latest"`
	Usage        quotaSummaryUsage         `json:"usage"`
	Estimate     *quotaSummaryEstimate     `json:"estimate"`
}

// parseQuotaSummaryQuery mirrors parseFilter's time rules and adds the required
// credential parameters plus the documented range cap.
func parseQuotaSummaryQuery(c *gin.Context) (quotaSummaryQuery, error) {
	query := quotaSummaryQuery{
		Provider: strings.ToLower(strings.TrimSpace(c.Query("provider"))),
		Account:  strings.TrimSpace(c.Query("account")),
		Window:   strings.TrimSpace(c.Query("window")),
	}
	if query.Provider == "" {
		return query, errors.New("provider is required")
	}
	if query.Provider == "" {
		return query, errors.New("provider is required")
	}
	// An account is optional: a credential that exposes no account property is
	// grouped by provider alone.
	if len(query.Provider) > maxQuotaIdentityText || len(query.Account) > maxQuotaIdentityText {
		return query, errors.New("provider and account must be at most 256 characters")
	}
	query.To = time.Now().UTC()
	if raw := c.Query("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return query, errors.New("to must be RFC3339")
		}
		query.To = parsed.UTC()
	}
	query.From = query.To.Add(-quotaSummaryDefaultRange)
	if raw := c.Query("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return query, errors.New("from must be RFC3339")
		}
		query.From = parsed.UTC()
	}
	if !query.From.Before(query.To) {
		return query, errors.New("from must be before to")
	}
	if query.To.Sub(query.From) > quotaSummaryMaxRange {
		return query, errors.New("range must not exceed 90 days")
	}
	return query, nil
}

// quotaSummary builds the response with no dependency on a live credential
// binding: a credential with no history still returns its real usage aggregate.
func (s *Store) quotaSummary(ctx context.Context, query quotaSummaryQuery) (quotaSummaryResponse, error) {
	if err := s.begin(); err != nil {
		return quotaSummaryResponse{}, err
	}
	defer s.active.Done()
	row, err := s.quotaHistory(ctx, query.Provider, query.Account)
	if err != nil {
		return quotaSummaryResponse{}, err
	}
	window := query.Window
	if window == "" {
		window = selectQuotaSummaryWindow(row.Observations, query.From, query.To)
	}
	observations := make([]quotaSummaryObservation, 0)
	for _, entry := range row.Observations {
		if entry.ObservedAt.Before(query.From) || !entry.ObservedAt.Before(query.To) {
			continue
		}
		percent, exists := entry.Windows[window]
		if !exists {
			continue
		}
		value := percent
		observations = append(observations, quotaSummaryObservation{ObservedAt: entry.ObservedAt, Source: entry.Source, UsedPercent: &value})
	}
	var latest *quotaSummaryObservation
	if len(observations) > 0 {
		last := observations[len(observations)-1]
		latest = &last
	}
	usage, err := s.quotaSummaryUsage(ctx, query)
	if err != nil {
		return quotaSummaryResponse{}, err
	}
	return quotaSummaryResponse{
		Provider: query.Provider, Account: query.Account, Window: window,
		Range:        quotaSummaryRange{From: query.From, To: query.To},
		Observations: observations, Latest: latest, Usage: usage,
		Estimate: deriveQuotaSummaryEstimate(usage, latest),
	}, nil
}

// selectQuotaSummaryWindow prefers the window with the most in-range
// observations, then the newest observed window. The fallback only labels an
// empty series, because no in-range observation exists to chart.
func selectQuotaSummaryWindow(observations []quotaHistoryEntry, from, to time.Time) string {
	counts := map[string]int{}
	for _, entry := range observations {
		if entry.ObservedAt.Before(from) || !entry.ObservedAt.Before(to) {
			continue
		}
		for id := range entry.Windows {
			counts[id]++
		}
	}
	selected, best := "", 0
	for _, id := range sortedKeys(counts) {
		if counts[id] > best {
			selected, best = id, counts[id]
		}
	}
	if selected != "" {
		return selected
	}
	for i := len(observations) - 1; i >= 0; i-- {
		for _, id := range sortedKeys(observations[i].Windows) {
			return id
		}
	}
	return ""
}

// quotaSummaryUsage reuses the existing Walk + applyPrice + Summary path so the
// aggregate always matches /events and /analysis.
func (s *Store) quotaSummaryUsage(ctx context.Context, query quotaSummaryQuery) (quotaSummaryUsage, error) {
	prices, err := s.prices(ctx)
	if err != nil {
		return quotaSummaryUsage{}, err
	}
	var summary Summary
	filter := Filter{Provider: query.Provider, Account: query.Account, From: query.From, To: query.To}
	err = s.store.Walk(ctx, filter, func(event Event) error {
		applyPrice(&event, prices)
		summary.add(event)
		return nil
	})
	if err != nil {
		return quotaSummaryUsage{}, err
	}
	cost, _ := finiteQuotaUSD(summary.CostUSD)
	return quotaSummaryUsage{
		Requests: summary.Requests, PricedRequests: summary.PricedRequests, UnpricedRequests: summary.UnpricedRequests,
		InputTokens: summary.InputTokens, OutputTokens: summary.OutputTokens,
		CacheReadTokens: summary.CacheReadTokens, CacheWriteTokens: summary.CacheWriteTokens,
		TotalTokens: summary.TotalTokens, CostUSD: cost,
	}, nil
}

// deriveQuotaSummaryEstimate derives the window cost projection. It returns nil when
// there is no usable latest observation, and null money fields when a projection
// would have to invent a price.
func deriveQuotaSummaryEstimate(usage quotaSummaryUsage, latest *quotaSummaryObservation) *quotaSummaryEstimate {
	if latest == nil || latest.UsedPercent == nil {
		return nil
	}
	percent := *latest.UsedPercent
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return nil
	}
	estimate := &quotaSummaryEstimate{UsedPercent: &percent}
	if used, ok := finiteQuotaUSD(usage.CostUSD); ok {
		estimate.UsedUSD = &used
	}
	if percent <= 0 || usage.PricedRequests <= 0 || usage.CostUSD <= 0 {
		return estimate
	}
	full, ok := finiteQuotaUSD(usage.CostUSD / (percent / 100))
	if !ok {
		return estimate
	}
	unused, _ := finiteQuotaUSD(math.Max(0, full-usage.CostUSD))
	estimate.FullUSD = &full
	estimate.UnusedUSD = &unused
	return estimate
}

// finiteQuotaUSD rounds money to at most 6 decimals and rejects non-finite values.
func finiteQuotaUSD(value float64) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return math.Round(value*1e6) / 1e6, true
}

func (s *Store) quotaSummaryHTTP(c *gin.Context) {
	query, err := parseQuotaSummaryQuery(c)
	if err != nil {
		badRequest(c, err)
		return
	}
	response, err := s.quotaSummary(c.Request.Context(), query)
	respond(c, response, err)
}
