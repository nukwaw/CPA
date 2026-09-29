package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func fixtureRecord(id string, at time.Time) usage.Record {
	return usage.Record{RequestID: id, Provider: "codex", Model: "gpt-test", AuthIndex: "account-1", APIKey: "sk-private-test-key", Source: "private-source", AuthID: "private-auth-file", BaseURL: "https://user:secret@upstream.invalid", RequestedAt: at, Latency: 250 * time.Millisecond, TTFT: 20 * time.Millisecond, Detail: usage.Detail{InputTokens: 1000, OutputTokens: 300, CacheReadTokens: 200, CacheCreationTokens: 100, ReasoningTokens: 50, TotalTokens: 1300, TokenBreakdown: usage.NewSubsetTokenBreakdown(1000, 200, 100, 300, 50, 1300)}, Fail: usage.Failure{Body: "private-error-body"}, ResponseHeaders: http.Header{"Set-Cookie": []string{"private-cookie"}, "Authorization": []string{"private-header-token"}}}
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
	for _, secret := range []string{record.APIKey, record.Source, record.AuthID, record.BaseURL, record.Fail.Body, "private-cookie", "private-header-token"} {
		if strings.Contains(string(journal), secret) {
			t.Errorf("journal leaked %q", secret)
		}
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
	page, err = s.Events(ctx, Filter{}, 1, 1)
	if err != nil || page.Total != 3 || page.Events[0].ID != "second" {
		t.Fatalf("page order: %#v %v", page, err)
	}
	values, err := s.Filters(ctx, Filter{})
	if err != nil || len(values.Models) != 2 || len(values.Providers) != 2 || len(values.AuthIndexes) != 1 || len(values.KeyIDs) != 1 {
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
	bindQuotaFixtures(t, s, quotaFixtureAuth("claude", "account-1", "auth.json", "source-token"))
	e := bindCacheFixture(s, QuotaCacheEntry{Provider: "claude", AuthIndex: "account-1", ObservedAt: at, State: json.RawMessage(`{"status":"success","windows":[{"id":"five-hour","usedPercent":20,"resetAtMs":1234}],"planType":"plus"}`)})
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
		if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{bad}); err == nil || errors.Is(err, ErrQuotaIdentity) {
			t.Errorf("unsafe state was not rejected by schema validation: %s: %v", state, err)
		}
	}
	r := fixtureRecord("quota-event", at)
	r.Provider = "claude"
	r.AccessTokenSHA256 = quotaHash("source-token")
	r.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}}
	if err := s.recordFixture(ctx, r); err != nil {
		t.Fatal(err)
	}
	snapshots, err := s.Quotas(ctx)
	if err != nil || len(snapshots) != 1 || len(snapshots[0].Windows) != 1 {
		t.Fatalf("header capture: %#v %v", snapshots, err)
	}
	if !snapshots[0].ObservedAt.Equal(at.Add(r.Latency)) {
		t.Fatalf("quota timestamp used start: %v", snapshots[0].ObservedAt)
	}
	if err := s.ResetQuota(ctx, "claude", "account-1"); err != nil {
		t.Fatal(err)
	}
	cached, err = s.QuotaCache(ctx)
	if err != nil || len(cached) != 0 {
		t.Fatalf("reset cache: %#v %v", cached, err)
	}
}

func TestQuotaCacheRejectsMissingOrMismatchedBindingBeforeBatchWrite(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func(*QuotaCacheEntry)
	}{
		{"missing-generation", func(e *QuotaCacheEntry) { e.CredentialGeneration = "" }},
		{"missing-revision", func(e *QuotaCacheEntry) { e.Revision = "" }},
		{"wrong-generation", func(e *QuotaCacheEntry) { e.CredentialGeneration += "-stale" }},
		{"wrong-provider", func(e *QuotaCacheEntry) { e.Provider = "codex" }},
		{"wrong-index", func(e *QuotaCacheEntry) { e.AuthIndex = "other-index" }},
		{"wrong-key", func(e *QuotaCacheEntry) { e.Key = "other.json" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openTestStore(t)
			bindQuotaFixtures(t, s, quotaFixtureAuth("claude", "account-1", "auth.json", "source-token"))
			valid := bindCacheFixture(s, QuotaCacheEntry{Provider: "claude", AuthIndex: "account-1", ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success","windows":[]}`)})
			invalid := valid
			test.mutate(&invalid)
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{valid, invalid}); !errors.Is(err, ErrQuotaIdentity) {
				t.Fatalf("unbound cache entry accepted: %v", err)
			}
			if raw, err := s.ListCache(ctx, quotaNamespace); err != nil || len(raw) != 0 {
				t.Fatalf("invalid batch partially wrote quota state: %s %v", raw, err)
			}
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{valid}); err != nil {
				t.Fatalf("valid source binding rejected: %v", err)
			}
			// A request-start revision that drifted while the credential stayed the
			// same must still be accepted, otherwise a browser refresh that raced an
			// ordinary auth-file write would silently lose its displayed state.
			drifted := valid
			drifted.Revision = "drifted-revision"
			drifted.ObservedAt = time.Now()
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{drifted}); err != nil {
				t.Fatalf("same-credential revision drift rejected: %v", err)
			}
			withoutSource := openTestStore(t)
			if err := withoutSource.SaveQuotaCache(ctx, []QuotaCacheEntry{valid}); !errors.Is(err, ErrQuotaIdentity) {
				t.Fatalf("store without identity source accepted cache: %v", err)
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
