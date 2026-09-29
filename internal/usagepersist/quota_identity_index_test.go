package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Keep the counting tests independent of filesystem throughput. Mutation hooks
// execute inside the same lock as the update, not before acquiring it.
type quotaIdentityIndexStore struct {
	store
	mu     sync.Mutex
	rows   map[string]json.RawMessage
	locked func(string)
	after  func(string)
}

func (backend *quotaIdentityIndexStore) Cache(context.Context, string) (map[string]json.RawMessage, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	out := make(map[string]json.RawMessage, len(backend.rows))
	for key, raw := range backend.rows {
		out[key] = append(json.RawMessage(nil), raw...)
	}
	return out, nil
}

func (backend *quotaIdentityIndexStore) MutateCache(_ context.Context, _, key string, update func(json.RawMessage) (json.RawMessage, error)) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.locked != nil {
		backend.locked(key)
	}
	raw, err := update(backend.rows[key])
	if backend.after != nil {
		backend.after(key)
	}
	if err == nil && len(raw) > 0 {
		backend.rows[key] = append(json.RawMessage(nil), raw...)
	}
	return err
}

func identityIndexEntry(binding QuotaBinding) QuotaCacheEntry {
	return QuotaCacheEntry{Provider: binding.Provider, Key: binding.Key, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, Revision: binding.Revision, ObservedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), State: json.RawMessage(`{"status":"success","windows":[]}`)}
}

func TestQuotaIdentityIndexLinearValidationCounts(t *testing.T) {
	for _, count := range []int{1, 32, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			backend := &quotaIdentityIndexStore{store: s.store, rows: map[string]json.RawMessage{}}
			s.store = backend
			manager := coreauth.NewManager(nil, nil, nil)
			for i := range count {
				auth := quotaFixtureAuth("codex", fmt.Sprintf("index-%04d", i), fmt.Sprintf("auth-%04d.json", i), fmt.Sprintf("secret-%04d", i))
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
			}
			var validations atomic.Int64
			var lockedKey string
			source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, func(auth *coreauth.Auth) bool {
				validations.Add(1)
				if lockedKey != "" && quotaKey(auth.Provider, auth.Index) != lockedKey {
					t.Errorf("locked mutation for %s validated unrelated %s", lockedKey, auth.Index)
				}
				return true
			})
			s.BindQuotaIdentitySource(source)
			entries := make([]QuotaCacheEntry, 0, count)
			for _, binding := range source.QuotaBindings(false) {
				entry := identityIndexEntry(binding)
				entries = append(entries, entry)
				snapshot := &quota.Snapshot{Provider: binding.Provider, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, ObservedAt: entry.ObservedAt, Windows: []quota.Window{}}
				raw, err := json.Marshal(quotaState{CredentialGeneration: binding.CredentialGeneration, Snapshot: snapshot})
				if err != nil {
					t.Fatal(err)
				}
				backend.rows[quotaKey(binding.Provider, binding.AuthIndex)] = raw
			}
			if len(entries) != count || validations.Load() != 0 {
				t.Fatal("memory-only snapshot performed validation or lost identities")
			}
			var before int64
			backend.locked = func(key string) { lockedKey, before = key, validations.Load() }
			backend.after = func(string) {
				if got := validations.Load() - before; got != 1 {
					t.Errorf("locked recheck validated %d files, want exactly 1", got)
				}
				lockedKey = ""
			}
			if err := s.SaveQuotaCache(ctx, entries); err != nil {
				t.Fatal(err)
			}
			if got := validations.Swap(0); got != int64(2*count) {
				t.Fatalf("batch validated %d files, want N snapshot + N locked = %d", got, 2*count)
			}
			if cached, err := s.QuotaCache(ctx); err != nil || len(cached) != count {
				t.Fatalf("cache count = %d, err = %v", len(cached), err)
			}
			if got := validations.Swap(0); got != int64(count) {
				t.Fatalf("cache GET validated %d files, want N = %d", got, count)
			}
			if snapshots, err := s.Quotas(ctx); err != nil || len(snapshots) != count {
				t.Fatalf("quota count = %d, err = %v", len(snapshots), err)
			}
			if got := validations.Swap(0); got != int64(count) {
				t.Fatalf("quotas GET validated %d files, want N = %d", got, count)
			}
			if got := source.QuotaBindings(true); len(got) != count || validations.Swap(0) != int64(count) {
				t.Fatal("identity GET did not validate exactly once per credential")
			}
			// Single-observation writes also validate only the target before and
			// after acquiring the lock, never all N files.
			entry := entries[0]
			if err := s.mergeQuota(ctx, quota.Snapshot{Provider: entry.Provider, AuthIndex: entry.AuthIndex, CredentialGeneration: entry.CredentialGeneration, Revision: entry.Revision, ObservedAt: entry.ObservedAt}); err != nil {
				t.Fatal(err)
			}
			if got := validations.Swap(0); got != 2 {
				t.Fatalf("single merge validated %d files, want 2", got)
			}
		})
	}
}

func TestQuotaIdentityIndexAmbiguityIsNotResolvedByInvalidCompetitor(t *testing.T) {
	for _, duplicate := range []string{"index", "key"} {
		for _, invalid := range []string{"metadata", "disk", "other-provider"} {
			t.Run(duplicate+"/"+invalid, func(t *testing.T) {
				if duplicate == "key" && invalid == "other-provider" {
					t.Skip("display keys are provider-scoped")
				}
				ctx := context.Background()
				manager := coreauth.NewManager(nil, nil, nil)
				first := quotaFixtureAuth("codex", "first", "first.json", "A")
				second := quotaFixtureAuth("codex", "second", "second.json", "B")
				if duplicate == "index" {
					second.Index = first.Index
				} else {
					second.FileName = first.FileName
				}
				if invalid == "metadata" {
					second.Metadata["unknown_account_selector"] = "private"
				} else if invalid == "other-provider" {
					second.Provider = "unsupported-provider"
				}
				for _, auth := range []*coreauth.Auth{first, second} {
					if _, err := manager.Register(ctx, auth); err != nil {
						t.Fatal(err)
					}
				}
				source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, func(auth *coreauth.Auth) bool { return auth.ID != second.ID })
				for _, disk := range []bool{false, true} {
					if bindings := source.QuotaBindings(disk); len(bindings) != 0 {
						t.Fatalf("ambiguous read escaped: %+v", bindings)
					}
					if _, ok := source.(TargetedQuotaIdentitySource).QuotaBinding("codex", first.Index, disk); ok {
						t.Fatal("targeted lookup accepted ambiguity")
					}
				}
				manager.Remove(ctx, second.ID)
				if bindings := source.QuotaBindings(true); len(bindings) != 1 {
					t.Fatal("removed ambiguity remained cached")
				}
			})
		}
	}
}

func TestQuotaIdentityIndexLockedMutationRechecksCurrent(t *testing.T) {
	// Display-cache writes are bound to durable credential identity. A credential
	// that returns to the same token, or is removed and re-created from it, keeps its
	// generation, so only a different credential must refuse the locked mutation.
	for _, change := range []string{"replacement", "A-B-A", "delete-recreate", "deletion", "duplicate-index", "duplicate-key", "disk-replacement", "manager-replacement"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			manager := coreauth.NewManager(nil, nil, nil)
			auth := quotaFixtureAuth("codex", "index", "same.json", "A")
			if _, err := manager.Register(ctx, auth); err != nil {
				t.Fatal(err)
			}
			var active atomic.Pointer[coreauth.Manager]
			active.Store(manager)
			var diskValid atomic.Bool
			diskValid.Store(true)
			var validations atomic.Int64
			s.BindQuotaIdentitySource(NewQuotaIdentitySource(active.Load, func(*coreauth.Auth) bool { validations.Add(1); return diskValid.Load() }))
			entry := identityIndexEntry(fixtureBinding(s, "codex", "index"))
			validations.Store(0) // Request-start identity acquisition is outside the measured batch.
			entered, release := make(chan struct{}), make(chan struct{})
			backend := &quotaIdentityIndexStore{store: s.store, rows: map[string]json.RawMessage{}, locked: func(string) { close(entered); <-release }}
			s.store = backend
			done := make(chan error, 1)
			go func() { done <- s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}) }()
			<-entered
			if got := validations.Load(); got != 1 {
				t.Errorf("pre-lock batch validations = %d, want 1", got)
			}
			update := func(token string) {
				current, _ := manager.GetByID(auth.ID)
				current.Metadata["access_token"] = token
				if _, err := manager.Update(ctx, current); err != nil {
					t.Error(err)
				}
			}
			sameCredential := false
			switch change {
			case "replacement":
				update("B")
			case "A-B-A":
				update("B")
				update("A")
				sameCredential = true
			case "delete-recreate":
				manager.Remove(ctx, auth.ID)
				if _, err := manager.Register(ctx, quotaFixtureAuth("codex", "index", "same.json", "A")); err != nil {
					t.Error(err)
				}
				sameCredential = true
			case "deletion":
				manager.Remove(ctx, auth.ID)
			case "duplicate-index", "duplicate-key":
				duplicate := quotaFixtureAuth("codex", "other-index", "other.json", "B")
				if change == "duplicate-index" {
					duplicate.Index = auth.Index
				} else {
					duplicate.FileName = auth.FileName
				}
				if _, err := manager.Register(ctx, duplicate); err != nil {
					t.Error(err)
				}
			case "disk-replacement":
				diskValid.Store(false)
			case "manager-replacement":
				active.Store(coreauth.NewManager(nil, nil, nil))
			}
			close(release)
			err := <-done
			if sameCredential {
				if err != nil {
					t.Fatalf("same-credential locked mutation refused: %v", err)
				}
				if len(backend.rows) != 1 {
					t.Fatal("same-credential locked mutation persisted no row")
				}
				return
			}
			if !errors.Is(err, ErrQuotaIdentity) {
				t.Fatalf("stale locked mutation returned %v", err)
			}
			if len(backend.rows) != 0 {
				t.Fatal("stale mutation persisted a row")
			}
		})
	}
}

func TestQuotaIdentityIndexReplacementDuringValidation(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		for _, change := range []string{"replacement", "deletion", "duplicate-index", "duplicate-key"} {
			t.Run(fmt.Sprintf("targeted=%t/%s", targeted, change), func(t *testing.T) {
				ctx := context.Background()
				manager := coreauth.NewManager(nil, nil, nil)
				auth := quotaFixtureAuth("codex", "index", "same.json", "A")
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, func(*coreauth.Auth) bool { close(entered); <-release; return true })
				done := make(chan bool, 1)
				go func() {
					if targeted {
						_, ok := source.(TargetedQuotaIdentitySource).QuotaBinding("codex", "index", true)
						done <- ok
					} else {
						done <- len(source.QuotaBindings(true)) != 0
					}
				}()
				<-entered
				switch change {
				case "replacement":
					current, _ := manager.GetByID(auth.ID)
					current.Metadata["access_token"] = "B"
					if _, err := manager.Update(ctx, current); err != nil {
						t.Error(err)
					}
				case "deletion":
					manager.Remove(ctx, auth.ID)
				default:
					other := quotaFixtureAuth("codex", "other", "other.json", "B")
					if change == "duplicate-index" {
						other.Index = auth.Index
					} else {
						other.FileName = auth.FileName
					}
					if _, err := manager.Register(ctx, other); err != nil {
						t.Error(err)
					}
				}
				close(release)
				if <-done {
					t.Fatal("binding invalidated during disk validation escaped")
				}
			})
		}
	}
}

func TestQuotaIdentityIndexPublishedAdmissionDoesNotValidateStorage(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("claude", "index", "same.json", "A")
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var allowed atomic.Bool
	allowed.Store(true)
	s := &Store{queue: make(chan queuedUsage, usageQueueCapacity), workerCtx: context.Background()}
	s.BindQuotaIdentitySource(NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, func(*coreauth.Auth) bool { calls.Add(1); return allowed.Load() }))
	if calls.Load() != 0 || len(s.quotaBindings(false)) != 0 {
		t.Fatal("first source binding queried live state or invented a publication")
	}
	binding := fixtureBinding(s, "claude", "index") // Explicit identity read primes proof.
	calls.Store(0)
	allowed.Store(false)
	if _, ok := s.observationBinding("claude", "index", []QuotaBinding{binding}); !ok {
		t.Fatal("published original observer proof refused")
	}
	if !s.validQuotaReset(&queuedQuotaReset{Provider: binding.Provider, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, Revision: binding.Revision}, false) {
		t.Fatal("published reset admission refused")
	}
	record := fixtureRecord("published-only", time.Now())
	record.Provider, record.AuthIndex = "claude", auth.Index
	record.AccessTokenSHA256 = coreauth.AccessTokenSHA256(auth)
	record.ResponseHeaders = map[string][]string{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}}
	s.Consume(context.Background(), fixturePayload(record))
	item := <-s.queue
	if item.Quota == nil || item.Quota.Revision != "" || len(s.quotaBindings(false)) != 1 || calls.Load() != 0 {
		t.Fatal("admission performed live validation or stamped a current revision")
	}
	s.bindQueuedUsageQuota(context.Background(), &item)
	if item.Quota != nil || calls.Load() != 1 {
		t.Fatal("isolated worker did not refuse failed authoritative validation")
	}
}

func TestQuotaIdentityIndexBoundsAndFallbackAmbiguity(t *testing.T) {
	binding := QuotaBinding{Provider: "codex", AuthIndex: "index", Key: "same.json", CredentialGeneration: "generation", Revision: "revision"}
	if _, ok := indexQuotaBindings([]QuotaBinding{binding, binding, binding}).binding("codex", "index"); ok {
		t.Fatal("third duplicate resurrected an ambiguous index")
	}
	other := binding
	other.AuthIndex = "other"
	if _, ok := indexQuotaBindings([]QuotaBinding{binding, other}).binding("codex", "index"); ok {
		t.Fatal("duplicate display key accepted")
	}
	if got := indexQuotaBindings(make([]QuotaBinding, maxQuotaIdentities+1)); got != nil {
		t.Fatal("unbounded source accepted")
	}
	if got := newQuotaAuthCatalog(make([]*coreauth.Auth, maxQuotaIdentities+1)); got.byIndex != nil || got.keys != nil {
		t.Fatal("unbounded catalog accepted")
	}
	index := indexQuotaBindings([]QuotaBinding{binding})
	for _, candidate := range []struct{ provider, index, generation, revision string }{
		{"claude", "index", "generation", "revision"},
		{"codex", "unknown", "generation", "revision"},
		{"codex", "index", "", "revision"},
		{"codex", "index", "stale", "revision"},
		{"codex", "index", "generation", "stale"},
	} {
		if index.valid(candidate.provider, candidate.index, candidate.generation, candidate.revision) {
			t.Fatal("indexed lookup weakened an identity fence")
		}
	}
}
