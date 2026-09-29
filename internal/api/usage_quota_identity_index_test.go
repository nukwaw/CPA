package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUsageQuotaIdentityIndexConcurrentReplacementHTTP(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "same.json")
	write := func(token string) error {
		data, err := json.Marshal(map[string]any{"type": "codex", "access_token": token, "account_id": "account-A", "request-retry": 2, "excluded-models": []any{"private-*"}})
		if err != nil {
			return err
		}
		pending := filepath.Join(directory, "pending.json")
		if err = os.WriteFile(pending, data, 0600); err != nil {
			return err
		}
		return os.Rename(pending, path)
	}
	if err := write("old-secret"); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "same.json", Index: "index", FileName: "same.json", Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": "old-secret", "account_id": "account-A", "request_retry": 2, "excluded_models": []any{"private-*"}}, Attributes: map[string]string{coreauth.AttributePath: path, coreauth.AttributeSourceBackend: coreauth.AuthSourceFile}}
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
	bindings := source.QuotaBindings(true)
	if len(bindings) != 1 {
		t.Fatal("initial identity missing")
	}
	binding := bindings[0]
	s := persistenceTestStore(t)
	s.BindQuotaIdentitySource(source)
	entry := usagepersist.QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, Revision: binding.Revision, ObservedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), State: json.RawMessage(`{"status":"success","windows":[]}`)}
	if err := s.SaveQuotaCache(ctx, []usagepersist.QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	s.RegisterRoutes(engine.Group("/stats"))
	payload, err := json.Marshal(map[string]any{"entries": []usagepersist.QuotaCacheEntry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, route string, body []byte) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		r := httptest.NewRequest(method, route, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(recorder, r)
		return recorder
	}
	update := func(iteration int) error {
		token := fmt.Sprintf("new-secret-%03d", iteration)
		if err := write(token); err != nil {
			return err
		}
		current, ok := manager.GetByID(auth.ID)
		if !ok {
			return fmt.Errorf("missing current credential")
		}
		current.Metadata["access_token"] = token
		_, err := manager.Update(ctx, current)
		return err
	}
	// Every reader starts after the old credential is gone. Subsequent manager
	// and atomic file replacements race with real add-on HTTP GETs and PUTs.
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
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &cache) != nil || len(cache.Entries) != 0 {
					t.Errorf("stale cached identity escaped concurrent replacement: %d %s", response.Code, response.Body)
					return
				}
				response = request(http.MethodGet, "/stats/quota/identities", nil)
				var identities struct {
					Bindings []usagepersist.QuotaBinding `json:"bindings"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &identities) != nil {
					t.Errorf("identity GET failed: %d %s", response.Code, response.Body)
					return
				}
				for _, current := range identities.Bindings {
					if current.CredentialGeneration == binding.CredentialGeneration || current.Revision == binding.Revision {
						t.Error("stale identity snapshot escaped concurrent replacement")
						return
					}
				}
				response = request(http.MethodPut, "/stats/quota/cache", payload)
				if response.Code != http.StatusConflict {
					t.Errorf("stale concurrent PUT = %d, want 409", response.Code)
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	current := source.QuotaBindings(true)
	if len(current) != 1 || current[0].CredentialGeneration == binding.CredentialGeneration {
		t.Fatal("latest reconciled replacement missing after stress")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	response := request(http.MethodGet, "/stats/quota/identities", nil)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"bindings":[]`)) {
		t.Fatalf("deleted credential survived a fresh read: %d %s", response.Code, response.Body)
	}
}
