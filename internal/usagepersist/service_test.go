package usagepersist

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fixtureAccount is the account fact every fixture credential publishes. It is a
// plain account label, never a credential: the record below still carries a real
// token, key and failure body that must never reach storage.
const fixtureAccount = "user@example.invalid"

// fixturePayload encodes a usage.Record into the built-in provider's JSON shape.
// It deliberately emits only the fields the migrated decoder accepts: account
// facts are `account`/`account_kind`, `api_key` is hashed on the way in, and the
// deleted `auth_index`/`access_token_sha256` identity fields are absent. No
// production API accepts raw SDK records.
func fixturePayload(r usage.Record) []byte {
	value := map[string]any{
		"execution_id": r.RequestID, "timestamp": r.RequestedAt, "provider": r.Provider,
		"executor_type": r.ExecutorType, "model": r.Model, "alias": r.Alias,
		"account": r.Account, "account_kind": r.AccountKind, "api_key": r.APIKey,
		"latency_ms": r.Latency.Milliseconds(),
		"ttft_ms":    r.TTFT.Milliseconds(), "failed": r.Failed, "stream": r.Stream,
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

// fixtureRecord carries account facts plus secrets. The failure body deliberately
// contains a credential: only its sanitized message may reach storage, while the
// account label is now persisted on purpose.
func fixtureRecord(id string, at time.Time) usage.Record {
	return usage.Record{RequestID: id, Provider: "codex", Model: "gpt-test", Account: fixtureAccount, AccountKind: "email", APIKey: "sk-private-test-key", Source: "private-source", AuthID: "private-auth-file", BaseURL: "https://user:secret@upstream.invalid", RequestedAt: at, Latency: 250 * time.Millisecond, TTFT: 20 * time.Millisecond, Detail: usage.Detail{InputTokens: 1000, OutputTokens: 300, CacheReadTokens: 200, CacheCreationTokens: 100, ReasoningTokens: 50, TotalTokens: 1300, TokenBreakdown: usage.NewSubsetTokenBreakdown(1000, 200, 100, 300, 50, 1300)}, Fail: usage.Failure{StatusCode: 429, Body: `{"error":{"type":"rate_limit_error","message":"slow down, key sk-private-error-key rejected"}}`}, ResponseHeaders: http.Header{"Set-Cookie": []string{"private-cookie"}, "Authorization": []string{"private-header-token"}}}
}

func fixturePrice() Price {
	return Price{Model: "gpt-test", InputPerMillion: 2, OutputPerMillion: 4, CacheReadPerMillion: 1, CacheWritePerMillion: 3}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

// wpQuotaAuth builds a credential whose account fact is its email metadata,
// the property every provider except kimi identifies an account by. The transient
// index still addresses the live credential in the manager.
func wpQuotaAuth(provider, index, name, token string) *coreauth.Auth {
	return wpQuotaAuthWithAccount(provider, index, name, token, fixtureAccount)
}

// wpQuotaAuthWithAccount books a distinct account fact on one credential, so
// fixtures can prove that two accounts never share a quota row.
func wpQuotaAuthWithAccount(provider, index, name, token, account string) *coreauth.Auth {
	metadata := map[string]any{"access_token": token}
	if account != "" {
		metadata["email"] = account
	}
	return &coreauth.Auth{ID: name, Index: index, FileName: name, Provider: provider, Metadata: metadata}
}

// wpBindFixtures registers the credentials and publishes an advisory binding
// copy, modelling the explicit add-on identity GET that arms observation.
func wpBindFixtures(t *testing.T, s *Store, auths ...*coreauth.Auth) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	s.BindQuotaIdentitySource(NewQuotaIdentitySource(func() *coreauth.Manager { return manager }))
	s.publishQuotaBindings()
	return manager
}

// wpBindManagementFixtures registers the two codex credentials the middleware and
// worker fixtures address by their transient index.
func wpBindManagementFixtures(t *testing.T, s *Store) *coreauth.Manager {
	t.Helper()
	return wpBindFixtures(t, s, wpQuotaAuth("codex", "account", "account.json", "fixture-secret"), wpQuotaAuth("codex", "later", "later.json", "later-secret"))
}

// wpReadBinding performs the authoritative read a trusted caller would use,
// then returns the live binding for one transient credential index, normalized
// exactly as the producer normalizes the index it reports.
func wpReadBinding(s *Store, provider, index string) QuotaBinding {
	provider = strings.ToLower(strings.TrimSpace(provider))
	index = strings.TrimSpace(index)
	for _, binding := range s.publishQuotaBindings() {
		if binding.Provider == provider && binding.AuthIndex == index {
			return binding
		}
	}
	panic("test requires a registered quota source fixture")
}

func wpObserveAPICall(s *Store, ctx context.Context, provider, index, url string, status int, header http.Header, body []byte) {
	s.ObserveAPICall(ctx, wpReadBinding(s, provider, index), url, status, header, body)
}

func wpObserveQuotaFetch(s *Store, ctx context.Context, provider, index string, response pluginapi.QuotaFetchResponse) {
	s.ObserveQuotaFetch(ctx, wpReadBinding(s, provider, index), response)
}

func wpObserveQuotaReset(s *Store, ctx context.Context, provider, index string) {
	s.ObserveQuotaReset(ctx, wpReadBinding(s, provider, index))
}

// wpObserveResetAt admits a reset with an explicit receipt time so ordering
// can be tested without a wall-clock sleep.
func wpObserveResetAt(s *Store, provider, index string, at time.Time) {
	binding := wpReadBinding(s, provider, index)
	s.observeQuotaResetAt(binding, at)
}

// wpBindCacheFixture fills a display entry from the live credential it addresses
// transiently: on input Key holds the credential index, on output it holds the
// durable display key and the account facts.
func wpBindCacheFixture(s *Store, entry QuotaCacheEntry) QuotaCacheEntry {
	binding := wpReadBinding(s, entry.Provider, entry.Key)
	entry.Key = binding.Key
	entry.Account = binding.Account
	entry.AccountKind = binding.AccountKind
	return entry
}

func TestFilePersistenceDedupAndSecretExclusion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	record := fixtureRecord("event-1", at)
	for range 2 {
		if err = s.recordFixture(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.SetPrice(ctx, fixturePrice()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(dir, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{record.APIKey, record.Source, record.AuthID, record.BaseURL, record.Fail.Body, "sk-private-error-key", "private-cookie", "private-header-token"} {
		if strings.Contains(string(journal), secret) {
			t.Errorf("journal leaked %q", secret)
		}
	}
	// The account fact is persisted deliberately: grouping usage by (provider,
	// account) is the approved model, and an account label is not a credential.
	if !strings.Contains(string(journal), `"account":"`+fixtureAccount+`"`) || !strings.Contains(string(journal), `"account_kind":"email"`) {
		t.Errorf("journal lost the persisted account facts: %s", journal)
	}
	if !strings.Contains(string(journal), "rate_limit_error: slow down, key [redacted] rejected") {
		t.Errorf("journal lost the sanitized failure message: %s", journal)
	}
	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	page, err := s.Events(ctx, Filter{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Events) != 1 {
		t.Fatalf("dedup/reopen failed: %#v", page)
	}
	e := page.Events[0]
	if !e.Priced || math.Abs(e.CostUSD-.0031) > 1e-12 {
		t.Fatalf("incorrect price: %#v", e)
	}
	if e.InputTokens != 1000 || e.OutputTokens != 300 || e.ReasoningTokens != 50 || e.KeyID == "" || e.KeyID == record.APIKey {
		t.Fatalf("incorrect normalized event: %#v", e)
	}
	if info, errStat := os.Stat(filepath.Join(dir, "usage.jsonl")); errStat != nil {
		t.Fatal(errStat)
	} else if info.Mode().Perm()&0077 != 0 {
		t.Errorf("journal permissions too broad: %v", info.Mode())
	}
}

func TestAnalysisFiltersAndCanonicalCacheCosts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := s.SetPrice(ctx, fixturePrice()); err != nil {
		t.Fatal(err)
	}
	first := fixtureRecord("first", at)
	second := fixtureRecord("second", at.Add(time.Hour))
	second.Failed = true
	second.Fail.StatusCode = 429
	third := fixtureRecord("third", at.Add(2*time.Hour))
	third.Provider = "claude"
	third.Model = "unknown"
	third.Detail = usage.Detail{TokenBreakdown: usage.NewIndependentTokenBreakdown(700, 200, 100, 250, 50, 1300)}
	for _, r := range []usage.Record{first, second, third} {
		if err := s.recordFixture(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Analyze(ctx, Filter{}, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Requests != 3 || result.Summary.Failures != 1 || result.Summary.PricedRequests != 2 || result.Summary.UnpricedRequests != 1 || result.Summary.TotalTokens != 3900 || len(result.Series) != 3 || len(result.Models) != 2 {
		t.Fatalf("incorrect analysis: %#v", result)
	}
	if math.Abs(result.Summary.CostUSD-.0062) > 1e-12 {
		t.Fatalf("cache double counted: %g", result.Summary.CostUSD)
	}
	page, err := s.Events(ctx, Filter{Provider: "codex", Status: "failed", From: at, To: at.Add(2 * time.Hour)}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Events[0].ID != "second" {
		t.Fatalf("filter mismatch: %#v", page)
	}
	// The per-credential request stream is now filtered by the account fact.
	page, err = s.Events(ctx, Filter{Provider: "codex", Account: fixtureAccount}, 10, 0)
	if err != nil || page.Total != 2 || page.Events[0].ID != "second" || page.Events[1].ID != "first" {
		t.Fatalf("account filter mismatch: %#v %v", page, err)
	}
	other := fixtureRecord("other-account", at.Add(3*time.Hour))
	other.Account, other.AccountKind = "other@example.invalid", "email"
	if err := s.recordFixture(ctx, other); err != nil {
		t.Fatal(err)
	}
	page, err = s.Events(ctx, Filter{Provider: "codex", Account: fixtureAccount}, 10, 0)
	if err != nil || page.Total != 2 {
		t.Fatalf("a different account leaked into the per-account stream: %#v %v", page, err)
	}
	page, err = s.Events(ctx, Filter{}, 1, 1)
	if err != nil || page.Total != 4 || page.Events[0].ID != "third" {
		t.Fatalf("page order: %#v %v", page, err)
	}
	values, err := s.Filters(ctx, Filter{})
	if err != nil || len(values.Models) != 2 || len(values.Providers) != 2 || len(values.Accounts) != 2 || len(values.KeyIDs) != 1 {
		t.Fatalf("filter options: %#v %v", values, err)
	}
}

func TestCanceledRequestStillPersistsAndNonGenerationIgnored(t *testing.T) {
	s := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := fixtureRecord("canceled", time.Now())
	s.Consume(ctx, fixturePayload(r))
	r.RequestID = "count-only"
	r.Generate = usage.GenerateFlag(false)
	s.Consume(ctx, fixturePayload(r))
	flushFixture(t, s)
	page, err := s.Events(context.Background(), Filter{}, 10, 0)
	if err != nil || page.Total != 1 {
		t.Fatalf("canceled context lost record or counted non-generation: %#v %v", page, err)
	}
}

func TestIncompleteAccountingNeverInventsCost(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.SetPrice(ctx, fixturePrice()); err != nil {
		t.Fatal(err)
	}
	r := fixtureRecord("incomplete", time.Now())
	r.Detail = usage.Detail{TokenBreakdown: usage.NewUnclassifiedTokenBreakdown(99)}
	if err := s.recordFixture(ctx, r); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(ctx, Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Events[0].Priced || page.Events[0].CostUSD != 0 || page.Events[0].TotalTokens != 99 {
		t.Fatalf("invented price: %#v", page.Events[0])
	}
}

func TestFileJournalRepairsOnlyIncompleteTail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.recordFixture(ctx, fixtureRecord("before-crash", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "usage.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(`{"event":`); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(ctx, Filter{}, 10, 0)
	if err != nil || page.Total != 1 {
		t.Fatalf("tail recovery: %#v %v", page, err)
	}
	_ = s.Close(ctx)
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("not-json\n")
	_ = file.Close()
	if unexpected, errOpen := Open(ctx, Options{DataDir: dir}); errOpen == nil {
		_ = unexpected.Close(ctx)
		t.Fatal("silently accepted corrupt complete record")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }
func TestPricingSyncPreservesManualAndResetRestoresCatalog(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: t.TempDir(), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != PricingSourceURL {
			t.Errorf("wrong pricing URL: %s", r.URL)
		}
		body := `{"reseller":{"models":{"gpt-test":{"cost":{"input":99,"output":99}}}},"openai":{"models":{"gpt-test":{"cost":{"input":2,"output":4,"cache_read":1,"cache_write":3}},"invalid":{"cost":{"input":-1,"output":1}},"missing":{"cost":{"input":1}}}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	p := fixturePrice()
	p.InputPerMillion = 10
	if _, err = s.SetPrice(ctx, p); err != nil {
		t.Fatal(err)
	}
	result, err := s.SyncPricing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.SkippedManual != 1 {
		t.Fatalf("manual count: %#v", result)
	}
	prices, err := s.prices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !prices["gpt-test"].Manual || prices["gpt-test"].InputPerMillion != 10 {
		t.Fatal("sync overwrote manual")
	}
	if _, ok := prices["invalid"]; ok {
		t.Fatal("negative price imported")
	}
	if _, ok := prices["missing"]; ok {
		t.Fatal("incomplete price imported")
	}
	if err = s.ResetPrice(ctx, "gpt-test"); err != nil {
		t.Fatal(err)
	}
	prices, err = s.prices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if prices["gpt-test"].Manual || prices["gpt-test"].InputPerMillion != 2 {
		t.Fatalf("native vendor/reset mismatch: %#v", prices["gpt-test"])
	}
	p.InputPerMillion = math.Inf(1)
	if _, err = s.SetPrice(ctx, p); err == nil {
		t.Fatal("accepted infinity")
	}
}

func TestQuotaCacheValidationFreshnessAndHeaders(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute)
	wpBindFixtures(t, s, wpQuotaAuth("claude", "account-1", "auth.json", "source-token"))
	e := wpBindCacheFixture(s, QuotaCacheEntry{Provider: "claude", Key: "account-1", ObservedAt: at, State: json.RawMessage(`{"status":"success","windows":[{"id":"five-hour","usedPercent":20,"resetAtMs":1234}],"planType":"plus"}`)})
	if e.Account != fixtureAccount || e.AccountKind != "email" {
		t.Fatalf("display entry lost its account facts: %#v", e)
	}
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{e}); err != nil {
		t.Fatal(err)
	}
	older := e
	older.ObservedAt = at.Add(-time.Hour)
	older.State = json.RawMessage(`{"status":"success","windows":[]}`)
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{older}); err != nil {
		t.Fatal(err)
	}
	cached, err := s.QuotaCache(ctx)
	if err != nil || len(cached) != 1 || !strings.Contains(string(cached[0].State), "plus") {
		t.Fatalf("stale cache overwritten: %#v %v", cached, err)
	}
	for _, state := range []string{`{"status":"error","error":"secret"}`, `{"status":"success","access_token":"secret"}`, `{"status":"success","windows":[{"id":"x","authorization":"secret"}]}`, `{"status":"success","windows":[{"labelParams":{"token":"secret"}}]}`} {
		bad := e
		bad.State = json.RawMessage(state)
		if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{bad}); err == nil {
			t.Errorf("unsafe state was not rejected by schema validation: %s", state)
		}
	}
	r := fixtureRecord("quota-event", at)
	r.Provider = "claude"
	r.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}}
	if err := s.recordFixture(ctx, r); err != nil {
		t.Fatal(err)
	}
	snapshots, err := s.Quotas(ctx)
	if err != nil || len(snapshots) != 1 || len(snapshots[0].Windows) != 1 {
		t.Fatalf("header capture: %#v %v", snapshots, err)
	}
	if snapshots[0].Account != fixtureAccount || snapshots[0].AccountKind != "email" {
		t.Fatalf("header capture lost its account facts: %#v", snapshots[0])
	}
	if !snapshots[0].ObservedAt.Equal(at.Add(r.Latency)) {
		t.Fatalf("quota timestamp used start: %v", snapshots[0].ObservedAt)
	}
	if err := s.ResetQuota(ctx, "claude", fixtureAccount); err != nil {
		t.Fatal(err)
	}
	cached, err = s.QuotaCache(ctx)
	if err != nil || len(cached) != 0 {
		t.Fatalf("reset cache: %#v %v", cached, err)
	}
}

// TestQuotaCacheRejectsUnsafeStateBeforeBatchWrite keeps the pre-write protection
// that survives the migration: a batch is validated in full before any row is
// written, an account fact used in a storage key stays bounded and separator-free,
// and an older observed_at never overwrites a newer entry.
func TestQuotaCacheRejectsUnsafeStateBeforeBatchWrite(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func(*QuotaCacheEntry)
	}{
		{"account-separator", func(e *QuotaCacheEntry) { e.Account = "a:b" }},
		{"account-control-character", func(e *QuotaCacheEntry) { e.Account = "a\x00b" }},
		{"account-too-long", func(e *QuotaCacheEntry) { e.Account = strings.Repeat("a", maxAccountFactBytes+1) }},
		{"empty-key", func(e *QuotaCacheEntry) { e.Key = "" }},
		{"unknown-provider", func(e *QuotaCacheEntry) { e.Provider = "unknown" }},
		{"missing-observation-time", func(e *QuotaCacheEntry) { e.ObservedAt = time.Time{} }},
		{"future-observation-time", func(e *QuotaCacheEntry) { e.ObservedAt = time.Now().Add(time.Hour) }},
		{"unsafe-state", func(e *QuotaCacheEntry) {
			e.State = json.RawMessage(`{"status":"success","windows":[{"id":"x","authorization":"secret"}]}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openTestStore(t)
			wpBindFixtures(t, s, wpQuotaAuth("claude", "account-1", "auth.json", "source-token"))
			valid := wpBindCacheFixture(s, QuotaCacheEntry{Provider: "claude", Key: "account-1", ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success","windows":[]}`)})
			invalid := valid
			test.mutate(&invalid)
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{valid, invalid}); err == nil {
				t.Fatal("invalid cache entry accepted")
			}
			if raw, err := s.ListCache(ctx, quotaNamespace); err != nil || len(raw) != 0 {
				t.Fatalf("invalid batch partially wrote quota state: %s %v", raw, err)
			}
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{valid}); err != nil {
				t.Fatalf("valid account-bound entry rejected: %v", err)
			}
			// A newer observation for the same account must be accepted: observed_at
			// is the only ordering fence left, and ordinary activity keeps advancing.
			drifted := valid
			drifted.ObservedAt = time.Now()
			drifted.State = json.RawMessage(`{"status":"success","windows":[]}`)
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{drifted}); err != nil {
				t.Fatalf("newer same-account observation rejected: %v", err)
			}
			// A display cache write needs no bound identity source: the account fact
			// is durable data now, so an unbound store still persists valid entries.
			withoutSource := openTestStore(t)
			if err := withoutSource.SaveQuotaCache(ctx, []QuotaCacheEntry{valid}); err != nil {
				t.Fatalf("unbound store rejected a valid entry: %v", err)
			}
		})
	}
}

func TestHTTPRoutesValidationAndShutdown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := openTestStore(t)
	router := gin.New()
	s.RegisterRoutes(router.Group("/stats"))
	for _, path := range []string{"/stats/overview?from=not-a-date", "/stats/events?limit=0", "/stats/events?status=bogus", "/stats/analysis?bucket=minute"} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 400 {
			t.Errorf("%s status=%d body=%s", path, r.Code, r.Body)
		}
	}
	r := httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest("PUT", "/stats/pricing", strings.NewReader(`{"model":"test","input_per_million":1,"unknown":"secret"}`)))
	if r.Code != 400 {
		t.Fatalf("unknown price field accepted: %d", r.Code)
	}
	r = httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest("GET", "/stats/overview", nil))
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"models":[]`) {
		t.Fatalf("empty response: %d %s", r.Code, r.Body)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest("GET", "/stats/events", nil))
	if r.Code != 503 {
		t.Fatalf("closed status=%d", r.Code)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
