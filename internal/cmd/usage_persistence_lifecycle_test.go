package cmd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
)

func waitUsageInitialization(t *testing.T, initializer *usagePersistenceInitializer) {
	t.Helper()
	select {
	case <-initializer.initialized:
	case <-time.After(5 * time.Second):
		t.Fatal("usage initializer did not finish")
	}
}

// The SQL driver holds the actual Open advisory-lock call, ignoring cancellation
// until explicitly released. Thus success cannot depend on a scheduling delay or
// on the driver turning a stall into an error. Only test failure guards use time.
func TestUsagePersistenceStalledDDLDoesNotBlockBuildRun(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "storage-canceled", "cancel-before-release"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("MANAGEMENT_PASSWORD", "")
			previousUsage, previousQueue := redisqueue.UsageStatisticsEnabled(), redisqueue.Enabled()
			t.Cleanup(func() { redisqueue.SetUsageStatisticsEnabled(previousUsage); redisqueue.SetEnabled(previousQueue) })
			gate := newUsageDDLGate()
			if outcome == "failure" {
				gate.failure = errors.New("private SQL initialization failure")
			} else if outcome == "storage-canceled" {
				gate.failure = context.Canceled
			}
			db := sql.OpenDB(usageDDLConnector{gate: gate})
			t.Cleanup(func() { _ = db.Close() })
			t.Cleanup(gate.release)
			selectUsageTestCore(t, sdkAuth.NewFileTokenStore())
			cfg := &config.Config{Host: "127.0.0.1", Port: 0, CommercialMode: true, AuthDir: t.TempDir(), UsageStatisticsEnabled: false}
			cfg.RemoteManagement.SecretKey = "configured-secret"
			cfg.RemoteManagement.DisableAutoUpdatePanel = true
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("usage-statistics-enabled: false\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			var server *api.Server
			builder := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(path).
				// Isolate storage startup from the unrelated core watcher/reload
				// lifecycle; the real service and listener still Build and Run.
				WithWatcherFactory(func(string, string, func(*config.Config)) (*cliproxy.WatcherWrapper, error) {
					return &cliproxy.WatcherWrapper{}, nil
				}).
				WithLocalManagementPassword("init-test-key").
				WithServerOptions(api.WithServerConfigurator(func(s *api.Server) { server = s })).
				WithHooks(cliproxy.Hooks{OnAfterStart: func(*cliproxy.Service) { close(started) }})
			storageCtx, cancelStorage := context.WithCancel(ctx)
			initializer := &usagePersistenceInitializer{ctx: storageCtx, cancel: cancelStorage, initialized: make(chan struct{})}
			builder.WithServerOptions(api.WithUsagePersistenceProvider(initializer.current))
			context.AfterFunc(storageCtx, initializer.stop)
			go initializer.initialize(func(ctx context.Context) (*usagepersist.Store, error) {
				return usagepersist.Open(ctx, usagepersist.Options{Database: db, Schema: "selected"})
			})
			defer initializer.stop()
			var initContext context.Context
			select {
			case initContext = <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("SQL advisory lock was not reached")
			}
			service, err := builder.Build()
			if err != nil {
				t.Fatalf("Build failed while optional DDL was stalled: %v", err)
			}
			done := make(chan error, 1)
			go func() {
				errRun := service.Run(ctx)
				initializer.stop()
				done <- errRun
			}()
			select {
			case <-started:
			case errRun := <-done:
				t.Fatalf("Run stopped before serving inference: %v", errRun)
			case <-time.After(5 * time.Second):
				t.Fatal("Run waited for optional DDL")
			}
			request := func(path string, authenticated bool, want int) {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.RemoteAddr = "127.0.0.1:1300"
				if authenticated {
					req.Header.Set("Authorization", "Bearer init-test-key")
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, req)
				if response.Code != want {
					t.Fatalf("%s status=%d want=%d body=%s", path, response.Code, want, response.Body)
				}
			}
			request("/v1/models", false, http.StatusOK)
			request("/v0/management/stats/status", true, http.StatusServiceUnavailable)
			request("/v0/management/stats/events", true, http.StatusServiceUnavailable)
			request("/v0/management/stats/status", false, http.StatusUnauthorized)
			request("/v0/management/usage-statistics-enabled", true, http.StatusOK)
			if outcome != "cancel-before-release" {
				gate.release()
				waitUsageInitialization(t, initializer)
				want := http.StatusOK
				if outcome != "success" {
					want = http.StatusServiceUnavailable
				}
				request("/v0/management/stats/status", true, want)
				request("/v0/management/stats/events", true, want)
				request("/v1/models", false, http.StatusOK)
				if ctx.Err() != nil {
					t.Fatal("optional storage outcome canceled inference")
				}
			}
			cancel()
			select {
			case <-initContext.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not reach optional SQL initialization")
			}
			select {
			case errRun := <-done:
				if errRun != nil && !errors.Is(errRun, context.Canceled) {
					t.Fatalf("Run failed: %v", errRun)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("application shutdown waited for stalled optional SQL")
			}
			if outcome == "cancel-before-release" {
				select {
				case <-initializer.initialized:
					t.Fatal("driver was expected to remain blocked after application exit")
				default:
				}
				gate.release()
				waitUsageInitialization(t, initializer)
			}
			if initializer.current() != nil {
				t.Fatal("storage reappeared after shutdown")
			}
			request("/v0/management/stats/status", true, http.StatusServiceUnavailable)
		})
	}
}

func TestUsagePersistenceLateSuccessfulOpenCannotResurrect(t *testing.T) {
	// Blocking Commit rather than Exec makes database/sql return successful Open
	// even after cancellation; its final context check already happened. This
	// exercises the late-store disposal path, not merely the error path.
	gate := newUsageDDLGate()
	gate.atCommit = true
	db := sql.OpenDB(usageDDLConnector{gate: gate})
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(gate.release)
	ctx, cancel := context.WithCancel(context.Background())
	initializer := &usagePersistenceInitializer{ctx: ctx, cancel: cancel, initialized: make(chan struct{})}
	defer initializer.stop()
	var opened *usagepersist.Store
	var openErr error
	go initializer.initialize(func(ctx context.Context) (*usagepersist.Store, error) {
		opened, openErr = usagepersist.Open(ctx, usagepersist.Options{Database: db, Schema: "selected"})
		return opened, openErr
	})
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not reach SQL commit")
	}
	stopped := make(chan struct{})
	go func() { initializer.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup waited for blocked Open")
	}
	gate.release()
	waitUsageInitialization(t, initializer)
	if openErr != nil || opened == nil {
		t.Fatalf("test must exercise late successful Open, got store=%v err=%v", opened, openErr)
	}
	if initializer.current() != nil || initializer.unsubscribe != nil || initializer.store != nil {
		t.Fatal("late successful Open published a store or attached an observer after stop")
	}
	if _, err := opened.Prices(context.Background()); !errors.Is(err, usagepersist.ErrClosed) {
		t.Fatalf("late store was not closed: %v", err)
	}
	if err := opened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("late-store cleanup closed borrowed core database: %v", err)
	}
}

// A blocked initializer does not need a real network or schema. The fake supports
// the SQL initialization contract and empty stats queries, with an explicit DDL
// or commit barrier that deliberately does not honor context cancellation.
type usageDDLGate struct {
	entered  chan context.Context
	released chan struct{}
	once     sync.Once
	atCommit bool
	failure  error
}

func newUsageDDLGate() *usageDDLGate {
	return &usageDDLGate{entered: make(chan context.Context, 1), released: make(chan struct{})}
}
func (g *usageDDLGate) release() { g.once.Do(func() { close(g.released) }) }
func (g *usageDDLGate) wait(ctx context.Context) error {
	g.entered <- ctx
	<-g.released
	return g.failure
}

type usageDDLConnector struct{ gate *usageDDLGate }

func (c usageDDLConnector) Connect(context.Context) (driver.Conn, error) {
	return &usageDDLConn{gate: c.gate}, nil
}
func (c usageDDLConnector) Driver() driver.Driver { return usageDDLDriver{} }

type usageDDLDriver struct{}

func (usageDDLDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not supported") }

type usageDDLConn struct{ gate *usageDDLGate }

func (c *usageDDLConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *usageDDLConn) Close() error                        { return nil }
func (c *usageDDLConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c *usageDDLConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	return usageDDLTx{gate: c.gate, ctx: ctx}, nil
}
func (c *usageDDLConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "pg_advisory_xact_lock") && !c.gate.atCommit {
		if err := c.gate.wait(ctx); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(0), nil
}
func (c *usageDDLConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "pg_namespace"):
		return &usageDDLRows{columns: []string{"nspname"}, values: [][]driver.Value{{"selected"}}}, nil
	case strings.Contains(query, "SELECT count(*)"):
		return &usageDDLRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	case strings.Contains(query, "SELECT key,value"):
		return &usageDDLRows{columns: []string{"key", "value"}}, nil
	case strings.Contains(query, "SELECT payload"):
		return &usageDDLRows{columns: []string{"payload"}}, nil
	default:
		return nil, errors.New("unexpected test query")
	}
}

type usageDDLTx struct {
	gate *usageDDLGate
	ctx  context.Context
}

func (tx usageDDLTx) Commit() error {
	if tx.gate.atCommit {
		return tx.gate.wait(tx.ctx)
	}
	return nil
}
func (tx usageDDLTx) Rollback() error { return nil }

type usageDDLRows struct {
	columns []string
	values  [][]driver.Value
}

func (r *usageDDLRows) Columns() []string { return r.columns }
func (r *usageDDLRows) Close() error      { return nil }
func (r *usageDDLRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}
