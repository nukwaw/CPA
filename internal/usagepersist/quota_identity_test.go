package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
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

func quotaFixtureAuth(provider, index, name, token string) *coreauth.Auth {
	return &coreauth.Auth{ID: name, Index: index, FileName: name, Provider: provider, Metadata: map[string]any{"access_token": token, "email": "same@example.invalid"}}
}
func bindQuotaFixtures(t *testing.T, s *Store, auths ...*coreauth.Auth) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	s.BindQuotaIdentitySource(NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil))
	// Model the explicit add-on identity GET that primes optional observation.
	s.quotaBindings(true)
	return manager
}
func bindManagementFixtures(t *testing.T, s *Store) *coreauth.Manager {
	return bindQuotaFixtures(t, s, quotaFixtureAuth("codex", "account", "account.json", "fixture-secret"), quotaFixtureAuth("codex", "later", "later.json", "later-secret"))
}
func fixtureBinding(s *Store, provider, index string) QuotaBinding {
	// A trusted test caller obtains new request-start proof through the same
	// authoritative full read used by the identity endpoint, not a cached guess.
	binding, ok := indexQuotaBindings(s.quotaBindings(true)).binding(provider, index)
	if !ok {
		panic("test requires registered immutable quota source fixture")
	}
	return binding
}

// Direct observer fixtures model a trusted caller holding source-time proof.
// Middleware tests exercise actual pre-handler acquisition and token placement.
func (s *Store) fixtureObserveAPICall(ctx context.Context, provider, index, url string, status int, header http.Header, body []byte) {
	s.ObserveAPICall(ctx, provider, index, url, status, header, body, fixtureBinding(s, provider, index))
}
func (s *Store) fixtureObserveQuotaFetch(ctx context.Context, provider, index string, response pluginapi.QuotaFetchResponse) {
	s.ObserveQuotaFetch(ctx, provider, index, response, fixtureBinding(s, provider, index))
}
func (s *Store) fixtureObserveQuotaReset(ctx context.Context, provider, index string) {
	s.ObserveQuotaReset(ctx, provider, index, fixtureBinding(s, provider, index))
}
func (s *Store) observeQuotaResetAt(provider, index string, at time.Time) {
	s.observeBoundQuotaResetAt(fixtureBinding(s, provider, index), at, false)
}
func bindSnapshotFixture(s *Store, snapshot quota.Snapshot) quota.Snapshot {
	binding := fixtureBinding(s, snapshot.Provider, snapshot.AuthIndex)
	snapshot.CredentialGeneration = binding.CredentialGeneration
	snapshot.Revision = binding.Revision
	return snapshot
}
func bindCacheFixture(s *Store, entry QuotaCacheEntry) QuotaCacheEntry {
	binding := fixtureBinding(s, entry.Provider, entry.AuthIndex)
	entry.Key = binding.Key
	entry.CredentialGeneration = binding.CredentialGeneration
	entry.Revision = binding.Revision
	return entry
}

func TestQuotaIdentityStableGenerationAndRevisionFences(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("codex", "same-index", "same.json", "secret-A")
	auth.Metadata["account_id"] = "account-A"
	registered, err := manager.Register(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
	original := source.QuotaBindings(false)[0]
	changed := registered.Clone()
	changed.Metadata["access_token"] = "secret-B"
	if _, err = manager.Update(ctx, changed); err != nil {
		t.Fatal(err)
	}
	other := source.QuotaBindings(false)[0]
	if original.CredentialGeneration == other.CredentialGeneration || original.Revision == other.Revision {
		t.Fatal("replacement reused identity")
	}
	changed.Metadata["access_token"] = "secret-A"
	if _, err = manager.Update(ctx, changed); err != nil {
		t.Fatal(err)
	}
	restored := source.QuotaBindings(false)[0]
	if restored.CredentialGeneration != original.CredentialGeneration || restored.Revision == original.Revision {
		t.Fatal("A-B-A did not separate stable identity and operation revision")
	}
	if _, _, err = manager.ResetQuota(ctx, auth.ID); err != nil {
		t.Fatal(err)
	}
	reset := source.QuotaBindings(false)[0]
	if reset.CredentialGeneration != original.CredentialGeneration || reset.Revision == restored.Revision {
		t.Fatal("ordinary runtime mutation changed persistent identity or reused fence")
	}
	restarted := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil).QuotaBindings(false)[0]
	if restarted.CredentialGeneration != reset.CredentialGeneration || restarted.Revision == reset.Revision {
		t.Fatal("restart changed identity or reused process fence")
	}
	changed.Metadata["account_id"] = "account-B"
	if _, err = manager.Update(ctx, changed); err != nil {
		t.Fatal(err)
	}
	scoped := source.QuotaBindings(false)[0]
	if scoped.CredentialGeneration == original.CredentialGeneration || scoped.TokenScoped {
		t.Fatal("same-token Codex selector incorrectly token scoped")
	}
	manager.Remove(ctx, auth.ID)
	if len(source.QuotaBindings(false)) != 0 {
		t.Fatal("deleted identity remains")
	}
	if _, err = manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	recreated := source.QuotaBindings(false)[0]
	if recreated.CredentialGeneration != original.CredentialGeneration || recreated.Revision == original.Revision || recreated.Lifetime == original.Lifetime {
		t.Fatal("recreate reused registration fence")
	}
	encoded, _ := json.Marshal(recreated)
	for _, secret := range []string{"secret-A", "account-A", "same@example.invalid", "access_token", "selector"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("identity leaked sensitive data: %s", encoded)
		}
	}
}

func TestQuotaIdentityProjectionsAndConflicts(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "antigravity", "kimi", "devin", "meta", "xai"} {
		t.Run(provider, func(t *testing.T) {
			auth := quotaFixtureAuth(provider, "idx", "file.json", "secret")
			plain, ok := ProjectQuotaBinding(auth)
			if !ok {
				t.Fatal("ordinary OAuth unsupported")
			}
			for _, metadata := range []map[string]any{{"accessToken": "secret"}, {"token": map[string]any{"access_token": "secret"}}, {"Token": map[string]string{"accessToken": "secret"}}} {
				auth.Metadata = metadata
				binding, valid := ProjectQuotaBinding(auth)
				if !valid || binding.CredentialGeneration != plain.CredentialGeneration {
					t.Fatal("OAuth token variants diverged")
				}
			}
			auth.Metadata = map[string]any{"access_token": "secret", "accessToken": "different"}
			if _, ok = ProjectQuotaBinding(auth); ok {
				t.Fatal("conflicting access aliases accepted")
			}
			for _, metadata := range []map[string]any{{}, {"access_token": 123}, {"access_token": "secret", "unknown_account": map[string]any{"id": "new"}}, {"access_token": "secret", "account_id": map[string]any{"id": "new"}}} {
				auth.Metadata = metadata
				if _, ok = ProjectQuotaBinding(auth); ok {
					t.Fatal("missing/unknown identity accepted")
				}
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
		})
	}
	manager := coreauth.NewManager(nil, nil, nil)
	for _, name := range []string{"one.json", "two.json"} {
		_, err := manager.Register(context.Background(), quotaFixtureAuth("codex", "duplicate", name, "secret"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil).QuotaBindings(false)) != 0 {
		t.Fatal("ambiguous duplicate index bound")
	}
}

func TestQuotaIdentityRolloverStalePUTAndRestart(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	s, err := Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	auth := quotaFixtureAuth("codex", "account", "same.json", "secret-A")
	manager := bindQuotaFixtures(t, s, auth)
	old := fixtureBinding(s, "codex", "account")
	entry := bindCacheFixture(s, QuotaCacheEntry{Provider: "codex", AuthIndex: "account", ObservedAt: time.Now().Add(-time.Minute), State: json.RawMessage(`{"status":"success","windows":[],"planType":"old-plan"}`)})
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); err != nil {
		t.Fatal(err)
	}
	snapshot := bindSnapshotFixture(s, managementSnapshotAt(t, s, time.Now().Add(-time.Minute), 25))
	snapshot.Plan = "old-plan"
	if err = s.mergeQuota(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	auth.Metadata["access_token"] = "secret-B"
	if _, err = manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if cached, _ := s.QuotaCache(ctx); len(cached) != 0 {
		t.Fatal("old cache visible after replacement")
	}
	if snapshots, _ := s.Quotas(ctx); len(snapshots) != 0 {
		t.Fatal("old snapshot visible after replacement")
	}
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{entry}); !errors.Is(err, ErrQuotaIdentity) {
		t.Fatal("stale PUT accepted")
	}
	engine := gin.New()
	s.RegisterRoutes(engine.Group("/stats"))
	body, _ := json.Marshal(quotaCacheEnvelope{Entries: []QuotaCacheEntry{entry}})
	response := middlewareRequest(t, engine, http.MethodPut, "/stats/quota/cache", body)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale PUT status %d", response.Code)
	}
	// Rollover must clear the entire old envelope, including its future reset.
	if err = s.store.MutateCache(ctx, quotaNamespace, quotaKey("codex", "account"), func(raw json.RawMessage) (json.RawMessage, error) {
		var state quotaState
		_ = json.Unmarshal(raw, &state)
		state.ResetAt = time.Now().Add(time.Hour)
		return json.Marshal(state)
	}); err != nil {
		t.Fatal(err)
	}
	current := fixtureBinding(s, "codex", "account")
	fresh := entry
	fresh.CredentialGeneration = current.CredentialGeneration
	fresh.Revision = current.Revision
	fresh.State = json.RawMessage(`{"status":"success","windows":[]}`)
	if err = s.SaveQuotaCache(ctx, []QuotaCacheEntry{fresh}); err != nil {
		t.Fatal(err)
	}
	state := managementState(t, s)
	if state.Snapshot != nil || !state.ResetAt.IsZero() || len(state.UIEntries) != 1 || state.CredentialGeneration != current.CredentialGeneration {
		t.Fatal("generation rollover merged old envelope")
	}
	s.observeBoundQuotaResetAt(old, time.Now(), false)
	flushFixture(t, s)
	if cached, _ := s.QuotaCache(ctx); len(cached) != 1 {
		t.Fatal("reset A cleared B")
	}
	if err = s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	bindQuotaFixtures(t, s, auth)
	if cached, _ := s.QuotaCache(ctx); len(cached) != 1 {
		t.Fatal("unchanged credentials failed restart hydration")
	}
	raw, err := os.ReadFile(filepath.Join(directory, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-A", "secret-B", "same@example.invalid", old.Revision} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("journal leaked credential or persistent operation fence")
		}
	}
}

func TestQuotaIdentityNoSourceAndLegacyRowsHidden(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	legacy := quotaState{Snapshot: &quota.Snapshot{Provider: "codex", AuthIndex: "account", ObservedAt: time.Now(), Windows: []quota.Window{}}, UIEntries: map[string]QuotaCacheEntry{}}
	raw, _ := json.Marshal(legacy)
	if err := s.store.MutateCache(ctx, quotaNamespace, quotaKey("codex", "account"), func(json.RawMessage) (json.RawMessage, error) { return raw, nil }); err != nil {
		t.Fatal(err)
	}
	bindManagementFixtures(t, s)
	if snapshots, _ := s.Quotas(ctx); len(snapshots) != 0 {
		t.Fatal("legacy generation inferred from current account")
	}
}

func TestQuotaIdentityRequestStartAndAPICallProof(t *testing.T) {
	for _, test := range []struct {
		name, header string
		mutate       bool
		want         int
	}{
		{"selected-token", `{"Authorization":"Bearer $TOKEN$","Chatgpt-Account-Id":"account-A"}`, false, 1},
		{"unrelated-bearer", `{"Authorization":"Bearer unrelated","Chatgpt-Account-Id":"account-A"}`, false, 0},
		{"wrong-scope", `{"Authorization":"Bearer $TOKEN$","Chatgpt-Account-Id":"account-B"}`, false, 0},
		{"missing-scope", `{"Authorization":"Bearer $TOKEN$"}`, false, 0},
		{"midflight-replacement", `{"Authorization":"Bearer $TOKEN$","Chatgpt-Account-Id":"account-A"}`, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openTestStore(t)
			auth := quotaFixtureAuth("codex", "account", "same.json", "secret-A")
			auth.Metadata["account_id"] = "account-A"
			manager := bindQuotaFixtures(t, s, auth)
			engine := gin.New()
			engine.Use(s.ManagementMiddleware(nil, nil))
			output := middlewareQuotaEnvelope(t, "")
			engine.POST("/v0/management/api-call", func(c *gin.Context) {
				var request map[string]any
				_ = c.ShouldBindJSON(&request)
				if test.mutate {
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
}

func TestQuotaIdentityQueuedObservationAndResetCannotCrossReplacement(t *testing.T) {
	s := openTestStore(t)
	auth := quotaFixtureAuth("codex", "account", "same.json", "secret-A")
	manager := bindQuotaFixtures(t, s, auth)
	original := fixtureBinding(s, "codex", "account")
	backend := blockManagementStore(t, s)
	s.ObserveQuotaFetch(context.Background(), "codex", "account", managementFetchFixture(t), original)
	awaitManagementMutation(t, backend)
	s.ObserveQuotaReset(context.Background(), "codex", "account", original)
	auth.Metadata["access_token"] = "secret-B"
	_, _ = manager.Update(context.Background(), auth)
	backend.unblock()
	flushFixture(t, s)
	if values, _ := s.ListCache(context.Background(), quotaNamespace); len(values) != 0 {
		t.Fatalf("stale queued observation/reset mutated storage: %s", values)
	}
	// A-B-A reuses stable identity, never an old in-flight revision.
	auth.Metadata["access_token"] = "secret-A"
	_, _ = manager.Update(context.Background(), auth)
	s.ObserveQuotaFetch(context.Background(), "codex", "account", managementFetchFixture(t), original)
	flushFixture(t, s)
	if got := middlewareQuotaCount(t, s); got != 0 {
		t.Fatal("A-B-A accepted old operation")
	}
}

func TestQuotaIdentityHeaderSourceEvidenceAndAccounting(t *testing.T) {
	s := openTestStore(t)
	auth := quotaFixtureAuth("claude", "account-1", "same.json", "secret-A")
	manager := bindQuotaFixtures(t, s, auth)
	record := fixtureRecord("verified", time.Now())
	record.Provider = "claude"
	record.AccessTokenSHA256 = quotaHash("secret-A")
	record.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}}
	s.Consume(context.Background(), fixturePayload(record))
	flushFixture(t, s)
	if quotas, _ := s.Quotas(context.Background()); len(quotas) != 1 {
		t.Fatal("verified token-scoped header missing")
	}
	auth.Metadata["access_token"] = "secret-B"
	_, _ = manager.Update(context.Background(), auth)
	record.RequestID = "stale-token"
	s.Consume(context.Background(), fixturePayload(record))
	flushFixture(t, s)
	if quotas, _ := s.Quotas(context.Background()); len(quotas) != 0 {
		t.Fatal("old token header attributed to B")
	}
	record.RequestID = "missing-hash"
	record.AccessTokenSHA256 = ""
	s.Consume(context.Background(), fixturePayload(record))
	flushFixture(t, s)
	if events, _ := s.Events(context.Background(), Filter{}, 10, 0); events.Total != 3 {
		t.Fatal("identity failure discarded accounting")
	}
	if s.skippedQuotaHeaders.Load() != 2 {
		t.Fatal("ambiguous header skips not exposed")
	}
	codexStore := openTestStore(t)
	codex := quotaFixtureAuth("codex", "account-1", "codex.json", "same-token")
	codex.Metadata["account_id"] = "A"
	codexManager := bindQuotaFixtures(t, codexStore, codex)
	record.Provider = "codex"
	record.ResponseHeaders = http.Header{"X-Codex-Primary-Used-Percent": {"40"}}
	record.AccessTokenSHA256 = quotaHash("same-token")
	record.RequestID = "codex-before"
	codexStore.Consume(context.Background(), fixturePayload(record))
	codex.Metadata["account_id"] = "B"
	_, _ = codexManager.Update(context.Background(), codex)
	record.RequestID = "codex-after"
	codexStore.Consume(context.Background(), fixturePayload(record))
	flushFixture(t, codexStore)
	if quotas, _ := codexStore.Quotas(context.Background()); len(quotas) != 0 || codexStore.skippedQuotaHeaders.Load() != 2 {
		t.Fatal("Codex headers claimed absent selector proof")
	}
	status := gin.New()
	codexStore.RegisterRoutes(status.Group("/stats"))
	response := httptest.NewRecorder()
	status.ServeHTTP(response, httptest.NewRequest("GET", "/stats/status", nil))
	if !strings.Contains(response.Body.String(), `"skipped_quota_headers":2`) {
		t.Fatal("header limitation missing from status")
	}
}
