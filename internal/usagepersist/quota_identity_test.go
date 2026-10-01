package usagepersist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaFixtureAuth publishes the account facts a live credential exposes: the
// provider's single account property (email, or device_id for kimi). The index
// is transient: it addresses the credential in the manager and in requests, and
// it is never durable identity.
func quotaFixtureAuth(provider, index, name, token string) *coreauth.Auth {
	account := index + "@example.invalid"
	return &coreauth.Auth{ID: name, Index: index, FileName: name, Provider: provider,
		Metadata: map[string]any{"access_token": token, "email": account, "device_id": account}}
}

// quotaFixtureAuthUnlabeled models a credential that exposes no account property.
// It is valid and is grouped by provider alone.
func quotaFixtureAuthUnlabeled(provider, index, name, token string) *coreauth.Auth {
	return &coreauth.Auth{ID: name, Index: index, FileName: name, Provider: provider, Metadata: map[string]any{"access_token": token}}
}

func bindQuotaFixtures(t *testing.T, s *Store, auths ...*coreauth.Auth) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	s.BindQuotaIdentitySource(NewQuotaIdentitySource(func() *coreauth.Manager { return manager }))
	// Model the explicit add-on identity GET that primes optional observation.
	if bindings := s.publishQuotaBindings(); len(bindings) != len(auths) {
		t.Fatalf("quota binding publication projected %d of %d fixtures", len(bindings), len(auths))
	}
	return manager
}

func bindManagementFixtures(t *testing.T, s *Store) *coreauth.Manager {
	return bindQuotaFixtures(t, s,
		quotaFixtureAuth("codex", "account", "account.json", "fixture-secret"),
		quotaFixtureAuth("codex", "later", "later.json", "later-secret"))
}

// fixtureBinding resolves one fixture credential's transient index to the
// immutable binding a trusted caller captures at request start. Like the
// identity endpoint, it performs the authoritative live projection and
// publishes it, so a caller that mutates a fixture afterwards captures the
// older binding itself.
func fixtureBinding(s *Store, provider, index string) QuotaBinding {
	binding, ok := indexQuotaBindings(s.publishQuotaBindings()).binding(provider, index)
	if !ok {
		panic("test requires registered immutable quota source fixture")
	}
	return binding
}

// Direct observer fixtures model a trusted caller holding source-time proof.
// Middleware tests exercise actual pre-handler acquisition and token placement.
func (s *Store) fixtureObserveAPICall(ctx context.Context, provider, index, url string, status int, header http.Header, body []byte) {
	s.ObserveAPICall(ctx, fixtureBinding(s, provider, index), url, status, header, body)
}
func (s *Store) fixtureObserveQuotaFetch(ctx context.Context, provider, index string, response pluginapi.QuotaFetchResponse) {
	s.ObserveQuotaFetch(ctx, fixtureBinding(s, provider, index), response)
}
func (s *Store) fixtureObserveQuotaReset(ctx context.Context, provider, index string) {
	s.ObserveQuotaReset(ctx, fixtureBinding(s, provider, index))
}

func bindCacheFixture(s *Store, index string, entry QuotaCacheEntry) QuotaCacheEntry {
	binding := fixtureBinding(s, entry.Provider, index)
	entry.Key = binding.Key
	entry.Account, entry.AccountKind = binding.Account, binding.AccountKind
	return entry
}

// TestQuotaIdentityAccountFactsReplaceTokenIdentity is the headline behavior: the
// durable identity is (provider, account), and an ordinary provider token refresh
// rotates the access token without changing it. A credential with no account
// property is still accepted, grouped by provider alone.
func TestQuotaIdentityAccountFactsReplaceTokenIdentity(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "antigravity", "kimi", "devin", "meta", "xai"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			manager := coreauth.NewManager(nil, nil, nil)
			auth := quotaFixtureAuth(provider, "same-index", "same.json", "secret-A")
			registered, err := manager.Register(ctx, auth)
			if err != nil {
				t.Fatal(err)
			}
			source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
			original := source.QuotaBindings()[0]
			if original.Account != "same-index@example.invalid" || original.AccountKind != accountProperty(provider) {
				t.Fatalf("account facts: %+v", original)
			}
			// The provider refreshes its access token for the same account.
			changed := registered.Clone()
			changed.Metadata["access_token"] = "secret-B"
			if _, err = manager.Update(ctx, changed); err != nil {
				t.Fatal(err)
			}
			rotated := source.QuotaBindings()[0]
			if rotated.Account != original.Account || rotated.AccountKind != original.AccountKind || rotated.Key != original.Key {
				t.Fatalf("token refresh changed durable identity: %+v -> %+v", original, rotated)
			}
			// A different account, even in the same slot, is a different identity.
			changed.Metadata[accountProperty(provider)] = "other-same-index@example.invalid"
			if _, err = manager.Update(ctx, changed); err != nil {
				t.Fatal(err)
			}
			replaced := source.QuotaBindings()[0]
			if replaced.Account == original.Account {
				t.Fatalf("different account reused identity: %+v", replaced)
			}
			// A restart re-projects the same live credential to the same identity.
			restarted := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }).QuotaBindings()[0]
			if restarted.Account != replaced.Account || restarted.AccountKind != replaced.AccountKind {
				t.Fatal("restart changed durable identity")
			}
			manager.Remove(ctx, auth.ID)
			if len(source.QuotaBindings()) != 0 {
				t.Fatal("deleted identity remains")
			}
			// A credential without an account property is still accepted.
			unlabeled := &coreauth.Auth{ID: "unlabeled.json", FileName: "unlabeled.json", Provider: provider, Metadata: map[string]any{"access_token": "secret"}}
			binding, ok := ProjectQuotaBinding(unlabeled)
			if !ok || binding.Account != "" || binding.AccountKind != "" {
				t.Fatalf("credential with no account property was rejected: %+v %v", binding, ok)
			}
		})
	}
}

// TestQuotaIdentitySameAccountDifferentTokensShareOneRow proves the grouping rule:
// two credentials that publish the same account are one account row, and two
// different accounts never mix.
func TestQuotaIdentitySameAccountDifferentTokensShareOneRow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	shared := quotaFixtureAuth("codex", "first-slot", "first.json", "secret-A")
	shared.Metadata["email"] = "shared@example.invalid"
	secondSlot := quotaFixtureAuth("codex", "second-slot", "second.json", "secret-B")
	secondSlot.Metadata["email"] = "shared@example.invalid"
	thirdAuth := quotaFixtureAuth("codex", "third-slot", "third.json", "secret-C")
	bindQuotaFixtures(t, s, shared, secondSlot, thirdAuth)
	third := fixtureBinding(s, "codex", "third-slot")

	first := fixtureBinding(s, "codex", "first-slot")
	twin := fixtureBinding(s, "codex", "second-slot")
	if first.Account != twin.Account || first.Key == twin.Key {
		t.Fatalf("same-account fixtures differ: %+v %+v", first, twin)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, binding := range []QuotaBinding{first, twin} {
		if err := s.mergeQuota(ctx, quota.Snapshot{Provider: binding.Provider, Account: binding.Account, AccountKind: binding.AccountKind,
			Source: quota.SourceFetch, ObservedAt: at, Windows: []quota.Window{{ID: "primary", UsedPercent: floatPtr(10), ObservedAt: at, Source: quota.SourceFetch}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.mergeQuota(ctx, quota.Snapshot{Provider: third.Provider, Account: third.Account, AccountKind: third.AccountKind,
		Source: quota.SourceFetch, ObservedAt: at, Windows: []quota.Window{{ID: "primary", UsedPercent: floatPtr(90), ObservedAt: at, Source: quota.SourceFetch}}}); err != nil {
		t.Fatal(err)
	}
	states, err := s.ListCache(ctx, quotaNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || len(states[quotaStoreKey("codex", "shared@example.invalid")]) == 0 || len(states[quotaStoreKey("codex", third.Account)]) == 0 {
		t.Fatalf("account rows: %v", sortedKeys(states))
	}
	history, err := s.quotaHistory(ctx, "codex", third.Account)
	if err != nil || len(history.Observations) != 1 || history.Observations[0].Windows["primary"] != 90 {
		t.Fatalf("unrelated account history: %+v %v", history, err)
	}
	sharedHistory, err := s.quotaHistory(ctx, "codex", "shared@example.invalid")
	if err != nil || len(sharedHistory.Observations) != 1 {
		t.Fatalf("shared account history: %+v %v", sharedHistory, err)
	}
}

// TestQuotaIdentityProjectionsAndConflicts pins the projection contract: the
// access token is never identity, so a token rotation or an edited token alias
// cannot revoke a credential, while a changed account property is a replacement.
func TestQuotaIdentityProjectionsAndConflicts(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "antigravity", "kimi", "devin", "meta", "xai"} {
		t.Run(provider, func(t *testing.T) {
			auth := quotaFixtureAuth(provider, "idx", "file.json", "secret")
			plain, ok := ProjectQuotaBinding(auth)
			if !ok {
				t.Fatal("ordinary OAuth unsupported")
			}
			kind := accountProperty(provider)
			// The access token rotates, or an unrelated token alias is edited: the
			// published account tells two credentials apart, not the token.
			for _, metadata := range []map[string]any{
				{"access_token": "rotated", kind: "idx@example.invalid"},
				{"accessToken": "secret", kind: "idx@example.invalid"},
				{"token": map[string]any{"access_token": "secret"}, kind: "idx@example.invalid"},
				{"Token": map[string]string{"accessToken": "secret"}, kind: "idx@example.invalid"},
				{"access_token": "secret", "accessToken": "different", kind: "idx@example.invalid"},
				{"access_token": 123, kind: "idx@example.invalid"},
			} {
				auth.Metadata = metadata
				binding, valid := ProjectQuotaBinding(auth)
				if !valid || binding.Account != plain.Account || binding.AccountKind != plain.AccountKind {
					t.Fatalf("token rotation changed durable identity: %+v", binding)
				}
			}
			// A credential without the account property is exported with empty facts
			// and stays a valid, provider-grouped credential.
			for _, metadata := range []map[string]any{{}, {"access_token": "secret", "account_id": "unrelated"}, {"access_token": "secret", "account_id": map[string]any{"id": "new"}}} {
				auth.Metadata = metadata
				binding, valid := ProjectQuotaBinding(auth)
				if !valid || binding.Account != "" || binding.AccountKind != "" {
					t.Fatalf("missing account property was not exported as an empty fact: %+v %v", binding, valid)
				}
			}
			// A different account property value is a different credential.
			auth.Metadata = map[string]any{"access_token": "secret", kind: "other@example.invalid"}
			different, valid := ProjectQuotaBinding(auth)
			if !valid || different.Account == plain.Account {
				t.Fatal("different account reused identity")
			}
			for _, field := range []string{"api_key", "session_token"} {
				auth.Metadata = map[string]any{field: "secret"}
				if _, ok = ProjectQuotaBinding(auth); !ok {
					t.Fatalf("%s unsupported", field)
				}
			}
			if provider == "meta" {
				auth.Metadata = map[string]any{"dca_token": "dca:secret"}
				if _, ok = ProjectQuotaBinding(auth); !ok {
					t.Fatal("DCA missing")
				}
			}
			// An account fact that carries the storage-key separator is refused
			// rather than silently re-keyed.
			auth.Metadata = map[string]any{"access_token": "secret", kind: "bad:account"}
			if _, ok = ProjectQuotaBinding(auth); ok {
				t.Fatal("unkeyable account fact accepted")
			}
		})
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, name := range []string{"one.json", "two.json"} {
		_, err := manager.Register(context.Background(), quotaFixtureAuth("codex", "duplicate", name, "secret"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(NewQuotaIdentitySource(func() *coreauth.Manager { return manager }).QuotaBindings()) != 0 {
		t.Fatal("ambiguous duplicate index bound")
	}
}

func TestQuotaIdentityTokenRotationKeepsAccountCache(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	s, err := Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	auth := quotaFixtureAuth("codex", "account", "same.json", "secret-A")
	manager := bindQuotaFixtures(t, s, auth)
	binding := fixtureBinding(s, "codex", "account")
	entry := bindCacheFixture(s, "account", QuotaCacheEntry{Provider: "codex", ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success","windows":[],"planType":"old-plan"}`)})
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	snapshot := quota.Snapshot{Provider: "codex", Account: binding.Account, AccountKind: binding.AccountKind,
		Source: quota.SourceFetch, ObservedAt: at, Plan: "old-plan",
		Windows: []quota.Window{{ID: "primary", UsedPercent: floatPtr(25), ObservedAt: at, Source: quota.SourceFetch}}}
	if err = s.mergeQuota(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	// A token refresh for the same account keeps the stored state...
	auth.Metadata["access_token"] = "secret-B"
	if _, err = manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if cached, _ := s.QuotaCache(ctx); len(cached) != 1 {
		t.Fatal("token refresh discarded the same account's displayed state")
	}
	if snapshots, _ := s.Quotas(ctx); len(snapshots) != 1 {
		t.Fatal("token refresh discarded the same account's snapshot")
	}
	// ...while a different account in the same slot is a different row. The old
	// account keeps its own state: it is a durable identity of its own, and the
	// replacement never inherits it.
	auth.Metadata["email"] = "replacement@example.invalid"
	if _, err = manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	current := fixtureBinding(s, "codex", "account")
	if current.Account != "replacement@example.invalid" {
		t.Fatalf("replacement fixture: %+v", current)
	}
	previous := managementStateFor(t, s, "codex", "account@example.invalid")
	if previous.Snapshot == nil || previous.Snapshot.Plan != "old-plan" || len(previous.UIEntries) != 1 {
		t.Fatalf("the previous account lost its own state: %+v", previous)
	}
	empty := accountRow(t, s, "codex", current.Account)
	if empty.Snapshot != nil || !empty.ResetAt.IsZero() || len(empty.UIEntries) != 0 {
		t.Fatalf("the replacement account inherited the previous account's state: %+v", empty)
	}
	// A future reset cutoff in the old account's row must not carry over.
	if err = s.store.MutateCache(ctx, quotaNamespace, quotaStoreKey("codex", "account@example.invalid"), func(raw json.RawMessage) (json.RawMessage, error) {
		var state quotaState
		_ = json.Unmarshal(raw, &state)
		state.ResetAt = time.Now().Add(time.Hour)
		return json.Marshal(state)
	}); err != nil {
		t.Fatal(err)
	}
	fresh := bindCacheFixture(s, "account", QuotaCacheEntry{Provider: "codex", ObservedAt: time.Now().Add(-time.Second), State: json.RawMessage(`{"status":"success","windows":[]}`)})
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{fresh}); err != nil {
		t.Fatal(err)
	}
	state := managementStateFor(t, s, "codex", current.Account)
	if state.Snapshot != nil || !state.ResetAt.IsZero() || len(state.UIEntries) != 1 {
		t.Fatal("the replacement account inherited the old account's envelope")
	}
	if previous := accountRow(t, s, "codex", "account@example.invalid"); previous.Snapshot == nil || len(previous.UIEntries) != 1 || !previous.ResetAt.After(time.Now()) {
		t.Fatalf("the old account's own envelope was disturbed: %+v", previous)
	}
	// A display-cache PUT that was captured before the handover is accepted into
	// the row it belongs to. There is no credential fence to return a conflict
	// for, so the guarantee under test is that it can never reach the
	// replacement's row, not that the request itself fails.
	engine := gin.New()
	s.RegisterRoutes(engine.Group("/stats"))
	staleBody, err := json.Marshal(quotaCacheEnvelope{Entries: []QuotaCacheEntry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	response := middlewareRequest(t, engine, http.MethodPut, "/stats/quota/cache", staleBody)
	if response.Code != http.StatusOK {
		t.Fatalf("stale PUT status %d: %s", response.Code, response.Body)
	}
	if state := managementStateFor(t, s, "codex", current.Account); state.Snapshot != nil || len(state.UIEntries) != 1 || !state.ResetAt.IsZero() {
		t.Fatalf("stale PUT reached the replacement row: %+v", state)
	}
	if old := managementStateFor(t, s, "codex", "account@example.invalid"); len(old.UIEntries) != 1 {
		t.Fatalf("stale PUT lost its own row: %+v", old)
	}
	// A reset recorded for the old account cannot clear the replacement.
	s.observeQuotaResetAt(QuotaBinding{Provider: "codex", Account: "account@example.invalid"}, time.Now())
	flushFixture(t, s)
	state = managementStateFor(t, s, "codex", current.Account)
	if len(state.UIEntries) != 1 {
		t.Fatalf("reset of the old account cleared the replacement: %+v", state)
	}
	cached, _ := s.QuotaCache(ctx)
	if len(cached) != 2 {
		t.Fatalf("both accounts must keep their own display entry: %+v", cached)
	}
	accounts := map[string]bool{}
	for _, record := range cached {
		accounts[record.Account] = true
	}
	if !accounts[current.Account] || !accounts["account@example.invalid"] {
		t.Fatalf("display entries mixed accounts: %+v", accounts)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	persisted := string(raw)
	// Inverted from the deleted rule "an email never reaches storage": the
	// account fact is durable on purpose, while every credential stays out.
	for _, expected := range []string{"account@example.invalid", "replacement@example.invalid"} {
		if !strings.Contains(persisted, expected) {
			t.Fatalf("journal lost the durable account fact %q", expected)
		}
	}
	for _, secret := range []string{"secret-A", "secret-B", "access_token"} {
		if strings.Contains(persisted, secret) {
			t.Fatalf("journal leaked %q", secret)
		}
	}
}

// accountRow reads one account's durable quota state. An account with no row is
// the zero state, exactly as the store reports it.
func accountRow(t *testing.T, s *Store, provider, account string) quotaState {
	t.Helper()
	values, err := s.ListCache(context.Background(), quotaNamespace)
	if err != nil {
		t.Fatal(err)
	}
	raw := values[quotaStoreKey(provider, account)]
	if len(raw) == 0 {
		return quotaState{}
	}
	var state quotaState
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode quota row %q: %v", quotaStoreKey(provider, account), err)
	}
	return state
}

// managementStateFor reads one account's durable quota state row and requires it
// to exist.
func managementStateFor(t *testing.T, s *Store, provider, account string) quotaState {
	t.Helper()
	values, err := s.ListCache(context.Background(), quotaNamespace)
	if err != nil {
		t.Fatal(err)
	}
	key := quotaStoreKey(provider, account)
	raw, exists := values[key]
	if !exists {
		t.Fatalf("missing quota row %q; rows=%v", key, sortedKeys(values))
	}
	var state quotaState
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode quota row %q: %v", key, err)
	}
	return state
}

// TestQuotaIdentityDurableCacheSurvivesRuntimeDrift reproduces the reported
// regression: a manual quota refresh captures request-start identity, an unrelated
// auth-file write occurs while the credential stays the same, and the captured
// observation arrives afterwards. The durable display cache is bound to the
// account, not to a runtime fence, so it must persist.
func TestQuotaIdentityDurableCacheSurvivesRuntimeDrift(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	auth := quotaFixtureAuth("claude", "account-1", "auth.json", "source-token")
	manager := bindQuotaFixtures(t, s, auth)
	captured := fixtureBinding(s, "claude", "account-1")
	entry := bindCacheFixture(s, "account-1", QuotaCacheEntry{Provider: "claude", ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success","windows":[]}`)})
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatalf("fresh observation rejected: %v", err)
	}
	// Any auth-file write rewrites the credential record.
	if _, err := manager.Update(ctx, auth.Clone()); err != nil {
		t.Fatal(err)
	}
	current := fixtureBinding(s, "claude", "account-1")
	if current.Account != captured.Account || current.AccountKind != captured.AccountKind {
		t.Fatal("benign credential activity changed durable identity")
	}
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatalf("captured request-start identity rejected after credential activity: %v", err)
	}
	if cached, err := s.QuotaCache(ctx); err != nil || len(cached) != 1 {
		t.Fatalf("displayed state lost after credential activity: %#v %v", cached, err)
	}
	// An ordinary provider refresh rotates the access token for the same account.
	// The displayed state must survive it, and a later observation must still be
	// accepted: this is the reported regression, where a card reverted on the
	// next page load because the credential's token had rotated.
	auth.Metadata["access_token"] = "rotated-token"
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if rotated := fixtureBinding(s, "claude", "account-1"); rotated.Account != captured.Account {
		t.Fatal("token rotation changed durable identity")
	}
	if cached, err := s.QuotaCache(ctx); err != nil || len(cached) != 1 {
		t.Fatalf("displayed state lost after token rotation: %#v %v", cached, err)
	}
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatalf("observation after token rotation rejected: %v", err)
	}
	// A different account in the same file is a different row, so an observation
	// captured for the old account can never reach the replacement's state.
	auth.Metadata["email"] = "other@example.invalid"
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	replaced := fixtureBinding(s, "claude", "account-1")
	if replaced.Account == captured.Account {
		t.Fatal("replacement fixture did not change the account")
	}
	// Re-uploading the pre-replacement capture lands in the old account's own
	// row; the replacement row stays empty.
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatalf("pre-replacement capture must not fail the replacement row: %v", err)
	}
	replacementState := accountRow(t, s, "claude", replaced.Account)
	if replacementState.Snapshot != nil || len(replacementState.UIEntries) != 0 {
		t.Fatalf("the old capture leaked into the replacement row: %+v", replacementState)
	}
	if snapshots, err := s.Quotas(ctx); err != nil || len(snapshots) != 0 {
		t.Fatalf("old account snapshot leaked into the replacement: %#v %v", snapshots, err)
	}
	cached, err := s.QuotaCache(ctx)
	if err != nil || len(cached) != 1 || cached[0].Account != captured.Account {
		t.Fatalf("the old account lost its own row: %#v %v", cached, err)
	}
	// The replacement now starts its own row, and the two accounts never mix.
	replacementEntry := bindCacheFixture(s, "account-1", QuotaCacheEntry{Provider: "claude", ObservedAt: time.Now(), State: json.RawMessage(`{"status":"success","windows":[]}`)})
	if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{replacementEntry}); err != nil {
		t.Fatal(err)
	}
	cached, err = s.QuotaCache(ctx)
	if err != nil || len(cached) != 2 {
		t.Fatalf("two accounts did not get two rows: %#v %v", cached, err)
	}
	accounts := map[string]bool{}
	for _, record := range cached {
		accounts[record.Account] = true
	}
	if len(accounts) != 2 || !accounts[captured.Account] || !accounts[replaced.Account] {
		t.Fatalf("account rows mixed: %#v", accounts)
	}
}

// TestQuotaIdentityProviderRotationKeepsAccountState reproduces the reported
// provider-refresh regression: the provider rotates its access token during
// ordinary operation, and the displayed quota state must not disappear.
func TestQuotaIdentityProviderRotationKeepsAccountState(t *testing.T) {
	ctx := context.Background()
	for _, provider := range []string{"claude", "codex", "kimi", "devin"} {
		t.Run(provider, func(t *testing.T) {
			s := openTestStore(t)
			auth := quotaFixtureAuth(provider, "rotating", "rotating.json", "token-1")
			manager := bindQuotaFixtures(t, s, auth)
			before := fixtureBinding(s, provider, "rotating")
			// Per-provider state schemas differ, so use the minimal common shape.
			entry := bindCacheFixture(s, "rotating", QuotaCacheEntry{Provider: provider, ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success"}`)})
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
				t.Fatalf("initial observation rejected: %v", err)
			}
			snapshot, ok := quota.ParseFetch(before.quotaIdentity(), managementFetchFixture(t), time.Now().Add(-time.Minute))
			if !ok {
				t.Fatal("normalized quota fixture rejected")
			}
			if err := s.mergeQuota(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			// The provider issues a new access token for the same account.
			auth.Metadata["access_token"] = "token-2"
			if _, err := manager.Update(ctx, auth); err != nil {
				t.Fatal(err)
			}
			after := fixtureBinding(s, provider, "rotating")
			if after.Account != before.Account || after.AccountKind != before.AccountKind {
				t.Fatal("token rotation changed durable identity")
			}
			cached, err := s.QuotaCache(ctx)
			if err != nil || len(cached) != 1 || cached[0].Account != before.Account {
				t.Fatalf("rotation lost displayed quota state: %#v %v", cached, err)
			}
			snapshots, err := s.Quotas(ctx)
			if err != nil || len(snapshots) != 1 {
				t.Fatalf("rotation lost normalized quota state: %#v %v", snapshots, err)
			}
			// A page load re-uploads the state it rendered before the rotation.
			if err := s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
				t.Fatalf("observation captured before rotation rejected: %v", err)
			}
		})
	}
}

// TestQuotaIdentityRestartRehydratesAccountRows proves a restart re-projects the
// same accounts from the credential files and keeps their persisted rows.
func TestQuotaIdentityRestartRehydratesAccountRows(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	s, err := Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	auth := quotaFixtureAuth("claude", "account-1", "auth.json", "token")
	bindQuotaFixtures(t, s, auth)
	binding := fixtureBinding(s, "claude", "account-1")
	entry := bindCacheFixture(s, "account-1", QuotaCacheEntry{Provider: "claude", ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success","windows":[]}`)})
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close(ctx) }()
	bindQuotaFixtures(t, reopened, auth)
	if got := fixtureBinding(reopened, "claude", "account-1"); got.Account != binding.Account || got.AccountKind != binding.AccountKind {
		t.Fatal("restart changed the durable account facts")
	}
	cached, err := reopened.QuotaCache(ctx)
	if err != nil || len(cached) != 1 || cached[0].Account != binding.Account {
		t.Fatalf("restart did not rehydrate the account row: %#v %v", cached, err)
	}
}

// TestQuotaIdentityDiskProjectionKeepsAccountFacts covers disk reconciliation:
// restating a credential from its backing file must preserve the account fact.
func TestQuotaIdentityDiskProjectionKeepsAccountFacts(t *testing.T) {
	metadata := map[string]any{"type": "kimi", "access_token": "kimi-token", "refresh_token": "rt-kimi", "device_id": "device-A"}
	auth := &coreauth.Auth{ID: "kimi.json", FileName: "kimi.json", Provider: "kimi", Metadata: metadata,
		Attributes: map[string]string{"base_url": "https://api.kimi.com/coding", "domain": "kimi.com"}}
	runtimeBinding, ok := ProjectQuotaBinding(auth)
	if !ok {
		t.Fatal("kimi runtime projection refused")
	}
	if runtimeBinding.Account != "device-A" || runtimeBinding.AccountKind != "device_id" {
		t.Fatalf("kimi device fact: %+v", runtimeBinding)
	}
	// Kimi exposes no operation selectors: only codex, antigravity and xai do.
	if len(runtimeBinding.SelectorHashes) != 0 {
		t.Fatalf("kimi published operation selectors: %#v", runtimeBinding.SelectorHashes)
	}
	// The account fact comes from the credential property, not from the derived
	// base_url/domain attributes a file cannot restate.
	if runtimeBinding.SelectorHashes == nil && runtimeBinding.Account == "" {
		t.Fatal("kimi lost its device account fact")
	}
	// A rotated token for the same device keeps the account fact.
	auth.Metadata["access_token"] = "rotated-token"
	if rotated, ok := ProjectQuotaBinding(auth); !ok || rotated.Account != runtimeBinding.Account {
		t.Fatal("token rotation changed kimi's account facts")
	}
	changed := auth.Clone()
	changed.Metadata["device_id"] = "device-B"
	if replaced, ok := ProjectQuotaBinding(changed); !ok || replaced.Account == runtimeBinding.Account {
		t.Fatal("different device reused kimi identity")
	}
	if _, ok := ProjectQuotaBinding(nil); ok {
		t.Fatal("absent runtime credential projected")
	}
}

// TestQuotaIdentityStoredRowsAreReadWithoutASource is the inverted form of the
// deleted "legacy generation row is hidden" test. A stored row is no longer
// inferred from (or rejected against) a live credential: it is an account row,
// so a query without any bound source still reports it.
func TestQuotaIdentityStoredRowsAreReadWithoutASource(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	stored := quotaState{Snapshot: &quota.Snapshot{Provider: "codex", Account: "stored@example.invalid", ObservedAt: time.Now(), Windows: []quota.Window{}}, UIEntries: map[string]QuotaCacheEntry{}}
	raw, _ := json.Marshal(stored)
	if err := s.store.MutateCache(ctx, quotaNamespace, quotaStoreKey("codex", "stored@example.invalid"), func(json.RawMessage) (json.RawMessage, error) { return raw, nil }); err != nil {
		t.Fatal(err)
	}
	// No source is bound: nothing is published, but the durable row is still read.
	if bindings := s.publishedQuotaBindings(); len(bindings) != 0 {
		t.Fatalf("unbound store published bindings: %+v", bindings)
	}
	snapshots, err := s.Quotas(ctx)
	if err != nil || len(snapshots) != 1 || snapshots[0].Account != "stored@example.invalid" {
		t.Fatalf("stored account row was hidden: %+v %v", snapshots, err)
	}
	// Binding the current credentials must not adopt or delete that row.
	bindManagementFixtures(t, s)
	snapshots, err = s.Quotas(ctx)
	if err != nil || len(snapshots) != 1 || snapshots[0].Account != "stored@example.invalid" {
		t.Fatalf("current credentials changed an unrelated stored row: %+v %v", snapshots, err)
	}
}

// TestQuotaIdentityRequestStartAndAPICallProof keeps the fail-closed
// browser-originated proof under test. A token refresh rotates the token without
// changing the account, so it must not revoke the captured binding; a binding
// whose selector digests do not describe the credential must still be refused.
func TestQuotaIdentityRequestStartAndAPICallProof(t *testing.T) {
	for _, test := range []struct {
		name, header string
		rotate       bool
		want         int
	}{
		{"selected-token", `{"Authorization":"Bearer $TOKEN$","Chatgpt-Account-Id":"account-A"}`, false, 1},
		{"unrelated-bearer", `{"Authorization":"Bearer unrelated","Chatgpt-Account-Id":"account-A"}`, false, 0},
		{"wrong-scope", `{"Authorization":"Bearer $TOKEN$","Chatgpt-Account-Id":"account-B"}`, false, 0},
		{"missing-scope", `{"Authorization":"Bearer $TOKEN$"}`, false, 0},
		{"token-rotation", `{"Authorization":"Bearer $TOKEN$","Chatgpt-Account-Id":"account-A"}`, true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openTestStore(t)
			auth := quotaFixtureAuth("codex", "account", "same.json", "secret-A")
			auth.Metadata["account_id"] = "account-A"
			manager := bindQuotaFixtures(t, s, auth)
			engine := gin.New()
			engine.Use(s.ManagementMiddleware())
			output := middlewareQuotaEnvelope(t, "")
			engine.POST("/v0/management/api-call", func(c *gin.Context) {
				var request map[string]any
				_ = c.ShouldBindJSON(&request)
				if test.rotate {
					auth.Metadata["access_token"] = "secret-B"
					_, _ = manager.Update(context.Background(), auth)
				}
				c.Data(200, "application/json", output)
			})
			input := []byte(`{"auth_index":"account","url":"` + middlewareQuotaURL + `","header":` + test.header + `}`)
			response := middlewareRequest(t, engine, http.MethodPost, "/v0/management/api-call", input)
			if response.Code != 200 || response.Body.String() != string(output) {
				t.Fatal("observer changed original response")
			}
			if count := middlewareQuotaCount(t, s); count != test.want {
				t.Fatalf("observed %d want %d", count, test.want)
			}
		})
	}
	// A captured binding whose selector digest does not match the credential's own
	// selector must not attribute a captured response to it.
	// The proof itself is fail-closed: a captured request whose selector digests
	// do not match the credential's own published digests is refused.
	t.Run("mismatched-selector-proof", func(t *testing.T) {
		binding := QuotaBinding{Provider: "codex", Key: "same.json", AuthIndex: "account",
			Account: "same@example.invalid", AccountKind: "email",
			SelectorHashes: map[string]string{"account_id": quotaHash("account-A")}}
		headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Chatgpt-Account-Id": "account-A"}
		if !quotaAPICallProof(binding, middlewareQuotaURL, headers, "") {
			t.Fatal("matching selector proof was refused")
		}
		headers["Chatgpt-Account-Id"] = "account-B"
		if quotaAPICallProof(binding, middlewareQuotaURL, headers, "") {
			t.Fatal("proof with a different account selector was accepted")
		}
		delete(headers, "Chatgpt-Account-Id")
		if quotaAPICallProof(binding, middlewareQuotaURL, headers, "") {
			t.Fatal("proof that omitted the credential's selector was accepted")
		}
		// A credential with no selectors must not accept a request that supplies
		// one, and a selector the provider does not define is refused outright.
		headers["Chatgpt-Account-Id"] = "account-A"
		if quotaAPICallProof(QuotaBinding{Provider: "codex", Key: "same.json", AuthIndex: "account"}, middlewareQuotaURL, headers, "") {
			t.Fatal("unexpected selector was accepted for a credential with no proof")
		}
		if quotaAPICallProof(QuotaBinding{Provider: "claude", Key: "same.json", AuthIndex: "account", SelectorHashes: map[string]string{"account_id": quotaHash("account-A")}}, "https://api.anthropic.com/api/oauth/usage", headers, "") {
			t.Fatal("proof for a provider without that selector was accepted")
		}
	})
}

// TestQuotaIdentityQueuedObservationAndResetCannotCrossReplacement keeps the
// stale-write protection under test after the credential fence was removed: an
// observation and a reset captured for one account can never mutate another
// account's row, while the same account stays addressable across token rotation.
func TestQuotaIdentityQueuedObservationAndResetCannotCrossReplacement(t *testing.T) {
	s := openTestStore(t)
	auth := quotaFixtureAuth("codex", "account", "same.json", "secret-A")
	manager := bindQuotaFixtures(t, s, auth)
	original := fixtureBinding(s, "codex", "account")
	backend := blockManagementStore(t, s)
	s.fixtureObserveQuotaFetch(context.Background(), "codex", "account", managementFetchFixture(t))
	awaitManagementMutation(t, backend)
	s.ObserveQuotaReset(context.Background(), original)
	// The slot is replaced by a different account while the work is queued.
	auth.Metadata["email"] = "replacement@example.invalid"
	_, _ = manager.Update(context.Background(), auth)
	backend.unblock()
	flushFixture(t, s)
	states, _ := s.ListCache(context.Background(), quotaNamespace)
	if len(states[quotaStoreKey("codex", "replacement@example.invalid")]) != 0 {
		t.Fatalf("stale queued observation/reset mutated the replacement row: %v", sortedKeys(states))
	}
	// The captured account still owns its own row, and a token rotation for the
	// same account keeps admitting new observations of it.
	auth.Metadata["email"] = "account@example.invalid"
	auth.Metadata["access_token"] = "secret-B"
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	s.fixtureObserveQuotaFetch(context.Background(), "codex", "account", managementFetchFixture(t))
	flushFixture(t, s)
	if got := middlewareQuotaCount(t, s); got != 1 {
		t.Fatalf("same-account observation after token rotation was refused: %d", got)
	}
}

// TestQuotaIdentityHeaderAttributionUsesProviderAndAccount: header samples are
// attributed from the account facts stamped by the producer, which are the live
// credential's own facts. Providers refresh access tokens during ordinary
// operation, so attribution must never be scoped to a token version.
func TestQuotaIdentityHeaderAttributionUsesProviderAndAccount(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	auth := quotaFixtureAuth("claude", "account-1", "same.json", "secret-A")
	manager := bindQuotaFixtures(t, s, auth)
	binding := fixtureBinding(s, "claude", "account-1")
	record := fixtureRecord("verified", time.Now().Add(-time.Minute))
	record.Provider = "claude"
	record.Account, record.AccountKind = binding.Account, binding.AccountKind
	record.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}}
	s.Consume(ctx, fixturePayload(record))
	flushFixture(t, s)
	first, _ := s.Quotas(ctx)
	if len(first) != 1 || first[0].Account != binding.Account {
		t.Fatalf("account-stamped header not attributed: %#v", first)
	}
	// The account refreshes its token. The same account keeps being attributed.
	auth.Metadata["access_token"] = "secret-B"
	if _, err := manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	record.RequestID = "after-refresh"
	record.RequestedAt = time.Now()
	s.Consume(ctx, fixturePayload(record))
	flushFixture(t, s)
	after, _ := s.Quotas(ctx)
	if len(after) != 1 || !after[0].ObservedAt.After(first[0].ObservedAt) {
		t.Fatalf("header of a rotated token was not attributed to its account: %#v -> %#v", first, after)
	}
	// A different account is a different row, never a merge into the first.
	record.RequestID = "other-account"
	record.Account = "other@example.invalid"
	record.RequestedAt = time.Now()
	s.Consume(ctx, fixturePayload(record))
	flushFixture(t, s)
	quotas, _ := s.Quotas(ctx)
	if len(quotas) != 2 {
		t.Fatalf("different account did not get its own row: %#v", quotas)
	}
	if events, _ := s.Events(ctx, Filter{}, 10, 0); events.Total != 3 {
		t.Fatal("header attribution changed usage accounting")
	}
	// Usage rows stay grouped and filterable by the immutable account fact.
	page, err := s.Events(ctx, Filter{Provider: "claude", Account: binding.Account}, 10, 0)
	if err != nil || page.Total != 2 {
		t.Fatalf("account filter: %+v %v", page, err)
	}
	values, err := s.Filters(ctx, Filter{})
	if err != nil || len(values.Accounts) != 2 {
		t.Fatalf("account filter values: %+v %v", values, err)
	}
}

// TestQuotaIdentityAccountFactsPersistVerifiedEmails is the deliberate inverse of
// the removed "an email never reaches storage" rule: the account fact is durable
// because the dashboard groups by it, while the credential token still must not be
// persisted.
func TestQuotaIdentityAccountFactsPersistVerifiedEmails(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	s, err := Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	auth := quotaFixtureAuth("claude", "account-1", "auth.json", "private-source-token")
	bindQuotaFixtures(t, s, auth)
	binding := fixtureBinding(s, "claude", "account-1")
	entry := bindCacheFixture(s, "account-1", QuotaCacheEntry{Provider: "claude", ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success","windows":[]}`)})
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute)
	if err = s.mergeQuota(ctx, quota.Snapshot{Provider: "claude", Account: binding.Account, AccountKind: binding.AccountKind,
		Source: quota.SourceFetch, ObservedAt: at,
		Windows: []quota.Window{{ID: "five_hour", UsedPercent: floatPtr(30), ObservedAt: at, Source: quota.SourceFetch}}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(directory, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	persisted := string(journal)
	// The approved change: the account fact (and only the fact) is durable.
	for _, expected := range []string{binding.Account, `"account_kind":"` + binding.AccountKind + `"`, `"provider":"claude"`} {
		if !strings.Contains(persisted, expected) {
			t.Fatalf("persisted account facts are missing %q: %s", expected, persisted)
		}
	}
	for _, secret := range []string{"private-source-token", `"access_token"`, "private-cookie", "private-header-token"} {
		if strings.Contains(persisted, secret) {
			t.Fatalf("persisted store leaked %q", secret)
		}
	}
	// The published binding deliberately exposes the account fact, but no token.
	published, err := json.Marshal(quotaBindingPublic(binding))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(published), binding.Account) {
		t.Fatalf("published binding lost its account fact: %s", published)
	}
	for _, secret := range []string{"private-source-token", `"access_token"`} {
		if strings.Contains(string(published), secret) {
			t.Fatalf("published binding leaked %q", secret)
		}
	}
}

// TestQuotaIdentityStatusReportsHeaderScope pins the surviving header contract:
// there is no skipped-header counter any more, because a header sample carries
// the same account facts as its usage record. A credential that exposes no
// account property is attributed to its provider instead of being skipped, and
// the status document describes how header samples are attributed.
func TestQuotaIdentityStatusReportsHeaderScope(t *testing.T) {
	s := openTestStore(t)
	auth := quotaFixtureAuthUnlabeled("claude", "unlabeled", "unlabeled.json", "token-1")
	bindQuotaFixtures(t, s, auth)
	ctx := context.Background()
	record := fixtureRecord("unlabeled", time.Now())
	record.Provider = "claude"
	// The credential exposes no account property, so the producer stamps none.
	record.Account, record.AccountKind = "", ""
	record.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}}
	if err := s.recordFixture(ctx, record); err != nil {
		t.Fatal(err)
	}
	quotas, _ := s.Quotas(ctx)
	if len(quotas) != 1 || quotas[0].Account != "" || quotas[0].AccountKind != "" {
		t.Fatalf("provider-grouped credential was not attributed: %#v", quotas)
	}
	if row, err := s.quotaHistory(ctx, "claude", ""); err != nil || len(row.Observations) != 1 {
		t.Fatalf("provider-grouped credential has no history row: %+v %v", row, err)
	}
	if events, _ := s.Events(ctx, Filter{}, 10, 0); events.Total != 1 {
		t.Fatal("provider-grouped attribution changed usage accounting")
	}
	status := gin.New()
	s.RegisterRoutes(status.Group("/stats"))
	response := httptest.NewRecorder()
	status.ServeHTTP(response, httptest.NewRequest("GET", "/stats/status", nil))
	body := response.Body.String()
	if !strings.Contains(body, `"quota_header_scope"`) {
		t.Fatal("header attribution scope missing from status")
	}
	if strings.Contains(body, "skipped_quota_headers") {
		t.Fatal("status still advertises a diagnostic the backend no longer tracks")
	}
}
