package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// Identity is projected from the live credential: a codex credential is grouped
// by its email account fact, and a credential that exposes no account property is
// still projected with empty account facts rather than dropped.
func TestUsageQuotaIdentityProjectsAccountFacts(t *testing.T) {
	ctx := context.Background()
	auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": "secret-A", "account_id": "account-A", "email": "same@example.invalid"}}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager })
	original := source.QuotaBindings()
	if len(original) != 1 {
		t.Fatal("live credential not projected")
	}
	if original[0].Provider != "codex" || original[0].Account != "same@example.invalid" || original[0].AccountKind != "email" {
		t.Fatalf("projected facts = %+v", original[0])
	}
	// Rotating the access token is the ordinary refresh core performs; it must not
	// change the account the credential is grouped under.
	delete(auth.Metadata, "access_token")
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	rotated := source.QuotaBindings()
	if len(rotated) != 1 || rotated[0].Account != original[0].Account || rotated[0].AccountKind != original[0].AccountKind || rotated[0].Key != original[0].Key {
		t.Fatal("token rotation changed durable identity")
	}
	// An account selector is not the account property: it never takes part.
	auth.Metadata["account_id"] = "account-B"
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if changed := source.QuotaBindings(); len(changed) != 1 || changed[0].Account != original[0].Account {
		t.Fatal("account selector was treated as identity")
	}
	auth.Metadata["email"] = "other@example.invalid"
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if moved := source.QuotaBindings(); len(moved) != 1 || moved[0].Account != "other@example.invalid" {
		t.Fatal("changed account property was not projected")
	}
	manager.Remove(ctx, auth.ID)
	if len(source.QuotaBindings()) != 0 {
		t.Fatal("removed credential still eligible")
	}
	// A credential with no account property is valid and grouped by provider alone.
	bare := &coreauth.Auth{ID: "bare.json", FileName: "bare.json", Provider: "codex", Metadata: map[string]any{"type": "codex", "access_token": "secret-C"}}
	if _, err := manager.Register(ctx, bare); err != nil {
		t.Fatal(err)
	}
	empty := source.QuotaBindings()
	if len(empty) != 1 || empty[0].Provider != "codex" || empty[0].Account != "" || empty[0].AccountKind != "" {
		t.Fatalf("credential without an account property = %+v", empty)
	}
}

// Kimi identifies its account by device_id, not by email: the projection must use
// device_id and must keep it stable while the access token rotates.
func TestUsageQuotaIdentityKimiAccountIsDeviceID(t *testing.T) {
	ctx := context.Background()
	auth := &coreauth.Auth{ID: "kimi.json", FileName: "kimi.json", Provider: "kimi", Attributes: map[string]string{
		coreauth.AttributePath:          filepath.Join(t.TempDir(), "kimi.json"),
		coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
		"auth_kind":                     "oauth",
	}, Metadata: map[string]any{"type": "kimi", "access_token": "kimi-token", "refresh_token": "rt-kimi", "email": "kimi@example.invalid", "device_id": "device-a"}}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager })
	bindings := source.QuotaBindings()
	if len(bindings) != 1 || bindings[0].Account != "device-a" || bindings[0].AccountKind != "device_id" {
		t.Fatalf("kimi account facts = %+v", bindings)
	}
	// Kimi issues no email, so the email value must not become the account.
	if bindings[0].Account == "kimi@example.invalid" {
		t.Fatal("kimi identity fell back to email")
	}
	auth.Metadata["access_token"] = "other-token"
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	rotated := source.QuotaBindings()
	if len(rotated) != 1 || rotated[0].Account != "device-a" || rotated[0].AccountKind != "device_id" {
		t.Fatal("kimi token rotation changed durable identity")
	}
}

// Stale-write protection is observation-time ordering only: there is no credential
// generation or revision fence, so an older observation must never overwrite newer
// stored state, while a newer observation replaces it.
func TestUsageQuotaIdentitiesAuthenticatedDynamicAndStalePUT(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "codex", Metadata: map[string]any{"access_token": "fixture-token", "account_id": "private-account", "email": "first@example.invalid"}}
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
	// The transient auth_index is published for in-page correlation, but the raw
	// credential token and the unrelated account selector are never published.
	if strings.Contains(response.Body.String(), "fixture-token") || strings.Contains(response.Body.String(), "private-account") {
		t.Fatal("identity response leaked credential or unrelated selector")
	}
	// The account fact IS published deliberately: it is the durable identity.
	if !strings.Contains(response.Body.String(), "first@example.invalid") {
		t.Fatalf("identity response withheld the account fact: %s", response.Body)
	}
	binding := envelope.Bindings[0]
	if binding.Account != "first@example.invalid" || binding.AccountKind != "email" {
		t.Fatalf("published account facts = %+v", binding)
	}
	// Rotating the access token must not change the account a stored observation
	// belongs to, and the identity endpoint must keep publishing that account.
	auth.Metadata["access_token"] = "replacement-token"
	_, _ = manager.Update(context.Background(), auth)
	if response = request("GET", endpoint, nil, true); response.Code != 200 || !strings.Contains(response.Body.String(), "first@example.invalid") {
		t.Fatalf("identity after token rotation: %d %s", response.Code, response.Body)
	}
	// Rotation must not re-key stored state either.
	newer := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	entry := usagepersist.QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, Account: binding.Account, AccountKind: binding.AccountKind, ObservedAt: newer, State: json.RawMessage(`{"status":"success","windows":[]}`)}
	payload, _ := json.Marshal(map[string]any{"entries": []usagepersist.QuotaCacheEntry{entry}})
	if response = request("PUT", "/v0/management/stats/quota/cache", payload, true); response.Code != 200 {
		t.Fatalf("valid PUT: %d %s", response.Code, response.Body)
	}
	// An older observation for the same key must not overwrite the newer one.
	stale := entry
	stale.ObservedAt = newer.Add(-time.Hour)
	stalePayload, _ := json.Marshal(map[string]any{"entries": []usagepersist.QuotaCacheEntry{stale}})
	if response = request("PUT", "/v0/management/stats/quota/cache", stalePayload, true); response.Code != 200 {
		t.Fatalf("stale PUT: %d %s", response.Code, response.Body)
	}
	entries, err := store.QuotaCache(context.Background())
	if err != nil || len(entries) != 1 || !entries[0].ObservedAt.Equal(newer) || entries[0].Account != "first@example.invalid" {
		t.Fatalf("stale observation overwrote newer state: %#v %v", entries, err)
	}
	// A newer observation for the same account does replace it.
	entry.ObservedAt = newer.Add(time.Minute)
	newerPayload, _ := json.Marshal(map[string]any{"entries": []usagepersist.QuotaCacheEntry{entry}})
	if response = request("PUT", "/v0/management/stats/quota/cache", newerPayload, true); response.Code != 200 {
		t.Fatalf("newer PUT: %d %s", response.Code, response.Body)
	}
	entries, err = store.QuotaCache(context.Background())
	if err != nil || len(entries) != 1 || !entries[0].ObservedAt.Equal(entry.ObservedAt) {
		t.Fatalf("newer observation did not replace older state: %#v %v", entries, err)
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
