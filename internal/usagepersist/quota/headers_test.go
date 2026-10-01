package quota

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var observed = time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)

// parserIdentity supplies the account facts a credential publishes: the
// provider and its account property (device_id for kimi, email otherwise).
// An account-less credential carries neither an account nor an account kind.
func parserIdentity(provider, account string) Identity {
	kind := ""
	if account != "" {
		kind = "email"
		if strings.EqualFold(strings.TrimSpace(provider), "kimi") {
			kind = "device_id"
		}
	}
	return Identity{Provider: provider, Account: account, AccountKind: kind}
}

// assertAccountFacts checks that a parser stamped the caller's account facts
// instead of any opaque credential identity.
func assertAccountFacts(t *testing.T, snapshot Snapshot, identity Identity) {
	t.Helper()
	wantProvider := strings.ToLower(strings.TrimSpace(identity.Provider))
	if snapshot.Provider != wantProvider || snapshot.Account != strings.TrimSpace(identity.Account) || snapshot.AccountKind != identity.AccountKind {
		t.Fatalf("parser lost account facts: %+v from %+v", snapshot, identity)
	}
}

func TestAccountFactsContract(t *testing.T) {
	identity := parserIdentity("codex", "user@example.invalid")
	encodedIdentity, errJSON := json.Marshal(identity)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	if want := `{"provider":"codex","account":"user@example.invalid","account_kind":"email"}`; string(encodedIdentity) != want {
		t.Fatalf("identity contract changed: got %s, want %s", encodedIdentity, want)
	}
	snapshot, ok := ParseHeaders(identity, http.Header{"X-Codex-Primary-Used-Percent": {"10"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"60"}}, observed)
	if !ok {
		t.Fatal("fixture not recognized")
	}
	assertAccountFacts(t, snapshot, identity)
	encoded, errJSON := json.Marshal(snapshot)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	prefix := `{"provider":"codex","account":"user@example.invalid","account_kind":"email","source":"response_headers","observed_at":"2026-09-29T12:00:00Z","windows":[`
	if !strings.HasPrefix(string(encoded), prefix) {
		t.Fatalf("snapshot contract changed: %s", encoded)
	}

	// A credential with no account property is still recorded: it is grouped by
	// provider alone and claims no account kind.
	accountless := parserIdentity("codex", "")
	snapshot, ok = ParseHeaders(accountless, http.Header{"X-Codex-Primary-Used-Percent": {"10"}}, observed)
	if !ok || snapshot.Account != "" || snapshot.AccountKind != "" {
		t.Fatalf("account-less credential was hidden: %+v, parsed=%v", snapshot, ok)
	}
	assertAccountFacts(t, snapshot, accountless)
	if account, exists := accountFact("   "); !exists || account != "" {
		t.Fatalf("blank account property not treated as absent: %q, %v", account, exists)
	}
}

func TestParseCodexHeadersMeasuredWindows(t *testing.T) {
	headers := http.Header{
		"x-codex-plan-type":                     {"pro"},
		"x-codex-primary-used-percent":          {"50", "0"},
		"x-codex-primary-window-minutes":        {"300"},
		"x-codex-primary-reset-at":              {"1790704800"},
		"x-codex-primary-reset-after-seconds":   {"999"},
		"x-codex-secondary-used-percent":        {"125"},
		"x-codex-secondary-window-minutes":      {"10080"},
		"x-codex-secondary-reset-after-seconds": {"0"},
		"x-codex-credits-has-credits":           {"False"},
		"x-codex-credits-balance":               {"0"},
		"Authorization":                         {"Bearer do-not-persist"},
		"Set-Cookie":                            {"secret-session"},
		"X-Request-Id":                          {"secret-request"},
		"X-Codex-Credits-Private":               {"secret-private"},
	}
	identity := parserIdentity(" CODEX ", "user@example.invalid")
	snapshot, ok := ParseHeaders(identity, headers, observed)
	if !ok || snapshot.Plan != "pro" || len(snapshot.Windows) != 2 {
		t.Fatalf("unexpected snapshot: %+v, recognized=%v", snapshot, ok)
	}
	assertAccountFacts(t, snapshot, identity)
	primary := getWindow(t, snapshot, "primary")
	assertNumber(t, primary.UsedPercent, 0)
	assertNumber(t, primary.RemainingPercent, 100)
	if primary.WindowSeconds == nil || *primary.WindowSeconds != 18000 || primary.ResetAt.Unix() != 1790704800 {
		t.Fatalf("primary window did not preserve absolute reset precedence: %+v", primary)
	}
	secondary := getWindow(t, snapshot, "secondary")
	assertNumber(t, secondary.UsedPercent, 125)
	assertNumber(t, secondary.RemainingPercent, 0)
	if secondary.ResetAt == nil || !secondary.ResetAt.Equal(observed) {
		t.Fatalf("explicit zero reset lost: %+v", secondary)
	}
	if snapshot.Credits == nil || snapshot.Credits.HasCredits == nil || *snapshot.Credits.HasCredits {
		t.Fatalf("explicit false credits lost: %+v", snapshot.Credits)
	}
	assertNumber(t, snapshot.Credits.Balance, 0)
	encoded, errJSON := json.Marshal(snapshot)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	for _, secret := range []string{"do-not-persist", "secret-", "Authorization", "Set-Cookie", "X-Request"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("unexpected secret %q retained: %s", secret, encoded)
		}
	}
	for _, removed := range []string{"auth_index", "credential_generation", "revision"} {
		if strings.Contains(string(encoded), removed) {
			t.Fatalf("removed credential identity %q still serialized: %s", removed, encoded)
		}
	}
	if headers["x-codex-primary-used-percent"][0] != "50" || len(headers["x-codex-primary-used-percent"]) != 2 {
		t.Fatal("parser mutated its input headers")
	}
}

// TestAccountFactsContract pins the persisted fact contract other packages
// decode: the exact JSON keys, their order, and the fact that account is an
// optional credential property rather than part of a credential identity.

func TestCodexAdditionalActiveLimitDoesNotPolluteMain(t *testing.T) {
	for _, additionalPrefix := range []string{"bengalfox", "additional-gpt-5-3-codex-spark"} {
		headers := http.Header{}
		headers.Set("X-Codex-Active-Limit", "Bengalfox")
		headers.Set("X-Codex-Primary-Used-Percent", "80")
		headers.Set("X-Codex-"+additionalPrefix+"-Limit-Name", "GPT-5.3-Codex-Spark")
		headers.Set("X-Codex-"+additionalPrefix+"-Primary-Used-Percent", "80")
		headers.Set("X-Codex-Code-Review-Primary-Used-Percent", "5")
		snapshot, ok := ParseHeaders(parserIdentity("codex", "a"), headers, observed)
		if !ok || len(snapshot.Windows) != 2 {
			t.Fatalf("additional quota not isolated: %+v", snapshot)
		}
		getWindow(t, snapshot, "additional:gpt-5-3-codex-spark:primary")
		getWindow(t, snapshot, "code_review:primary")
		for _, window := range snapshot.Windows {
			if window.ID == "primary" {
				t.Fatal("active additional limit overwrote main quota")
			}
		}
	}
}

func TestClaudeHeadersFractionAndProviderIsolation(t *testing.T) {
	headers := http.Header{
		"anthropic-ratelimit-unified-5h-utilization":      {"0"},
		"Anthropic-Ratelimit-Unified-5h-Reset":            {"1790704800"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":      {"0.53"},
		"Anthropic-Ratelimit-Unified-7d-Status":           {"rejected"},
		"Anthropic-Ratelimit-Unified-Fallback-Percentage": {"0.5"},
		"X-Codex-Primary-Used-Percent":                    {"99"},
	}
	snapshot, ok := ParseHeaders(parserIdentity("claude", "a"), headers, observed)
	if !ok || len(snapshot.Windows) != 2 {
		t.Fatalf("unexpected Claude observation %+v", snapshot)
	}
	assertNumber(t, getWindow(t, snapshot, "five_hour").UsedPercent, 0)
	weekly := getWindow(t, snapshot, "seven_day")
	assertNumber(t, weekly.UsedPercent, 53)
	if weekly.Allowed == nil || *weekly.Allowed || weekly.LimitReached == nil || !*weekly.LimitReached {
		t.Fatal("Claude window state lost")
	}
	for _, provider := range []string{"kimi", "xai", "grok", "gemini", "gemini-cli", "antigravity", "devin", "openai", "unknown-provider"} {
		if _, parsed := ParseHeaders(parserIdentity(provider, "a"), headers, observed); parsed {
			t.Fatalf("misattributed headers to %s", provider)
		}
	}
}

func TestHeadersRejectMalformedAndNonQuotaSignals(t *testing.T) {
	for _, value := range []string{"", "NaN", "+Inf", "-1", "1\r\nsecret", strings.Repeat("1", maxText+1)} {
		if _, ok := ParseHeaders(parserIdentity("codex", "a"), http.Header{"X-Codex-Primary-Used-Percent": {value}}, observed); ok {
			t.Fatalf("accepted invalid percentage %q", value)
		}
	}
	// Replaces the removed auth-index/generation validation: the provider must
	// be present and the optional account property must be well formed.
	for _, identity := range []Identity{
		{},
		parserIdentity("", "a"),
		parserIdentity("codex", "a\nsecret"),
		parserIdentity("codex", "a\x00secret"),
		parserIdentity("codex", strings.Repeat("a", maxText+1)),
		{Provider: "co\ndex", Account: "a"},
	} {
		accepted, ok := ParseHeaders(identity, http.Header{"X-Codex-Primary-Used-Percent": {"0"}}, observed)
		if ok {
			t.Fatalf("accepted invalid account facts %+v: %+v", identity, accepted)
		}
	}
	// The size bound itself is inclusive.
	bounded, ok := ParseHeaders(parserIdentity("codex", strings.Repeat("a", maxText)), http.Header{"X-Codex-Primary-Used-Percent": {"0"}}, observed)
	if !ok || len(bounded.Account) != maxText {
		t.Fatalf("rejected an account at the size bound: %+v, parsed=%v", bounded, ok)
	}
	if _, ok := ParseHeaders(parserIdentity("codex", "a"), http.Header{"X-Codex-Primary-Used-Percent": {"0"}}, time.Time{}); ok {
		t.Fatal("accepted missing observation time")
	}
	for _, headers := range []http.Header{
		{"Retry-After": {"60"}},
		{"X-Ratelimit-Remaining-Requests": {"0"}},
		{"X-Codex-Plan-Type": {"pro"}},
		{"X-Codex-Limit-Reached": {"true"}},
	} {
		if _, ok := ParseHeaders(parserIdentity("codex", "a"), headers, observed); ok {
			t.Fatalf("non-measurement fabricated quota: %+v", headers)
		}
	}
	snapshot, _ := ParseHeaders(parserIdentity("codex", "a"), http.Header{
		"X-Codex-Primary-Used-Percent":        {"1"},
		"X-Codex-Primary-Window-Minutes":      {"9223372036854775807"},
		"X-Codex-Primary-Reset-After-Seconds": {"9223372036854775807"},
		"X-Codex-Primary-Reset-At":            {"9223372036854775807"},
	}, observed)
	window := getWindow(t, snapshot, "primary")
	if window.WindowSeconds != nil || window.ResetAt != nil {
		t.Fatalf("overflow time was retained: %+v", window)
	}
}

func TestMergeOrdersWindowsAndDoesNotAlias(t *testing.T) {
	identity := parserIdentity("codex", "a@example.invalid")
	initial, _ := ParseHeaders(identity, http.Header{
		"X-Codex-Plan-Type":                {"pro"},
		"X-Codex-Primary-Used-Percent":     {"10"},
		"X-Codex-Secondary-Used-Percent":   {"20"},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
	}, observed)
	newer, _ := ParseHeaders(identity, http.Header{"X-Codex-Primary-Used-Percent": {"30"}}, observed.Add(time.Minute))
	merged := Merge(initial, newer)
	if len(merged.Windows) != 2 || merged.Plan != "pro" {
		t.Fatalf("partial header erased unobserved quota: %+v", merged)
	}
	assertNumber(t, getWindow(t, merged, "primary").UsedPercent, 30)
	assertNumber(t, getWindow(t, merged, "secondary").UsedPercent, 20)
	older, _ := ParseHeaders(identity, http.Header{"X-Codex-Primary-Used-Percent": {"5"}, "X-Codex-Secondary-Used-Percent": {"25"}}, observed.Add(30*time.Second))
	merged = Merge(merged, older)
	assertNumber(t, getWindow(t, merged, "primary").UsedPercent, 30)
	assertNumber(t, getWindow(t, merged, "secondary").UsedPercent, 25)
	if !merged.ObservedAt.Equal(newer.ObservedAt) || !getWindow(t, merged, "secondary").ObservedAt.Equal(older.ObservedAt) {
		t.Fatal("per-window observation ordering lost")
	}
	*merged.Windows[0].UsedPercent = 99
	assertNumber(t, getWindow(t, initial, "primary").UsedPercent, 10)
	assertNumber(t, getWindow(t, newer, "primary").UsedPercent, 30)
	other := newer
	other.Account = "other@example.invalid"
	isolated := Merge(initial, other)
	if isolated.Account != "other@example.invalid" || len(isolated.Windows) != 1 {
		t.Fatal("different account quota was merged")
	}
	// A different account is a different credential: it starts an entirely new
	// snapshot, even when its observation is older than the retained state.
	replacement, ok := ParseHeaders(parserIdentity("codex", "replacement@example.invalid"), http.Header{"X-Codex-Primary-Used-Percent": {"5"}}, observed.Add(-time.Minute))
	if !ok {
		t.Fatal("invalid replacement fixture")
	}
	isolated = Merge(initial, replacement)
	if isolated.Account != "replacement@example.invalid" || isolated.AccountKind != "email" || isolated.Provider != "codex" || len(isolated.Windows) != 1 || isolated.Plan != "" || !isolated.ObservedAt.Equal(replacement.ObservedAt) {
		t.Fatal("account change retained the previous account's state")
	}
	assertNumber(t, getWindow(t, isolated, "primary").UsedPercent, 5)
	// The same account with a different recorded kind is still one account, and
	// the previous snapshot's facts are preserved by the merge.
	sameAccount := newer
	sameAccount.AccountKind = "device_id"
	merged = Merge(initial, sameAccount)
	if len(merged.Windows) != 2 || merged.AccountKind != "email" || !merged.ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("same-account merge changed: %+v", merged)
	}
	// A previous snapshot without an observation time cannot seed a merge.
	if zeroed := Merge(Snapshot{Provider: "codex", Account: "a@example.invalid"}, replacement); zeroed.Account != "replacement@example.invalid" || len(zeroed.Windows) != 1 {
		t.Fatalf("zero-time previous snapshot was merged: %+v", zeroed)
	}
}

func TestMergeRetainsStableMetadataButNotExpiredResetOrFlags(t *testing.T) {
	identity := parserIdentity("codex", "a")
	initial, _ := ParseHeaders(identity, http.Header{
		"X-Codex-Allowed":                     {"false"},
		"X-Codex-Limit-Reached":               {"true"},
		"X-Codex-Primary-Used-Percent":        {"100"},
		"X-Codex-Primary-Window-Minutes":      {"300"},
		"X-Codex-Primary-Reset-After-Seconds": {"3600"},
	}, observed)
	update, _ := ParseHeaders(identity, http.Header{"X-Codex-Primary-Used-Percent": {"10"}}, observed.Add(time.Minute))
	merged := Merge(initial, update)
	window := getWindow(t, merged, "primary")
	if window.WindowSeconds == nil || *window.WindowSeconds != 18000 || window.Label != "5h" || window.ResetAt == nil || !window.ResetAt.Equal(observed.Add(time.Hour)) {
		t.Fatalf("partial header dropped stable metadata: %+v", window)
	}
	if window.Allowed != nil || window.LimitReached != nil || !window.ObservedAt.Equal(update.ObservedAt) || window.Source != SourceHeaders {
		t.Fatalf("old transient flags or provenance leaked: %+v", window)
	}
	later, _ := ParseHeaders(identity, http.Header{"X-Codex-Primary-Used-Percent": {"5"}}, observed.Add(2*time.Hour))
	window = getWindow(t, Merge(merged, later), "primary")
	if window.ResetAt != nil || window.WindowSeconds == nil || *window.WindowSeconds != 18000 {
		t.Fatalf("expired reset survived or stable duration lost: %+v", window)
	}
}

func getWindow(t *testing.T, snapshot Snapshot, id string) Window {
	t.Helper()
	for _, window := range snapshot.Windows {
		if window.ID == id {
			return window
		}
	}
	t.Fatalf("missing window %s in %+v", id, snapshot)
	return Window{}
}

func assertNumber(t *testing.T, value *float64, want float64) {
	t.Helper()
	if value == nil || math.Abs(*value-want) > 0.000001 {
		t.Fatalf("value=%v, want %v", value, want)
	}
}

func TestFetchKnownFields(t *testing.T) {
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "pro", TierName: "Premium", TierID: "p"},
		Groups: []pluginapi.QuotaGroup{{DisplayName: "Custom", Buckets: []pluginapi.QuotaBucket{
			{Window: "monthly", RemainingFraction: 0, ResetTime: "2026-10-01T00:00:00Z", Description: "not-retained"},
			{Window: "invalid", RemainingFraction: math.NaN()},
			{Window: "overflow", RemainingFraction: 2},
		}}},
		Summary: []pluginapi.QuotaMetric{{Key: "balance", Label: "Balance", Value: 0, Unit: "credits", Format: "number"}, {Key: "bad", Value: math.Inf(1)}},
	}
	// Declarative plugins name their own provider; any non-empty provider is a
	// valid account fact.
	identity := parserIdentity("custom", "a")
	snapshot, ok := ParseFetch(identity, response, observed)
	if !ok || len(snapshot.Windows) != 1 || len(snapshot.Summary) != 1 {
		t.Fatalf("unexpected plugin snapshot: %+v", snapshot)
	}
	assertAccountFacts(t, snapshot, identity)
	window := getWindow(t, snapshot, "fetch:custom:monthly")
	assertNumber(t, window.RemainingPercent, 0)
	assertNumber(t, window.UsedPercent, 100)
	if snapshot.Plan != "pro" || snapshot.TierName != "Premium" || snapshot.TierID != "p" {
		t.Fatalf("subscription fields lost: %+v", snapshot)
	}
	if !snapshot.ObservedAt.Equal(observed) || window.Source != SourceFetch {
		t.Fatal("normalization changed observation metadata")
	}
	encoded, errJSON := json.Marshal(snapshot)
	if errJSON != nil || strings.Contains(string(encoded), "not-retained") {
		t.Fatalf("unexpected serialized snapshot: %s, %v", encoded, errJSON)
	}
	// kimi credentials publish device_id instead of email.
	if kimi, ok := ParseFetch(parserIdentity("kimi", "device-1"), response, observed); !ok || kimi.Account != "device-1" || kimi.AccountKind != "device_id" {
		t.Fatalf("kimi device_id account fact lost: %+v, parsed=%v", kimi, ok)
	}
	// The fetch parser validates account facts exactly like the other parsers.
	for _, identity := range []Identity{parserIdentity("", "a"), parserIdentity("custom", "a\nsecret"), parserIdentity("custom", strings.Repeat("a", maxText+1))} {
		if _, ok := ParseFetch(identity, response, observed); ok {
			t.Fatalf("fetch accepted invalid account facts %+v", identity)
		}
	}
	pluginSnapshot, ok := ParseFetch(parserIdentity("custom", ""), response, observed)
	if !ok || pluginSnapshot.Account != "" {
		t.Fatalf("account-less plugin credential was hidden: %+v, parsed=%v", pluginSnapshot, ok)
	}
	if _, ok := ParseFetch(parserIdentity("custom", "a"), pluginapi.QuotaFetchResponse{}, observed); ok {
		t.Fatal("empty fetch fabricated quota")
	}
	if _, ok := ParseFetch(parserIdentity("custom", "a"), response, time.Time{}); ok {
		t.Fatal("fetch accepted a missing observation time")
	}
}
