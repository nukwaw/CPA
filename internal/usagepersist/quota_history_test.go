package usagepersist

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
)

const (
	historyTestProvider = "claude"
	historyTestIndex    = "account-1"
)

func floatPtr(value float64) *float64 { return &value }

// mergeHistorySnapshot merges one explicit-timestamp observation. Tests never
// sleep: ordering comes from the supplied times.
func mergeHistorySnapshot(t *testing.T, s *Store, binding QuotaBinding, at time.Time, source string, windows map[string]float64) {
	t.Helper()
	ordered := make([]quota.Window, 0, len(windows))
	for _, id := range sortedKeys(windows) {
		ordered = append(ordered, quota.Window{ID: id, UsedPercent: floatPtr(windows[id]), ObservedAt: at, Source: source})
	}
	snapshot := quota.Snapshot{
		Provider: binding.Provider, Account: binding.Account, AccountKind: binding.AccountKind,
		Source: source, ObservedAt: at, Windows: ordered,
	}
	if err := s.mergeQuota(context.Background(), snapshot); err != nil {
		t.Fatalf("merge quota observation: %v", err)
	}
}

// loadQuotaHistoryRow reads one account's durable history row. History is keyed
// by account, so an account with no history is not an error.
func loadQuotaHistoryRow(t *testing.T, s *Store, provider, account string) quotaHistoryRow {
	t.Helper()
	row, err := s.quotaHistory(context.Background(), provider, account)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func closeTestStore(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// historyFailingStore fails only the history namespace so a failing optional
// time series can be proven not to fail the observation itself. The durable
// quota-state row shares the same cache backend and must still be written.
type historyFailingStore struct {
	store
	failures atomic.Int64
}

func (backend *historyFailingStore) MutateCache(ctx context.Context, namespace, key string, update func(json.RawMessage) (json.RawMessage, error)) error {
	if namespace == quotaHistoryNamespace {
		backend.failures.Add(1)
		return errors.New("history backend unavailable")
	}
	return backend.store.MutateCache(ctx, namespace, key, update)
}

func TestQuotaHistoryAppendOrderingBoundAndDedupe(t *testing.T) {
	s := openTestStore(t)
	bindQuotaFixtures(t, s, quotaFixtureAuth(historyTestProvider, historyTestIndex, "auth.json", "source-token"))
	binding := fixtureBinding(s, historyTestProvider, historyTestIndex)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range quotaHistoryLimit + 1 {
		mergeHistorySnapshot(t, s, binding, base.Add(time.Duration(i)*time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": float64(i)})
	}
	// A repeat of the same (observed_at, source) pair is deduplicated, even when
	// it arrives with a different percentage.
	mergeHistorySnapshot(t, s, binding, base.Add(time.Duration(quotaHistoryLimit)*time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 999})
	row := loadQuotaHistoryRow(t, s, binding.Provider, binding.Account)
	if len(row.Observations) != quotaHistoryLimit {
		t.Fatalf("history bound: %d observations", len(row.Observations))
	}
	if !row.Observations[0].ObservedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("oldest observation was not dropped: %v", row.Observations[0].ObservedAt)
	}
	for i := 1; i < len(row.Observations); i++ {
		if row.Observations[i].ObservedAt.Before(row.Observations[i-1].ObservedAt) {
			t.Fatalf("observations are not ascending at %d: %v then %v", i, row.Observations[i-1].ObservedAt, row.Observations[i].ObservedAt)
		}
	}
	newest := base.Add(time.Duration(quotaHistoryLimit) * time.Minute)
	if last := row.Observations[len(row.Observations)-1]; !last.ObservedAt.Equal(newest) || last.Source != quota.SourceHeaders || last.Windows["five_hour"] != float64(quotaHistoryLimit) {
		t.Fatalf("deduplicated observation overwrote the stored entry: %+v", last)
	}
	// The same instant from another source is a distinct observation, and the
	// bound still holds: the oldest entry is dropped again.
	mergeHistorySnapshot(t, s, binding, newest, quota.SourceFetch, map[string]float64{"five_hour": 77})
	row = loadQuotaHistoryRow(t, s, binding.Provider, binding.Account)
	if len(row.Observations) != quotaHistoryLimit || !row.Observations[0].ObservedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("bound after a distinct source: %d %v", len(row.Observations), row.Observations[0].ObservedAt)
	}
	sources := map[string]float64{}
	for _, observation := range row.Observations[len(row.Observations)-2:] {
		if !observation.ObservedAt.Equal(newest) {
			t.Fatalf("tail timestamp: %+v", observation)
		}
		sources[observation.Source] = observation.Windows["five_hour"]
	}
	if sources[quota.SourceHeaders] != float64(quotaHistoryLimit) || sources[quota.SourceFetch] != 77 {
		t.Fatalf("source isolation: %+v", sources)
	}
	// Only bounded window ids and percentages are stored.
	for _, observation := range row.Observations {
		for id, value := range observation.Windows {
			if id != "five_hour" || math.IsNaN(value) || math.IsInf(value, 0) {
				t.Fatalf("unexpected stored window: %q=%v", id, value)
			}
		}
	}
}

func TestQuotaHistoryIsolationAndUnusableWindows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindQuotaFixtures(t, s,
		quotaFixtureAuth(historyTestProvider, "account-1", "one.json", "token-one"),
		quotaFixtureAuth(historyTestProvider, "account-2", "two.json", "token-two"),
		quotaFixtureAuth("codex", "grouped", "grouped.json", "grouped-token"))
	first := fixtureBinding(s, historyTestProvider, "account-1")
	second := fixtureBinding(s, historyTestProvider, "account-2")
	grouped := fixtureBinding(s, "codex", "grouped")
	if first.Account == second.Account {
		t.Fatal("fixture accounts are not distinct")
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergeHistorySnapshot(t, s, first, at, quota.SourceHeaders, map[string]float64{"five_hour": 10})
	mergeHistorySnapshot(t, s, second, at, quota.SourceHeaders, map[string]float64{"five_hour": 20})
	// A provider that reports groups/buckets has no windows: record nothing.
	if err := s.mergeQuota(ctx, quota.Snapshot{
		Provider: "codex", Account: grouped.Account, AccountKind: grouped.AccountKind,
		Source: quota.SourceFetch, ObservedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	// Windows without an id or without a percentage cannot be charted.
	if err := s.mergeQuota(ctx, quota.Snapshot{
		Provider: historyTestProvider, Account: first.Account, AccountKind: first.AccountKind,
		Source: quota.SourceHeaders, ObservedAt: at.Add(time.Minute),
		Windows: []quota.Window{{ID: "", UsedPercent: floatPtr(5)}, {ID: "five_hour"}, {ID: "seven_day"}},
	}); err != nil {
		t.Fatal(err)
	}
	if row := loadQuotaHistoryRow(t, s, historyTestProvider, first.Account); len(row.Observations) != 1 {
		t.Fatalf("unusable windows were recorded: %+v", row)
	}
	if row := loadQuotaHistoryRow(t, s, historyTestProvider, second.Account); len(row.Observations) != 1 || row.Observations[0].Windows["five_hour"] != 20 {
		t.Fatalf("history leaked between accounts: %+v", row)
	}
	if row := loadQuotaHistoryRow(t, s, "codex", grouped.Account); len(row.Observations) != 0 {
		t.Fatalf("grouped provider recorded history: %+v", row)
	}
	// The observation itself must still be merged for both providers.
	snapshots, err := s.Quotas(ctx)
	if err != nil || len(snapshots) != 3 {
		t.Fatalf("quota snapshots: %+v %v", snapshots, err)
	}
}

// TestQuotaHistoryAccountIsolationAndRestart replaces the deleted
// generation-reset test. A generation no longer exists: a *different account*
// gets a different history row, and the *same account* keeps its row across
// restarts and token rotations.
func TestQuotaHistoryAccountIsolationAndRestart(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	s, err := Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	auth := quotaFixtureAuth(historyTestProvider, historyTestIndex, "auth.json", "source-token")
	manager := bindQuotaFixtures(t, s, auth)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := fixtureBinding(s, historyTestProvider, historyTestIndex)
	mergeHistorySnapshot(t, s, first, base, quota.SourceHeaders, map[string]float64{"five_hour": 10})
	mergeHistorySnapshot(t, s, first, base.Add(time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 20})
	if row := loadQuotaHistoryRow(t, s, first.Provider, first.Account); len(row.Observations) != 2 || row.Observations[1].Windows["five_hour"] != 20 {
		t.Fatalf("initial history: %+v", row)
	}
	// A different account taking over the same slot must never inherit the
	// previous account's history.
	auth.Metadata["email"] = "second@example.invalid"
	if _, err = manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	second := fixtureBinding(s, historyTestProvider, historyTestIndex)
	if second.Account == first.Account {
		t.Fatal("fixture did not change the account")
	}
	mergeHistorySnapshot(t, s, second, base.Add(2*time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 30})
	if row := loadQuotaHistoryRow(t, s, second.Provider, second.Account); len(row.Observations) != 1 || row.Observations[0].Windows["five_hour"] != 30 {
		t.Fatalf("account change did not start a fresh history row: %+v", row)
	}
	if row := loadQuotaHistoryRow(t, s, first.Provider, first.Account); len(row.Observations) != 2 {
		t.Fatalf("the previous account's history was disturbed: %+v", row)
	}
	// A token rotation for the second account keeps its row and identity.
	auth.Metadata["access_token"] = "rotated-token"
	if _, err = manager.Update(ctx, auth); err != nil {
		t.Fatal(err)
	}
	rotated := fixtureBinding(s, historyTestProvider, historyTestIndex)
	if rotated.Account != second.Account {
		t.Fatal("token rotation changed the account")
	}
	mergeHistorySnapshot(t, s, rotated, base.Add(3*time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 40})
	if row := loadQuotaHistoryRow(t, s, second.Provider, second.Account); len(row.Observations) != 2 {
		t.Fatalf("token rotation started a fresh history row: %+v", row)
	}
	closeTestStore(t, s)
	// History is durable across a restart with the file backend.
	s, err = Open(ctx, Options{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := s.Close(ctx); errClose != nil {
			t.Error(errClose)
		}
	}()
	bindQuotaFixtures(t, s, auth)
	restarted := fixtureBinding(s, historyTestProvider, historyTestIndex)
	if restarted.Account != second.Account {
		t.Fatalf("restart changed the account: %+v", restarted)
	}
	if row := loadQuotaHistoryRow(t, s, restarted.Provider, restarted.Account); len(row.Observations) != 2 {
		t.Fatalf("history did not survive a restart: %+v", row)
	}
	mergeHistorySnapshot(t, s, restarted, base.Add(4*time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 50})
	if row := loadQuotaHistoryRow(t, s, restarted.Provider, restarted.Account); len(row.Observations) != 3 {
		t.Fatalf("restart appended into a fresh row: %+v", row)
	}
	if row := loadQuotaHistoryRow(t, s, first.Provider, first.Account); len(row.Observations) != 2 {
		t.Fatalf("restart leaked history between accounts: %+v", row)
	}
}

func TestQuotaHistoryWriteFailureDoesNotFailObservation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindQuotaFixtures(t, s, quotaFixtureAuth(historyTestProvider, historyTestIndex, "auth.json", "source-token"))
	binding := fixtureBinding(s, historyTestProvider, historyTestIndex)
	backend := &historyFailingStore{store: s.store}
	s.store = backend
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	snapshot := quota.Snapshot{
		Provider: historyTestProvider, Account: binding.Account, AccountKind: binding.AccountKind,
		Source: quota.SourceHeaders, ObservedAt: at,
		Windows: []quota.Window{{ID: "five_hour", UsedPercent: floatPtr(42.5), ObservedAt: at, Source: quota.SourceHeaders}},
	}
	if err := s.mergeQuota(ctx, snapshot); err != nil {
		t.Fatalf("history failure escaped mergeQuota: %v", err)
	}
	if backend.failures.Load() != 1 || s.quotaHistoryFailures.Load() != 1 {
		t.Fatalf("history failure not counted: %d/%d", backend.failures.Load(), s.quotaHistoryFailures.Load())
	}
	snapshots, err := s.Quotas(ctx)
	if err != nil || len(snapshots) != 1 || len(snapshots[0].Windows) != 1 {
		t.Fatalf("observation was not merged: %+v %v", snapshots, err)
	}
}

func TestQuotaSummaryRangeWindowSelectionAndMissingHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindQuotaFixtures(t, s, quotaFixtureAuth(historyTestProvider, historyTestIndex, "auth.json", "source-token"))
	binding := fixtureBinding(s, historyTestProvider, historyTestIndex)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergeHistorySnapshot(t, s, binding, base, quota.SourceHeaders, map[string]float64{"five_hour": 10})
	mergeHistorySnapshot(t, s, binding, base.Add(10*time.Minute), quota.SourceHeaders, map[string]float64{"seven_day": 5})
	mergeHistorySnapshot(t, s, binding, base.Add(20*time.Minute), quota.SourceFetch, map[string]float64{"five_hour": 30, "seven_day": 6})
	mergeHistorySnapshot(t, s, binding, base.Add(30*time.Minute), quota.SourceHeaders, map[string]float64{"seven_day": 9})
	query := quotaSummaryQuery{Provider: historyTestProvider, Account: binding.Account, Window: "five_hour", From: base, To: base.Add(25 * time.Minute)}
	response, err := s.quotaSummary(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Observations) != 2 || !response.Observations[0].ObservedAt.Equal(base) || !response.Observations[1].ObservedAt.Equal(base.Add(20*time.Minute)) {
		t.Fatalf("range filtering: %+v", response.Observations)
	}
	if response.Observations[1].Source != quota.SourceFetch || response.Latest == nil || *response.Latest.UsedPercent != 30 {
		t.Fatalf("latest observation: %+v %+v", response.Observations[1], response.Latest)
	}
	if response.Usage.Requests != 0 || response.Estimate == nil || response.Estimate.FullUSD != nil {
		t.Fatalf("empty usage must not invent money: %+v %+v", response.Usage, response.Estimate)
	}
	// Without an explicit window, the most observed in-range window wins.
	response, err = s.quotaSummary(ctx, quotaSummaryQuery{Provider: historyTestProvider, Account: binding.Account, From: base, To: base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if response.Window != "seven_day" || len(response.Observations) != 3 {
		t.Fatalf("default window selection: %q %+v", response.Window, response.Observations)
	}
	// With nothing in range, the newest observed window labels an empty series.
	response, err = s.quotaSummary(ctx, quotaSummaryQuery{Provider: historyTestProvider, Account: binding.Account, From: base.Add(time.Hour), To: base.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if response.Window != "seven_day" || len(response.Observations) != 0 || response.Latest != nil || response.Estimate != nil {
		t.Fatalf("empty range response: %+v", response)
	}
	// An account with no history is not an error.
	response, err = s.quotaSummary(ctx, quotaSummaryQuery{Provider: "codex", Account: "absent", From: base, To: base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if response.Window != "" || len(response.Observations) != 0 || response.Latest != nil || response.Estimate != nil {
		t.Fatalf("missing history response: %+v", response)
	}
	// Another account's history is never charted for this one.
	response, err = s.quotaSummary(ctx, quotaSummaryQuery{Provider: historyTestProvider, Account: "other@example.invalid", From: base, To: base.Add(time.Hour)})
	if err != nil || len(response.Observations) != 0 {
		t.Fatalf("account isolation in summary: %+v %v", response, err)
	}
}

func TestQuotaSummaryEstimateMath(t *testing.T) {
	for _, test := range []struct {
		name        string
		usage       quotaSummaryUsage
		latest      *quotaSummaryObservation
		wantNil     bool
		wantUsedUSD *float64
		wantFullUSD *float64
		wantUnused  *float64
	}{
		{name: "no-latest", usage: quotaSummaryUsage{}, latest: nil, wantNil: true},
		{name: "no-percent", usage: quotaSummaryUsage{}, latest: &quotaSummaryObservation{}, wantNil: true},
		{
			name: "unpriced", usage: quotaSummaryUsage{Requests: 2, UnpricedRequests: 2},
			latest:      &quotaSummaryObservation{UsedPercent: floatPtr(42.5)},
			wantUsedUSD: floatPtr(0), wantFullUSD: nil, wantUnused: nil,
		},
		{
			name: "zero-percent", usage: quotaSummaryUsage{Requests: 1, PricedRequests: 1, CostUSD: 1.2},
			latest:      &quotaSummaryObservation{UsedPercent: floatPtr(0)},
			wantUsedUSD: floatPtr(1.2), wantFullUSD: nil, wantUnused: nil,
		},
		{
			name: "priced", usage: quotaSummaryUsage{Requests: 12, PricedRequests: 12, CostUSD: 1.2},
			latest:      &quotaSummaryObservation{UsedPercent: floatPtr(42.5)},
			wantUsedUSD: floatPtr(1.2), wantFullUSD: floatPtr(2.823529), wantUnused: floatPtr(1.623529),
		},
		{
			name: "cost-without-priced-requests", usage: quotaSummaryUsage{Requests: 1, CostUSD: 1.2},
			latest:      &quotaSummaryObservation{UsedPercent: floatPtr(50)},
			wantUsedUSD: floatPtr(1.2), wantFullUSD: nil, wantUnused: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			estimate := deriveQuotaSummaryEstimate(test.usage, test.latest)
			if test.wantNil {
				if estimate != nil {
					t.Fatalf("expected no estimate: %+v", estimate)
				}
				return
			}
			if estimate == nil {
				t.Fatal("missing estimate")
			}
			assertOptionalFloat(t, "used_usd", estimate.UsedUSD, test.wantUsedUSD)
			assertOptionalFloat(t, "full_usd", estimate.FullUSD, test.wantFullUSD)
			assertOptionalFloat(t, "unused_usd", estimate.UnusedUSD, test.wantUnused)
		})
	}
}

func assertOptionalFloat(t *testing.T, name string, got, want *float64) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s presence: got %v want %v", name, got, want)
	}
	if got == nil {
		return
	}
	if math.Abs(*got-*want) > 1e-9 {
		t.Fatalf("%s value: got %v want %v", name, *got, *want)
	}
}

func TestQuotaSummaryHTTPRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := openTestStore(t)
	ctx := context.Background()
	auth := quotaFixtureAuth(historyTestProvider, historyTestIndex, "auth.json", "source-token")
	bindQuotaFixtures(t, s, auth)
	binding := fixtureBinding(s, historyTestProvider, historyTestIndex)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergeHistorySnapshot(t, s, binding, base.Add(5*time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 10})
	mergeHistorySnapshot(t, s, binding, base.Add(15*time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 25})
	if _, err := s.SetPrice(ctx, fixturePrice()); err != nil {
		t.Fatal(err)
	}
	record := fixtureRecord("summary-event", base.Add(10*time.Minute))
	record.Provider = historyTestProvider
	record.Account, record.AccountKind = binding.Account, binding.AccountKind
	if err := s.recordFixture(ctx, record); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	s.RegisterRoutes(router.Group("/stats"))
	parameters := url.Values{
		"provider": {strings.ToUpper(historyTestProvider)},
		"account":  {binding.Account},
		"window":   {"five_hour"},
		"from":     {base.Format(time.RFC3339Nano)},
		"to":       {base.Add(time.Hour).Format(time.RFC3339Nano)},
	}
	for _, test := range []struct {
		name   string
		mutate func(url.Values)
	}{
		{"missing-provider", func(values url.Values) { values.Del("provider") }},
		{"long-provider", func(values url.Values) { values.Set("provider", strings.Repeat("a", maxQuotaIdentityText+1)) }},
		{"long-account", func(values url.Values) { values.Set("account", strings.Repeat("a", maxQuotaIdentityText+1)) }},
		{"invalid-from", func(values url.Values) { values.Set("from", "not-a-date") }},
		{"invalid-to", func(values url.Values) { values.Set("to", "not-a-date") }},
		{"empty-range", func(values url.Values) { values.Set("from", values.Get("to")) }},
		{"over-wide-range", func(values url.Values) {
			values.Set("from", base.Add(-quotaSummaryMaxRange-time.Hour).Format(time.RFC3339Nano))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := url.Values{}
			for key, value := range parameters {
				values[key] = append([]string(nil), value...)
			}
			test.mutate(values)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats/quota/summary?"+values.Encode(), nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s status=%d body=%s", test.name, response.Code, response.Body)
			}
		})
	}
	// An omitted account is valid: a credential without an account property is
	// grouped by provider alone, so the query answers with an empty series.
	t.Run("missing-account-is-valid", func(t *testing.T) {
		values := url.Values{}
		for key, value := range parameters {
			values[key] = append([]string(nil), value...)
		}
		values.Del("account")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats/quota/summary?"+values.Encode(), nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body)
		}
		if !strings.Contains(response.Body.String(), `"observations":[]`) {
			t.Fatalf("missing account response lacks an empty series: %s", response.Body)
		}
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/stats/quota/summary?"+parameters.Encode(), nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	var document map[string]any
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"provider", "account", "window", "range", "observations", "latest", "usage", "estimate"} {
		if _, ok := document[field]; !ok {
			t.Fatalf("response is missing %q: %s", field, body)
		}
	}
	var decoded quotaSummaryResponse
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider != historyTestProvider || decoded.Account != binding.Account || decoded.Window != "five_hour" {
		t.Fatalf("account fields: %+v", decoded)
	}
	if !decoded.Range.From.Equal(base) || !decoded.Range.To.Equal(base.Add(time.Hour)) {
		t.Fatalf("range: %+v", decoded.Range)
	}
	if len(decoded.Observations) != 2 || decoded.Latest == nil || *decoded.Latest.UsedPercent != 25 {
		t.Fatalf("observations: %+v %+v", decoded.Observations, decoded.Latest)
	}
	if decoded.Observations[0].Source != quota.SourceHeaders {
		t.Fatalf("observation source: %+v", decoded.Observations[0])
	}
	if decoded.Usage.Requests != 1 || decoded.Usage.PricedRequests != 1 || decoded.Usage.UnpricedRequests != 0 || decoded.Usage.TotalTokens != 1300 {
		t.Fatalf("usage aggregate: %+v", decoded.Usage)
	}
	if math.Abs(decoded.Usage.CostUSD-0.0031) > 1e-9 {
		t.Fatalf("usage cost: %v", decoded.Usage.CostUSD)
	}
	if decoded.Estimate == nil || decoded.Estimate.FullUSD == nil || decoded.Estimate.UnusedUSD == nil {
		t.Fatalf("estimate: %+v", decoded.Estimate)
	}
	if math.Abs(*decoded.Estimate.UsedUSD-0.0031) > 1e-9 || math.Abs(*decoded.Estimate.FullUSD-0.0124) > 1e-9 || math.Abs(*decoded.Estimate.UnusedUSD-0.0093) > 1e-9 {
		t.Fatalf("estimate math: %+v", decoded.Estimate)
	}
	usage, ok := document["usage"].(map[string]any)
	if !ok || len(usage) != 9 {
		t.Fatalf("usage fields: %s", body)
	}
	for _, field := range []string{"requests", "priced_requests", "unpriced_requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "total_tokens", "cost_usd"} {
		if _, ok := usage[field]; !ok {
			t.Fatalf("usage is missing %q: %s", field, body)
		}
	}
	observations, ok := document["observations"].([]any)
	if !ok || len(observations) != 2 {
		t.Fatalf("observations payload: %s", body)
	}
	first, ok := observations[0].(map[string]any)
	if !ok {
		t.Fatalf("observation payload: %s", body)
	}
	for _, field := range []string{"observed_at", "source", "used_percent"} {
		if _, ok := first[field]; !ok {
			t.Fatalf("observation is missing %q: %s", field, body)
		}
	}
	for _, secret := range []string{"source-token", `"access_token"`, "auth.json", "sk-private-test-key", "private-auth-file", "private-source", "upstream.invalid", binding.Key} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked %q: %s", secret, body)
		}
	}
	// An unknown account is not an error: it is an empty series.
	empty := url.Values{"provider": {"codex"}, "account": {"no-such-account"}, "from": parameters["from"], "to": parameters["to"]}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats/quota/summary?"+empty.Encode(), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unknown account status=%d body=%s", response.Code, response.Body)
	}
	body = response.Body.String()
	for _, fragment := range []string{`"observations":[]`, `"latest":null`, `"estimate":null`} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("unknown account response lacks %s: %s", fragment, body)
		}
	}
	if !strings.Contains(body, `"usage":{`) || !strings.Contains(body, `"requests":0`) {
		t.Fatalf("unknown account response lacks a usage aggregate: %s", body)
	}
	// Another account's observations are never charted for this account.
	other := url.Values{"provider": {"codex"}, "account": {"other@example.invalid"}, "from": parameters["from"], "to": parameters["to"]}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stats/quota/summary?"+other.Encode(), nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"observations":[]`) {
		t.Fatalf("account isolation status=%d body=%s", response.Code, response.Body)
	}
}

func TestQuotaSummaryUnpricedUsageKeepsEstimateNull(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	bindQuotaFixtures(t, s, quotaFixtureAuth(historyTestProvider, historyTestIndex, "auth.json", "source-token"))
	binding := fixtureBinding(s, historyTestProvider, historyTestIndex)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mergeHistorySnapshot(t, s, binding, base.Add(time.Minute), quota.SourceHeaders, map[string]float64{"five_hour": 50})
	record := fixtureRecord("unpriced-event", base.Add(2*time.Minute))
	record.Provider = historyTestProvider
	record.Model = "unpriced-model"
	record.Account, record.AccountKind = binding.Account, binding.AccountKind
	if err := s.recordFixture(ctx, record); err != nil {
		t.Fatal(err)
	}
	response, err := s.quotaSummary(ctx, quotaSummaryQuery{Provider: historyTestProvider, Account: binding.Account, From: base, To: base.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.Requests != 1 || response.Usage.PricedRequests != 0 || response.Usage.UnpricedRequests != 1 || response.Usage.CostUSD != 0 {
		t.Fatalf("unpriced usage: %+v", response.Usage)
	}
	if response.Estimate == nil || response.Estimate.FullUSD != nil || response.Estimate.UnusedUSD != nil {
		t.Fatalf("unpriced estimate invented money: %+v", response.Estimate)
	}
	if response.Estimate.UsedUSD == nil || *response.Estimate.UsedUSD != 0 {
		t.Fatalf("unpriced estimate cost: %+v", response.Estimate)
	}
}
