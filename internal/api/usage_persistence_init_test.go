package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	usageweb "github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/web"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUsagePersistenceDynamicRoutesActivateWithoutReregistration(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{CommercialMode: true, UsageStatisticsEnabled: false}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	var current atomic.Pointer[usagepersist.Store]
	server := NewServer(cfg, nil, nil, filepath.Join(t.TempDir(), "config.yaml"),
		WithUsagePersistenceProvider(current.Load), WithLocalManagementPassword("dynamic-test-key"),
		WithServerConfigurator(func(server *Server) {
			// A plugin can own other stats paths and unclaimed methods. Broad Any
			// or wildcard fallback registration would conflict with these routes.
			group := usageManagementGroup(server)
			group.GET("/plugin-extension", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			group.POST("/status", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		}))
	routes := server.engine.Routes()
	request := func(method, path string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "127.0.0.1:1700"
		if authenticated {
			r.Header.Set("Authorization", "Bearer dynamic-test-key")
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "overview"}, {http.MethodGet, "analysis"}, {http.MethodGet, "events"},
		{http.MethodGet, "live"}, {http.MethodGet, "filters"}, {http.MethodGet, "pricing"},
		{http.MethodPut, "pricing"}, {http.MethodDelete, "pricing"}, {http.MethodPost, "pricing/sync"},
		{http.MethodGet, "quota"}, {http.MethodGet, "quota/cache"}, {http.MethodPut, "quota/cache"},
		{http.MethodGet, "status"},
	} {
		w := request(route.method, "/v0/management/stats/"+route.path, true)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"collection_enabled":false`) {
			t.Fatalf("pending %s %s: %d %s", route.method, route.path, w.Code, w.Body)
		}
	}
	if w := request(http.MethodGet, "/v0/management/stats/status", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("pending store bypassed management authentication: %d", w.Code)
	}
	for _, path := range []string{"/v0/management/stats", "/v0/management/stats/unknown/deep", "/v0/management/stats/status/extra"} {
		if w := request(http.MethodGet, path, true); w.Code != http.StatusNotFound {
			t.Fatalf("unowned route %s was swallowed: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/stats.html", usageweb.AssetsPrefix + "/stats.js", usageweb.AssetsPrefix + "/management-bridge.js"} {
		if w := request(http.MethodGet, path, false); w.Code != http.StatusOK {
			t.Fatalf("pending store hid canonical asset %s: %d", path, w.Code)
		}
	}
	store := persistenceTestStore(t)
	current.Store(store)
	for _, endpoint := range []string{"overview", "analysis", "events", "live", "filters", "pricing", "quota", "quota/cache", "status"} {
		if w := request(http.MethodGet, "/v0/management/stats/"+endpoint, true); w.Code != http.StatusOK {
			t.Fatalf("ready %s: %d %s", endpoint, w.Code, w.Body)
		}
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(request(http.MethodGet, "/v0/management/stats/status", true).Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"write_failures", "last_write_failure_at", "dropped_events", "last_drop_at", "queue_overflows", "validation_failures", "pending_events", "queue_capacity"} {
		if _, ok := status[field]; !ok {
			t.Fatalf("dynamic status dropped existing health counter %s", field)
		}
	}
	current.Store(nil)
	if w := request(http.MethodGet, "/v0/management/stats/status", true); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn store is still available: %d", w.Code)
	}
	afterRoutes := server.engine.Routes()
	if len(routes) != len(afterRoutes) {
		t.Fatal("store publication changed live router registration")
	}
	for i, route := range routes {
		after := afterRoutes[i]
		if route.Method != after.Method || route.Path != after.Path || route.Handler != after.Handler || reflect.ValueOf(route.HandlerFunc).Pointer() != reflect.ValueOf(after.HandlerFunc).Pointer() {
			t.Fatal("store publication changed live router registration")
		}
	}
	if w := request(http.MethodGet, "/v0/management/stats/plugin-extension", true); w.Code != http.StatusNoContent {
		t.Fatalf("unrelated plugin route was swallowed: %d", w.Code)
	}
	if w := request(http.MethodPost, "/v0/management/stats/status", true); w.Code != http.StatusNoContent {
		t.Fatalf("unowned method was swallowed: %d", w.Code)
	}
}

func TestUsagePersistenceDynamicMiddlewareActivatesAfterReady(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	// The shared fixture contains actual pinned declarations for the stores,
	// native generation helpers, and all verified provider selector sources.
	data, err := os.ReadFile(filepath.Join("testdata", "management-upstream.html"))
	if err != nil {
		t.Fatalf("read reduced management fixture: %v", err)
	}
	if _, recognized := usageweb.InjectManagementHTML(data); !recognized {
		t.Fatal("pinned reduced management fixture no longer satisfies strict recognition")
	}
	original := string(data)
	dir := t.TempDir()
	assetPath := filepath.Join(dir, "management.html")
	if err := os.WriteFile(assetPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANAGEMENT_STATIC_PATH", assetPath)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "dynamic-quota-fixture", FileName: "fixture.json", Provider: "codex", Metadata: map[string]any{"access_token": "fixture-token", "email": "observed@example.invalid"}}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	cfg.RemoteManagement.DisableAutoUpdatePanel = true
	var current atomic.Pointer[usagepersist.Store]
	server := NewServer(cfg, manager, nil, filepath.Join(dir, "config.yaml"),
		WithUsagePersistenceProvider(current.Load), WithLocalManagementPassword("dynamic-test-key"))
	installUsageQuotaFixture(server)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1700"
		r.Header.Set("Authorization", "Bearer dynamic-test-key")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w
	}
	store := persistenceTestStore(t)
	body := `{"auth_index":"` + index + `"}`
	beforeHTML := request(http.MethodGet, "/management.html", "")
	if beforeHTML.Body.String() != original || beforeHTML.Header().Get("X-CPA-Quota-Persistence") != "" {
		t.Fatal("pending initialization changed original management HTML")
	}
	beforeQuota := request(http.MethodPost, "/v0/management/quota/fetch", body)
	if snapshots, err := store.Quotas(context.Background()); err != nil || len(snapshots) != 0 {
		t.Fatalf("pending store observed management output: %#v %v", snapshots, err)
	}
	current.Store(store)
	afterHTML := request(http.MethodGet, "/management.html", "")
	if !strings.Contains(afterHTML.Body.String(), "CPAQuotaPersistence.attach") || afterHTML.Header().Get("X-CPA-Quota-Persistence") != "enabled" {
		t.Fatal("ready store did not activate the HTML bridge")
	}
	// The bridge primes immediate, sanitized request-start evidence through an
	// explicit add-on read; original handlers never query live manager locks.
	request(http.MethodGet, "/v0/management/stats/quota/identities", "")
	afterQuota := request(http.MethodPost, "/v0/management/quota/fetch", body)
	if !bytes.Equal(beforeQuota.Body.Bytes(), afterQuota.Body.Bytes()) {
		t.Fatal("dynamic observer changed original management quota response")
	}
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The credential's account fact, not its transient index, groups the stored
	// observation: the projection publishes the email the credential carries, and
	// the observation is keyed by (provider, account).
	snapshots, err := store.Quotas(context.Background())
	if err != nil || len(snapshots) != 1 || snapshots[0].Provider != "codex" || snapshots[0].Account != "observed@example.invalid" || snapshots[0].AccountKind != "email" {
		t.Fatalf("ready store did not activate account-scoped quota observation: %#v %v", snapshots, err)
	}
	if len(snapshots[0].Windows) != 1 || snapshots[0].Windows[0].RemainingPercent == nil || *snapshots[0].Windows[0].RemainingPercent != 75 {
		t.Fatalf("observed quota windows = %#v", snapshots[0].Windows)
	}
	// Rotating the access token is the ordinary refresh core performs during normal
	// operation. It must not move the account the observation is grouped under:
	// identity is (provider, account), never a token.
	auth.Metadata["access_token"] = "rotated-token"
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	request(http.MethodGet, "/v0/management/stats/quota/identities", "")
	request(http.MethodPost, "/v0/management/quota/fetch", body)
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.Quotas(context.Background())
	if err != nil || len(rotated) != 1 || rotated[0].Provider != "codex" || rotated[0].Account != "observed@example.invalid" || rotated[0].AccountKind != "email" {
		t.Fatalf("token rotation changed the account the observation is grouped under: %#v %v", rotated, err)
	}
	if len(rotated[0].Windows) != 1 || rotated[0].Windows[0].RemainingPercent == nil || *rotated[0].Windows[0].RemainingPercent != 75 {
		t.Fatalf("rotated observation windows = %#v", rotated[0].Windows)
	}
	current.Store(nil)
	if w := request(http.MethodGet, "/management.html", ""); w.Body.String() != original {
		t.Fatal("withdrawn store still changed management HTML")
	}
	if stored, err := os.ReadFile(assetPath); err != nil || string(stored) != original {
		t.Fatal("dynamic bridge modified original asset on disk")
	}
}
