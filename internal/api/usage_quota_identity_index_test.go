package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUsageQuotaIdentityIndexConcurrentReplacementHTTP(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": "old-secret", "account_id": "account-A", "email": "account-000@example.invalid"}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager })
	bindings := source.QuotaBindings()
	if len(bindings) != 1 {
		t.Fatal("initial identity missing")
	}
	binding := bindings[0]
	if binding.Account != "account-000@example.invalid" || binding.AccountKind != "email" {
		t.Fatalf("projected facts = %+v", binding)
	}
	s := persistenceTestStore(t)
	s.BindQuotaIdentitySource(source)
	// The entry belongs to the account the credential exposes right now. Its
	// observation time is explicit so stale-write ordering stays deterministic.
	entry := usagepersist.QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, Account: binding.Account, AccountKind: binding.AccountKind, ObservedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), State: json.RawMessage(`{"status":"success","windows":[]}`)}
	if err := s.SaveQuotaCache(ctx, []usagepersist.QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	s.RegisterRoutes(engine.Group("/stats"))
	request := func(method, route string, body []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		r := httptest.NewRequest(method, route, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(recorder, r)
		return recorder
	}
	// Each round hands the credential to a different account. A rotated token for
	// the same account would instead be an ordinary refresh that keeps identity.
	update := func(iteration int) error {
		current, ok := manager.GetByID(auth.ID)
		if !ok {
			return fmt.Errorf("missing current credential")
		}
		current.Metadata["access_token"] = fmt.Sprintf("new-secret-%03d", iteration)
		current.Metadata["email"] = fmt.Sprintf("account-%03d@example.invalid", iteration)
		_, err := manager.Update(ctx, current)
		return err
	}
	if err := update(0); err != nil {
		t.Fatal(err)
	}
	const rounds = 48
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for i := 1; i <= rounds; i++ {
			if err := update(i); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for range rounds {
				response := request(http.MethodGet, "/stats/quota/cache", nil)
				var cache struct {
					Entries []usagepersist.QuotaCacheEntry `json:"entries"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &cache) != nil {
					t.Errorf("cache GET failed: %d %s", response.Code, response.Body)
					return
				}
				for _, current := range cache.Entries {
					if current.Account != entry.Account {
						t.Errorf("stored display state crossed accounts: %s", response.Body)
						return
					}
				}
				response = request(http.MethodGet, "/stats/quota/identities", nil)
				var identities struct {
					Bindings []usagepersist.QuotaBinding `json:"bindings"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &identities) != nil {
					t.Errorf("identity GET failed: %d %s", response.Code, response.Body)
					return
				}
				// The credential may switch account between two reads, but every
				// published binding must be a complete, consistent projection: one
				// credential can never publish two accounts at once.
				if len(identities.Bindings) != 1 {
					t.Errorf("identity GET published %d bindings for one credential: %s", len(identities.Bindings), response.Body)
					return
				}
				if current := identities.Bindings[0]; current.AccountKind != "email" || current.Provider != "codex" || !strings.HasSuffix(current.Account, "@example.invalid") {
					t.Errorf("published binding is not a coherent account projection: %s", response.Body)
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	current := source.QuotaBindings()
	if len(current) != 1 || current[0].Account != fmt.Sprintf("account-%03d@example.invalid", rounds) {
		t.Fatalf("latest replacement missing after stress: %+v", current)
	}
}
