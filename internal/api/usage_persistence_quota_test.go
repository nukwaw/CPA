package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// TestUsagePersistenceRecordsBuiltinQuotaRefresh proves the whole manual
// refresh path the dashboard depends on: the management quota handler fetches
// the provider's own endpoint through its builtin support, and the observer
// the adapter registered records the normalized result under the credential's
// account facts.
func TestUsagePersistenceRecordsBuiltinQuotaRefresh(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer kimi-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"limit":100,"remaining":25},"limits":[{"name":"short","title":"5h limit","detail":{"limit":10,"remaining":0}}]}`))
	}))
	defer upstream.Close()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "kimi-refresh-fixture",
		FileName:   "kimi-1.json",
		Provider:   "kimi",
		Metadata:   map[string]any{"access_token": "kimi-token", "device_id": "device-42"},
		Attributes: map[string]string{"base_url": upstream.URL},
	}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	store := persistenceTestStore(t)
	server := NewServer(cfg, manager, nil, filepath.Join(t.TempDir(), "config.yaml"), WithUsagePersistence(store), WithLocalManagementPassword("quota-test-key"))

	request := httptest.NewRequest(http.MethodPost, "/v0/management/quota/fetch", strings.NewReader(`{"auth_index":"`+index+`"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer quota-test-key")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("builtin quota fetch: %d %s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), `"remainingFraction":0.25`) {
		t.Fatalf("normalized response: %s", response.Body)
	}
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.Quotas(context.Background())
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("refresh was not recorded: %#v %v", snapshots, err)
	}
	snapshot := snapshots[0]
	if snapshot.Provider != "kimi" || snapshot.Account != "device-42" || snapshot.AccountKind != "device_id" {
		t.Fatalf("account facts: %+v", snapshot)
	}
	var overall *float64
	for _, window := range snapshot.Windows {
		if window.RemainingPercent != nil && *window.RemainingPercent == 25 {
			overall = window.RemainingPercent
		}
	}
	if overall == nil {
		t.Fatalf("refreshed windows: %+v", snapshot.Windows)
	}
}
