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

func TestUsagePersistenceObservesUnmodifiedQuotaHandlers(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "middleware-quota-fixture", FileName: "fixture.json", Provider: "codex", Metadata: map[string]any{"access_token": "fixture-token"}}
	index := auth.EnsureIndex()
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	store := persistenceTestStore(t)
	server := NewServer(cfg, manager, nil, filepath.Join(t.TempDir(), "config.yaml"), WithUsagePersistence(store), WithLocalManagementPassword("quota-test-key"))
	installUsageQuotaFixture(server)
	identityRequest := httptest.NewRequest(http.MethodGet, "/v0/management/stats/quota/identities", nil)
	identityRequest.RemoteAddr = "127.0.0.1:1234"
	identityRequest.Header.Set("Authorization", "Bearer quota-test-key")
	identityResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(identityResponse, identityRequest)
	if identityResponse.Code != http.StatusOK {
		t.Fatalf("prime quota identity evidence: %d %s", identityResponse.Code, identityResponse.Body)
	}
	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v0/management/"+path, strings.NewReader(`{"auth_index":"`+index+`"}`))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer quota-test-key")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body)
		}
		return response
	}
	response := request("quota/fetch")
	if !strings.Contains(response.Body.String(), `"remainingFraction":0.75`) {
		t.Fatalf("original quota contract changed: %s", response.Body)
	}
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.Quotas(context.Background())
	// This credential exposes no account property, so it is grouped by provider
	// alone and still recorded: an empty account fact is valid, not an error.
	if err != nil || len(snapshots) != 1 || snapshots[0].Provider != "codex" || snapshots[0].Account != "" || snapshots[0].AccountKind != "" || len(snapshots[0].Windows) != 1 || snapshots[0].Windows[0].RemainingPercent == nil || *snapshots[0].Windows[0].RemainingPercent != 75 {
		t.Fatalf("middleware did not persist original handler output: %#v %v", snapshots, err)
	}
	request("reset-quota")
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshots, err = store.Quotas(context.Background())
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("original reset response was not observed: %#v %v", snapshots, err)
	}
}
