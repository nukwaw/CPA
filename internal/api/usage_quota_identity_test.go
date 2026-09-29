package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The original quota handler receives a real manager credential and selects an
// existing plugin host. Only the external quota service is a local fixture.
type usageQuotaFixtureProvider struct{}

func (*usageQuotaFixtureProvider) Identifier() string { return "codex" }
func (*usageQuotaFixtureProvider) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"codex"}}, nil
}
func (*usageQuotaFixtureProvider) FetchQuota(_ context.Context, request pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	var response pluginapi.QuotaFetchResponse
	if request.Metadata["access_token"] != "fixture-token" {
		return response, nil
	}
	_ = json.Unmarshal([]byte(`{"groups":[{"displayName":"Fixture","buckets":[{"window":"daily","remainingFraction":0.75}]}]}`), &response)
	return response, nil
}
func (*usageQuotaFixtureProvider) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{Success: true}, nil
}
func installUsageQuotaFixture(server *Server) {
	host := pluginhost.New()
	host.RegisterPluginForTest("quota-fixture", pluginapi.Plugin{Metadata: pluginapi.Metadata{Name: "quota fixture"}, Capabilities: pluginapi.Capabilities{QuotaProvider: &usageQuotaFixtureProvider{}}})
	server.mgmt.SetPluginHost(host)
}

func TestUsageQuotaIdentityDiskReplacementAndRestart(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "same.json")
	write := func(token, account string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"type": "codex", "access_token": token, "account_id": account, "email": "same@example.invalid"})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("secret-A", "account-A")
	auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "codex", Attributes: map[string]string{coreauth.AttributePath: path, coreauth.AttributeSourceBackend: coreauth.AuthSourceFile}, Metadata: map[string]any{"type": "codex", "access_token": "secret-A", "account_id": "account-A", "email": "same@example.invalid"}}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: directory}
	source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager }, func() *config.Config { return cfg })
	original := source.QuotaBindings(true)
	if len(original) != 1 {
		t.Fatal("known OAuth file not bound")
	}
	write("secret-B", "account-A")
	if len(source.QuotaBindings(false)) != 1 {
		t.Fatal("request path performed disk reconciliation")
	}
	if len(source.QuotaBindings(true)) != 0 {
		t.Fatal("same name/index/email/copied-mtime replacement restored A quota")
	}
	auth.Metadata["access_token"] = "secret-B"
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	changed := source.QuotaBindings(true)
	if len(changed) != 1 || changed[0].CredentialGeneration == original[0].CredentialGeneration {
		t.Fatal("reconciled replacement not identified")
	}
	write("secret-B", "account-B")
	if len(source.QuotaBindings(true)) != 0 {
		t.Fatal("same-token account selector replacement trusted")
	}
	auth.Metadata["account_id"] = "account-B"
	_, _ = manager.Update(ctx, auth)
	current := source.QuotaBindings(true)
	if len(current) != 1 || current[0].CredentialGeneration == changed[0].CredentialGeneration {
		t.Fatal("account selector omitted from generation")
	}
	restartedManager := coreauth.NewManager(nil, nil, nil)
	_, _ = restartedManager.Register(ctx, auth)
	restarted := newUsageQuotaIdentitySource(func() *coreauth.Manager { return restartedManager }, func() *config.Config { return cfg }).QuotaBindings(true)
	if len(restarted) != 1 || restarted[0].CredentialGeneration != current[0].CredentialGeneration || restarted[0].Revision == current[0].Revision {
		t.Fatal("unchanged restart failed stable identity/process revision")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if len(source.QuotaBindings(true)) != 0 {
		t.Fatal("deleted file still eligible")
	}
}

func TestUsageQuotaIdentitiesAuthenticatedDynamicAndStalePUT(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "codex", Metadata: map[string]any{"access_token": "fixture-token", "account_id": "private-account"}}
	_, _ = manager.Register(context.Background(), auth)
	var current atomic.Pointer[usagepersist.Store]
	server := NewServer(cfg, manager, nil, filepath.Join(t.TempDir(), "config.yaml"), WithUsagePersistenceProvider(current.Load), WithLocalManagementPassword("identity-test-key"))
	request := func(method, path string, body []byte, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1700"
		r.Header.Set("Content-Type", "application/json")
		if authenticated {
			r.Header.Set("Authorization", "Bearer identity-test-key")
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	const endpoint = "/v0/management/stats/quota/identities"
	if response := request("GET", endpoint, nil, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("identity bypassed management auth: %d", response.Code)
	}
	if response := request("GET", endpoint, nil, true); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("pending source: %d", response.Code)
	}
	store := persistenceTestStore(t)
	current.Store(store)
	response := request("GET", endpoint, nil, true)
	if response.Code != http.StatusOK {
		t.Fatalf("ready source: %d %s", response.Code, response.Body)
	}
	var envelope struct {
		Bindings []usagepersist.QuotaBinding `json:"bindings"`
	}
	if json.Unmarshal(response.Body.Bytes(), &envelope) != nil || len(envelope.Bindings) != 1 {
		t.Fatalf("binding missing: %s", response.Body)
	}
	if strings.Contains(response.Body.String(), "fixture-token") || strings.Contains(response.Body.String(), "private-account") {
		t.Fatal("identity response leaked credential/account")
	}
	binding := envelope.Bindings[0]
	entry := usagepersist.QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, Revision: binding.Revision, ObservedAt: time.Now().UTC(), State: json.RawMessage(`{"status":"success","windows":[]}`)}
	payload, _ := json.Marshal(map[string]any{"entries": []usagepersist.QuotaCacheEntry{entry}})
	if response = request("PUT", "/v0/management/stats/quota/cache", payload, true); response.Code != 200 {
		t.Fatalf("valid PUT: %d %s", response.Code, response.Body)
	}
	auth.Metadata["access_token"] = "replacement-token"
	_, _ = manager.Update(context.Background(), auth)
	if response = request("PUT", "/v0/management/stats/quota/cache", payload, true); response.Code != 409 {
		t.Fatalf("stale PUT: %d %s", response.Code, response.Body)
	}
	if response = request("GET", "/v0/management/stats/quota/cache", nil, true); response.Code != 200 || !strings.Contains(response.Body.String(), `"entries":[]`) {
		t.Fatalf("stale GET: %d %s", response.Code, response.Body)
	}
	// F's alias paths must still belong to the original handlers, not the addon.
	if response = request("POST", "/v8/management/requests/api-call", []byte(`{}`), true); response.Code != 400 {
		t.Fatalf("API alias swallowed: %d %s", response.Code, response.Body)
	}
	current.Store(nil)
	if response = request("GET", endpoint, nil, true); response.Code != 503 {
		t.Fatal("withdrawn store still exposes identities")
	}
}
