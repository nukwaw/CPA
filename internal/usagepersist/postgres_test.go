package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	corestore "github.com/router-for-me/CLIProxyAPI/v8/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
)

// testPostgresCore initializes the same resource selected by PGSTORE in main.
// PGSTORE_TEST_DSN is a test fixture only, never a runtime persistence setting.
func testPostgresCore(t *testing.T) *corestore.PostgresStore {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGSTORE_TEST_DSN to an isolated PostgreSQL test database")
	}
	ctx := context.Background()
	core, err := corestore.NewPostgresStore(ctx, corestore.PostgresStoreConfig{
		DSN: dsn, Schema: "usage_test_" + strings.ReplaceAll(uuid.NewString(), "-", "") + `"quoted`, SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db, schema := core.UsageDatabase()
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
	return core
}

// TestPostgresPersistence borrows an initialized core pool rather than opening one.
func TestPostgresPersistence(t *testing.T) {
	core := testPostgresCore(t)
	db, schema := core.UsageDatabase()
	ctx := context.Background()
	s, err := Open(ctx, Options{Database: db, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	// Each adapter has its own operation revision, but unchanged credentials
	// must retain the same persistent identity across reopen and replicas.
	bindFixtures := func(target *Store) {
		bindQuotaFixtures(t, target,
			quotaFixtureAuth("claude", "account-1", "claude.json", "source-token"),
			quotaFixtureAuth("devin", "devin-account", "devin.json", "devin-token"),
			quotaFixtureAuth("claude", "parallel", "parallel.json", "parallel-token"))
	}
	bindFixtures(s)
	at := time.Now().UTC().Add(-time.Minute)
	record := fixtureRecord("pg-event", at)
	record.Model = "gpt-test\x00suffix"
	record.Alias = "alias\x00suffix"
	record.Provider = "claude"
	record.AccessTokenSHA256 = quotaHash("source-token")
	record.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"}}
	if err = s.recordFixture(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err = s.recordFixture(ctx, record); err != nil {
		t.Fatal(err)
	}
	price := fixturePrice()
	price.Model = "gpt-test�suffix"
	if _, err = s.SetPrice(ctx, price); err != nil {
		t.Fatal(err)
	}
	cache := bindCacheFixture(s, QuotaCacheEntry{Provider: "devin", AuthIndex: "devin-account", ObservedAt: at, State: json.RawMessage(`{"status":"success","windows":[{"id":"daily","remainingPercent":80,"resetAtMs":1800000000000,"periodHours":24}],"observedAtMs":1700000000000,"plan":"pro"}`)})
	if cache.Key != "devin.json\x00devin-account" {
		t.Fatalf("fixture lost Devin's native composite key: %q", cache.Key)
	}
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{cache}); err != nil {
		t.Fatalf("Devin NUL composite key must round trip through JSONB: %v", err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, Options{Database: db, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	bindFixtures(s)
	page, err := s.Events(ctx, Filter{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || !page.Events[0].Priced || page.Events[0].Model != price.Model {
		t.Fatalf("PostgreSQL event/price persistence: %#v", page)
	}
	cached, err := s.QuotaCache(ctx)
	if err != nil || len(cached) != 1 || cached[0].Key != cache.Key || cached[0].CredentialGeneration != cache.CredentialGeneration || cached[0].Revision != "" {
		t.Fatalf("PostgreSQL cache roundtrip: %#v %v", cached, err)
	}
	if current := fixtureBinding(s, "devin", "devin-account"); current.CredentialGeneration != cache.CredentialGeneration || current.Revision == cache.Revision {
		t.Fatal("reopen changed stable credential identity or reused an old request revision")
	}
	snapshots, err := s.Quotas(ctx)
	if err != nil || len(snapshots) != 1 || len(snapshots[0].Windows) != 1 {
		t.Fatalf("PostgreSQL quota persistence: %#v %v", snapshots, err)
	}

	if err = db.PingContext(ctx); err != nil {
		t.Fatalf("usage Close closed the borrowed core pool: %v", err)
	}
	// Independent adapters share the initialized core pool. Per-identity advisory
	// locks retain all independently arriving windows during concurrent updates.
	other, err := Open(ctx, Options{Database: db, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close(ctx) }()
	bindFixtures(other)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := s
			if i%2 == 0 {
				target = other
			}
			percent := float64(i)
			observed := at.Add(time.Duration(i) * time.Millisecond)
			snapshot := bindSnapshotFixture(target, quota.Snapshot{Provider: "claude", AuthIndex: "parallel", ObservedAt: observed, Source: quota.SourceHeaders, Windows: []quota.Window{{ID: fmt.Sprintf("window-%d", i), UsedPercent: &percent, ObservedAt: observed, Source: quota.SourceHeaders}}})
			errs <- target.mergeQuota(ctx, snapshot)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err = s.Quotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, snapshot := range snapshots {
		if snapshot.AuthIndex == "parallel" {
			found = true
			if len(snapshot.Windows) != 12 {
				t.Fatalf("lost concurrent windows: %d", len(snapshot.Windows))
			}
		}
	}
	if !found {
		t.Fatal("parallel snapshot missing")
	}

	if err = s.ResetQuota(ctx, "devin", "devin-account"); err != nil {
		t.Fatal(err)
	}
	// A replica must acquire its own live revision rather than replay another
	// process's request fence; the old observation time still hits the reset.
	if err = other.SaveQuotaCache(ctx, []QuotaCacheEntry{cache}); !errors.Is(err, ErrQuotaIdentity) {
		t.Fatalf("replica accepted another process's request revision: %v", err)
	}
	if err = other.SaveQuotaCache(ctx, []QuotaCacheEntry{bindCacheFixture(other, cache)}); err != nil {
		t.Fatal(err)
	}
	cached, err = other.QuotaCache(ctx)
	if err != nil || len(cached) != 0 {
		t.Fatalf("reset allowed stale cache resurrection: %#v %v", cached, err)
	}
	if err = s.ResetQuota(ctx, "claude", "account-1"); err != nil {
		t.Fatal(err)
	}
	if err = other.recordFixture(ctx, record); err != nil {
		t.Fatal(err)
	}
	snapshots, err = s.Quotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if snapshot.AuthIndex == "account-1" {
			t.Fatal("reset allowed stale header resurrection")
		}
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, Options{Database: db, Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	bindFixtures(s)
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{bindCacheFixture(s, cache)}); err != nil {
		t.Fatal(err)
	}
	cached, err = s.QuotaCache(ctx)
	if err != nil || len(cached) != 0 {
		t.Fatal("reset watermark not persistent")
	}
}

func TestPostgresSchemasIsolateAllUsageState(t *testing.T) {
	ctx := context.Background()
	first, second := testPostgresCore(t), testPostgresCore(t)
	dbA, schemaA := first.UsageDatabase()
	dbB, schemaB := second.UsageDatabase()
	a, err := openPostgresStore(ctx, dbA, schemaA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := openPostgresStore(ctx, dbB, schemaB)
	if err != nil {
		t.Fatal(err)
	}
	if a.lockKey("metadata", "quota", "key") == b.lockKey("metadata", "quota", "key") {
		t.Fatal("schema missing from advisory lock identity")
	}
	for i, target := range []*postgresStore{a, b} {
		model := fmt.Sprintf("model-%d", i)
		if inserted, errInsert := target.Insert(ctx, Event{ID: "same-id", Model: model, RequestedAt: time.Now().UTC()}); errInsert != nil || !inserted {
			t.Fatalf("isolated insert: %t %v", inserted, errInsert)
		}
		if err = target.UpdateCache(ctx, []cacheUpdate{{Namespace: "same-namespace", Key: "same-key", Value: json.RawMessage(fmt.Sprintf(`{"owner":%d}`, i))}}); err != nil {
			t.Fatal(err)
		}
		if err = target.MutateCache(ctx, "same-namespace", "mutated", func(json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(fmt.Sprintf(`{"owner":%d}`, i)), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i, target := range []*postgresStore{a, b} {
		page, total, errPage := target.Events(ctx, Filter{}, 10, 0)
		if errPage != nil || total != 1 || len(page) != 1 || page[0].Model != fmt.Sprintf("model-%d", i) {
			t.Fatalf("cross-schema events: %#v total=%d err=%v", page, total, errPage)
		}
		walked := 0
		if err = target.Walk(ctx, Filter{}, func(e Event) error {
			walked++
			if e.Model != fmt.Sprintf("model-%d", i) {
				t.Fatal("cross-schema walk")
			}
			return nil
		}); err != nil || walked != 1 {
			t.Fatalf("walk: %d %v", walked, err)
		}
		values, errCache := target.Cache(ctx, "same-namespace")
		if errCache != nil || len(values) != 2 {
			t.Fatalf("cache: %#v %v", values, errCache)
		}
		for _, value := range values {
			var entry struct{ Owner int }
			if err = json.Unmarshal(value, &entry); err != nil || entry.Owner != i {
				t.Fatalf("cross-schema metadata: %s %v", value, err)
			}
		}
	}
	if err = a.UpdateCache(ctx, []cacheUpdate{{Namespace: "same-namespace", Key: "same-key", Delete: true}}); err != nil {
		t.Fatal(err)
	}
	if err = a.MutateCache(ctx, "same-namespace", "mutated", func(json.RawMessage) (json.RawMessage, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if values, errCache := b.Cache(ctx, "same-namespace"); errCache != nil || len(values) != 2 {
		t.Fatalf("cross-schema deletion: %#v %v", values, errCache)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = dbA.PingContext(ctx); err != nil {
		t.Fatalf("borrowed core connection closed: %v", err)
	}
}

func TestPostgresEmptySchemaUsesCoreSearchPath(t *testing.T) {
	core := testPostgresCore(t)
	_, schema := core.UsageDatabase()
	pgcfg, err := pgx.ParseConfig(os.Getenv("PGSTORE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	// ConnString returns the original input, so supply this test-only search_path
	// through the DSN accepted by the same core constructor.
	dsn := pgcfg.ConnString() + " search_path='" + strings.ReplaceAll(pgx.Identifier{schema}.Sanitize(), "'", "\\'") + "'"
	if strings.HasPrefix(pgcfg.ConnString(), "postgres://") || strings.HasPrefix(pgcfg.ConnString(), "postgresql://") {
		u, errURL := url.Parse(pgcfg.ConnString())
		if errURL != nil {
			t.Fatal(errURL)
		}
		values := u.Query()
		values.Set("search_path", pgx.Identifier{schema}.Sanitize())
		u.RawQuery = values.Encode()
		dsn = u.String()
	}
	ctx := context.Background()
	searchCore, err := corestore.NewPostgresStore(ctx, corestore.PostgresStoreConfig{DSN: dsn, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = searchCore.Close() }()
	if err = searchCore.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	db, selected := searchCore.UsageDatabase()
	if selected != "" {
		t.Fatalf("expected unresolved core schema: %q", selected)
	}
	s, err := openPostgresStore(ctx, db, selected)
	if err != nil || s.schema != schema {
		t.Fatalf("effective schema mismatch: %#v %v", s, err)
	}
	if _, err = s.Insert(ctx, Event{ID: "search-path", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "usage_events"}.Sanitize()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("effective schema was not used: %d %v", count, err)
	}
}

func TestPostgresAddonDDLFailureKeepsCoreUsable(t *testing.T) {
	core := testPostgresCore(t)
	db, schema := core.UsageDatabase()
	ctx := context.Background()
	// A conflicting addon table fails index DDL without changing or closing the
	// initialized core pool or touching its configuration/authentication tables.
	if _, err := db.ExecContext(ctx, `CREATE TABLE `+pgx.Identifier{schema, "usage_events"}.Sanitize()+` (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, Options{Database: db, Schema: schema}); err == nil {
		t.Fatal("incompatible usage table did not fail addon initialization")
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("failed addon initialization closed core pool: %v", err)
	}
	if err := core.EnsureSchema(ctx); err != nil {
		t.Fatalf("failed addon initialization damaged core schema: %v", err)
	}
}

func TestPostgresInitializationPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := openPostgresStore(ctx, nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("initialization lost cancellation: %v", err)
	}
}
