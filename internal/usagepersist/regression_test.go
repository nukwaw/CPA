package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
)

func TestMissingCacheRatesStayUnpricedUnlessExplicitZero(t *testing.T) {
	catalog, err := decodeCatalog([]byte(`{"openai":{"models":{"gpt-missing":{"cost":{"input":2,"output":4}},"gpt-zero":{"cost":{"input":2,"output":4,"cache_read":0,"cache_write":0}}}}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := fixtureEvent(fixtureRecord("test", time.Now()))
	e.Model = "gpt-missing"
	applyPrice(&e, catalog)
	if e.Priced {
		t.Fatal("missing cache rates were treated as free")
	}
	e.Model = "gpt-zero"
	applyPrice(&e, catalog)
	if !e.Priced {
		t.Fatal("explicit free cache was not priced")
	}
}

func TestPricingHTTPRequiresEveryRateAndDisablesCaching(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := openTestStore(t)
	router := gin.New()
	s.RegisterRoutes(router.Group("/stats"))
	for _, body := range []string{`{"model":"gpt-test"}`, `{"model":"gpt-test","input_per_million":1,"output_per_million":2,"cache_read_per_million":null,"cache_write_per_million":0}`} {
		r := httptest.NewRecorder()
		router.ServeHTTP(r, httptest.NewRequest("PUT", "/stats/pricing", strings.NewReader(body)))
		if r.Code != 400 {
			t.Fatalf("missing/null rate accepted: %d %s", r.Code, r.Body)
		}
	}
	r := httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest("PUT", "/stats/pricing", strings.NewReader(`{"model":"gpt-test","input_per_million":0,"output_per_million":0,"cache_read_per_million":0,"cache_write_per_million":0}`)))
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" || r.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("free manual/caching response: %d %#v", r.Code, r.Header())
	}
}

// TestAutomaticCodexHeadersUseSlotIdentityWithoutLosingAccounting keeps the
// accounting protection of the former slot-identity test: a credential's
// rate-limit headers are attributed to the account it publishes, a credential
// that publishes none is still recorded (grouped by provider alone), and neither
// case can drop or fabricate a usage event.
func TestAutomaticCodexHeadersUseSlotIdentityWithoutLosingAccounting(t *testing.T) {
	for _, test := range []struct {
		name, account string
	}{
		{name: "account-selector", account: fixtureAccount},
		{name: "no-account-property"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			r := fixtureRecord("codex-sample", time.Now().Add(-time.Minute))
			r.ResponseHeaders = http.Header{"X-Codex-Primary-Used-Percent": {"40"}, "X-Codex-Primary-Window-Minutes": {"300"}}
			if test.account == "" {
				// Model a credential that publishes no account property: the fact is
				// empty and the sample is grouped by provider alone, never guessed.
				r.Account, r.AccountKind = "", ""
			}
			if err := s.recordFixture(ctx, r); err != nil {
				t.Fatal(err)
			}
			snapshots, err := s.Quotas(ctx)
			if err != nil || len(snapshots) != 1 || snapshots[0].Account != test.account {
				t.Fatalf("Codex header attribution = %+v %v, want account=%q", snapshots, err, test.account)
			}
			page, err := s.Events(ctx, Filter{}, 10, 0)
			if err != nil || page.Total != 1 || len(page.Events) != 1 || page.Events[0].ID != r.RequestID || page.Events[0].TotalTokens != r.Detail.TokenBreakdown.TotalTokens {
				t.Fatalf("quota attribution dropped or altered accounting: %+v %v", page, err)
			}
			if s.droppedEvents.Load() != 0 || s.writeFailures.Load() != 0 {
				t.Fatal("quota-only attribution was accounted as an ingestion failure")
			}
		})
	}
}

type failingInsertStore struct{ store }

func (s failingInsertStore) Insert(context.Context, Event) (bool, error) {
	return false, errors.New("private DB error detail")
}
func TestIngestionFailureHealthIsExplicitAndSecretFree(t *testing.T) {
	s := openTestStore(t)
	s.store = failingInsertStore{store: s.store}
	s.Consume(context.Background(), fixturePayload(fixtureRecord("failed", time.Now())))
	flushFixture(t, s)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s.RegisterRoutes(router.Group("/stats"))
	r := httptest.NewRecorder()
	router.ServeHTTP(r, httptest.NewRequest("GET", "/stats/status", nil))
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"write_failures":1`) || strings.Contains(r.Body.String(), "private DB") || strings.Contains(r.Body.String(), `"last_write_failure_at":null`) {
		t.Fatalf("incorrect health: %d %s", r.Code, r.Body)
	}
}

func TestOpenRequiresExplicitDataDirectory(t *testing.T) {
	for _, dir := range []string{"", " \t "} {
		if s, err := Open(context.Background(), Options{DataDir: dir}); err == nil {
			_ = s.Close(context.Background())
			t.Fatal("opened implicit working-directory storage")
		}
	}
}

func TestStatusUsesOriginalCollectionSettingAndHistoryStaysAvailable(t *testing.T) {
	previous := redisqueue.UsageStatisticsEnabled()
	defer redisqueue.SetUsageStatisticsEnabled(previous)
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.recordFixture(ctx, fixtureRecord("persisted-history", time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	redisqueue.SetUsageStatisticsEnabled(false)
	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil || s == nil {
		t.Fatalf("disabled provider blocked store reopen: %v", err)
	}
	defer func() {
		if errClose := s.Close(ctx); errClose != nil {
			t.Error(errClose)
		}
	}()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s.RegisterRoutes(router.Group("/stats"))
	for _, enabled := range []bool{false, true, false} {
		redisqueue.SetUsageStatisticsEnabled(enabled)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("GET", "/stats/status", nil))
		var status map[string]any
		if err = json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || status["collection_enabled"] != enabled {
			t.Fatalf("status diverged from provider: %s", response.Body)
		}
		if _, exists := status["enabled"]; exists {
			t.Fatalf("status retained independent enabled flag: %s", response.Body)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/stats/events", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "persisted-history") {
		t.Fatalf("history unavailable while collection disabled: %d %s", response.Code, response.Body)
	}
}
