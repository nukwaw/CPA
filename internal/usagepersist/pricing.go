package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const PricingSourceURL = "https://models.dev/api.json"
const syncedPricesNamespace = "pricing_synced"
const manualPricesNamespace = "pricing_manual"

func validPrice(p Price) error {
	if strings.TrimSpace(p.Model) == "" || !safeCacheText(p.Model, 512) {
		return errors.New("model must contain 1 to 512 characters")
	}
	for _, value := range []float64{p.InputPerMillion, p.OutputPerMillion, p.CacheReadPerMillion, p.CacheWritePerMillion} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e9 {
			return errors.New("rates must be finite nonnegative USD per million tokens (maximum 1e9)")
		}
	}
	return nil
}

func (s *Store) prices(ctx context.Context) (map[string]Price, error) {
	values, err := s.store.Cache(ctx, syncedPricesNamespace)
	if err != nil {
		return nil, err
	}
	prices, err := cacheDecode[Price](values)
	if err != nil {
		return nil, err
	}
	values, err = s.store.Cache(ctx, manualPricesNamespace)
	if err != nil {
		return nil, err
	}
	manual, err := cacheDecode[Price](values)
	if err != nil {
		return nil, err
	}
	for key, p := range manual {
		prices[key] = p
	}
	return prices, nil
}

func (s *Store) Prices(ctx context.Context) ([]Price, error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	defer s.active.Done()
	prices, err := s.prices(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Price, 0, len(prices))
	for _, p := range prices {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Model < result[j].Model })
	return result, nil
}

func (s *Store) SetPrice(ctx context.Context, p Price) (Price, error) {
	if err := s.begin(); err != nil {
		return Price{}, err
	}
	defer s.active.Done()
	p.Model = strings.TrimSpace(p.Model)
	if err := validPrice(p); err != nil {
		return Price{}, err
	}
	p.Manual = true
	p.CacheReadAvailable = true
	p.CacheWriteAvailable = true
	p.Source = "manual"
	p.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(p)
	if err != nil {
		return Price{}, err
	}
	err = s.store.UpdateCache(ctx, []cacheUpdate{{Namespace: manualPricesNamespace, Key: p.Model, Value: data}})
	return p, err
}

func (s *Store) ResetPrice(ctx context.Context, model string) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.active.Done()
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model is required")
	}
	return s.store.UpdateCache(ctx, []cacheUpdate{{Namespace: manualPricesNamespace, Key: model, Delete: true}})
}

func applyPrice(e *Event, prices map[string]Price) {
	// A manual exact model override always wins. Provider-qualified catalog
	// rates then avoid accidentally pricing an aggregator using another vendor.
	p, ok := prices[e.Model]
	if !ok || !p.Manual {
		if qualified, exists := prices[e.Provider+"/"+e.Model]; exists {
			p = qualified
			ok = true
		}
	}
	if !ok {
		return
	}
	if e.AccountingQuality != "complete" || e.UnclassifiedTokens > 0 {
		return
	}
	if !p.Manual && ((e.CacheReadTokens > 0 && !p.CacheReadAvailable) || (e.CacheWriteTokens > 0 && !p.CacheWriteAvailable)) {
		return
	}
	uncached := max(e.InputTokens-e.CacheReadTokens-e.CacheWriteTokens, 0)
	e.CostUSD = (float64(uncached)*p.InputPerMillion + float64(e.OutputTokens)*p.OutputPerMillion + float64(e.CacheReadTokens)*p.CacheReadPerMillion + float64(e.CacheWriteTokens)*p.CacheWritePerMillion) / 1e6
	e.Priced = true
}

type SyncResult struct {
	Updated       int       `json:"updated"`
	SkippedManual int       `json:"skipped_manual"`
	Source        string    `json:"source"`
	SyncedAt      time.Time `json:"synced_at"`
}
type catalogProvider struct {
	Models map[string]catalogModel `json:"models"`
}
type catalogModel struct {
	ID   string `json:"id"`
	Cost struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	} `json:"cost"`
}

// SyncPricing uses the same default catalog as cpa-usage-keeper. No caller-provided
// URL is accepted, and manual settings live separately so sync cannot overwrite them.
func (s *Store) SyncPricing(ctx context.Context) (SyncResult, error) {
	if err := s.begin(); err != nil {
		return SyncResult{}, err
	}
	defer s.active.Done()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, PricingSourceURL, nil)
	if err != nil {
		return SyncResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return SyncResult{}, errors.New("fetch Models.dev pricing failed")
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.Debug("usage persistence: failed to close pricing response")
		}
	}()
	if response.StatusCode != http.StatusOK {
		return SyncResult{}, fmt.Errorf("pricing source returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<20)+1))
	if err != nil {
		return SyncResult{}, errors.New("read pricing catalog failed")
	}
	if len(data) > 64<<20 {
		return SyncResult{}, errors.New("pricing catalog exceeds 64 MiB")
	}
	prices, err := decodeCatalog(data, time.Now().UTC())
	if err != nil {
		return SyncResult{}, err
	}
	manual, err := s.store.Cache(ctx, manualPricesNamespace)
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{Source: "models.dev", SyncedAt: time.Now().UTC()}
	updates := make([]cacheUpdate, 0, len(prices))
	for _, key := range sortedKeys(prices) {
		p := prices[key]
		value, errEncode := json.Marshal(p)
		if errEncode != nil {
			return SyncResult{}, errEncode
		}
		updates = append(updates, cacheUpdate{Namespace: syncedPricesNamespace, Key: key, Value: value})
		if _, exists := manual[key]; exists {
			result.SkippedManual++
		} else {
			result.Updated++
		}
	}
	if len(updates) == 0 {
		return SyncResult{}, errors.New("pricing catalog contained no valid token prices")
	}
	if err = s.store.UpdateCache(ctx, updates); err != nil {
		return SyncResult{}, err
	}
	return result, nil
}

func decodeCatalog(data []byte, at time.Time) (map[string]Price, error) {
	var catalog map[string]catalogProvider
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, errors.New("invalid Models.dev pricing catalog")
	}
	prices := map[string]Price{}
	ranks := map[string]int{}
	for _, provider := range sortedKeys(catalog) {
		for _, key := range sortedKeys(catalog[provider].Models) {
			model := catalog[provider].Models[key]
			id := strings.TrimSpace(model.ID)
			if id == "" {
				id = key
			}
			if model.Cost.Input == nil || model.Cost.Output == nil {
				continue
			}
			p := Price{Model: id, InputPerMillion: *model.Cost.Input, OutputPerMillion: *model.Cost.Output, Source: "models.dev", UpdatedAt: at}
			if model.Cost.CacheRead != nil {
				p.CacheReadPerMillion = *model.Cost.CacheRead
				p.CacheReadAvailable = true
			}
			if model.Cost.CacheWrite != nil {
				p.CacheWritePerMillion = *model.Cost.CacheWrite
				p.CacheWriteAvailable = true
			}
			if validPrice(p) != nil {
				continue
			}
			qualified := provider + "/" + id
			p.Model = qualified
			prices[qualified] = p
			// Native model vendor has precedence over resale/subscription catalogs.
			rank := catalogRank(provider, id)
			if existing, ok := ranks[id]; !ok || rank < existing {
				p.Model = id
				prices[id] = p
				ranks[id] = rank
			}
		}
	}
	return prices, nil
}
func catalogRank(provider, model string) int {
	model = strings.ToLower(model)
	preferred := ""
	switch {
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"), strings.HasPrefix(model, "chatgpt-"):
		preferred = "openai"
	case strings.HasPrefix(model, "claude-"):
		preferred = "anthropic"
	case strings.HasPrefix(model, "gemini-"):
		preferred = "google"
	case strings.HasPrefix(model, "grok-"):
		preferred = "xai"
	case strings.HasPrefix(model, "deepseek-"):
		preferred = "deepseek"
	case strings.HasPrefix(model, "kimi-"):
		preferred = "moonshotai"
	}
	if provider == preferred {
		return 0
	}
	switch provider {
	case "openai", "anthropic", "google", "xai", "deepseek", "moonshotai":
		return 10
	default:
		return 100
	}
}
