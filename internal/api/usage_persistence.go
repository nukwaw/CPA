package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	usageweb "github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/web"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// WithServerConfigurator composes an extension hook after standard middleware
// and management handlers are ready, but before routes are registered.
func WithServerConfigurator(configure func(*Server)) ServerOption {
	return func(options *serverOptionConfig) {
		if configure == nil {
			return
		}
		previous := options.serverConfigurator
		options.serverConfigurator = func(server *Server) {
			if previous != nil {
				previous(server)
			}
			configure(server)
		}
	}
}

// WithUsagePersistence is the API-side adapter for the optional CLI add-on. It
// reuses live management authentication and configuration instead of copying
// credentials or introducing store-specific fields into Server.
func WithUsagePersistence(store *usagepersist.Store) ServerOption {
	if store == nil {
		return WithServerConfigurator(nil)
	}
	return WithUsagePersistenceProvider(func() *usagepersist.Store { return store })
}

// WithUsagePersistenceProvider registers routes once and resolves the optional
// store per request. current must be concurrency-safe; nil means initialization
// is pending, failed or stopped. No authentication state is copied or replaced.
func WithUsagePersistenceProvider(current func() *usagepersist.Store) ServerOption {
	if current == nil {
		current = func() *usagepersist.Store { return nil }
	}
	return WithServerConfigurator(func(server *Server) {
		source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return server.handlers.AuthManager })
		currentStore := func() *usagepersist.Store {
			store := current()
			if store != nil {
				store.BindQuotaIdentitySource(source)
			}
			return store
		}
		// The sidebar entry is independent of storage availability: the
		// dashboard itself reports an unavailable store.
		server.engine.Use(usagepersist.ManagementNavMiddleware(server.getConfig))
		server.engine.Use(func(c *gin.Context) {
			store := currentStore()
			if store == nil {
				c.Next()
				return
			}
			store.ManagementMiddleware()(c)
		})
		usagepersist.RegisterDynamicRoutes(usageManagementGroup(server), currentStore, func() bool {
			cfg := server.getConfig()
			return cfg != nil && cfg.UsageStatisticsEnabled
		})
		registerUsageAssets(server)
	})
}

// WithUsagePersistenceUnavailable keeps storage initialization failures visible
// without preventing inference startup. It intentionally accepts no raw error:
// database errors and filesystem paths can include private deployment details.
func WithUsagePersistenceUnavailable() ServerOption {
	return WithUsagePersistenceProvider(nil)
}

func usageManagementGroup(server *Server) *gin.RouterGroup {
	group := server.engine.Group("/v0/management/stats")
	group.Use(server.managementAvailabilityMiddleware(), server.mgmt.Middleware())
	return group
}

func registerUsageAssets(server *Server) {
	serveAsset := func(c *gin.Context, name string) {
		cfg := server.getConfig()
		if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		body, mime, ok := usageweb.Asset(name)
		if !ok {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Data(http.StatusOK, mime, body)
	}
	server.engine.GET("/stats.html", func(c *gin.Context) { serveAsset(c, "stats.html") })
	server.engine.GET(usageweb.AssetsPrefix+"/:name", func(c *gin.Context) { serveAsset(c, c.Param("name")) })
}
