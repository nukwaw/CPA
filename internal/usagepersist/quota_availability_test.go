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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// MarkResult calls this Save while holding the real core Manager write lock.
// This is intentionally different from blocking the add-on's Insert: even a
// nominally memory-only Manager.List would join this storage stall.
type quotaCoreSaveGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	stop    sync.Once
}

func (gate *quotaCoreSaveGate) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (gate *quotaCoreSaveGate) Delete(context.Context, string) error           { return nil }
func (gate *quotaCoreSaveGate) Save(context.Context, *coreauth.Auth) (string, error) {
	gate.once.Do(func() { close(gate.entered) })
	<-gate.release
	return "", nil
}
func (gate *quotaCoreSaveGate) unblock() { gate.stop.Do(func() { close(gate.release) }) }

func quotaCoreLockFixture(t *testing.T) (*Store, *coreauth.Manager, *quotaCoreSaveGate, *coreauth.Auth, QuotaIdentitySource) {
	t.Helper()
	s := openTestStore(t)
	gate := &quotaCoreSaveGate{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(gate.unblock)
	manager := coreauth.NewManager(gate, nil, nil)
	auth := quotaFixtureAuth("claude", "core-lock-index", "core-lock.json", "private-core-token")
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatal(err)
	}
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
	return s, manager, gate, auth, source
}

func holdQuotaCoreLock(t *testing.T, manager *coreauth.Manager, gate *quotaCoreSaveGate, auth *coreauth.Auth) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), coreauth.Result{AuthID: auth.ID, Provider: auth.Provider, Model: "core-lock-model", Success: true})
		close(done)
	}()
	awaitSignal(t, gate.entered)
	return done
}

// A stack barrier proves the isolated operation is actually waiting on the core
// lock; a test cannot pass merely because that goroutine has not been scheduled.
func awaitQuotaCoreLockStack(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data := make([]byte, 1<<20)
		n := runtime.Stack(data, true)
		for _, stack := range strings.Split(string(data[:n]), "\n\n") {
			if strings.Contains(stack, marker) && strings.Contains(stack, "auth.(*Manager).List") && strings.Contains(stack, "sync.(*RWMutex).RLock") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatal("isolated live validation did not reach the held core lock")
}

func primeQuotaIdentityHTTP(t *testing.T, s *Store) {
	t.Helper()
	engine := gin.New()
	s.RegisterRoutes(engine.Group("/stats"))
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats/quota/identities", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"credential_generation"`) || len(s.quotaBindings(false)) != 1 {
		t.Fatalf("identity GET did not immediately publish request-start proof: %d %s", response.Code, response.Body)
	}
}

func TestQuotaCoreIOLockDoesNotBlockSDKAdmissionOrDetach(t *testing.T) {
	s, manager, gate, auth, source := quotaCoreLockFixture(t)
	marked := holdQuotaCoreLock(t, manager, gate, auth)
	bound := make(chan struct{})
	go func() { s.BindQuotaIdentitySource(source); close(bound) }()
	awaitSignal(t, bound) // First binding itself must not touch the live manager.
	previousQueue, previousUsage := redisqueue.Enabled(), redisqueue.UsageStatisticsEnabled()
	defer redisqueue.SetEnabled(previousQueue)
	defer redisqueue.SetUsageStatisticsEnabled(previousUsage)
	redisqueue.SetEnabled(true)
	redisqueue.SetUsageStatisticsEnabled(true)
	redisqueue.PopOldest(10000)
	defer redisqueue.PopOldest(10000)
	stop := redisqueue.ObserveUsage(s.Consume)
	var otherCalls atomic.Int64
	stopOther := redisqueue.ObserveUsage(func(context.Context, []byte) { otherCalls.Add(1) })
	defer func() { gate.unblock(); stop(); stopOther() }()
	record := fixtureRecord("core-lock-first", time.Now())
	record.Provider, record.AuthIndex = auth.Provider, auth.Index
	record.AccessTokenSHA256 = coreauth.AccessTokenSHA256(auth)
	record.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}}
	publishThroughBuiltin(t, record) // Existing SDK last-plugin barrier must finish.
	awaitQuotaCoreLockStack(t, "usagepersist.(*Store).bindQueuedUsageQuota")
	page, err := s.Events(context.Background(), Filter{}, 10, 0)
	if err != nil || page.Total != 1 {
		t.Fatalf("quota verification blocked independent accounting insertion: %+v %v", page, err)
	}
	const overflow = 3
	for i := range usageQueueCapacity + overflow {
		record.RequestID = fmt.Sprintf("core-lock-%d", i)
		publishThroughBuiltin(t, record)
	}
	want := int64(1 + usageQueueCapacity + overflow)
	if otherCalls.Load() != want || len(redisqueue.PopOldest(int(want)+1)) != int(want) {
		t.Fatal("core lock blocked another consumer or changed original queue delivery")
	}
	if len(s.queue) != usageQueueCapacity || s.pendingEvents.Load() != usageQueueCapacity+1 || s.queueOverflows.Load() != overflow {
		t.Fatal("core lock bypassed bounded admission/overflow accounting")
	}
	detached := make(chan struct{})
	go func() { stop(); close(detached) }()
	awaitSignal(t, detached)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- s.Close(ctx) }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the uninterruptible core lock")
	}
	select {
	case <-s.workerDone:
		t.Fatal("test lost the actually blocked independent worker")
	default:
	}
	record.RequestID = "core-lock-after-detach"
	publishThroughBuiltin(t, record)
	if s.pendingEvents.Load() != usageQueueCapacity+1 {
		t.Fatal("detached observer resurrected")
	}
	gate.unblock()
	awaitSignal(t, marked)
	awaitSignal(t, s.workerDone)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.pendingEvents.Load() != 0 || s.droppedEvents.Load() != usageQueueCapacity+overflow {
		t.Fatal("canceled core-lock backlog was not discarded/countable")
	}
}

func TestQuotaCoreIOLockDoesNotBlockOriginalManagementBeforeHandler(t *testing.T) {
	for _, primed := range []bool{false, true} {
		t.Run(fmt.Sprintf("primed=%v", primed), func(t *testing.T) {
			s, manager, gate, auth, source := quotaCoreLockFixture(t)
			s.BindQuotaIdentitySource(source)
			if primed {
				primeQuotaIdentityHTTP(t, s)
			}
			marked := holdQuotaCoreLock(t, manager, gate, auth)
			defer gate.unblock()
			const input = `{"url":"https://example.invalid/anonymous","header":{}}`
			const output = `{"status_code":200,"header":{},"body":"unchanged"}`
			engine := gin.New()
			engine.Use(s.ManagementMiddleware(nil, nil))
			engine.POST("/v0/management/api-call", func(c *gin.Context) {
				body, err := io.ReadAll(c.Request.Body)
				if err != nil || string(body) != input {
					t.Errorf("original body: %s %v", body, err)
				}
				c.Header("X-Original", "preserved")
				c.Data(http.StatusOK, "application/json", []byte(output))
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(input))
			originalBody := request.Body
			done := make(chan struct{})
			go func() { engine.ServeHTTP(response, request); close(done) }()
			awaitSignal(t, done)
			if response.Code != http.StatusOK || response.Body.String() != output || response.Header().Get("X-Original") != "preserved" || request.Body != originalBody || s.pendingEvents.Load() != 0 {
				t.Fatal("pre-handler identity observation changed or delayed anonymous original request")
			}
			gate.unblock()
			awaitSignal(t, marked)
		})
	}
}

func TestQuotaCoreIOLockDoesNotBlockCompletedManagementResponse(t *testing.T) {
	s, manager, gate, auth, source := quotaCoreLockFixture(t)
	s.BindQuotaIdentitySource(source)
	primeQuotaIdentityHTTP(t, s)
	defer gate.unblock()
	const input = `{"auth_index":"core-lock-index","url":"https://api.anthropic.com/api/oauth/usage","header":{"Authorization":"Bearer $TOKEN$"}}`
	const output = `{"status_code":200,"header":{},"body":"{\"five_hour\":{\"utilization\":40}}"}`
	engine := gin.New()
	engine.Use(s.ManagementMiddleware(nil, nil))
	var marked <-chan struct{}
	engine.POST("/v0/management/api-call", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil || string(body) != input {
			t.Errorf("original body: %s %v", body, err)
		}
		marked = holdQuotaCoreLock(t, manager, gate, auth)
		c.Data(http.StatusOK, "application/json", []byte(output))
	})
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(input)))
		close(done)
	}()
	awaitSignal(t, done)
	if response.Code != http.StatusOK || response.Body.String() != output || s.pendingEvents.Load() != 1 {
		t.Fatalf("completed original response waited or changed: %d %s pending=%d", response.Code, response.Body, s.pendingEvents.Load())
	}
	awaitQuotaCoreLockStack(t, "usagepersist.(*Store).mergeQuota")
	gate.unblock()
	awaitSignal(t, marked)
	flushFixture(t, s)
	if snapshots, err := s.Quotas(context.Background()); err != nil || len(snapshots) != 0 {
		t.Fatal("worker relabeled a captured old revision after the held core mutation")
	}
}

func TestQuotaPublishedStaleEvidenceCannotOverwriteOrResetReplacement(t *testing.T) {
	s := openTestStore(t)
	manager := bindQuotaFixtures(t, s, quotaFixtureAuth("codex", "same-index", "same.json", "private-A"))
	bindingA := fixtureBinding(s, "codex", "same-index")
	backend := blockUsageStore(s)
	var released sync.Once
	release := func() { released.Do(func() { close(backend.release) }) }
	defer release()
	s.Consume(context.Background(), fixturePayload(fixtureRecord("hold-worker-before-quota", time.Now())))
	awaitSignal(t, backend.entered)
	current, _ := manager.GetByID("same.json")
	current.Metadata["access_token"] = "private-B"
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	// A targeted authoritative worker/write check deliberately does not prime
	// the advisory publication: retain A to model a lagging explicit identity GET.
	bindingB, ok := s.quotaBinding("codex", "same-index", true)
	if !ok || bindingA.CredentialGeneration == bindingB.CredentialGeneration {
		t.Fatal("replacement fixture failed")
	}
	snapshotB, ok := quota.ParseFetch(bindingB.quotaIdentity(), managementFetchFixture(t), time.Now().UTC())
	if !ok {
		t.Fatal("replacement quota fixture failed")
	}
	if err := s.mergeQuota(context.Background(), snapshotB); err != nil {
		t.Fatal(err)
	}
	if cached, ok := s.quotaBinding("codex", "same-index", false); !ok || cached.CredentialGeneration != bindingA.CredentialGeneration {
		t.Fatal("test failed to retain stale request-start proof")
	}
	s.ObserveQuotaFetch(context.Background(), "codex", "same-index", managementFetchFixture(t), bindingA)
	s.ObserveQuotaReset(context.Background(), "codex", "same-index", bindingA)
	if len(s.queue) != 2 {
		t.Fatal("stale evidence did not reach bounded worker validation")
	}
	release()
	flushFixture(t, s)
	states, err := s.store.Cache(context.Background(), quotaNamespace)
	if err != nil {
		t.Fatal(err)
	}
	var state quotaState
	if err := json.Unmarshal(states[quotaKey("codex", "same-index")], &state); err != nil {
		t.Fatal(err)
	}
	if state.CredentialGeneration != bindingB.CredentialGeneration || state.Snapshot == nil || !state.ResetAt.IsZero() || !state.Snapshot.ObservedAt.Equal(snapshotB.ObservedAt) {
		t.Fatal("lagging A observation/reset overwrote B instead of being refused")
	}
	encoded, err := json.Marshal(s.quotaBindings(false))
	if err != nil || bytes.Contains(encoded, []byte("private-A")) || bytes.Contains(encoded, []byte("private-B")) {
		t.Fatal("publication retained raw credentials")
	}
}

func TestQuotaPublicationCannotResurrectAfterClose(t *testing.T) {
	s, manager, gate, auth, source := quotaCoreLockFixture(t)
	s.BindQuotaIdentitySource(source)
	primeQuotaIdentityHTTP(t, s)
	marked := holdQuotaCoreLock(t, manager, gate, auth)
	defer gate.unblock()
	readDone := make(chan struct{})
	go func() { s.quotaBindings(true); close(readDone) }()
	awaitQuotaCoreLockStack(t, "usagepersist.(*Store).quotaBindings")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(s.quotaBindings(false)) != 0 {
		t.Fatal("close did not withdraw immediate proof")
	}
	gate.unblock()
	awaitSignal(t, marked)
	awaitSignal(t, readDone)
	if len(s.quotaBindings(false)) != 0 || s.quotaSource.Load().published.Load() != nil {
		t.Fatal("late authoritative read resurrected observation eligibility after close")
	}
}
