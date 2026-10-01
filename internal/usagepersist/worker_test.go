package usagepersist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
)

type blockingUsageStore struct {
	store
	entered       chan struct{}
	release       chan struct{}
	seenContext   chan context.Context
	once          sync.Once
	uncooperative bool
	writes        atomic.Int64
}

func (s *blockingUsageStore) Insert(ctx context.Context, event Event) (bool, error) {
	s.once.Do(func() {
		if s.seenContext != nil {
			s.seenContext <- ctx
		}
		close(s.entered)
	})
	if s.uncooperative {
		<-s.release
	} else {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-s.release:
		}
	}
	inserted, err := s.store.Insert(ctx, event)
	if err == nil && inserted {
		s.writes.Add(1)
	}
	return inserted, err
}

func blockUsageStore(s *Store) *blockingUsageStore {
	backend := &blockingUsageStore{store: s.store, entered: make(chan struct{}), release: make(chan struct{})}
	s.store = backend
	return backend
}

func awaitSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for deterministic test barrier")
	}
}

func TestHungPersistenceDoesNotBlockSDKDispatcherOrOtherConsumers(t *testing.T) {
	s := openTestStore(t)
	backend := blockUsageStore(s)
	defer close(backend.release)
	previousEnabled, previousStatistics := redisqueue.Enabled(), redisqueue.UsageStatisticsEnabled()
	defer redisqueue.SetEnabled(previousEnabled)
	defer redisqueue.SetUsageStatisticsEnabled(previousStatistics)
	redisqueue.SetEnabled(true)
	redisqueue.SetUsageStatisticsEnabled(true)
	redisqueue.PopOldest(10000)
	defer redisqueue.PopOldest(10000)
	stop := redisqueue.ObserveUsage(s.Consume)
	defer stop()
	var otherCalls atomic.Int64
	stopOther := redisqueue.ObserveUsage(func(context.Context, []byte) { otherCalls.Add(1) })
	defer stopOther()

	// The SDK's last plugin barrier completes even though persistence is hung.
	publishThroughBuiltin(t, fixtureRecord("hung-first", time.Now()))
	awaitSignal(t, backend.entered)
	const overflow = 7
	for i := range usageQueueCapacity + overflow {
		publishThroughBuiltin(t, fixtureRecord(fmt.Sprintf("hung-%d", i), time.Now()))
	}
	want := int64(usageQueueCapacity + overflow + 1)
	if otherCalls.Load() != want {
		t.Fatalf("other observer starved: %d/%d", otherCalls.Load(), want)
	}
	if got := len(redisqueue.PopOldest(int(want) + 1)); got != int(want) {
		t.Fatalf("persistence stole or redirected original queue entries: %d/%d", got, want)
	}
	if len(s.queue) != usageQueueCapacity || s.pendingEvents.Load() != usageQueueCapacity+1 {
		t.Fatalf("admission not bounded: queued=%d pending=%d", len(s.queue), s.pendingEvents.Load())
	}
	if s.queueOverflows.Load() != overflow || s.droppedEvents.Load() != overflow || s.writeFailures.Load() != 0 {
		t.Fatalf("incorrect overflow counters: overflow=%d dropped=%d failed=%d", s.queueOverflows.Load(), s.droppedEvents.Load(), s.writeFailures.Load())
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s.RegisterRoutes(router.Group("/stats"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("GET", "/stats/status", nil))
	var health map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || health["queue_capacity"] != float64(usageQueueCapacity) || health["pending_events"] != float64(usageQueueCapacity+1) || health["dropped_events"] != float64(overflow) || health["queue_overflows"] != float64(overflow) || health["last_drop_at"] == nil {
		t.Fatalf("overflow missing from health: %s", response.Body)
	}
}

func TestQueuedUsageContainsOnlySanitizedSnapshots(t *testing.T) {
	// Hold admission's output without starting a worker so the exact queued
	// representation can be inspected, not just its eventual persisted subset.
	s := &Store{queue: make(chan queuedUsage, usageQueueCapacity), workerCtx: context.Background()}
	r := fixtureRecord("sanitized", time.Now())
	r.Provider = "claude"
	r.ResponseHeaders = http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"},
		"Authorization":      []string{"private-header"},
		"Set-Cookie":         []string{"private-cookie"},
		"X-Arbitrary-Header": []string{"private-arbitrary"},
		"X-Codex-Unknown":    []string{"private-unknown"},
	}
	payload := fixturePayload(r)
	type requestKey struct{}
	ctx := context.WithValue(context.Background(), requestKey{}, "private-context")
	s.Consume(ctx, payload)
	for i := range payload {
		payload[i] = '!'
	}
	item := <-s.queue
	typ := reflect.TypeOf(item)
	fields := []struct {
		name   string
		typeOf reflect.Type
	}{
		{"Event", reflect.TypeOf(Event{})},
		{"Quota", reflect.TypeOf((*quota.Snapshot)(nil))},
		{"Kind", reflect.TypeOf(queuedWorkKind(0))},
	}
	if typ.NumField() != len(fields) {
		t.Fatalf("queue retains fields outside the sanitized work union: %v", typ)
	}
	for i, field := range fields {
		if typ.Field(i).Name != field.name || typ.Field(i).Type != field.typeOf {
			t.Fatalf("unexpected queued field %d: %v", i, typ.Field(i))
		}
	}
	if item.Event.ID != r.RequestID || len(item.Event.KeyID) != 64 || item.Quota == nil || len(item.Quota.Windows) != 1 {
		t.Fatalf("normalization must happen before enqueue: %#v", item)
	}
	// Admission carries the producer's account facts directly: the worker performs
	// no credential lookup, and no identity field survives on the queued work.
	if item.Event.Account != fixtureAccount || item.Event.AccountKind != "email" || item.Quota.Account != fixtureAccount {
		t.Fatalf("queued work lost the producer's account facts: %#v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{r.APIKey, r.Source, r.AuthID, r.BaseURL, "sk-private-error-key", "private-source-token", "private-header", "private-cookie", "private-arbitrary", "private-unknown", "private-context", "Authorization", `"response_headers":`} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("queued data retained forbidden input %q", secret)
		}
	}
}

func TestAdmissionRejectsMissingInvalidCanonicalAndOversizedData(t *testing.T) {
	s := openTestStore(t)
	var canonical map[string]any
	if err := json.Unmarshal(fixturePayload(fixtureRecord("invalid", time.Now())), &canonical); err != nil {
		t.Fatal(err)
	}
	delete(canonical, "token_breakdown") // Legacy tokens must never be guessed.
	missing, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	invalid := fixtureRecord("bad-breakdown", time.Now())
	invalid.Detail.TokenBreakdown.Input.TotalTokens++
	longField := fixtureRecord(strings.Repeat("x", maxUsageTextBytes+1), time.Now())
	inputs := [][]byte{missing, fixturePayload(invalid), fixturePayload(longField), bytes.Repeat([]byte(" "), maxUsagePayloadBytes+1), []byte(`not-json`)}
	for _, payload := range inputs {
		s.Consume(context.Background(), payload)
	}
	flushFixture(t, s)
	if s.validationFailures.Load() != int64(len(inputs)) || s.droppedEvents.Load() != int64(len(inputs)) || s.pendingEvents.Load() != 0 || s.writeFailures.Load() != 0 {
		t.Fatalf("invalid admission counters: validation=%d dropped=%d pending=%d writes=%d", s.validationFailures.Load(), s.droppedEvents.Load(), s.pendingEvents.Load(), s.writeFailures.Load())
	}
	page, err := s.Events(context.Background(), Filter{}, 10, 0)
	if err != nil || page.Total != 0 {
		t.Fatalf("invalid canonical data persisted: %#v %v", page, err)
	}
}

func TestWorkerUsesLifecycleContextNotRequestContext(t *testing.T) {
	s := openTestStore(t)
	backend := blockUsageStore(s)
	backend.seenContext = make(chan context.Context, 1)
	defer close(backend.release)
	type requestKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), requestKey{}, "private-request-value"))
	cancel()
	s.Consume(ctx, fixturePayload(fixtureRecord("canceled-request", time.Now())))
	awaitSignal(t, backend.entered)
	workerCtx := <-backend.seenContext
	if workerCtx.Err() != nil || workerCtx.Value(requestKey{}) != nil {
		t.Fatal("worker retained request cancellation or context values")
	}
}

func TestCloseCancellationReleasesWorkerAndCountsAbandonedQueue(t *testing.T) {
	s := openTestStore(t)
	backend := blockUsageStore(s)
	s.Consume(context.Background(), fixturePayload(fixtureRecord("cancel-active", time.Now())))
	awaitSignal(t, backend.entered)
	for i := range 3 {
		s.Consume(context.Background(), fixturePayload(fixtureRecord(fmt.Sprintf("cancel-queued-%d", i), time.Now())))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close did not honor cancellation: %v", err)
	}
	awaitSignal(t, s.workerDone)
	if s.pendingEvents.Load() != 0 || s.droppedEvents.Load() != 3 || s.writeFailures.Load() != 1 {
		t.Fatalf("abandoned queue not accounted: pending=%d dropped=%d failed=%d", s.pendingEvents.Load(), s.droppedEvents.Load(), s.writeFailures.Load())
	}
	if err := s.Flush(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Flush concealed abandoned records: %v", err)
	}
	s.Consume(context.Background(), fixturePayload(fixtureRecord("late", time.Now())))
	if s.droppedEvents.Load() != 4 {
		t.Fatal("shutdown admission was not rejected and counted")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCloseCanceledContextDoesNotWaitForUncooperativeBackend(t *testing.T) {
	s := openTestStore(t)
	backend := blockUsageStore(s)
	backend.uncooperative = true
	defer close(backend.release)
	s.Consume(context.Background(), fixturePayload(fixtureRecord("uninterruptible", time.Now())))
	awaitSignal(t, backend.entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Close canceled error: %v", err)
		}
	}()
	awaitSignal(t, done)
	select {
	case <-s.closed:
		t.Fatal("closed storage while an uninterruptible write still uses it")
	default:
	}
}

func TestLifecycleCancellationStopsCollectionButKeepsManagementAvailable(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Open(lifecycle, Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = s.Close(context.Background())
	}()
	backend := blockUsageStore(s)
	s.Consume(context.Background(), fixturePayload(fixtureRecord("lifecycle", time.Now())))
	awaitSignal(t, backend.entered)
	cancel()
	awaitSignal(t, s.workerDone)
	if _, err = s.SetPrice(context.Background(), fixturePrice()); err != nil {
		t.Fatalf("worker cancellation disabled independent management pricing: %v", err)
	}
	if _, err = s.Events(context.Background(), Filter{}, 10, 0); err != nil {
		t.Fatalf("worker cancellation disabled independent history: %v", err)
	}
	s.Consume(context.Background(), fixturePayload(fixtureRecord("lifecycle-late", time.Now())))
	if s.droppedEvents.Load() != 1 {
		t.Fatal("lifecycle-canceled worker still admitted events")
	}
}

func TestFlushCancellationDoesNotCancelWorkerAndCloseDrains(t *testing.T) {
	s := openTestStore(t)
	backend := blockUsageStore(s)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(backend.release) })
	s.Consume(context.Background(), fixturePayload(fixtureRecord("drain-first", time.Now())))
	awaitSignal(t, backend.entered)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush failed to honor cancellation: %v", err)
	}
	if s.workerCtx.Err() != nil {
		t.Fatal("Flush canceled persistence instead of only its barrier")
	}
	s.Consume(context.Background(), fixturePayload(fixtureRecord("drain-second", time.Now())))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("Close drain: %v", err)
		}
	}()
	releaseOnce.Do(func() { close(backend.release) })
	awaitSignal(t, done)
	if backend.writes.Load() != 2 || s.pendingEvents.Load() != 0 || s.droppedEvents.Load() != 0 {
		t.Fatalf("graceful Close lost accepted writes: writes=%d pending=%d dropped=%d", backend.writes.Load(), s.pendingEvents.Load(), s.droppedEvents.Load())
	}
}
