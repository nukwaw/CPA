package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

// The live producer's credential metadata must project the account facts for each
// provider: email everywhere except kimi, which identifies its account by device_id.
// Informational timestamps are not identity, so continuity survives them.
func TestUsageQuotaIdentityAcceptsCurrentProducerMetadata(t *testing.T) {
	antigravity := sdkAuth.BuildAntigravityAuth(&sdkAuth.AntigravityTokenResponse{AccessToken: "ag-token", RefreshToken: "ag-refresh", ExpiresIn: 3600}, "same@example.invalid", "project-a")
	fixtures := []struct {
		auth        *coreauth.Auth
		account     string
		accountKind string
		selectors   int
	}{
		{antigravity, "same@example.invalid", "email", 1},
		{&coreauth.Auth{ID: "kimi.json", FileName: "kimi.json", Provider: "kimi", Metadata: map[string]any{
			"type": "kimi", "access_token": "kimi-token", "refresh_token": "kimi-refresh", "token_type": "Bearer", "scope": "all", "timestamp": int64(123), "domain": "kimi.com", "base_url": "https://api.kimi.com/coding", "expired": "2099-01-01T00:00:00Z", "device_id": "device-a",
		}}, "device-a", "device_id", 0},
		{&coreauth.Auth{ID: "meta.json", FileName: "meta.json", Provider: "meta", Metadata: map[string]any{
			"type": "meta", "access_token": "minted-key", "api_key": "minted-key", "dca_token": "dca-secret", "token_type": "Bearer", "expires_in": 3600, "expired": "2099-01-01T00:00:00Z", "last_refresh": "2026-09-29T00:00:00Z", "base_url": "https://api.meta.ai", "auth_kind": "oauth", "dca_expired": "2099-01-01T00:00:00Z", "dca_expires_at": int64(4070908800), "subs_tier_name": "Pro", "subs_tier_id": "tier-a", "is_subs_active": true, "has_payment_method": true,
		}, Attributes: map[string]string{"api_key": "minted-key", "dca_token": "dca-secret", "base_url": "https://api.meta.ai", "auth_kind": "oauth"}}, "", "", 0},
		{&coreauth.Auth{ID: "devin.json", FileName: "devin.json", Provider: "devin", Metadata: map[string]any{
			"type": "devin", "api_key": "session-secret", "session_token": "session-secret", "user_name": "user-a", "user_id": "user-id", "org_id": "org-a", "auth_kind": "oauth",
		}, Attributes: map[string]string{"api_key": "session-secret", "session_token": "session-secret", "user_id": "user-id", "org_id": "org-a", "auth_kind": "oauth"}}, "", "", 0},
	}
	for _, fixture := range fixtures {
		auth := fixture.auth
		t.Run(auth.Provider, func(t *testing.T) {
			before, ok := usagepersist.ProjectQuotaBinding(auth)
			if !ok {
				t.Fatal("ordinary producer metadata lost quota continuity")
			}
			if before.Provider != auth.Provider || before.Account != fixture.account || before.AccountKind != fixture.accountKind {
				t.Fatalf("projected facts = %+v, want account=%q kind=%q", before, fixture.account, fixture.accountKind)
			}
			if before.Key == "" {
				t.Fatal("projection lost the credential display key")
			}
			// Operation selectors are display preconditions, never identity, and
			// only providers that define them publish a digest.
			if len(before.SelectorHashes) != fixture.selectors {
				t.Fatalf("selector digests = %+v, want %d", before.SelectorHashes, fixture.selectors)
			}
			changed := auth.Clone()
			changed.Metadata["timestamp"] = int64(456)
			changed.Metadata["last_refresh"] = "2026-09-29T01:00:00Z"
			after, ok := usagepersist.ProjectQuotaBinding(changed)
			if !ok {
				t.Fatal("informational timestamp revoked quota continuity")
			}
			if before.Account != after.Account || before.AccountKind != after.AccountKind {
				t.Fatalf("informational timestamp changed account facts: %+v -> %+v", before, after)
			}
			// A token rotation is the ordinary refresh core performs; account facts
			// must survive it, and no token may appear in the projection.
			changed.Metadata["access_token"] = "rotated-token"
			rotated, ok := usagepersist.ProjectQuotaBinding(changed)
			if !ok || rotated.Account != before.Account || rotated.AccountKind != before.AccountKind {
				t.Fatalf("token rotation changed account facts: %+v", rotated)
			}
			if strings.Contains(rotated.Key, "rotated-token") {
				t.Fatal("projection exposed a rotated token")
			}
			// Unknown metadata is not identity and is refused rather than silently
			// coerced: it could select a different account than the projection shows.
			changed.Metadata["unknown_account_selector"] = "another-account"
			if _, ok := usagepersist.ProjectQuotaBinding(changed); ok {
				t.Fatal("unknown selector was silently trusted")
			}
		})
	}
}

// Operation selectors are not identity and carry no credential material: codex
// publishes a digest of its account selector while never publishing the value.
func TestUsageQuotaIdentityProjectsOperationSelectorsWithoutSecrets(t *testing.T) {
	auth := &coreauth.Auth{ID: "codex.json", FileName: "codex.json", Provider: "codex", Metadata: map[string]any{"access_token": "private-token", "email": "same@example.invalid", "account_id": "account-a"}}
	binding, ok := usagepersist.ProjectQuotaBinding(auth)
	if !ok {
		t.Fatal("codex credential was not projected")
	}
	if binding.Account != "same@example.invalid" || binding.AccountKind != "email" {
		t.Fatalf("codex account facts = %+v", binding)
	}
	digest, ok := binding.SelectorHashes["account_id"]
	if !ok {
		t.Fatal("codex operation selector digest missing")
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte("account-a"))); digest != want {
		t.Fatalf("codex selector digest = %q, want %q", digest, want)
	}
	for _, secret := range []string{"private-token", "account-a"} {
		if strings.Contains(binding.Key, secret) {
			t.Fatalf("projection leaked %q in its key", secret)
		}
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
			source := usagepersist.NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
			store.BindQuotaIdentitySource(source)
			// The explicit add-on identity read publishes bounded request-start
			// evidence through the real endpoint, exactly as the dashboard bridge
			// primes it; original handlers never query the live manager themselves.
			prime := gin.New()
			store.RegisterRoutes(prime.Group("/stats"))
			primeResponse := httptest.NewRecorder()
			prime.ServeHTTP(primeResponse, httptest.NewRequest(http.MethodGet, "/stats/quota/identities", nil))
			if primeResponse.Code != http.StatusOK {
				t.Fatalf("prime identities: %d %s", primeResponse.Code, primeResponse.Body)
			}
			bindings := source.QuotaBindings()
			if len(bindings) != 1 {
				t.Fatalf("codex credential was not projected: %+v", bindings)
			}
			binding := bindings[0]
			// This credential exposes no account property, so its account facts are
			// empty and it is still projected and observable.
			if binding.Account != "" || binding.AccountKind != "" {
				t.Fatalf("accountless credential invented facts: %+v", binding)
			}
			entry := usagepersist.QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, Account: binding.Account, AccountKind: binding.AccountKind, ObservedAt: time.Now().Add(-time.Hour), State: json.RawMessage(`{"status":"success","windows":[]}`)}
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
