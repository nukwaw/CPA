package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	usageweb "github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/web"
	"golang.org/x/crypto/bcrypt"
)

func TestUsagePersistenceUnavailableReusesManagementAccessControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", "")
	for _, test := range []struct {
		name              string
		secret, home, key bool
		want              int
	}{
		{"authorized", true, false, true, http.StatusServiceUnavailable},
		{"unauthorized", true, false, false, http.StatusUnauthorized},
		{"management unavailable", false, false, true, http.StatusNotFound},
		{"home", true, true, true, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{CommercialMode: true}
			cfg.Home.Enabled = test.home
			options := []ServerOption{WithUsagePersistenceUnavailable()}
			if test.secret {
				cfg.RemoteManagement.SecretKey = "configured-secret"
				options = append(options, WithLocalManagementPassword("usage-unavailable-key"))
			}
			server := NewServer(cfg, nil, nil, filepath.Join(t.TempDir(), "config.yaml"), options...)
			for _, endpoint := range []string{"events", "analysis", "pricing", "quota", "quota/cache", "status"} {
				request := httptest.NewRequest(http.MethodGet, "/v0/management/stats/"+endpoint, nil)
				request.RemoteAddr = "127.0.0.1:1200"
				if test.key {
					request.Header.Set("Authorization", "Bearer usage-unavailable-key")
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if response.Code != test.want && !(test.want == http.StatusUnauthorized && response.Code == http.StatusForbidden) {
					t.Fatalf("%s status=%d body=%s", endpoint, response.Code, response.Body)
				}
				if response.Code == http.StatusServiceUnavailable {
					var body struct {
						Available         bool   `json:"available"`
						Storage           string `json:"storage"`
						CollectionEnabled bool   `json:"collection_enabled"`
						Error             string `json:"error"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if body.Available || body.Storage != "unavailable" || body.CollectionEnabled || body.Error == "" {
						t.Fatalf("unclear unavailable response: %s", response.Body)
					}
					if response.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("unavailable state must not be cached")
					}
				} else if strings.Contains(response.Body.String(), "storage") {
					t.Fatal("unauthenticated request received persistence status")
				}
			}
		})
	}
}

func TestUsagePersistenceUnavailableAssetsAndLiveConfig(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	previousUsage, previousQueue := redisqueue.UsageStatisticsEnabled(), redisqueue.Enabled()
	t.Cleanup(func() { redisqueue.SetUsageStatisticsEnabled(previousUsage); redisqueue.SetEnabled(previousQueue) })
	hash := func(password string) string {
		t.Helper()
		encoded, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	cfg := &config.Config{CommercialMode: true}
	cfg.RemoteManagement.SecretKey = hash("old-key")
	server := NewServer(cfg, nil, nil, filepath.Join(t.TempDir(), "config.yaml"), WithUsagePersistenceUnavailable())
	request := func(path, password string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "127.0.0.1:1500"
		if password != "" {
			r.Header.Set("Authorization", "Bearer "+password)
		}
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	for _, asset := range []string{"/stats.html", usageweb.AssetsPrefix + "/stats.css", usageweb.AssetsPrefix + "/stats.js", usageweb.AssetsPrefix + "/management-nav.js"} {
		if w := request(asset, ""); w.Code != http.StatusOK {
			t.Fatalf("disabled collection/unavailable storage hid asset %s: %d", asset, w.Code)
		}
	}
	if w := request("/telemetry/stats.js", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unexpected legacy asset alias: %d", w.Code)
	}
	if w := request("/v0/management/stats/status", "old-key"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"collection_enabled":false`) {
		t.Fatalf("initial status: %d %s", w.Code, w.Body)
	}
	cfg = cfg.CloneForRuntime()
	cfg.RemoteManagement.SecretKey = hash("new-key")
	cfg.UsageStatisticsEnabled = true
	cfg.RemoteManagement.DisableControlPanel = true
	server.UpdateClients(cfg)
	if w := request("/v0/management/stats/status", "old-key"); w.Code != http.StatusUnauthorized {
		t.Fatalf("unavailable route retained stale management secret: %d", w.Code)
	}
	if w := request("/v0/management/stats/status", "new-key"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"collection_enabled":true`) {
		t.Fatalf("live status: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/stats.html", usageweb.AssetsPrefix + "/stats.js"} {
		if w := request(path, ""); w.Code != http.StatusNotFound {
			t.Fatalf("asset ignored live panel configuration: %d", w.Code)
		}
	}
	cfg = cfg.CloneForRuntime()
	cfg.Home.Enabled = true
	server.UpdateClients(cfg)
	if w := request("/v0/management/stats/status", "new-key"); w.Code != http.StatusNotFound {
		t.Fatalf("unavailable route ignored live Home mode: %d", w.Code)
	}
}
