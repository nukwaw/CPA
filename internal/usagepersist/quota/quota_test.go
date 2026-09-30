package quota

import (
	"encoding/json"
	"fmt"
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
func TestAccountFactsContract(t *testing.T) {
	encodedIdentity, errJSON := json.Marshal(parserIdentity("codex", "user@example.invalid"))
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	if want := `{"provider":"codex","account":"user@example.invalid","account_kind":"email"}`; string(encodedIdentity) != want {
		t.Fatalf("identity contract changed: got %s, want %s", encodedIdentity, want)
	}
	identity := parserIdentity("codex", "user@example.invalid")
	snapshot, ok := ParseAPICall(identity, "https://chatgpt.com/backend-api/wham/usage", 200, nil, []byte(`{"rate_limit":{"primary_window":{"used_percent":10}}}`), observed)
	if !ok {
		t.Fatal("fixture not recognized")
	}
	assertAccountFacts(t, snapshot, identity)
	encoded, errJSON := json.Marshal(snapshot)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	prefix := `{"provider":"codex","account":"user@example.invalid","account_kind":"email","source":"management_api","observed_at":"2026-09-29T12:00:00Z","windows":[`
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

func TestAPICallKnownProviderPayloads(t *testing.T) {
	tests := []struct {
		provider, endpoint, body, id string
		wantUsed                     float64
	}{
		{"codex", "https://chatgpt.com/backend-api/wham/usage", `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":0}},"secret":"body-secret"}`, "primary", 0},
		{"codex", "https://chatgpt.com/backend-api/wham/usage", `{"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","rate_limit":{"primary_window":{"used_percent":"15"}}}]}`, "additional:gpt-5-3-codex-spark:primary", 15},
		{"claude", "https://api.anthropic.com/api/oauth/usage", `{"five_hour":{"utilization":37,"resets_at":"2026-09-30T10:00:00Z"},"account":{"access_token":"body-secret"}}`, "five_hour", 37},
		{"claude", "https://api.anthropic.com/api/oauth/usage", `{"seven_day_sonnet":{"utilization":"0"}}`, "seven_day_sonnet", 0},
		{"gemini-cli", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", `{"buckets":[{"modelId":"gemini-2.5-pro","tokenType":"REQUESTS","remainingFraction":0.8,"remainingAmount":80,"resetTime":"2026-10-01T00:00:00Z"}]}`, "bucket:gemini-2-5-pro:requests", 20},
		{"antigravity", "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary", `{"body":{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"pro","window":"weekly","remainingFraction":"25%","resetTime":"2026-10-01T00:00:00Z"}]}]}}`, "group:gemini-models:pro", 75},
		{"kimi", "https://api.kimi.com/coding/v1/usages", `{"usage":{"limit":100,"remaining":25},"limits":[{"name":"short","detail":{"limit":10,"remaining":0},"window":{"duration":5,"timeUnit":"hour"}}]}`, "limits:short", 100},
		{"xai", "https://cli-chat-proxy.grok.com/v1/billing?format=credits", `{"config":{"currentPeriod":{"type":"weekly","end":"2026-10-01T00:00:00Z"},"creditUsagePercent":10,"productUsage":[{"product":"grok","usagePercent":30}]}}`, "billing.weekly", 10},
		{"xai", "https://cli-chat-proxy.grok.com/v1/billing", `{"config":{"monthlyLimit":{"val":20000},"used":{"val":10000},"onDemandCap":{"val":500},"onDemandUsed":{"val":0},"billingPeriodEnd":"2026-10-01T00:00:00Z"}}`, "billing.monthly", 50},
	}
	for _, test := range tests {
		t.Run(test.provider+"/"+test.id, func(t *testing.T) {
			identity := parserIdentity(test.provider, "a@example.invalid")
			snapshot, ok := ParseAPICall(identity, test.endpoint, 200, nil, []byte(test.body), observed)
			if !ok {
				t.Fatalf("known quota body unrecognized: %s", test.body)
			}
			assertAccountFacts(t, snapshot, identity)
			window := getWindow(t, snapshot, test.id)
			assertNumber(t, window.UsedPercent, test.wantUsed)
			if window.Source != SourceAPICall || !window.ObservedAt.Equal(observed) {
				t.Fatalf("wrong provenance: %+v", window)
			}
			encoded, errJSON := json.Marshal(snapshot)
			if errJSON != nil {
				t.Fatal(errJSON)
			}
			if strings.Contains(string(encoded), "body-secret") || strings.Contains(string(encoded), "https://") {
				t.Fatalf("raw response/URL persisted: %s", encoded)
			}
		})
	}
}

func TestAPICallRejectsUnknownErrorAndMalformedResponses(t *testing.T) {
	body := []byte(`{"rate_limit":{"primary_window":{"used_percent":100}}}`)
	for _, endpoint := range []string{
		"https://evil.example/backend-api/wham/usage", "https://chatgpt.com.evil.example/backend-api/wham/usage",
		"https://chatgpt.com/backend-api/conversation", "http://chatgpt.com/backend-api/wham/usage",
		"https://chatgpt.com:444/backend-api/wham/usage", "https://user:password@chatgpt.com/backend-api/wham/usage",
		":not-a-url", "https://chatgpt.com/backend-api/wham/usage/extra",
	} {
		if _, ok := ParseAPICall(parserIdentity("codex", "a"), endpoint, 200, nil, body, observed); ok {
			t.Fatalf("accepted untrusted endpoint %s", endpoint)
		}
	}
	endpoint := "https://chatgpt.com/backend-api/wham/usage"
	for _, status := range []int{0, 199, 300, 401, 429, 500} {
		if _, ok := ParseAPICall(parserIdentity("codex", "a"), endpoint, status, nil, body, observed); ok {
			t.Fatalf("accepted error/status %d as body quota", status)
		}
	}
	for _, invalid := range []string{`not-json`, `{}`, `null`, `{"rate_limit":{"primary_window":{"used_percent":null}}}`, `{"rate_limit":{"primary_window":{"used_percent":"NaN"}}}`, strings.Repeat(" ", maxBodyBytes+1)} {
		if _, ok := ParseAPICall(parserIdentity("codex", "a"), endpoint, 200, nil, []byte(invalid), observed); ok {
			t.Fatal("invalid response fabricated quota")
		}
	}
	if _, ok := ParseAPICall(parserIdentity("claude", "a"), endpoint, 200, nil, body, observed); ok {
		t.Fatal("accepted provider-mismatched endpoint")
	}
	// Unknown providers own no quota endpoint; the account facts cannot widen it.
	for _, provider := range []string{"unknown-provider", "", "co\ndex"} {
		if _, ok := ParseAPICall(parserIdentity(provider, "a"), endpoint, 200, nil, body, observed); ok {
			t.Fatalf("accepted unknown provider %q", provider)
		}
	}
	snapshot, ok := ParseAPICall(parserIdentity("codex", "a"), endpoint, 429, http.Header{"X-Codex-Primary-Used-Percent": {"100"}}, []byte(`{"secret":"error-body"}`), observed)
	if !ok || getWindow(t, snapshot, "primary").Source != SourceHeaders {
		t.Fatal("measured error-response header lost")
	}
}

func TestAPICallAliasesNullAndIndependentWindows(t *testing.T) {
	body := `{"planType":"pro","rateLimit":{"primaryWindow":{"usedPercent":"NaN"},"secondaryWindow":{"usedPercent":"0","limitWindowSeconds":"604800","resetAt":"1790704800","resetAfterSeconds":"10"}}}`
	encoded, errJSON := json.Marshal(body)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	snapshot, ok := ParseAPICall(parserIdentity("codex", "a"), "https://chatgpt.com/backend-api/wham/usage", 200, nil, encoded, observed)
	if !ok || snapshot.Plan != "pro" || len(snapshot.Windows) != 1 {
		t.Fatalf("valid independent window lost: %+v", snapshot)
	}
	window := getWindow(t, snapshot, "secondary")
	assertNumber(t, window.UsedPercent, 0)
	if window.ResetAt.Unix() != 1790704800 || *window.WindowSeconds != 604800 {
		t.Fatal("camelCase fields or numeric strings lost")
	}
}

func TestMergeOrdersWindowsAndDoesNotAlias(t *testing.T) {
	identity := parserIdentity("codex", "a@example.invalid")
	initial, _ := ParseAPICall(identity, "https://chatgpt.com/backend-api/wham/usage", 200, nil,
		[]byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":20}}}`), observed)
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
	initial, _ := ParseAPICall(identity, "https://chatgpt.com/backend-api/wham/usage", 200, nil,
		[]byte(`{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`), observed)
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

func TestClaudeCurrentScopedFablePayload(t *testing.T) {
	body := []byte(`{"iguana_necktie":{"utilization":90},"limits":[
		{"kind":"weekly_scoped","percent":20,"scope":{"model":{"display_name":"Fable"}}},
		{"kind":"weekly_scoped","percent":0,"is_active":true,"resets_at":"2026-10-01T00:00:00Z","scope":{"model":{"display_name":"Fable 5"}}},
		{"kind":"weekly_scoped","percent":70,"scope":{"model":{"display_name":"Other"}}}
	]}`)
	snapshot, ok := ParseAPICall(parserIdentity("claude", "a"), "https://api.anthropic.com/api/oauth/usage", 200, nil, body, observed)
	if !ok || len(snapshot.Windows) != 1 {
		t.Fatalf("Fable compatibility row duplicated: %+v", snapshot)
	}
	window := getWindow(t, snapshot, "iguana_necktie")
	assertNumber(t, window.UsedPercent, 0)
	if window.Model != "fable" || window.WindowSeconds == nil || *window.WindowSeconds != 604800 || window.ResetAt == nil {
		t.Fatalf("active Fable limit lost: %+v", window)
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

func TestBoundedWindows(t *testing.T) {
	var buckets []string
	for i := 0; i < maxWindows*2; i++ {
		buckets = append(buckets, fmt.Sprintf(`{"modelId":"model-%d","remainingFraction":0}`, i))
	}
	body := []byte(`{"buckets":[` + strings.Join(buckets, ",") + `]}`)
	snapshot, ok := ParseAPICall(parserIdentity("gemini-cli", "a"), "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", 200, nil, body, observed)
	if !ok || len(snapshot.Windows) != maxWindows {
		t.Fatalf("windows not bounded: %d", len(snapshot.Windows))
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
