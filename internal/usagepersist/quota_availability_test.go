package usagepersist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// quotaCoreSaveGate blocks the core credential store while the Manager holds its
// write lock. A caller that joins a live Manager read therefore stalls until the
// gate is released.
type quotaCoreSaveGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	stop    sync.Once
}

func (gate *quotaCoreSaveGate) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}
func (gate *quotaCoreSaveGate) Delete(context.Context, string) error { return nil }
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
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("claude", "core-lock-index", "core-lock.json", "private-core-token")
	// Register with the real (never blocking) store, then swap in the gate so the
	// next Manager write blocks while holding the manager lock.
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	manager.SetStore(gate)
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
	return s, manager, gate, auth, source
}

// holdQuotaCoreLock joins a real Manager write that persists through the gate, so
// the manager holds its lock while the gate is blocked.
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

// primeQuotaIdentity publishes request-start evidence through the explicit
// add-on identity read, exactly as the identity endpoint does.
func primeQuotaIdentity(t *testing.T, s *Store) {
	t.Helper()
	if bindings := s.publishQuotaBindings(); len(bindings) != 1 {
		t.Fatalf("identity GET did not immediately publish request-start proof: %d", len(bindings))
	}
}

// TestQuotaCoreLockBlocksLiveProjectionOnly proves the isolated call is really
// waiting on the core lock, while binding a source does not query it at all.
func TestQuotaCoreLockBlocksLiveProjectionOnly(t *testing.T) {
	s, manager, gate, auth, source := quotaCoreLockFixture(t)
	marked := holdQuotaCoreLock(t, manager, gate, auth)
	defer gate.unblock()
	bound := make(chan struct{})
	go func() {
		s.BindQuotaIdentitySource(source)
		close(bound)
	}()
	// Binding must never touch the live manager, even while it is locked.
	awaitSignal(t, bound)
	// There is deliberately no publication to read yet, so an original handler
	// simply skips observation instead of querying a locked manager.
	if len(s.publishedQuotaBindings()) != 0 {
		t.Fatal("binding invented a publication")
	}
	projected := make(chan []QuotaBinding, 1)
	go func() { projected <- s.publishQuotaBindings() }()
	select {
	case <-projected:
		t.Fatal("live projection did not wait for the held core lock")
	case <-time.After(100 * time.Millisecond):
	}
	gate.unblock()
	awaitSignal(t, marked)
	select {
	case bindings := <-projected:
		if len(bindings) != 1 || bindings[0].Account == "" {
			t.Fatalf("projection after release: %+v", bindings)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("projection never completed after the core lock was released")
	}
}

// TestQuotaCoreIOLockDoesNotBlockOriginalManagement covers the original
// management path: with a primed publication and a locked core manager, an
// anonymous original request is neither changed nor delayed, and a request that
// carries the transient index is only queued, never resolved against the lock.
func TestQuotaCoreIOLockDoesNotBlockOriginalManagement(t *testing.T) {
	for _, primed := range []bool{false, true} {
		t.Run(map[bool]string{false: "primed=false", true: "primed=true"}[primed], func(t *testing.T) {
			s, manager, gate, auth, source := quotaCoreLockFixture(t)
			s.BindQuotaIdentitySource(source)
			if primed {
				primeQuotaIdentity(t, s)
			}
			marked := holdQuotaCoreLock(t, manager, gate, auth)
			defer gate.unblock()
			const input = `{"url":"https://example.invalid/anonymous","header":{}}`
			const output = `{"status_code":200,"header":{},"body":"unchanged"}`
			engine := gin.New()
			engine.Use(s.ManagementMiddleware())
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

// TestQuotaCoreIOLockQueuesCompletedManagementResponse proves a completed
// original response is returned immediately while the derived observation is
// only retained and applied later by the isolated worker.
func TestQuotaCoreIOLockQueuesCompletedManagementResponse(t *testing.T) {
	s, manager, gate, auth, source := quotaCoreLockFixture(t)
	s.BindQuotaIdentitySource(source)
	primeQuotaIdentity(t, s)
	defer gate.unblock()
	const input = `{"auth_index":"core-lock-index","url":"https://api.anthropic.com/api/oauth/usage","header":{"Authorization":"Bearer $TOKEN$"}}`
	const output = `{"status_code":200,"header":{},"body":"{\"five_hour\":{\"utilization\":40}}"}`
	engine := gin.New()
	engine.Use(s.ManagementMiddleware())
	var marked <-chan struct{}
	resolvedInHandler := false
	engine.POST("/v0/management/api-call", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil || string(body) != input {
			t.Errorf("original body: %s %v", body, err)
		}
		// The handler itself must not need the core lock to answer.
		resolvedInHandler = true
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
	if !resolvedInHandler {
		t.Fatal("original handler did not run")
	}
	if response.Code != http.StatusOK || response.Body.String() != output || s.pendingEvents.Load() != 1 {
		t.Fatalf("completed original response waited or changed: %d %s pending=%d", response.Code, response.Body, s.pendingEvents.Load())
	}
	gate.unblock()
	awaitSignal(t, marked)
	flushFixture(t, s)
	// The queued observation still lands under the account it was captured for.
	snapshots, err := s.Quotas(context.Background())
	if err != nil || len(snapshots) != 1 || snapshots[0].Account == "" {
		t.Fatalf("queued observation lost its account: %+v %v", snapshots, err)
	}
}

// TestQuotaPublishedStaleEvidenceCannotOverwriteAnotherAccount keeps the stale
// protection under test after the generation/revision fence was removed. Stale
// write protection is observation ordering plus account keying: an observation
// captured for one account can never overwrite another account's newer state.
func TestQuotaPublishedStaleEvidenceCannotOverwriteAnotherAccount(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	manager := bindQuotaFixtures(t, s, quotaFixtureAuth("codex", "same-index", "same.json", "private-A"))
	stale := fixtureBinding(s, "codex", "same-index")
	// The slot is taken over by a different account: a different durable identity.
	current, _ := manager.GetByID("same.json")
	current.Metadata["access_token"] = "private-B"
	current.Metadata["email"] = "second@example.invalid"
	if _, err := manager.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	fresh := fixtureBinding(s, "codex", "same-index")
	if fresh.Account == stale.Account {
		t.Fatalf("replacement fixture failed: %+v -> %+v", stale, fresh)
	}
	// The advisory publication now describes the replacement, while the caller
	// that started before the handover still holds the stale binding.
	if published, ok := s.publishedQuotaBinding("codex", "same-index"); !ok || published.Account != fresh.Account {
		t.Fatalf("publication was not refreshed to the replacement: %+v", published)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := quota.Snapshot{Provider: "codex", Account: fresh.Account, AccountKind: fresh.AccountKind,
		Source: quota.SourceFetch, ObservedAt: base.Add(time.Minute), Plan: "newer-plan",
		Windows: []quota.Window{{ID: "primary", UsedPercent: floatPtr(60), ObservedAt: base.Add(time.Minute), Source: quota.SourceFetch}}}
	staleSnapshot := quota.Snapshot{Provider: "codex", Account: stale.Account, AccountKind: stale.AccountKind,
		Source: quota.SourceFetch, ObservedAt: base, Plan: "stale-plan",
		Windows: []quota.Window{{ID: "primary", UsedPercent: floatPtr(5), ObservedAt: base, Source: quota.SourceFetch}}}
	backend := blockUsageStore(s)
	var released sync.Once
	release := func() { released.Do(func() { close(backend.release) }) }
	defer release()
	s.Consume(ctx, fixturePayload(fixtureRecord("hold-worker", base)))
	awaitSignal(t, backend.entered)
	// Refresh the replacement account first so its observation is applied ahead
	// of the stale work that was captured before the handover.
	s.mergeQuota(ctx, newer)
	// Model a lagging explicit identity read: retain the pre-handover bindings.
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &staleSnapshot})
	s.observeQuotaResetAt(stale, base.Add(-time.Minute))
	release()
	flushFixture(t, s)

	newerState := managementStateFor(t, s, "codex", fresh.Account)
	if newerState.Snapshot == nil || newerState.Snapshot.Account != fresh.Account || newerState.Snapshot.Plan != "newer-plan" || !newerState.Snapshot.ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("replacement account state was overwritten by stale evidence: %+v", newerState)
	}
	if !newerState.ResetAt.IsZero() {
		t.Fatalf("stale reset advanced the replacement account's cutoff: %+v", newerState)
	}
	staleState := managementStateFor(t, s, "codex", stale.Account)
	if staleState.Snapshot == nil || staleState.Snapshot.Account != stale.Account || staleState.Snapshot.Plan != "stale-plan" {
		t.Fatalf("stale observation did not stay in its own account row: %+v", staleState)
	}
	if !staleState.ResetAt.Equal(base.Add(-time.Minute)) {
		t.Fatalf("stale reset did not land in its own account row: %+v", staleState)
	}
	// No persisted row may carry a raw credential value.
	encoded, err := json.Marshal(s.publishedQuotaBindings())
	if err != nil || bytes.Contains(encoded, []byte("private-A")) || bytes.Contains(encoded, []byte("private-B")) {
		t.Fatal("publication retained raw credentials")
	}
}

func TestQuotaPublicationCannotResurrectAfterClose(t *testing.T) {
	s, manager, gate, auth, source := quotaCoreLockFixture(t)
	s.BindQuotaIdentitySource(source)
	// Prime the publication first: the explicit identity read must not need the
	// core lock that the next step holds.
	primeQuotaIdentity(t, s)
	marked := holdQuotaCoreLock(t, manager, gate, auth)
	defer gate.unblock()
	// A live projection is now in flight and blocked on the held core lock, so a
	// late completion cannot resurrect eligibility after close.
	readDone := make(chan struct{})
	go func() { s.publishQuotaBindings(); close(readDone) }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(s.publishedQuotaBindings()) != 0 {
		t.Fatal("close did not withdraw immediate proof")
	}
	gate.unblock()
	awaitSignal(t, marked)
	awaitSignal(t, readDone)
	if len(s.publishedQuotaBindings()) != 0 || s.quotaSource.Load().published.Load() != nil {
		t.Fatal("late authoritative read resurrected observation eligibility after close")
	}
}
