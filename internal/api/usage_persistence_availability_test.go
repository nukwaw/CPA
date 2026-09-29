package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type usageAPIAuthSaveGate struct {
	entered, release chan struct{}
	once, stop       sync.Once
}

func (gate *usageAPIAuthSaveGate) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (gate *usageAPIAuthSaveGate) Delete(context.Context, string) error           { return nil }
func (gate *usageAPIAuthSaveGate) Save(context.Context, *coreauth.Auth) (string, error) {
	gate.once.Do(func() { close(gate.entered) })
	<-gate.release
	return "", nil
}
func (gate *usageAPIAuthSaveGate) unblock() { gate.stop.Do(func() { close(gate.release) }) }

func TestUsagePersistenceFirstPerRequestBindAvoidsCoreStorageLock(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gate := &usageAPIAuthSaveGate{entered: make(chan struct{}), release: make(chan struct{})}
	defer gate.unblock()
	manager := coreauth.NewManager(gate, nil, nil)
	auth := &coreauth.Auth{ID: "availability.json", FileName: "availability.json", Provider: "claude", Metadata: map[string]any{"access_token": "private-test-token"}}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CommercialMode: true, AuthDir: t.TempDir()}
	cfg.RemoteManagement.DisableAutoUpdatePanel = true
	store := persistenceTestStore(t)
	server := NewServer(cfg, manager, nil, filepath.Join(t.TempDir(), "config.yaml"), WithUsagePersistence(store), WithServerConfigurator(func(server *Server) {
		server.engine.GET("/review-availability", func(c *gin.Context) { c.Data(http.StatusOK, "text/plain", []byte("original-response")) })
	}))
	marked := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: "claude", Model: "availability-model", Success: true})
		close(marked)
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("core Save did not hold the manager lock")
	}
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/review-availability", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("first per-request source bind joined the core I/O lock")
	}
	if response.Code != http.StatusOK || response.Body.String() != "original-response" {
		t.Fatalf("original response changed: %d %s", response.Code, response.Body)
	}
	gate.unblock()
	select {
	case <-marked:
	case <-time.After(5 * time.Second):
		t.Fatal("core result failed to leave its test barrier")
	}
}
