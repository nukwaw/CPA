package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestUsagePersistenceActualManagementResponse(t *testing.T) {
	fixture := os.Getenv("CPA_MANAGEMENT_FIXTURE")
	if fixture == "" {
		t.Skip("set CPA_MANAGEMENT_FIXTURE to a compiled upstream management bundle")
	}
	original, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "management.html")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANAGEMENT_STATIC_PATH", path)
	cfg := &config.Config{CommercialMode: true}
	cfg.RemoteManagement.DisableAutoUpdatePanel = true
	server := NewServer(cfg, nil, nil, filepath.Join(dir, "config.yaml"), WithUsagePersistence(persistenceTestStore(t)))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/management.html?safe-mode=configure", nil))
	if response.Code != 200 || response.Header().Get("X-CPA-Quota-Persistence") != "enabled" {
		t.Fatalf("real management response not adapted: status=%d header=%v", response.Code, response.Header())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("CPAQuotaPersistence.attach")) {
		t.Fatal("compiled module did not receive bridge binding")
	}
	if response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatal("injected response content length is incorrect")
	}
	if response.Header().Get("ETag") != "" || response.Header().Get("Last-Modified") != "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("injected response retained stale validators")
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(stored, original) {
		t.Fatal("upstream asset file was changed")
	}
	partial := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/management.html?safe-mode=configure", nil)
	request.Header.Set("Range", "bytes=0-9")
	server.Handler().ServeHTTP(partial, request)
	if partial.Code != http.StatusPartialContent || !bytes.Equal(partial.Body.Bytes(), original[:10]) {
		t.Fatalf("partial response changed: status=%d body=%q", partial.Code, partial.Body)
	}
}
