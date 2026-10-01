package usagepersist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const managementResetURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"

// Every cache write stops before touching storage until released or canceled.
// Stepped releases make Flush/reset ordering deterministic without timer sleeps.
type managementBlockingStore struct {
	store
	entered chan context.Context
	release chan struct{}
	once    sync.Once
	writes  atomic.Int64
	inserts atomic.Int64
}

// MutateCache gates the durable quota-state backend. The bounded quota history
// row is separate best-effort diagnostic state written after the state merge, so
// it passes through without consuming a stepped release or a write count: gating
// it here would require two releases per observation.
func (backend *managementBlockingStore) MutateCache(ctx context.Context, namespace, key string, update func(json.RawMessage) (json.RawMessage, error)) error {
	if namespace == quotaHistoryNamespace {
		return backend.store.MutateCache(ctx, namespace, key, update)
	}
	backend.entered <- ctx
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-backend.release:
	}
	err := backend.store.MutateCache(ctx, namespace, key, update)
	if err == nil {
		backend.writes.Add(1)
	}
	return err
}

func (backend *managementBlockingStore) Insert(ctx context.Context, event Event) (bool, error) {
	backend.inserts.Add(1)
	return backend.store.Insert(ctx, event)
}

func (backend *managementBlockingStore) unblock() {
	backend.once.Do(func() { close(backend.release) })
}

func blockManagementStore(t *testing.T, s *Store) *managementBlockingStore {
	t.Helper()
	backend := &managementBlockingStore{store: s.store, entered: make(chan context.Context, usageQueueCapacity+16), release: make(chan struct{})}
	s.store = backend
	t.Cleanup(backend.unblock)
	return backend
}

func awaitManagementMutation(t *testing.T, backend *managementBlockingStore) context.Context {
	t.Helper()
	select {
	case ctx := <-backend.entered:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach cache mutation barrier")
		return nil
	}
}

func managementFetchFixture(t *testing.T) pluginapi.QuotaFetchResponse {
	t.Helper()
	response, valid := middlewareFetchResponse([]byte(`{"groups":[{"displayName":"Plan","buckets":[{"window":"monthly","remainingFraction":0.75}]}]}`))
	if !valid {
		t.Fatal("invalid management fetch fixture")
	}
	return response
}

func TestManagementWorkerBlockedBackendPreservesOriginalResponses(t *testing.T) {
	for _, test := range []struct {
		name, path, input, output string
		reset                     bool
	}{
		{"api-call", "/v0/management/api-call", `{"auth_index":"account","url":"` + middlewareQuotaURL + `","header":{"Authorization":"Bearer $TOKEN$"}}`, string(middlewareQuotaEnvelope(t, "")), false},
		{"api-call-v8", "/v8/management/requests/api-call", `{"auth_index":"account","url":"` + middlewareQuotaURL + `","header":{"Authorization":"Bearer $TOKEN$"}}`, string(middlewareQuotaEnvelope(t, "")), false},
		{"fetch", "/v0/management/quota/fetch", `{"auth_index":"account"}`, `{"groups":[{"buckets":[{"window":"monthly","remainingFraction":0.75}]}]}`, false},
		{"fetch-v8", "/v8/management/credentials/quota/fetch", `{"auth_index":"account"}`, `{"groups":[{"buckets":[{"window":"monthly","remainingFraction":0.75}]}]}`, false},
		{"reset", "/v0/management/reset-quota", `{"auth_index":"account"}`, `{"status":"ok","auth_index":"account"}`, true},
		{"reset-v8", "/v8/management/credentials/quota/reset", `{"auth_index":"account"}`, `{"status":"ok","auth_index":"account"}`, true},
		{"reset-credits", "/v0/management/api-call", `{"auth_index":"account","url":"` + managementResetURL + `","header":{"Authorization":"Bearer $TOKEN$"}}`, `{"status_code":200,"header":{"Set-Cookie":["private-cookie"]},"body":"{\"code\":\"reset\",\"windows_reset\":1}"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := middlewareTestStore(t)
			if test.reset {
				wpObserveQuotaFetch(s, context.Background(), "codex", "account", managementFetchFixture(t))
				flushFixture(t, s)
			}
			backend := blockManagementStore(t, s)
			engine := gin.New()
			engine.Use(s.ManagementMiddleware())
			engine.POST(test.path, func(c *gin.Context) {
				body, errRead := io.ReadAll(c.Request.Body)
				if errRead != nil || string(body) != test.input {
					t.Errorf("original handler request changed: %q %v", body, errRead)
				}
				c.Header("X-Original", "preserved")
				c.Data(http.StatusCreated, "application/json", []byte(test.output))
			})
			type privateKey struct{}
			requestCtx, cancel := context.WithCancel(context.WithValue(context.Background(), privateKey{}, "private-request-context"))
			cancel()
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.input)).WithContext(requestCtx)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				engine.ServeHTTP(response, request)
			}()
			// The complete middleware/handler chain, not merely a recorder write,
			// must return while the optional backend remains blocked.
			awaitSignal(t, done)
			workerCtx := awaitManagementMutation(t, backend)
			if workerCtx.Err() != nil || workerCtx.Value(privateKey{}) != nil {
				t.Fatal("management worker retained request context")
			}
			if response.Code != http.StatusCreated || response.Body.String() != test.output || response.Header().Get("X-Original") != "preserved" {
				t.Fatalf("original response changed: %d %v %q", response.Code, response.Header(), response.Body.String())
			}
			if s.pendingEvents.Load() != 1 || backend.writes.Load() != 0 || backend.inserts.Load() != 0 {
				t.Fatal("management observation bypassed worker or inserted a dummy event")
			}
			backend.unblock()
			flushFixture(t, s)
			if (middlewareQuotaCount(t, s) == 0) != test.reset {
				t.Fatal("admitted quota/reset work did not finish before Flush")
			}
			page, err := s.Events(context.Background(), Filter{}, 10, 0)
			if err != nil || page.Total != 0 || backend.inserts.Load() != 0 {
				t.Fatalf("quota-only work leaked into usage statistics: %+v %v", page, err)
			}
		})
	}
}

func TestManagementWorkerSharesBoundedQueueAndReportsOverflow(t *testing.T) {
	s := middlewareTestStore(t)
	backend := blockManagementStore(t, s)
	ctx := context.Background()
	fetch := managementFetchFixture(t)
	wpObserveQuotaFetch(s, ctx, "codex", "account", fetch)
	awaitManagementMutation(t, backend)
	admit := func(index int) {
		switch index % 4 {
		case 0:
			wpObserveAPICall(s, ctx, "codex", "account", middlewareQuotaURL, 200, nil, []byte(`{"rate_limit":{"primary_window":{"used_percent":25}}}`))
		case 1:
			wpObserveQuotaFetch(s, ctx, "codex", "account", fetch)
		case 2:
			wpObserveQuotaReset(s, ctx, "codex", "account")
		case 3:
			s.Consume(ctx, fixturePayload(fixtureRecord(fmt.Sprintf("mixed-%d", index), time.Now())))
		}
	}
	for i := range usageQueueCapacity {
		admit(i)
	}
	// Every kind, including a reset, uses the same drop-new overflow policy.
	for i := range 4 {
		admit(usageQueueCapacity + i)
	}
	if cap(s.queue) != usageQueueCapacity || len(s.queue) != usageQueueCapacity || s.pendingEvents.Load() != usageQueueCapacity+1 || s.accepted != usageQueueCapacity+1 {
		t.Fatalf("management admission grew beyond shared bound: capacity=%d queued=%d pending=%d accepted=%d", cap(s.queue), len(s.queue), s.pendingEvents.Load(), s.accepted)
	}
	if s.queueOverflows.Load() != 4 || s.droppedEvents.Load() != 4 || s.writeFailures.Load() != 0 {
		t.Fatal("mixed work overflow was not counted")
	}
	engine := gin.New()
	s.RegisterRoutes(engine.Group("/stats"))
	response := middlewareRequest(t, engine, http.MethodGet, "/stats/status", nil)
	var health map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || health["queue_capacity"] != float64(usageQueueCapacity) || health["pending_events"] != float64(usageQueueCapacity+1) || health["queue_overflows"] != float64(4) || health["dropped_events"] != float64(4) || health["last_drop_at"] == nil {
		t.Fatalf("management overflow missing from existing health: %s", response.Body)
	}
	backend.unblock()
	flushFixture(t, s)
	page, err := s.Events(ctx, Filter{}, 100, 0)
	if err != nil || page.Total != usageQueueCapacity/4 || backend.inserts.Load() != usageQueueCapacity/4 || s.pendingEvents.Load() != 0 {
		t.Fatalf("mixed queue lost usage or fabricated management events: %+v %v", page, err)
	}
}

func TestManagementWorkerQueuesOnlyNormalizedDataAndReceiptTimes(t *testing.T) {
	// No worker: inspect exactly what is retained, including confirmed resets.
	s := &Store{queue: make(chan queuedUsage, usageQueueCapacity), workerCtx: context.Background()}
	wpBindManagementFixtures(t, s)
	type privateKey struct{}
	ctx := context.WithValue(context.Background(), privateKey{}, "private-context")
	headers := http.Header{"Authorization": {"private-header"}, "Set-Cookie": {"private-cookie"}}
	body := []byte(`{"rate_limit":{"primary_window":{"used_percent":25}},"token":"private-body"}`)
	fetch := managementFetchFixture(t)
	before := time.Now().UTC()
	wpObserveAPICall(s, ctx, "codex", "account", middlewareQuotaURL+"?token=private-query", 200, headers, body)
	wpObserveQuotaFetch(s, ctx, "codex", "account", fetch)
	wpObserveQuotaReset(s, ctx, " CODEX ", " account ")
	wpObserveAPICall(s, ctx, "codex", "account", managementResetURL+"?token=private-query", 200, headers, []byte(`{"code":"reset","windows_reset":1,"token":"private-reset-body"}`))
	after := time.Now().UTC()
	for i := range body {
		body[i] = '!'
	}
	headers.Set("Authorization", "changed-private-header")
	fetch.Groups[0].Buckets[0].RemainingFraction = 0
	fetch.Groups[0].Buckets[0].Window = "changed-private-window"
	if len(s.queue) != 4 {
		t.Fatalf("normalization did not admit expected observations: %d", len(s.queue))
	}
	typ := reflect.TypeOf(queuedUsage{})
	if typ.NumField() != 4 || typ.Field(0).Type != reflect.TypeOf(Event{}) || typ.Field(1).Type != reflect.TypeOf((*quota.Snapshot)(nil)) || typ.Field(2).Type != reflect.TypeOf(queuedWorkKind(0)) || typ.Field(3).Type != reflect.TypeOf((*queuedQuotaReset)(nil)) {
		t.Fatalf("queue gained an unsanitized field: %v", typ)
	}
	// A queued reset now carries only the account it applies to and the receipt
	// time: provider, account, observed-at. There is no credential fence left.
	resetType := reflect.TypeOf(queuedQuotaReset{})
	if resetType.NumField() != 3 || resetType.Field(0).Type.Kind() != reflect.String || resetType.Field(1).Type.Kind() != reflect.String || resetType.Field(2).Type != reflect.TypeOf(time.Time{}) {
		t.Fatalf("reset retained more than account and receipt time: %v", resetType)
	}
	for i := range 4 {
		item := <-s.queue
		if item.Event != (Event{}) {
			t.Fatal("management work contains a dummy usage event")
		}
		var observedAt time.Time
		if i < 2 {
			if item.Kind != queuedQuotaObservation || item.Quota == nil || item.Reset != nil || len(item.Quota.Windows) != 1 {
				t.Fatalf("quota was not normalized before admission: %+v", item)
			}
			observedAt = item.Quota.ObservedAt
			if i == 1 && (*item.Quota.Windows[0].RemainingPercent != 75 || item.Quota.Windows[0].Label != "monthly") {
				t.Fatal("queued quota aliased caller-owned plugin response")
			}
		} else {
			if item.Kind != queuedQuotaResetBarrier || item.Reset == nil || item.Quota != nil || item.Reset.Provider != "codex" || item.Reset.Account != fixtureAccount {
				t.Fatalf("reset was not sanitized before admission: %+v", item)
			}
			observedAt = item.Reset.ObservedAt
		}
		if observedAt.Before(before) || observedAt.After(after) {
			t.Fatalf("observation timestamp was not captured at receipt: %v", observedAt)
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-", "Authorization", "Set-Cookie", middlewareQuotaURL, managementResetURL} {
			if bytes.Contains(encoded, []byte(secret)) {
				t.Fatalf("queue retained private request/response data: %s", encoded)
			}
		}
	}
}

// Done is evaluated only after Flush has captured its accepted-work target.
type managementFlushContext struct {
	context.Context
	sampled chan struct{}
	once    sync.Once
}

func (ctx *managementFlushContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.sampled) })
	return ctx.Context.Done()
}

func TestManagementWorkerFlushIncludesNewKindsButNotLaterAdmissions(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			s := middlewareTestStore(t)
			backend := blockManagementStore(t, s)
			if reset {
				wpObserveQuotaReset(s, context.Background(), "codex", "account")
			} else {
				wpObserveQuotaFetch(s, context.Background(), "codex", "account", managementFetchFixture(t))
			}
			awaitManagementMutation(t, backend)
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.Flush(canceled); !errors.Is(err, context.Canceled) || s.workerCtx.Err() != nil {
				t.Fatalf("Flush failed to wait for new work kind or canceled worker: %v", err)
			}
			ctx := &managementFlushContext{Context: context.Background(), sampled: make(chan struct{})}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if err := s.Flush(ctx); err != nil {
					t.Errorf("Flush: %v", err)
				}
			}()
			awaitSignal(t, ctx.sampled)
			wpObserveQuotaFetch(s, context.Background(), "codex", "later", managementFetchFixture(t))
			backend.release <- struct{}{}
			awaitManagementMutation(t, backend)
			awaitSignal(t, done)
			if s.pendingEvents.Load() != 1 || backend.writes.Load() != 1 {
				t.Fatal("Flush barrier extended to later work or preceded target completion")
			}
			backend.unblock()
			flushFixture(t, s)
		})
	}
}

func TestManagementWorkerShutdownCancellationDropsQueuedKinds(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%v", reset), func(t *testing.T) {
			s := middlewareTestStore(t)
			backend := blockManagementStore(t, s)
			fetch := managementFetchFixture(t)
			if reset {
				wpObserveQuotaReset(s, context.Background(), "codex", "account")
			} else {
				wpObserveQuotaFetch(s, context.Background(), "codex", "account", fetch)
			}
			awaitManagementMutation(t, backend)
			wpObserveQuotaReset(s, context.Background(), "codex", "account")
			wpObserveQuotaFetch(s, context.Background(), "codex", "account", fetch)
			wpObserveAPICall(s, context.Background(), "codex", "account", middlewareQuotaURL, 200, nil, []byte(`{"rate_limit":{"primary_window":{"used_percent":25}}}`))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Close ignored cancellation: %v", err)
			}
			awaitSignal(t, s.workerDone)
			if s.pendingEvents.Load() != 0 || s.droppedEvents.Load() != 3 || s.writeFailures.Load() != 1 || backend.writes.Load() != 0 || backend.inserts.Load() != 0 {
				t.Fatalf("new work kinds escaped cancellation accounting: pending=%d dropped=%d failed=%d", s.pendingEvents.Load(), s.droppedEvents.Load(), s.writeFailures.Load())
			}
			if err := s.Flush(context.Background()); !errors.Is(err, ErrClosed) {
				t.Fatalf("Flush concealed abandoned management work: %v", err)
			}
			if len(s.publishedQuotaBindings()) != 0 {
				t.Fatal("shutdown retained an eligible observation publication")
			}
			// Already normalized late work is still counted by admission. New
			// optional observations have no published proof after shutdown and
			// therefore never query live core state merely to count a drop.
			s.enqueueManagement(queuedUsage{Kind: queuedQuotaResetBarrier})
			s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation})
			if s.droppedEvents.Load() != 5 {
				t.Fatal("post-shutdown management admissions were not counted")
			}
		})
	}
}

func managementSnapshotAt(t *testing.T, s *Store, observedAt time.Time, used int) quota.Snapshot {
	t.Helper()
	// Account facts are supplied by the observation itself now; the transient
	// index only located the live credential that produced it.
	snapshot, ok := quota.ParseAPICall(quota.Identity{Provider: "codex", Account: fixtureAccount, AccountKind: "email"}, middlewareQuotaURL, 200, nil, []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d}}}`, used)), observedAt)
	if !ok {
		t.Fatal("invalid timestamped quota fixture")
	}
	return snapshot
}

func managementState(t *testing.T, s *Store) quotaState {
	t.Helper()
	values, err := s.ListCache(context.Background(), quotaNamespace)
	if err != nil {
		t.Fatal(err)
	}
	var state quotaState
	if err = json.Unmarshal(values[quotaStoreKey("codex", fixtureAccount)], &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func testManagementResetOrdering(t *testing.T, s *Store) {
	t.Helper()
	wpBindManagementFixtures(t, s)
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-time.Minute)
	old := managementSnapshotAt(t, s, cutoff.Add(-time.Second), 25)
	fresh := managementSnapshotAt(t, s, cutoff.Add(time.Second), 60)
	entry := wpBindCacheFixture(s, QuotaCacheEntry{Provider: "codex", Key: "account", ObservedAt: old.ObservedAt, State: json.RawMessage(`{"status":"success","windows":[]}`)})
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	backend := blockManagementStore(t, s)
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &old})
	awaitManagementMutation(t, backend)
	wpObserveResetAt(s, "codex", "account", cutoff)
	// A pre-reset sample admitted after the reset still must be rejected by
	// the durable cutoff, not resurrected based on worker execution order.
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &old})
	backend.unblock()
	flushFixture(t, s)
	state := managementState(t, s)
	if !state.ResetAt.Equal(cutoff) || state.Snapshot != nil || len(state.UIEntries) != 0 {
		t.Fatalf("queued reset lost receipt cutoff or resurrected stale state: %+v", state)
	}
	// Already-observed post-reset data predates worker execution; it must
	// remain valid. This fails if reset stamps time.Now inside MutateCache.
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &fresh})
	flushFixture(t, s)
	state = managementState(t, s)
	if state.Snapshot == nil || *state.Snapshot.Windows[0].UsedPercent != 60 || !state.ResetAt.Equal(cutoff) {
		t.Fatalf("delayed reset rejected genuinely post-reset quota: %+v", state)
	}
	// Older resets cannot lower the cutoff, and snapshots exactly at it are
	// stale as well. Keep the post-reset state while rejecting both cases.
	wpObserveResetAt(s, "codex", "account", cutoff.Add(-time.Second))
	atCutoff := managementSnapshotAt(t, s, cutoff, 90)
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &atCutoff})
	flushFixture(t, s)
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	state = managementState(t, s)
	if !state.ResetAt.Equal(cutoff) || state.Snapshot == nil || *state.Snapshot.Windows[0].UsedPercent != 60 || len(state.UIEntries) != 0 {
		t.Fatalf("older reset/snapshot/cache regressed durable state: %+v", state)
	}
	// A newer reset removes the old snapshot, then even a lower-timestamped
	// delayed reset must not permit its resurrection after the next drain.
	newCutoff := cutoff.Add(2 * time.Second)
	wpObserveResetAt(s, "codex", "account", newCutoff)
	wpObserveResetAt(s, "codex", "account", cutoff)
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &fresh})
	flushFixture(t, s)
	state = managementState(t, s)
	if !state.ResetAt.Equal(newCutoff) || state.Snapshot != nil {
		t.Fatalf("stale work resurrected state behind newest reset: %+v", state)
	}
}

// The durable protection the old generation fence provided survives here: an
// older observation time can never overwrite newer stored state, neither on the
// local backend nor across a replica.
func TestManagementWorkerResetOrderingAndDurableCutoff(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		directory := t.TempDir()
		s, err := Open(context.Background(), Options{DataDir: directory})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		testManagementResetOrdering(t, s)
		cutoff := managementState(t, s).ResetAt
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(context.Background(), Options{DataDir: directory})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close(context.Background()) })
		wpBindManagementFixtures(t, reopened)
		old := managementSnapshotAt(t, reopened, cutoff.Add(-time.Second), 99)
		reopened.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &old})
		flushFixture(t, reopened)
		state := managementState(t, reopened)
		if !state.ResetAt.Equal(cutoff) || state.Snapshot != nil {
			t.Fatalf("durable reset cutoff lost across file-store reopen: %+v", state)
		}
	})
	t.Run("postgres", func(t *testing.T) {
		core := testPostgresCore(t)
		db, schema := core.UsageDatabase()
		s, err := Open(context.Background(), Options{Database: db, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		testManagementResetOrdering(t, s)
		cutoff := managementState(t, s).ResetAt
		other, err := Open(context.Background(), Options{Database: db, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close(context.Background()) })
		wpBindManagementFixtures(t, other)
		wpObserveResetAt(other, "codex", "account", cutoff.Add(-time.Second))
		old := managementSnapshotAt(t, other, cutoff.Add(-time.Second), 99)
		other.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &old})
		flushFixture(t, other)
		state := managementState(t, s)
		if !state.ResetAt.Equal(cutoff) || state.Snapshot != nil {
			t.Fatalf("another replica regressed durable reset cutoff: %+v", state)
		}
	})
}

func TestManagementWorkerDelayedResetPreservesNewerWindowsAndUICache(t *testing.T) {
	s := middlewareTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-time.Minute)
	old := managementSnapshotAt(t, s, cutoff.Add(-time.Second), 25)
	fresh := managementSnapshotAt(t, s, cutoff.Add(time.Second), 60)
	fresh.Windows[0].ID = "newer-window"
	if err := s.mergeQuota(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.mergeQuota(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	// Both entries address the same transient credential index; the binding
	// supplies the durable display key. Only the newer observation may survive.
	entries := []QuotaCacheEntry{
		{Provider: "codex", Key: "account", ObservedAt: old.ObservedAt, State: json.RawMessage(`{"status":"success","windows":[]}`)},
		{Provider: "codex", Key: "account", ObservedAt: fresh.ObservedAt, State: json.RawMessage(`{"status":"success","windows":[]}`)},
	}
	for i := range entries {
		entries[i] = wpBindCacheFixture(s, entries[i])
	}
	if err := s.SaveQuotaCache(ctx, entries); err != nil {
		t.Fatal(err)
	}
	wpObserveResetAt(s, "codex", "account", cutoff)
	flushFixture(t, s)
	state := managementState(t, s)
	if !state.ResetAt.Equal(cutoff) || state.Snapshot == nil || len(state.Snapshot.Windows) != 1 || state.Snapshot.Windows[0].ID != "newer-window" || len(state.UIEntries) != 1 {
		t.Fatalf("delayed reset retained old windows or discarded newer state: %+v", state)
	}
	cached, err := s.QuotaCache(ctx)
	if err != nil || len(cached) != 1 || cached[0].Key != wpReadBinding(s, "codex", "account").Key {
		t.Fatalf("delayed reset did not preserve only newer UI cache: %+v %v", cached, err)
	}
}
