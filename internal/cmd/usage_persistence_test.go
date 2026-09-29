package cmd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	corestore "github.com/router-for-me/CLIProxyAPI/v8/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func selectUsageTestCore(t *testing.T, selected coreauth.Store) {
	t.Helper()
	previous := sdkAuth.GetTokenStore()
	sdkAuth.RegisterTokenStore(selected)
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(previous) })
}

func TestUsagePersistenceCommandAdapterSkipsHome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "must-not-open")
	t.Setenv("WRITABLE_PATH", dir)
	selectUsageTestCore(t, &corestore.PostgresStore{})
	cfg := &config.Config{}
	cfg.Home.Enabled = true
	closeStore, err := attachUsagePersistence(context.Background(), cliproxy.NewBuilder(), cfg, "config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	closeStore.stop()
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Home unexpectedly opened standalone persistence: %v", err)
	}
}

func TestUsagePersistenceNativeRootAndConfigIdentity(t *testing.T) {
	selectUsageTestCore(t, sdkAuth.NewFileTokenStore())
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := &config.Config{AuthDir: filepath.Join(dir, "auths")}
	first, err := usagePersistenceOptions(cfg, "first.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.DataDir, filepath.Join(cfg.AuthDir, "usage")+string(os.PathSeparator)) {
		t.Fatalf("native state did not use resolved auth dir: %s", first.DataDir)
	}
	absolute, err := usagePersistenceOptions(cfg, filepath.Join(dir, "first.yaml"))
	if err != nil || absolute.DataDir != first.DataDir {
		t.Fatalf("relative/absolute identities differ: %#v %v", absolute, err)
	}
	second, err := usagePersistenceOptions(cfg, "second.yaml")
	if err != nil || second.DataDir == first.DataDir {
		t.Fatalf("selected configs share native state: %#v %v", second, err)
	}
	writable := filepath.Join(dir, "existing-writable")
	t.Setenv("WRITABLE_PATH", writable)
	options, err := usagePersistenceOptions(cfg, "first.yaml")
	if err != nil || !strings.HasPrefix(options.DataDir, filepath.Join(writable, "usage")+string(os.PathSeparator)) {
		t.Fatalf("WRITABLE_PATH not honored: %#v %v", options, err)
	}
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", writable)
	lower, err := usagePersistenceOptions(cfg, "first.yaml")
	if err != nil || lower.DataDir != options.DataDir {
		t.Fatalf("existing lowercase writable_path convention not honored: %#v %v", lower, err)
	}
}

func TestUsagePersistenceReadonlyConfigWithCollectionOff(t *testing.T) {
	selectUsageTestCore(t, sdkAuth.NewFileTokenStore())
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	configDir := filepath.Join(t.TempDir(), "readonly-config")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("usage-statistics-enabled: false\n"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configDir, 0700) })
	cfg := &config.Config{AuthDir: t.TempDir(), UsageStatisticsEnabled: false}
	builder := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(configPath)
	cleanup, err := attachUsagePersistence(context.Background(), builder, cfg, configPath)
	if err != nil {
		t.Fatalf("read-only config prevented initialization: %v", err)
	}
	defer cleanup.stop()
	if _, err = builder.Build(); err != nil {
		t.Fatalf("read-only config prevented otherwise valid service construction: %v", err)
	}
	options, err := usagePersistenceOptions(cfg, configPath)
	if err != nil {
		t.Fatal(err)
	}
	waitUsageInitialization(t, cleanup)
	if _, err = os.Stat(filepath.Join(options.DataDir, "usage.jsonl")); err != nil {
		t.Fatalf("history unavailable while collection disabled: %v", err)
	}
	entries, err := os.ReadDir(configDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("config location was used as writable root: %v %v", entries, err)
	}
	cleanup.stop()
	cleanup.stop()
}

func TestUsagePersistenceCommandAdapterNonfatalAndRedacted(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	oldHooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewLocal(log.StandardLogger())
	t.Cleanup(func() { log.StandardLogger().ReplaceHooks(oldHooks) })
	secret := "private-database-secret"
	db := sql.OpenDB(usageTestConnector{failure: errors.New("postgresql://user:" + secret + "@invalid")})
	t.Cleanup(func() { _ = db.Close() })
	selectUsageTestCore(t, &usageTestCore{FileTokenStore: sdkAuth.NewFileTokenStore(), db: db})
	cfg := &config.Config{AuthDir: t.TempDir()}
	builder := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
	cleanup, err := attachUsagePersistence(context.Background(), builder, cfg, "config.yaml")
	if err != nil || cleanup == nil {
		t.Fatalf("add-on failure blocked inference: %v", err)
	}
	defer cleanup.stop()
	if _, err = builder.Build(); err != nil {
		t.Fatalf("add-on failure invalidated service builder: %v", err)
	}
	waitUsageInitialization(t, cleanup)
	found := false
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, secret) {
			t.Fatal("initialization log leaked database credentials")
		}
		if strings.Contains(entry.Message, "usage persistence initialization failed") {
			found = true
		}
	}
	if !found {
		t.Fatal("add-on initialization failure was not explicitly logged")
	}
}

func TestUsagePersistenceUnwritableNativeStateStillStartsProxy(t *testing.T) {
	selectUsageTestCore(t, sdkAuth.NewFileTokenStore())
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WRITABLE_PATH", root)
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg := &config.Config{Host: "127.0.0.1", Port: 0, CommercialMode: true, AuthDir: t.TempDir(), UsageStatisticsEnabled: false}
	cfg.RemoteManagement.SecretKey = "configured-secret"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("usage-statistics-enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	started := make(chan *api.Server, 1)
	cancel, done := StartServiceBackgroundWithPluginHost(cfg, path, "local-key", nil,
		api.WithServerConfigurator(func(server *api.Server) { started <- server }))
	defer cancel()
	var server *api.Server
	select {
	case server = <-started:
	case <-done:
		t.Fatal("unwritable add-on state prevented proxy construction")
	case <-time.After(10 * time.Second):
		t.Fatal("proxy construction stalled after add-on failure")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("proxy cleanup stalled")
	}
	// The complete constructed handler remains usable after service shutdown,
	// avoiding concurrent reads while NewServer is still registering routes.
	request := httptest.NewRequest(http.MethodGet, "/v0/management/stats/status", nil)
	request.RemoteAddr = "127.0.0.1:1700"
	request.Header.Set("Authorization", "Bearer local-key")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed add-on did not expose authenticated unavailable status: %d %s", response.Code, response.Body)
	}
}

func TestUsagePersistenceAlreadyCanceledDoesNotInitialize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cleanup, err := attachUsagePersistence(ctx, cliproxy.NewBuilder(), &config.Config{}, "config.yaml")
	if !errors.Is(err, context.Canceled) || cleanup != nil {
		t.Fatalf("intentional cancellation was hidden: initializer=%v err=%v", cleanup, err)
	}
	cleanup.stop()
}

func TestUsagePersistencePGSTOREOnlySelection(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGSTORE_TEST_DSN to an isolated PostgreSQL test database")
	}
	ctx := context.Background()
	core, err := corestore.NewPostgresStore(ctx, corestore.PostgresStoreConfig{
		DSN: dsn, Schema: "usage_cmd_" + strings.ReplaceAll(uuid.NewString(), "-", ""), SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	db, schema := core.UsageDatabase()
	t.Cleanup(func() {
		if _, errDrop := db.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); errDrop != nil {
			t.Error(errDrop)
		}
		if errClose := core.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	if err = core.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	selectUsageTestCore(t, core)
	// Changing the original environment after initialization must not change the
	// selected pool/schema/local path, and no native state should be opened.
	t.Setenv("PGSTORE_DSN", "not-a-valid-dsn")
	t.Setenv("PGSTORE_SCHEMA", "wrong_schema")
	t.Setenv("PGSTORE_LOCAL_PATH", filepath.Join(t.TempDir(), "wrong-local"))
	root := filepath.Join(t.TempDir(), "native-not-selected")
	t.Setenv("WRITABLE_PATH", root)
	options, err := usagePersistenceOptions(&config.Config{}, core.ConfigPath())
	if err != nil || options.Database != db || options.Schema != schema || options.DataDir != "" {
		t.Fatalf("selected core PGSTORE not reused: %#v %v", options, err)
	}
	store, err := usagepersist.Open(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.PingContext(ctx); err != nil {
		t.Fatalf("usage close closed core PGSTORE: %v", err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("PGSTORE selection unexpectedly created native state: %v", err)
	}
}

// A tiny driver makes blocking/failing addon initialization deterministic. It
// cannot perform any storage operations and is only exposed by the test core.
type usageTestCore struct {
	*sdkAuth.FileTokenStore
	db *sql.DB
}

func (s *usageTestCore) UsageDatabase() (*sql.DB, string) { return s.db, "selected" }

type usageTestConnector struct {
	started chan context.Context
	failure error
}

func (c usageTestConnector) Connect(context.Context) (driver.Conn, error) { return c, nil }
func (c usageTestConnector) Driver() driver.Driver                        { return usageTestDriver{} }
func (c usageTestConnector) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not supported")
}
func (c usageTestConnector) Close() error              { return nil }
func (c usageTestConnector) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }
func (c usageTestConnector) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if c.failure != nil {
		return nil, c.failure
	}
	c.started <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

type usageTestDriver struct{}

func (usageTestDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not supported") }
