package api

import (
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
	for _, path := range []string{"/stats.html", usageweb.AssetsPrefix + "/stats.js", usageweb.AssetsPrefix + "/management-nav.js"} {
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
	// The shared fixture is a reduced management document; the navigation asset
	// does not recognize upstream builds, so it must accept any anchored HTML.
	data, err := os.ReadFile(filepath.Join("testdata", "management-upstream.html"))
	if err != nil {
		t.Fatalf("read reduced management fixture: %v", err)
	}
	if _, injected := usageweb.InjectManagementNav(data); !injected {
		t.Fatal("reduced management fixture has no navigation injection anchor")
	}
	original := string(data)
	dir := t.TempDir()
	assetPath := filepath.Join(dir, "management.html")
	if err := os.WriteFile(assetPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANAGEMENT_STATIC_PATH", assetPath)
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	cfg.RemoteManagement.DisableAutoUpdatePanel = true
	var current atomic.Pointer[usagepersist.Store]
	server := NewServer(cfg, nil, nil, filepath.Join(dir, "config.yaml"),
		WithUsagePersistenceProvider(current.Load), WithLocalManagementPassword("dynamic-test-key"))
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
	// The sidebar entry is independent of storage availability: the document is
	// augmented even while initialization is pending, and the dashboard itself
	// reports an unavailable store.
	beforeHTML := request(http.MethodGet, "/management.html", "")
	if !strings.Contains(beforeHTML.Body.String(), "data-cpa-stats-nav") || beforeHTML.Header().Get("X-CPA-Stats-Nav") != "enabled" {
		t.Fatal("pending initialization hid the navigation asset")
	}
	current.Store(persistenceTestStore(t))
	afterHTML := request(http.MethodGet, "/management.html", "")
	if !strings.Contains(afterHTML.Body.String(), "data-cpa-stats-nav") || afterHTML.Header().Get("X-CPA-Stats-Nav") != "enabled" {
		t.Fatal("ready store did not keep the navigation asset")
	}
	current.Store(nil)
	if w := request(http.MethodGet, "/management.html", ""); !strings.Contains(w.Body.String(), "data-cpa-stats-nav") {
		t.Fatal("withdrawn store hid the navigation asset")
	}
	if stored, err := os.ReadFile(assetPath); err != nil || string(stored) != original {
		t.Fatal("navigation injection modified original asset on disk")
	}
}
