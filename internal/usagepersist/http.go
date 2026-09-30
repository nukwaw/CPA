package usagepersist

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	log "github.com/sirupsen/logrus"
)

// RegisterRoutes registers relative routes on an ALREADY AUTHENTICATED management
// group (normally /v0/management/stats). This method does not bypass management auth.
func (s *Store) RegisterRoutes(group *gin.RouterGroup) {
	if s == nil {
		return
	}
	RegisterDynamicRoutes(group, func() *Store { return s }, redisqueue.UsageStatisticsEnabled)
}

// RegisterDynamicRoutes registers exactly the existing statistics routes on an
// already authenticated group. It resolves a store for each request, allowing
// optional initialization to finish without mutating a live router. A nil store
// reports unavailable; collectionEnabled is consulted only for that response.
func RegisterDynamicRoutes(group *gin.RouterGroup, current func() *Store, collectionEnabled func() bool) {
	if group == nil {
		return
	}
	register := func(method, path string, handler func(*Store, *gin.Context)) {
		group.Handle(method, path, func(c *gin.Context) {
			var store *Store
			if current != nil {
				store = current()
			}
			if store == nil {
				enabled := collectionEnabled != nil && collectionEnabled()
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"available": false, "storage": "unavailable", "collection_enabled": enabled,
					"error": "usage persistence unavailable; check server logs and core storage configuration",
				})
				return
			}
			handler(store, c)
		})
	}
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	})
	register(http.MethodGet, "/overview", (*Store).analysisHTTP)
	register(http.MethodGet, "/analysis", (*Store).analysisHTTP)
	register(http.MethodGet, "/events", (*Store).eventsHTTP)
	register(http.MethodGet, "/live", (*Store).eventsHTTP)
	register(http.MethodGet, "/filters", func(s *Store, c *gin.Context) {
		f, err := parseFilter(c)
		if err != nil {
			badRequest(c, err)
			return
		}
		values, err := s.Filters(c.Request.Context(), f)
		respond(c, values, err)
	})
	register(http.MethodGet, "/pricing", func(s *Store, c *gin.Context) {
		prices, err := s.Prices(c.Request.Context())
		respond(c, gin.H{"prices": prices, "currency": "USD", "sync_source": "models.dev", "source_url": PricingSourceURL}, err)
	})
	register(http.MethodPut, "/pricing", func(s *Store, c *gin.Context) {
		var input struct {
			Model      string   `json:"model"`
			Input      *float64 `json:"input_per_million"`
			Output     *float64 `json:"output_per_million"`
			CacheRead  *float64 `json:"cache_read_per_million"`
			CacheWrite *float64 `json:"cache_write_per_million"`
		}
		if err := decodeRequest(c, &input, 16<<10); err != nil {
			badRequest(c, err)
			return
		}
		if input.Input == nil || input.Output == nil || input.CacheRead == nil || input.CacheWrite == nil {
			badRequest(c, errors.New("all four rates are required; zero must be explicit"))
			return
		}
		price := Price{Model: input.Model, InputPerMillion: *input.Input, OutputPerMillion: *input.Output, CacheReadPerMillion: *input.CacheRead, CacheWritePerMillion: *input.CacheWrite}
		if err := validPrice(price); err != nil {
			badRequest(c, err)
			return
		}
		result, err := s.SetPrice(c.Request.Context(), price)
		respond(c, result, err)
	})
	register(http.MethodDelete, "/pricing", func(s *Store, c *gin.Context) {
		model := strings.TrimSpace(c.Query("model"))
		if model == "" {
			badRequest(c, errors.New("model is required"))
			return
		}
		err := s.ResetPrice(c.Request.Context(), model)
		respond(c, gin.H{"ok": true}, err)
	})
	register(http.MethodPost, "/pricing/sync", func(s *Store, c *gin.Context) {
		result, err := s.SyncPricing(c.Request.Context())
		if err != nil {
			log.Warn("usage persistence: pricing sync failed")
			c.JSON(http.StatusBadGateway, gin.H{"error": "pricing sync failed; stored prices are unchanged"})
			return
		}
		c.JSON(http.StatusOK, result)
	})
	register(http.MethodGet, "/quota", (*Store).quotaHTTP)
	register(http.MethodGet, "/quota/summary", (*Store).quotaSummaryHTTP)
	register(http.MethodGet, "/quota/identities", (*Store).quotaIdentitiesHTTP)
	register(http.MethodGet, "/quota/cache", (*Store).quotaCacheGetHTTP)
	register(http.MethodPut, "/quota/cache", (*Store).quotaCachePutHTTP)
	register(http.MethodGet, "/status", func(s *Store, c *gin.Context) {
		backend := "file"
		if _, ok := s.store.(*postgresStore); ok {
			backend = "postgres"
		}
		var last any
		if timestamp := s.lastWriteFailure.Load(); timestamp > 0 {
			last = time.Unix(0, timestamp).UTC()
		}
		var lastDrop any
		if timestamp := s.lastDrop.Load(); timestamp > 0 {
			lastDrop = time.Unix(0, timestamp).UTC()
		}
		status := gin.H{
			"collection_enabled": redisqueue.UsageStatisticsEnabled(), "storage": backend,
			"write_failures": s.writeFailures.Load(), "last_write_failure_at": last,
			"dropped_events": s.droppedEvents.Load(), "last_drop_at": lastDrop,
			"queue_overflows": s.queueOverflows.Load(), "validation_failures": s.validationFailures.Load(),
			"pending_events": s.pendingEvents.Load(), "queue_capacity": usageQueueCapacity,
			"quota_header_scope": "header samples are attributed by provider and account (an email, or a device id for Kimi); a credential with no account property is grouped by provider",
		}
		if capacity, local := s.LocalCapacity(); local {
			status["local_capacity"] = capacity
		}
		c.JSON(http.StatusOK, status)
	})
}
func (s *Store) analysisHTTP(c *gin.Context) {
	f, err := parseFilter(c)
	if err != nil {
		badRequest(c, err)
		return
	}
	bucket := c.DefaultQuery("bucket", "hour")
	if bucket != "hour" && bucket != "day" {
		badRequest(c, errors.New("bucket must be hour or day"))
		return
	}
	result, err := s.Analyze(c.Request.Context(), f, bucket)
	respond(c, result, err)
}
func (s *Store) eventsHTTP(c *gin.Context) {
	f, err := parseFilter(c)
	if err != nil {
		badRequest(c, err)
		return
	}
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || limit < 1 || limit > 500 {
		badRequest(c, errors.New("limit must be between 1 and 500"))
		return
	}
	offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil || offset < 0 || offset > 10000000 {
		badRequest(c, errors.New("offset must be between 0 and 10000000"))
		return
	}
	page, err := s.Events(c.Request.Context(), f, limit, offset)
	respond(c, page, err)
}
func parseFilter(c *gin.Context) (Filter, error) {
	f := Filter{Model: c.Query("model"), Provider: c.Query("provider"), Account: c.Query("account"), KeyID: c.Query("key_id"), Status: c.Query("status")}
	if f.Status != "" && f.Status != "success" && f.Status != "failed" {
		return f, errors.New("status must be success or failed")
	}
	f.To = time.Now().UTC()
	if raw := c.Query("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return f, errors.New("to must be RFC3339")
		}
		f.To = parsed.UTC()
	}
	f.From = f.To.Add(-24 * time.Hour)
	if raw := c.Query("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return f, errors.New("from must be RFC3339")
		}
		f.From = parsed.UTC()
	}
	if !f.From.Before(f.To) {
		return f, errors.New("from must be before to")
	}
	return f, nil
}
func decodeRequest(c *gin.Context, target any, maxBytes int64) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON payload or unknown fields")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("payload must contain one JSON object")
	}
	return nil
}
func badRequest(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}
func respond(c *gin.Context, value any, err error) {
	if err == nil {
		c.JSON(http.StatusOK, value)
		return
	}
	if errors.Is(err, ErrClosed) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage persistence is shutting down"})
		return
	}
	log.Error("usage persistence: management operation failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "usage persistence operation failed"})
}
