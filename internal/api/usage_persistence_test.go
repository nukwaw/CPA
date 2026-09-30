package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func persistenceTestStore(t *testing.T) *usagepersist.Store {
	t.Helper()
	store, err := usagepersist.Open(context.Background(), usagepersist.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestUsagePersistenceReusesManagementAccessControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "")
	store := persistenceTestStore(t)
	for _, test := range []struct {
		name              string
		secret, home, key bool
		want              int
	}{
		{"authorized", true, false, true, 200}, {"unauthorized", true, false, false, 401},
		{"management unavailable", false, false, true, 404}, {"home", true, true, true, 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{CommercialMode: true}
			cfg.Home.Enabled = test.home
			if test.secret {
				cfg.RemoteManagement.SecretKey = "configured-secret"
			}
			options := []ServerOption{WithUsagePersistence(store)}
			if test.secret {
				options = append(options, WithLocalManagementPassword("persistence-test-key"))
			}
			server := NewServer(cfg, nil, nil, filepath.Join(t.TempDir(), "config.yaml"), options...)
			for _, endpoint := range []string{"events", "analysis", "pricing", "quota", "quota/cache", "status"} {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "/v0/management/stats/"+endpoint, nil)
				request.RemoteAddr = "127.0.0.1:1200"
				if test.key {
					request.Header.Set("Authorization", "Bearer persistence-test-key")
				}
				server.Handler().ServeHTTP(response, request)
				if response.Code != test.want && !(test.want == 401 && response.Code == 403) {
					t.Fatalf("%s status=%d body=%s", endpoint, response.Code, response.Body)
				}
			}
		})
	}
}

func TestUsagePersistenceConfiguratorComposesAndKeepsCoreOptional(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	var calls []int
	cfg := &config.Config{CommercialMode: true}
	store := persistenceTestStore(t)
	server := NewServer(cfg, nil, nil, filepath.Join(t.TempDir(), "config.yaml"),
		WithServerConfigurator(func(*Server) { calls = append(calls, 1) }), WithUsagePersistence(store),
		WithServerConfigurator(func(*Server) { calls = append(calls, 2) }))
	if !reflect.DeepEqual(calls, []int{1, 2}) {
		t.Fatalf("configurators replaced rather than composed: %v", calls)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats.html", nil))
	if response.Code != 200 || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("extension asset: %d", response.Code)
	}
	plain := NewServer(&config.Config{CommercialMode: true}, nil, nil, filepath.Join(t.TempDir(), "config.yaml"))
	response = httptest.NewRecorder()
	plain.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats.html", nil))
	if response.Code != 404 {
		t.Fatal("persistence was implicitly installed into the SDK/API server")
	}
	cfg = cfg.CloneForRuntime()
	cfg.RemoteManagement.DisableControlPanel = true
	server.UpdateClients(cfg)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats.html", nil))
	if response.Code != 404 {
		t.Fatal("extension did not observe live panel configuration")
	}
}

type persistenceDeliveryBarrier struct {
	prefix string
	done   chan string
}

func (b *persistenceDeliveryBarrier) HandleUsage(_ context.Context, record usage.Record) {
	if strings.HasPrefix(record.RequestID, b.prefix) {
		b.done <- record.RequestID
	}
}

func TestUsagePersistenceConsumesBuiltinOutputAndExistingHotReload(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	previousUsage, previousQueue := redisqueue.UsageStatisticsEnabled(), redisqueue.Enabled()
	t.Cleanup(func() { redisqueue.SetUsageStatisticsEnabled(previousUsage); redisqueue.SetEnabled(previousQueue) })
	store := persistenceTestStore(t)
	stop := redisqueue.ObserveUsage(func(ctx context.Context, payload []byte) {
		var event struct {
			ID string `json:"execution_id"`
		}
		if json.Unmarshal(payload, &event) == nil && strings.HasPrefix(event.ID, t.Name()) {
			store.Consume(ctx, payload)
		}
	})
	t.Cleanup(stop)
	barrier := &persistenceDeliveryBarrier{prefix: t.Name(), done: make(chan string, 8)}
	usage.RegisterNamedPlugin(t.Name(), barrier)
	t.Cleanup(func() {
		usage.RegisterNamedPlugin(t.Name(), &persistenceDeliveryBarrier{prefix: "no-match", done: make(chan string, 1)})
	})
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("usage-statistics-enabled: false\nremote-management:\n  secret-key: configured-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	// The normal command entry point initializes this existing provider switch.
	redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	reloaded := make(chan bool, 1)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "sample-credential", FileName: "sample.json", Index: "sample-account", Provider: "claude", Metadata: map[string]any{"access_token": "sample-token"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	var server *Server
	server = NewServer(cfg, manager, nil, path, WithUsagePersistence(store), WithLocalManagementPassword("persistence-test-key"),
		WithConfigReloadHook(func(ctx context.Context, next *config.Config) { reloaded <- server.UpdateClientsContext(ctx, next) }))
	request := func(method, endpoint, body string) []byte {
		t.Helper()
		r := httptest.NewRequest(method, "/v0/management/"+endpoint, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1400"
		r.Header.Set("Authorization", "Bearer persistence-test-key")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, endpoint, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}
	toggle := func(enabled bool) {
		t.Helper()
		request(http.MethodPut, "usage-statistics-enabled", `{"value":`+strconv.FormatBool(enabled)+`}`)
		select {
		case ok := <-reloaded:
			if !ok {
				t.Fatal("reload failed")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("reload did not run")
		}
		if redisqueue.UsageStatisticsEnabled() != enabled {
			t.Fatal("existing setting did not reload")
		}
	}
	publish := func(suffix string) {
		t.Helper()
		id := t.Name() + suffix
		// The producer stamps the account facts where the live credential exists;
		// the token is not identity and never groups persisted usage.
		usage.PublishRecord(context.Background(), usage.Record{RequestID: id, RequestedAt: time.Now().Add(-time.Second), Provider: "claude", Model: "sample-model", AuthIndex: "sample-account", Account: "sample@example.invalid", AccountKind: "email", AccessTokenSHA256: coreauth.AccessTokenSHA256(auth), Detail: usage.Detail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}, ResponseHeaders: http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.24"}}})
		// A separate existing SDK plugin is only a test barrier. It runs after
		// the built-in provider has applied its unchanged gate and emitted output.
		select {
		case got := <-barrier.done:
			if got != id {
				t.Fatalf("unexpected barrier %s", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SDK dispatch timed out")
		}
		// Provider dispatch only admits sanitized work; persistence runs independently.
		if err := store.Flush(context.Background()); err != nil {
			t.Fatalf("persistence completion: %v", err)
		}
	}
	total := func() int64 {
		t.Helper()
		var page usagepersist.EventPage
		if err := json.Unmarshal(request(http.MethodGet, "stats/events", ""), &page); err != nil {
			t.Fatal(err)
		}
		return page.Total
	}
	publish("-off")
	if total() != 0 {
		t.Fatal("consumer bypassed disabled built-in provider")
	}
	toggle(true)
	publish("-on")
	if total() != 1 {
		t.Fatal("enabled provider output was not persisted")
	}
	if len(redisqueue.PopOldest(100)) == 0 {
		t.Fatal("persistence stole the original usage queue")
	}
	toggle(false)
	publish("-off-again")
	request(http.MethodPut, "stats/pricing", `{"model":"sample-model","input_per_million":2,"output_per_million":4,"cache_read_per_million":1,"cache_write_per_million":3}`)
	if total() != 1 {
		t.Fatal("disabled collection erased history or admitted records")
	}
	if !strings.Contains(string(request(http.MethodGet, "stats/quota", "")), `"used_percent":24`) {
		t.Fatal("persisted quota history unavailable")
	}
	toggle(true)
	publish("-resumed")
	if total() != 2 {
		t.Fatal("collection did not resume through the original provider")
	}
}
