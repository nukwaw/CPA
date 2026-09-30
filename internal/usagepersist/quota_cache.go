package usagepersist

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
)

// QuotaCacheEntry is the existing management UI's display-only quota state.
// Its strict recursive schema excludes arbitrary response/request objects and errors.
// It is keyed by (Provider, Account); there is no credential generation or revision,
// because neither survives ordinary credential activity.
type QuotaCacheEntry struct {
	Provider    string          `json:"provider"`
	Key         string          `json:"key"`
	Account     string          `json:"account"`
	AccountKind string          `json:"account_kind,omitempty"`
	ObservedAt  time.Time       `json:"observed_at"`
	State       json.RawMessage `json:"state"`
}
type quotaCacheEnvelope struct {
	Entries []QuotaCacheEntry `json:"entries"`
}

type cacheSchema struct {
	kind    string
	fields  map[string]*cacheSchema
	element *cacheSchema
}

func scalar(kind string) *cacheSchema { return &cacheSchema{kind: kind} }
func object(fields map[string]*cacheSchema) *cacheSchema {
	return &cacheSchema{kind: "object", fields: fields}
}
func array(element *cacheSchema) *cacheSchema { return &cacheSchema{kind: "array", element: element} }

var quotaCacheSchemas = buildQuotaCacheSchemas()

func buildQuotaCacheSchemas() map[string]*cacheSchema {
	s, n, b := scalar("string"), scalar("number"), scalar("bool")
	labelParams := object(map[string]*cacheSchema{"name": s, "duration": s, "index": n})
	window := object(map[string]*cacheSchema{"id": s, "label": s, "labelKey": s, "labelParams": labelParams, "usedPercent": n, "remainingPercent": n, "resetLabel": s, "resetAtMs": n, "periodHours": n})
	credit := object(map[string]*cacheSchema{"id": s, "status": s, "grantedAt": s, "expiresAt": s})
	bucket := object(map[string]*cacheSchema{"id": s, "label": s, "window": s, "remainingFraction": n, "resetTime": s, "description": s, "resetAtMs": n, "periodHours": n})
	group := object(map[string]*cacheSchema{"id": s, "label": s, "description": s, "buckets": array(bucket)})
	billing := object(map[string]*cacheSchema{
		"mode": s, "source": s, "planType": s, "healthStatus": s, "periodType": s, "usagePercent": n, "periodStart": s, "periodEnd": s,
		"productUsage":      array(object(map[string]*cacheSchema{"product": s, "usagePercent": n})),
		"monthlyLimitCents": n, "usedCents": n, "includedUsedCents": n, "onDemandCapCents": n, "onDemandUsedCents": n, "onDemandUsedPercent": n,
		"billingPeriodStart": s, "billingPeriodEnd": s, "usedPercent": n, "resetAtMs": n, "periodHours": n,
	})
	return map[string]*cacheSchema{
		"codex":       object(map[string]*cacheSchema{"status": s, "windows": array(window), "planType": s, "subscriptionActiveUntil": scalar("string_or_number"), "rateLimitResetCreditsAvailableCount": n, "rateLimitResetCreditsApplicableAvailableCount": n, "rateLimitResetCredits": array(credit)}),
		"claude":      object(map[string]*cacheSchema{"status": s, "windows": array(window), "planType": s, "extraUsage": object(map[string]*cacheSchema{"is_enabled": b, "monthly_limit": n, "used_credits": n, "utilization": n})}),
		"antigravity": object(map[string]*cacheSchema{"status": s, "groups": array(group), "subscription": object(map[string]*cacheSchema{"plan": s, "tierName": s, "tierId": s}), "serverTimeOffsetMs": n}),
		"devin":       object(map[string]*cacheSchema{"status": s, "windows": array(window), "observedAtMs": n, "plan": s, "planStartMs": n, "planEndMs": n}),
		"kimi":        object(map[string]*cacheSchema{"status": s, "rows": array(object(map[string]*cacheSchema{"id": s, "label": s, "labelKey": s, "labelParams": labelParams, "used": n, "limit": n, "resetHint": s, "resetAtMs": n, "periodHours": n}))}),
		"meta":        object(map[string]*cacheSchema{"status": s, "data": object(map[string]*cacheSchema{"planName": s, "isSubscriptionActive": b, "windows": array(object(map[string]*cacheSchema{"id": s, "usedPercent": n, "resetAt": n, "durationMinutes": n}))})}),
		"xai":         object(map[string]*cacheSchema{"status": s, "billing": billing}),
	}
}
func (schema *cacheSchema) validate(value any) error {
	if value == nil {
		return nil
	}
	switch schema.kind {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return errors.New("expected quota object")
		}
		for key, v := range obj {
			field, ok := schema.fields[key]
			if !ok {
				return errors.New("unknown quota state field")
			}
			if err := field.validate(v); err != nil {
				return err
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok || len(items) > 256 {
			return errors.New("invalid quota array")
		}
		for _, item := range items {
			if err := schema.element.validate(item); err != nil {
				return err
			}
		}
	case "string":
		str, ok := value.(string)
		if !ok || !safeCacheText(str, 1024) {
			return errors.New("invalid quota string")
		}
	case "number":
		num, ok := value.(float64)
		if !ok || math.IsNaN(num) || math.IsInf(num, 0) || math.Abs(num) > 1e18 {
			return errors.New("invalid quota number")
		}
	case "bool":
		if _, ok := value.(bool); !ok {
			return errors.New("invalid quota boolean")
		}
	case "string_or_number":
		switch typed := value.(type) {
		case string:
			if !safeCacheText(typed, 1024) {
				return errors.New("invalid quota string")
			}
		case float64:
			if math.Abs(typed) > 1e18 {
				return errors.New("invalid quota number")
			}
		default:
			return errors.New("invalid quota scalar")
		}
	}
	return nil
}
func safeCacheText(value string, limit int) bool {
	if len(value) > limit {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) < 0
}
func validateQuotaCacheEntry(entry QuotaCacheEntry) error {
	if entry.Key == "" || len(entry.Key) > 512 || !safeCacheText(strings.ReplaceAll(entry.Key, "\x00", ""), 512) {
		return errors.New("invalid quota cache key")
	}
	if entry.Account != "" && (len(entry.Account) > 256 || !safeCacheText(entry.Account, 256) || strings.Contains(entry.Account, ":")) {
		return errors.New("invalid account fact")
	}
	schema, ok := quotaCacheSchemas[entry.Provider]
	if !ok {
		return errors.New("unsupported quota cache provider")
	}
	if entry.ObservedAt.IsZero() || entry.ObservedAt.After(time.Now().Add(5*time.Minute)) {
		return errors.New("invalid observation time")
	}
	var state map[string]any
	if err := json.Unmarshal(entry.State, &state); err != nil {
		return errors.New("invalid quota state")
	}
	if state["status"] != "success" {
		return errors.New("only successful quota state can be persisted")
	}
	return schema.validate(state)
}
func (s *Store) QuotaCache(ctx context.Context) ([]QuotaCacheEntry, error) {
	values, err := s.ListCache(ctx, quotaNamespace)
	if err != nil {
		return nil, err
	}
	states, err := cacheDecode[quotaState](values)
	if err != nil {
		return nil, err
	}
	result := make([]QuotaCacheEntry, 0)
	for _, identity := range sortedKeys(states) {
		for _, key := range sortedKeys(states[identity].UIEntries) {
			entry := states[identity].UIEntries[key]
			decoded, errDecode := base64.RawURLEncoding.DecodeString(key)
			if errDecode != nil {
				return nil, errDecode
			}
			entry.Key = string(decoded)
			result = append(result, entry)
		}
	}
	return result, nil
}
func (s *Store) SaveQuotaCache(ctx context.Context, entries []QuotaCacheEntry) error {
	if err := s.begin(); err != nil {
		return err
	}
	defer s.active.Done()
	if len(entries) > 1000 {
		return errors.New("too many quota cache entries")
	}
	for _, entry := range entries {
		if err := validateQuotaCacheEntry(entry); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		// Devin uses filename+NUL+auth-index in its browser cache. JSONB cannot
		// represent NUL, so encode the map key and reconstruct it only at the API boundary.
		encodedKey := base64.RawURLEncoding.EncodeToString([]byte(entry.Key))
		persisted := entry
		persisted.Key = ""
		err := s.store.MutateCache(ctx, quotaNamespace, quotaStoreKey(entry.Provider, entry.Account), func(raw json.RawMessage) (json.RawMessage, error) {
			var state quotaState
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &state); err != nil {
					return nil, err
				}
			}
			if !state.ResetAt.IsZero() && !entry.ObservedAt.After(state.ResetAt) {
				return raw, nil
			}
			if previous, exists := state.UIEntries[encodedKey]; exists && previous.ObservedAt.After(entry.ObservedAt) {
				return raw, nil
			}
			if state.UIEntries == nil {
				state.UIEntries = map[string]QuotaCacheEntry{}
			}
			state.UIEntries[encodedKey] = persisted
			return json.Marshal(state)
		})
		if err != nil {
			return fmt.Errorf("save quota display cache: %w", err)
		}
	}
	return nil
}

func (s *Store) quotaCacheGetHTTP(c *gin.Context) {
	entries, err := s.QuotaCache(c.Request.Context())
	respond(c, quotaCacheEnvelope{Entries: entries}, err)
}
func (s *Store) quotaCachePutHTTP(c *gin.Context) {
	var envelope quotaCacheEnvelope
	if err := decodeRequest(c, &envelope, 1<<20); err != nil {
		badRequest(c, err)
		return
	}
	if len(envelope.Entries) > 1000 {
		badRequest(c, errors.New("too many quota cache entries"))
		return
	}
	for _, entry := range envelope.Entries {
		if err := validateQuotaCacheEntry(entry); err != nil {
			badRequest(c, err)
			return
		}
	}
	err := s.SaveQuotaCache(c.Request.Context(), envelope.Entries)
	respond(c, gin.H{"ok": true}, err)
}
