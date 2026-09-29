package usagepersist

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Fixtures use the existing provider JSON shape; no production API accepts raw SDK records.
func fixturePayload(r usage.Record) []byte {
	value := map[string]any{
		"execution_id": r.RequestID, "timestamp": r.RequestedAt, "provider": r.Provider,
		"executor_type": r.ExecutorType, "model": r.Model, "alias": r.Alias,
		"auth_index": r.AuthIndex, "api_key": r.APIKey, "access_token_sha256": r.AccessTokenSHA256, "latency_ms": r.Latency.Milliseconds(),
		"ttft_ms": r.TTFT.Milliseconds(), "failed": r.Failed, "stream": r.Stream,
		"generate": usage.GenerateEnabled(r.Generate), "token_breakdown": r.Detail.TokenBreakdown,
		"tokens":           map[string]int64{"input_tokens": r.Detail.InputTokens, "output_tokens": r.Detail.OutputTokens, "reasoning_tokens": r.Detail.ReasoningTokens, "cached_tokens": r.Detail.CachedTokens, "cache_read_tokens": r.Detail.CacheReadTokens, "cache_creation_tokens": r.Detail.CacheCreationTokens, "total_tokens": r.Detail.TotalTokens},
		"fail":             map[string]any{"status_code": r.Fail.StatusCode, "body": r.Fail.Body},
		"response_headers": r.ResponseHeaders, "source": r.Source, "auth_id": r.AuthID, "endpoint": r.BaseURL,
	}
	payload, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return payload
}
func (s *Store) recordFixture(ctx context.Context, r usage.Record) error {
	s.Consume(ctx, fixturePayload(r))
	return s.Flush(ctx)
}
func flushFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}
func fixtureEvent(r usage.Record) Event {
	var p providerUsage
	if err := json.Unmarshal(fixturePayload(r), &p); err != nil {
		panic(err)
	}
	event, err := eventFromProvider(p)
	if err != nil {
		panic(err)
	}
	return event
}

type usageConsumerFunc func(context.Context, usage.Record)

func (f usageConsumerFunc) HandleUsage(ctx context.Context, r usage.Record) { f(ctx, r) }

// A test-only last SDK plugin provides a deterministic processing barrier without
// changing, stopping, or adding a drain method to the upstream SDK manager.
func publishThroughBuiltin(t *testing.T, r usage.Record) {
	t.Helper()
	done := make(chan struct{})
	usage.RegisterNamedPlugin("usagepersist-test-barrier", usageConsumerFunc(func(_ context.Context, record usage.Record) {
		if record.RequestID == r.RequestID {
			close(done)
		}
	}))
	usage.PublishRecord(context.Background(), r)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("built-in usage provider did not finish test record")
	}
	usage.RegisterNamedPlugin("usagepersist-test-barrier", usageConsumerFunc(func(context.Context, usage.Record) {}))
}

func TestStoreIsPassiveConsumerNotSDKPlugin(t *testing.T) {
	store := openTestStore(t)
	if _, ok := any(store).(usage.Plugin); ok {
		t.Fatal("Store must not implement a parallel raw SDK usage plugin")
	}
}

func TestPassiveConsumerUsesOriginalGatesWithoutStealingQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	previousEnabled, previousStatistics := redisqueue.Enabled(), redisqueue.UsageStatisticsEnabled()
	defer redisqueue.SetEnabled(previousEnabled)
	defer redisqueue.SetUsageStatisticsEnabled(previousStatistics)
	redisqueue.SetEnabled(true)
	redisqueue.SetUsageStatisticsEnabled(true)
	redisqueue.PopOldest(10000)
	stop := redisqueue.ObserveUsage(s.Consume)
	defer stop()
	publishThroughBuiltin(t, fixtureRecord("provider-enabled", time.Now()))
	redisqueue.SetUsageStatisticsEnabled(false)
	publishThroughBuiltin(t, fixtureRecord("statistics-disabled", time.Now()))
	redisqueue.SetUsageStatisticsEnabled(true)
	publishThroughBuiltin(t, fixtureRecord("provider-reenabled", time.Now()))
	// Persistence observes a copy. The original queue remains available to its
	// normal management/Home consumers instead of being popped by this addon.
	queued := redisqueue.PopOldest(100)
	if len(queued) != 2 {
		t.Fatalf("passive observer stole or changed provider queue: got %d", len(queued))
	}
	for _, payload := range queued {
		if strings.Contains(string(payload), "statistics-disabled") {
			t.Fatal("builtin statistics gate bypassed")
		}
	}
	redisqueue.SetEnabled(false)
	publishThroughBuiltin(t, fixtureRecord("provider-disabled", time.Now()))
	redisqueue.SetEnabled(true)
	publishThroughBuiltin(t, fixtureRecord("provider-restored", time.Now()))
	flushFixture(t, s)
	page, err := s.Events(ctx, Filter{}, 100, 0)
	if err != nil || page.Total != 3 {
		t.Fatalf("original provider gates: %#v %v", page, err)
	}
	stop()
	publishThroughBuiltin(t, fixtureRecord("after-detach", time.Now()))
	page, err = s.Events(ctx, Filter{}, 100, 0)
	if err != nil || page.Total != 3 {
		t.Fatalf("detached observer still consumed: %#v %v", page, err)
	}

	redisqueue.SetUsageStatisticsEnabled(false)
	if _, err = s.SetPrice(ctx, fixturePrice()); err != nil {
		t.Fatal(err)
	}
	bindQuotaFixtures(t, s, quotaFixtureAuth("codex", "management-only", "management.json", "secret"))
	s.fixtureObserveAPICall(ctx, "codex", "management-only", "https://chatgpt.com/backend-api/wham/usage", 200, nil, []byte(`{"rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000}}}`))
	flushFixture(t, s)
	quotas, err := s.Quotas(ctx)
	if err != nil || len(quotas) != 1 {
		t.Fatalf("disabled usage blocked independent management quota: %#v %v", quotas, err)
	}
}

func TestConsumerJSONValidationMissingGenerateAndFailureStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, invalid := range []string{`not-json`, `{}`, `{"execution_id":"id"}`, `{"timestamp":"2026-09-01T00:00:00Z"}`} {
		if _, err := normalizeUsage([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid provider output: %s", invalid)
		}
	}
	r := fixtureRecord("missing-generate", time.Now())
	r.Failed = true
	r.Fail.StatusCode = 429
	var value map[string]any
	if err := json.Unmarshal(fixturePayload(r), &value); err != nil {
		t.Fatal(err)
	}
	delete(value, "generate")
	value["client_ip"] = "private-client-ip"
	value["prompt"] = "private-prompt-body"
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	s.Consume(ctx, payload)
	s.Consume(ctx, payload)
	flushFixture(t, s)
	page, err := s.Events(ctx, Filter{}, 100, 0)
	if err != nil || page.Total != 1 {
		t.Fatalf("missing generate/dedup: %#v %v", page, err)
	}
	e := page.Events[0]
	if !e.Failed || e.StatusCode != 429 || len(e.KeyID) != 64 || e.KeyID == r.APIKey || e.TotalTokens != 1300 {
		t.Fatalf("provider fields lost: %#v", e)
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{r.APIKey, r.Fail.Body, "private-client-ip", "private-prompt-body"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("event contains ignored input %q", secret)
		}
	}
}

func TestConsumeNormalizesOnlyKnownProviderHeaders(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := fixtureRecord("header-provider", time.Now().Add(-time.Minute))
	r.Provider = "claude"
	r.AccessTokenSHA256 = quotaHash("source-token")
	bindQuotaFixtures(t, s, quotaFixtureAuth("claude", r.AuthIndex, "claude.json", "source-token"))
	r.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.4"}, "Authorization": []string{"private-header"}, "Set-Cookie": []string{"private-cookie"}}
	s.Consume(ctx, fixturePayload(r))
	flushFixture(t, s)
	quotas, err := s.Quotas(ctx)
	if err != nil || len(quotas) != 1 {
		t.Fatalf("quota extraction failed: %#v %v", quotas, err)
	}
	encoded, err := json.Marshal(quotas)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("raw response headers persisted")
	}
	r.RequestID = "unknown-provider"
	r.Provider = "unknown"
	r.AuthIndex = "other"
	s.Consume(ctx, fixturePayload(r))
	flushFixture(t, s)
	quotas, err = s.Quotas(ctx)
	if err != nil || len(quotas) != 1 {
		t.Fatalf("unknown provider accepted quota headers: %#v %v", quotas, err)
	}
}
