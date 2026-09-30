package usagepersist

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func identityIndexEntry(binding QuotaBinding) QuotaCacheEntry {
	return QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, Account: binding.Account, AccountKind: binding.AccountKind, ObservedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), State: json.RawMessage(`{"status":"success","windows":[]}`)}
}

// quotaIdentityIndexStore counts cache mutations per namespace without touching
// the filesystem.
type quotaIdentityIndexStore struct {
	store
	mu      sync.Mutex
	rows    map[string]json.RawMessage
	state   atomic.Int64
	history atomic.Int64
}

func (backend *quotaIdentityIndexStore) Cache(context.Context, string) (map[string]json.RawMessage, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	out := make(map[string]json.RawMessage, len(backend.rows))
	for key, raw := range backend.rows {
		out[key] = append(json.RawMessage(nil), raw...)
	}
	return out, nil
}

func (backend *quotaIdentityIndexStore) MutateCache(_ context.Context, namespace, key string, update func(json.RawMessage) (json.RawMessage, error)) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	raw, err := update(backend.rows[key])
	if err == nil && len(raw) > 0 {
		backend.rows[key] = append(json.RawMessage(nil), raw...)
		if namespace == quotaHistoryNamespace {
			backend.history.Add(1)
		} else {
			backend.state.Add(1)
		}
	}
	return err
}

func (backend *quotaIdentityIndexStore) writes() int64 {
	return backend.state.Load() + backend.history.Load()
}

// TestQuotaIdentityIndexLinearValidationCounts replaces the deleted generation
// fence. Identity is now a fact on the binding, so the projection, the encoded
// account row and the persisted display cache must all name the same account and
// stay linear in the number of credentials.
func TestQuotaIdentityIndexLinearValidationCounts(t *testing.T) {
	for _, count := range []int{1, 32, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			backend := &quotaIdentityIndexStore{store: s.store, rows: map[string]json.RawMessage{}}
			s.store = backend
			manager := coreauth.NewManager(nil, nil, nil)
			for i := range count {
				auth := quotaFixtureAuth("codex", fmt.Sprintf("index-%04d", i), fmt.Sprintf("auth-%04d.json", i), fmt.Sprintf("secret-%04d", i))
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
			}
			source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
			s.BindQuotaIdentitySource(source)
			bindings := s.publishQuotaBindings()
			if len(bindings) != count {
				t.Fatalf("projected %d of %d bindings", len(bindings), count)
			}
			if backend.writes() != 0 {
				t.Fatal("projection performed storage writes")
			}
			entries := make([]QuotaCacheEntry, 0, count)
			for _, binding := range bindings {
				if binding.Account == "" || binding.AccountKind != "email" {
					t.Fatalf("binding lost its account facts: %+v", binding)
				}
				entry := identityIndexEntry(binding)
				entries = append(entries, entry)
				snapshot := &quota.Snapshot{Provider: binding.Provider, Account: binding.Account, AccountKind: binding.AccountKind, ObservedAt: entry.ObservedAt, Windows: []quota.Window{}}
				raw, err := json.Marshal(quotaState{Snapshot: snapshot})
				if err != nil {
					t.Fatal(err)
				}
				backend.rows[quotaStoreKey(binding.Provider, binding.Account)] = raw
			}
			// One display-cache row per credential, and nothing else.
			if err := s.SaveQuotaCache(ctx, entries); err != nil {
				t.Fatal(err)
			}
			if got := backend.state.Load(); got != int64(count) {
				t.Fatalf("batch wrote %d state rows, want one per credential = %d", got, count)
			}
			cached, err := s.QuotaCache(ctx)
			if err != nil || len(cached) != count {
				t.Fatalf("cache count = %d, err = %v", len(cached), err)
			}
			for _, entry := range cached {
				if entry.Account == "" || !strings.HasSuffix(entry.Account, "@example.invalid") {
					t.Fatalf("cached entry lost its account fact: %+v", entry)
				}
			}
			if snapshots, err := s.Quotas(ctx); err != nil || len(snapshots) != count {
				t.Fatalf("quota count = %d, err = %v", len(snapshots), err)
			}
			// A single merge writes one account row and one history row, and must
			// never touch any other account's row.
			beforeState, beforeHistory := backend.state.Load(), backend.history.Load()
			entry := entries[0]
			at := entry.ObservedAt.Add(time.Minute)
			if err := s.mergeQuota(ctx, quota.Snapshot{Provider: entry.Provider, Account: entry.Account, AccountKind: entry.AccountKind,
				Source: quota.SourceFetch, ObservedAt: at,
				Windows: []quota.Window{{ID: "primary", UsedPercent: floatPtr(40), ObservedAt: at, Source: quota.SourceFetch}}}); err != nil {
				t.Fatal(err)
			}
			if got := backend.state.Load() - beforeState; got != 1 {
				t.Fatalf("single merge wrote %d state rows, want 1", got)
			}
			if got := backend.history.Load() - beforeHistory; got != 1 {
				t.Fatalf("single merge wrote %d history rows, want 1", got)
			}
			history, err := s.quotaHistory(ctx, entry.Provider, entry.Account)
			if err != nil || len(history.Observations) != 1 || history.Observations[0].Windows["primary"] != 40 {
				t.Fatalf("history for the merged account: %+v %v", history, err)
			}
		})
	}
}

// TestQuotaIdentityIndexAmbiguityIsNotResolvedByInvalidCompetitor keeps the
// ambiguity rules: a shared transient index or display key must not bind, and
// removing the competitor must make the remaining credential bindable again.
func TestQuotaIdentityIndexAmbiguityIsNotResolvedByInvalidCompetitor(t *testing.T) {
	for _, duplicate := range []string{"index", "key"} {
		for _, invalid := range []string{"metadata", "disk", "other-provider"} {
			t.Run(duplicate+"/"+invalid, func(t *testing.T) {
				if duplicate == "key" && invalid == "other-provider" {
					t.Skip("display keys are provider-scoped")
				}
				ctx := context.Background()
				manager := coreauth.NewManager(nil, nil, nil)
				first := quotaFixtureAuth("codex", "first", "first.json", "A")
				second := quotaFixtureAuth("codex", "second", "second.json", "B")
				if duplicate == "index" {
					second.Index = first.Index
				} else {
					second.FileName = first.FileName
				}
				if invalid == "metadata" {
					second.Metadata["unknown_account_selector"] = "private"
				} else if invalid == "other-provider" {
					second.Provider = "unsupported-provider"
				}
				for _, auth := range []*coreauth.Auth{first, second} {
					if _, err := manager.Register(ctx, auth); err != nil {
						t.Fatal(err)
					}
				}
				source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
				if bindings := source.QuotaBindings(); len(bindings) != 0 {
					t.Fatalf("ambiguous read escaped: %+v", bindings)
				}
				manager.Remove(ctx, second.ID)
				if bindings := source.QuotaBindings(); len(bindings) != 1 {
					t.Fatalf("removed ambiguity remained: %+v", bindings)
				}
			})
		}
	}
}

// TestQuotaIdentityIndexRowsAreAccountScoped proves the durable rows are
// account-scoped: a saved display row for one account can never appear as
// another account's row, and a later account takes over its own row only.
func TestQuotaIdentityIndexRowsAreAccountScoped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	bindQuotaFixtures(t, s,
		quotaFixtureAuth("codex", "first", "first.json", "A"),
		quotaFixtureAuth("codex", "second", "second.json", "B"))
	first := fixtureBinding(s, "codex", "first")
	second := fixtureBinding(s, "codex", "second")
	if first.Account == "" || first.Account == second.Account {
		t.Fatalf("fixture accounts: %+v %+v", first, second)
	}
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{identityIndexEntry(first)}); err != nil {
		t.Fatal(err)
	}
	values, err := s.ListCache(ctx, quotaNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(values[quotaStoreKey("codex", first.Account)]) == 0 {
		t.Fatal("row was not keyed by the account fact")
	}
	if len(values[quotaStoreKey("codex", second.Account)]) != 0 {
		t.Fatal("an unrelated account inherited the row")
	}
	if cached, err := s.QuotaCache(ctx); err != nil || len(cached) != 1 || cached[0].Account != first.Account {
		t.Fatalf("display cache: %+v %v", cached, err)
	}
}

// TestQuotaIdentityIndexBoundsAndFallbackAmbiguity keeps the source bounds: an
// oversized binding list and an oversized credential catalog are refused whole,
// and a duplicate transient index stays ambiguous.
func TestQuotaIdentityIndexBoundsAndFallbackAmbiguity(t *testing.T) {
	binding := QuotaBinding{Provider: "codex", AuthIndex: "index", Key: "same.json", Account: "same@example.invalid", AccountKind: "email"}
	if _, ok := indexQuotaBindings([]QuotaBinding{binding, binding, binding}).binding("codex", "index"); ok {
		t.Fatal("third duplicate resurrected an ambiguous index")
	}
	other := binding
	other.AuthIndex = "other"
	index := indexQuotaBindings([]QuotaBinding{binding, other})
	if _, ok := index.binding("codex", "index"); !ok {
		t.Fatal("distinct transient indexes did not bind")
	}
	for _, candidate := range []struct{ provider, index string }{
		{"claude", "index"},
		{"codex", "unknown"},
	} {
		if _, ok := index.binding(candidate.provider, candidate.index); ok {
			t.Fatal("indexed lookup accepted a mismatched provider or unknown index")
		}
	}
	if got := indexQuotaBindings(make([]QuotaBinding, maxQuotaIdentities+1)); got != nil {
		t.Fatal("unbounded source accepted")
	}
	if got := newQuotaAuthCatalog(make([]*coreauth.Auth, maxQuotaIdentities+1)); got.byIndex != nil || got.keys != nil {
		t.Fatal("unbounded catalog accepted")
	}
}

// TestQuotaIdentitiesHTTPPublishesAccountFacts covers the dashboard-facing
// identity document: it publishes the account facts and the transient
// correlation index, and never the removed generation/revision material.
func TestQuotaIdentitiesHTTPPublishesAccountFacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := openTestStore(t)
	auth := quotaFixtureAuth("codex", "index", "same.json", "private-token")
	auth.Metadata["account_id"] = "account-A"
	bindQuotaFixtures(t, s, auth)
	binding := fixtureBinding(s, "codex", "index")
	if len(s.publishedQuotaBindings()) != 1 {
		t.Fatal("identity publication missing")
	}
	engine := gin.New()
	s.RegisterRoutes(engine.Group("/stats"))
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats/quota/identities", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("identity GET status %d", response.Code)
	}
	body := response.Body.String()
	for _, field := range []string{`"provider":"codex"`, `"account":"` + binding.Account + `"`, `"account_kind":"email"`, `"auth_index":"index"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("identity response lacks %s: %s", field, body)
		}
	}
	for _, removed := range []string{"credential_generation", "revision", "lifetime", "private-token", "access_token"} {
		if strings.Contains(body, removed) {
			t.Fatalf("identity response published removed material %q: %s", removed, body)
		}
	}
	// Codex has operation selectors, so it publishes a proof of their digests.
	// The raw selector value is an account id the request must supply itself and
	// must never be echoed back.
	if !strings.Contains(body, `"selector_hashes":{"account_id":`) {
		t.Fatalf("codex proof is missing its selector digests: %s", body)
	}
	if strings.Contains(body, "account-A") {
		t.Fatalf("identity response echoed a raw selector value: %s", body)
	}
}

// TestQuotaIdentityObserverSurvivesCredentialTokenRotation keeps the availability
// contract: a credential token rotation does not withdraw the observation
// publication, so a later original-handler request still observes quota.
func TestQuotaIdentityObserverSurvivesCredentialTokenRotation(t *testing.T) {
	s := openTestStore(t)
	auth := quotaFixtureAuth("codex", "index", "same.json", "A")
	manager := bindQuotaFixtures(t, s, auth)
	original := fixtureBinding(s, "codex", "index")
	auth.Metadata["access_token"] = "B"
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	rotated := fixtureBinding(s, "codex", "index")
	if rotated.Account != original.Account || rotated.Key != original.Key {
		t.Fatal("token rotation changed the published identity")
	}
	s.ObserveQuotaFetch(context.Background(), rotated, managementFetchFixture(t))
	flushFixture(t, s)
	if got := middlewareQuotaCount(t, s); got != 1 {
		t.Fatalf("observation after rotation = %d, want 1", got)
	}
}
