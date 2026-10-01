package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// stubBuiltinEndpoint points one provider's builtin endpoint at a fixture
// server for the duration of a test.
func stubBuiltinEndpoint(t *testing.T, provider string, upstream *httptest.Server) {
	t.Helper()
	endpoint, ok := builtinQuotaEndpoints[provider]
	if !ok {
		t.Fatalf("no builtin endpoint for %q", provider)
	}
	original := endpoint.url
	endpoint.url = upstream.URL
	builtinQuotaEndpoints[provider] = endpoint
	t.Cleanup(func() {
		endpoint.url = original
		builtinQuotaEndpoints[provider] = endpoint
	})
}

func quotaUpstream(t *testing.T, wantAuth, body string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func fetchBuiltinForAuth(t *testing.T, auth *coreauth.Auth) (int, pluginapi.QuotaFetchResponse, *coreauth.Auth) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	authIndex := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(pluginhost.New())

	var observed *coreauth.Auth
	h.SetQuotaFetchObserver(func(_ context.Context, observedAuth *coreauth.Auth, _ pluginapi.QuotaFetchResponse) {
		observed = observedAuth
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/quota/fetch", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.FetchCredentialQuota(ctx)

	var quotaResp pluginapi.QuotaFetchResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &quotaResp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return rec.Code, quotaResp, observed
}

func TestFetchCredentialQuota_BuiltinKimi(t *testing.T) {
	upstream := quotaUpstream(t, "Bearer kimi-token", `{
		"usage": {"limit": 100, "remaining": 25},
		"limits": [{"name": "short", "title": "5h limit", "detail": {"limit": 10, "remaining": 0}, "resetAt": "2026-10-01T00:00:00Z"}]
	}`)

	auth := &coreauth.Auth{
		ID:       "kimi-builtin",
		FileName: "kimi-1.json",
		Provider: "kimi",
		Metadata: map[string]any{"access_token": "kimi-token", "device_id": "device-1"},
		// A custom base_url drives the endpoint per credential.
		Attributes: map[string]string{"base_url": upstream.URL},
	}
	code, quotaResp, observed := fetchBuiltinForAuth(t, auth)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if observed == nil || observed.ID != auth.ID {
		t.Fatalf("observer not notified with the live credential: %+v", observed)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 2 {
		t.Fatalf("unexpected groups: %+v", quotaResp.Groups)
	}
	buckets := quotaResp.Groups[0].Buckets
	if buckets[0].Window != "overall" || buckets[0].RemainingFraction != 0.25 {
		t.Fatalf("overall bucket: %+v", buckets[0])
	}
	if buckets[1].Window != "short" || buckets[1].RemainingFraction != 0 || buckets[1].Description != "5h limit" {
		t.Fatalf("limit bucket: %+v", buckets[1])
	}
}

func TestBuiltinKimiDomainResolution(t *testing.T) {
	comAuth := &coreauth.Auth{Provider: "kimi"}
	if got := builtinKimiUsagesURL(comAuth); got != "https://api.kimi.com/coding/v1/usages" {
		t.Fatalf("kimi.com url = %q", got)
	}
	aiAuth := &coreauth.Auth{Provider: "kimi-ai"}
	if got := builtinKimiUsagesURL(aiAuth); got != "https://api.kimi.ai/coding/v1/usages" {
		t.Fatalf("kimi.ai url = %q", got)
	}
	custom := &coreauth.Auth{Provider: "kimi", Attributes: map[string]string{"base_url": "https://gateway.example/coding/v1/"}}
	if got := builtinKimiUsagesURL(custom); got != "https://gateway.example/coding/v1/usages" {
		t.Fatalf("custom base_url = %q", got)
	}
	if _, ok := builtinQuotaEndpointFor(&coreauth.Auth{Provider: "unknown"}); ok {
		t.Fatal("unknown provider resolved a builtin endpoint")
	}
}

func TestFetchCredentialQuota_BuiltinClaudeUsedPercentInversion(t *testing.T) {
	upstream := quotaUpstream(t, "Bearer claude-token", `{
		"five_hour": {"utilization": 37, "resets_at": "2026-09-30T10:00:00Z"},
		"seven_day_sonnet": {"utilization": "0"}
	}`)
	stubBuiltinEndpoint(t, "claude", upstream)

	auth := &coreauth.Auth{ID: "claude-builtin", FileName: "claude.json", Provider: "claude", Metadata: map[string]any{"access_token": "claude-token", "email": "a@example.test"}}
	code, quotaResp, _ := fetchBuiltinForAuth(t, auth)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 2 {
		t.Fatalf("unexpected groups: %+v", quotaResp.Groups)
	}
	first := quotaResp.Groups[0].Buckets[0]
	if first.Window != "5h" || first.RemainingFraction != 0.63 || first.ResetTime != "2026-09-30T10:00:00Z" {
		t.Fatalf("utilization was not inverted into remaining fraction: %+v", first)
	}
	if second := quotaResp.Groups[0].Buckets[1]; second.Window != "7d Sonnet" || second.RemainingFraction != 1 {
		t.Fatalf("zero utilization bucket: %+v", second)
	}
}

func TestFetchCredentialQuota_BuiltinCodex(t *testing.T) {
	upstream := quotaUpstream(t, "Bearer codex-token", `{
		"plan_type": "plus",
		"rate_limit": {"primary_window": {"used_percent": 10, "limit_window_seconds": 18000}, "secondary_window": {"used_percent": 5}},
		"additional_rate_limits": [{"limit_name": "GPT-Spark", "rate_limit": {"primary_window": {"used_percent": 15}}}],
		"credits": {"balance": 42}
	}`)
	stubBuiltinEndpoint(t, "codex", upstream)

	auth := &coreauth.Auth{ID: "codex-builtin", FileName: "codex.json", Provider: "codex", Metadata: map[string]any{"access_token": "codex-token", "email": "a@example.test"}}
	code, quotaResp, _ := fetchBuiltinForAuth(t, auth)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if quotaResp.Subscription == nil || quotaResp.Subscription.Plan != "plus" {
		t.Fatalf("plan lost: %+v", quotaResp.Subscription)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 3 {
		t.Fatalf("unexpected groups: %+v", quotaResp.Groups)
	}
	if bucket := quotaResp.Groups[0].Buckets[2]; bucket.Window != "GPT-Spark primary" || bucket.RemainingFraction != 0.85 {
		t.Fatalf("additional limit bucket: %+v", bucket)
	}
	if len(quotaResp.Summary) != 1 || quotaResp.Summary[0].Key != "credits_balance" || quotaResp.Summary[0].Value != 42 {
		t.Fatalf("credits summary: %+v", quotaResp.Summary)
	}
}

func TestFetchCredentialQuota_BuiltinAntigravityPercentSuffix(t *testing.T) {
	upstream := quotaUpstream(t, "Bearer ag-token", `{"body": {"groups": [
		{"displayName": "Gemini Models", "buckets": [{"bucketId": "pro", "window": "weekly", "remainingFraction": "25%", "resetTime": "2026-10-01T00:00:00Z"}]}
	]}}`)
	stubBuiltinEndpoint(t, "antigravity", upstream)

	auth := &coreauth.Auth{ID: "ag-builtin", FileName: "ag.json", Provider: "antigravity", Metadata: map[string]any{"access_token": "ag-token", "email": "a@example.test", "expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}
	code, quotaResp, _ := fetchBuiltinForAuth(t, auth)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 1 {
		t.Fatalf("unexpected groups: %+v", quotaResp.Groups)
	}
	if bucket := quotaResp.Groups[0].Buckets[0]; bucket.RemainingFraction != 0.25 || bucket.Window != "weekly" {
		t.Fatalf("percent-suffixed fraction: %+v", bucket)
	}
}

func TestFetchCredentialQuota_BuiltinGeminiAndXAI(t *testing.T) {
	geminiUpstream := quotaUpstream(t, "Bearer gemini-token", `{"buckets": [{"modelId": "gemini-2.5-pro", "tokenType": "REQUESTS", "remainingFraction": 0.8, "resetTime": "2026-10-01T00:00:00Z"}]}`)
	stubBuiltinEndpoint(t, "gemini-cli", geminiUpstream)
	geminiAuth := &coreauth.Auth{ID: "gemini-builtin", FileName: "gemini.json", Provider: "gemini-cli", Metadata: map[string]any{"access_token": "gemini-token", "email": "a@example.test"}}
	code, quotaResp, _ := fetchBuiltinForAuth(t, geminiAuth)
	if code != http.StatusOK {
		t.Fatalf("gemini expected 200, got %d", code)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 1 || quotaResp.Groups[0].Buckets[0].RemainingFraction != 0.8 {
		t.Fatalf("gemini buckets: %+v", quotaResp.Groups)
	}

	xaiUpstream := quotaUpstream(t, "Bearer xai-token", `{"body": {"config": {"creditUsagePercent": 25, "currentPeriod": {"end": "2026-10-01"}, "productUsage": [{"product": "grok-code", "usagePercent": 50}]}}}`)
	stubBuiltinEndpoint(t, "xai", xaiUpstream)
	xaiAuth := &coreauth.Auth{ID: "xai-builtin", FileName: "xai.json", Provider: "xai", Metadata: map[string]any{"access_token": "xai-token", "email": "a@example.test"}}
	code, quotaResp, _ = fetchBuiltinForAuth(t, xaiAuth)
	if code != http.StatusOK {
		t.Fatalf("xai expected 200, got %d", code)
	}
	if len(quotaResp.Groups) != 1 || len(quotaResp.Groups[0].Buckets) != 2 {
		t.Fatalf("xai buckets: %+v", quotaResp.Groups)
	}
	if bucket := quotaResp.Groups[0].Buckets[0]; bucket.Window != "weekly" || bucket.RemainingFraction != 0.75 {
		t.Fatalf("xai weekly bucket: %+v", bucket)
	}
}

func TestFetchCredentialQuota_BuiltinUpstreamFailureSurfaces(t *testing.T) {
	upstream := quotaUpstream(t, "Bearer other-token", `{}`)
	stubBuiltinEndpoint(t, "claude", upstream)
	auth := &coreauth.Auth{ID: "claude-fail", FileName: "claude.json", Provider: "claude", Metadata: map[string]any{"access_token": "claude-token"}}
	code, _, observed := fetchBuiltinForAuth(t, auth)
	if code != http.StatusBadGateway {
		t.Fatalf("expected 502 on upstream 401, got %d", code)
	}
	if observed != nil {
		t.Fatal("observer fired on a failed fetch")
	}
}

func TestFetchCredentialQuota_UnknownProviderStillNotImplemented(t *testing.T) {
	auth := &coreauth.Auth{ID: "unknown-builtin", FileName: "unknown.json", Provider: "unknown", Metadata: map[string]any{"access_token": "token"}}
	code, _, _ := fetchBuiltinForAuth(t, auth)
	if code != http.StatusNotImplemented {
		t.Fatalf("expected 501 for unknown provider, got %d", code)
	}
}
