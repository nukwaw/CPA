package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// usagePersistenceOptions follows the already selected core storage resource.
// main registers that resource after resolving PGSTORE and the auth directory;
// this add-on must not reparse credentials or initialize a second database pool.
func usagePersistenceOptions(cfg *config.Config, configPath string) (usagepersist.Options, error) {
	if core, ok := sdkAuth.GetTokenStore().(interface{ UsageDatabase() (*sql.DB, string) }); ok {
		db, schema := core.UsageDatabase()
		if db == nil {
			return usagepersist.Options{}, errors.New("selected PostgreSQL store is unavailable")
		}
		return usagepersist.Options{Database: db, Schema: schema}, nil
	}
	root := util.WritablePath()
	if root == "" {
		authDir := ""
		if cfg != nil {
			authDir = cfg.AuthDir
		}
		var err error
		root, err = util.ResolveAuthDir(authDir)
		if err != nil {
			return usagepersist.Options{}, errors.New("usage state directory is unavailable")
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return usagepersist.Options{}, errors.New("usage state directory is unavailable")
	}
	// A config path identifies an instance, never a writable directory. This
	// permits read-only config mounts without mixing histories for other configs.
	identity, err := filepath.Abs(configPath)
	if err != nil {
		return usagepersist.Options{}, errors.New("usage instance identity is unavailable")
	}
	digest := sha256.Sum256([]byte(identity))
	return usagepersist.Options{DataDir: filepath.Join(root, "usage", fmt.Sprintf("%x", digest))}, nil
}

// attachUsagePersistence arranges the standalone CLI add-on before Build/Run,
// but never waits for optional storage I/O. Both stalled and failed initialization
// leave authenticated statistics unavailable without preventing inference startup.
func attachUsagePersistence(ctx context.Context, builder *cliproxy.Builder, cfg *config.Config, configPath string) (*usagePersistenceInitializer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg == nil || cfg.Home.Enabled {
		return nil, nil
	}
	storageCtx, cancelStorage := context.WithCancel(ctx)
	initializer := &usagePersistenceInitializer{ctx: storageCtx, cancel: cancelStorage, initialized: make(chan struct{})}
	builder.WithServerOptions(api.WithUsagePersistenceProvider(initializer.current))
	options, err := usagePersistenceOptions(cfg, configPath)
	context.AfterFunc(storageCtx, initializer.stop)
	go initializer.initialize(func(ctx context.Context) (*usagepersist.Store, error) {
		if err != nil {
			return nil, err
		}
		return usagepersist.Open(ctx, options)
	})
	return initializer, nil
}

// The initializer retains only lifecycle state and the published store; it never
// queues provider payloads or copies configuration/credentials. Publication and
// shutdown share one lock, so a late Open cannot resurrect the passive observer.
type usagePersistenceInitializer struct {
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	store       *usagepersist.Store
	unsubscribe func()
	stopped     bool
	stopOnce    sync.Once
	initialized chan struct{}
}

func (p *usagePersistenceInitializer) current() *usagepersist.Store {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || p.ctx.Err() != nil {
		return nil
	}
	return p.store
}

func (p *usagePersistenceInitializer) initialize(open func(context.Context) (*usagepersist.Store, error)) {
	defer close(p.initialized)
	store, err := open(p.ctx)
	if err != nil {
		if p.ctx.Err() == nil {
			// Never include driver/filesystem errors: they may contain DSN secrets
			// or private paths. An add-on error never cancels the proxy context.
			reason := "storage_unavailable"
			if errors.Is(err, usagepersist.ErrCapacity) {
				reason = "local_capacity"
			} else if errors.Is(err, usagepersist.ErrLocked) {
				reason = "journal_locked"
			}
			log.WithField("reason", reason).Error("usage persistence initialization failed; statistics unavailable; proxy service will continue")
		}
		return
	}
	p.mu.Lock()
	if p.stopped || p.ctx.Err() != nil {
		p.mu.Unlock()
		p.closeStore(store)
		return
	}
	p.unsubscribe = redisqueue.ObserveUsage(store.Consume)
	p.store = store
	p.mu.Unlock()
}

// stop cancels Open and withdraws publication without waiting for initialization.
// Even a driver that ignores cancellation can only finish into background cleanup.
func (p *usagePersistenceInitializer) stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		store, unsubscribe := p.store, p.unsubscribe
		p.store, p.unsubscribe = nil, nil
		p.cancel()
		p.mu.Unlock()
		if unsubscribe != nil {
			unsubscribe()
		}
		p.closeStore(store)
	})
}

func (p *usagePersistenceInitializer) closeStore(store *usagepersist.Store) {
	// The context is already canceled, including on Build/Run failure. Close
	// seals admission promptly and releases storage asynchronously if needed.
	if err := store.Close(p.ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Error("usage persistence store close failed")
	}
}
