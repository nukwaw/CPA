package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUsageQuotaIdentityAcceptsCurrentProducerMetadata(t *testing.T) {
	antigravity := sdkAuth.BuildAntigravityAuth(&sdkAuth.AntigravityTokenResponse{AccessToken: "ag-token", RefreshToken: "ag-refresh", ExpiresIn: 3600}, "same@example.invalid", "project-a")
	fixtures := []*coreauth.Auth{
		antigravity,
		{ID: "kimi.json", FileName: "kimi.json", Provider: "kimi", Metadata: map[string]any{
			"type": "kimi", "access_token": "kimi-token", "refresh_token": "kimi-refresh", "token_type": "Bearer", "scope": "all", "timestamp": int64(123), "domain": "kimi.com", "base_url": "https://api.kimi.com/coding", "expired": "2099-01-01T00:00:00Z", "device_id": "device-a",
		}},
		{ID: "meta.json", FileName: "meta.json", Provider: "meta", Metadata: map[string]any{
			"type": "meta", "access_token": "minted-key", "api_key": "minted-key", "dca_token": "dca-secret", "token_type": "Bearer", "expires_in": 3600, "expired": "2099-01-01T00:00:00Z", "last_refresh": "2026-09-29T00:00:00Z", "base_url": "https://api.meta.ai", "auth_kind": "oauth", "dca_expired": "2099-01-01T00:00:00Z", "dca_expires_at": int64(4070908800), "subs_tier_name": "Pro", "subs_tier_id": "tier-a", "is_subs_active": true, "has_payment_method": true,
		}, Attributes: map[string]string{"api_key": "minted-key", "dca_token": "dca-secret", "base_url": "https://api.meta.ai", "auth_kind": "oauth"}},
		{ID: "devin.json", FileName: "devin.json", Provider: "devin", Metadata: map[string]any{
			"type": "devin", "api_key": "session-secret", "session_token": "session-secret", "user_name": "user-a", "user_id": "user-id", "org_id": "org-a", "auth_kind": "oauth",
		}, Attributes: map[string]string{"api_key": "session-secret", "session_token": "session-secret", "user_id": "user-id", "org_id": "org-a", "auth_kind": "oauth"}},
	}
	for _, auth := range fixtures {
		t.Run(auth.Provider, func(t *testing.T) {
			before, ok := usagepersist.ProjectQuotaBinding(auth)
			if !ok || before.CredentialGeneration == "" {
				t.Fatal("ordinary producer metadata lost quota continuity")
			}
			changed := auth.Clone()
			changed.Metadata["timestamp"] = int64(456)
			changed.Metadata["last_refresh"] = "2026-09-29T01:00:00Z"
			after, ok := usagepersist.ProjectQuotaBinding(changed)
			if !ok || before.CredentialGeneration != after.CredentialGeneration {
				t.Fatal("informational timestamp changed credential identity")
			}
			changed.Metadata["unknown_account_selector"] = "another-account"
			if _, ok := usagepersist.ProjectQuotaBinding(changed); ok {
				t.Fatal("unknown selector was silently ignored")
			}
		})
	}
}

func TestUsageQuotaConfirmedCodexConsumeBodyPreservesResetBarrier(t *testing.T) {
	for _, tc := range []struct {
		name, account string
		data          map[string]any
		cleared       bool
	}{
		{"original idempotency body", "account-a", map[string]any{"redeem_request_id": "91d30927-3f04-44f8-b39e-91f528b57915"}, true},
		{"wrong account", "account-b", map[string]any{"redeem_request_id": "request-id"}, false},
		{"unexpected selector", "account-a", map[string]any{"redeem_request_id": "request-id", "account_id": "account-b"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := persistenceTestStore(t)
			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{ID: "codex.json", FileName: "codex.json", Provider: "codex", Metadata: map[string]any{"access_token": "private-token", "account_id": "account-a"}}
			if _, err := manager.Register(ctx, auth); err != nil {
				t.Fatal(err)
			}
			source := usagepersist.NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
			store.BindQuotaIdentitySource(source)
			binding := source.QuotaBindings(false)[0]
			entry := usagepersist.QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, Revision: binding.Revision, ObservedAt: time.Now().Add(-time.Hour), State: json.RawMessage(`{"status":"success","windows":[]}`)}
			if err := store.SaveQuotaCache(ctx, []usagepersist.QuotaCacheEntry{entry}); err != nil {
				t.Fatal(err)
			}
			router := gin.New()
			router.Use(store.ManagementMiddleware(nil, nil))
			const original = `{"status_code":200,"header":{},"body":"{\"code\":\"reset\",\"windows_reset\":1}"}`
			router.POST("/v0/management/api-call", func(c *gin.Context) {
				var input map[string]any
				if err := c.ShouldBindJSON(&input); err != nil {
					t.Fatal(err)
				}
				c.Data(http.StatusOK, "application/json", []byte(original))
			})
			data, _ := json.Marshal(tc.data)
			body, _ := json.Marshal(map[string]any{"auth_index": binding.AuthIndex, "method": "POST", "url": "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", "header": map[string]string{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": tc.account}, "data": string(data)})
			request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != original {
				t.Fatal("observation changed the original reset response")
			}
			if err := store.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			entries, err := store.QuotaCache(ctx)
			if err != nil || (len(entries) == 0) != tc.cleared {
				t.Fatalf("reset attribution: entries=%d cleared=%v error=%v", len(entries), tc.cleared, err)
			}
		})
	}
}
