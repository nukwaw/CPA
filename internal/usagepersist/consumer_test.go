package usagepersist

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestStoreIsPassiveConsumerNotSDKPlugin(t *testing.T) {
	store := openTestStore(t)
	if _, ok := any(store).(usage.Plugin); ok {
		t.Fatal("Store must not implement a parallel raw SDK usage plugin")
	}
}

func TestPassiveConsumerUsesOriginalGatesWithoutStealingQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	previousEnabled, previousStatistics := redisqueue.Enabled(), redisqueue.UsageStatisticsEnabled()
	defer redisqueue.SetEnabled(previousEnabled)
	defer redisqueue.SetUsageStatisticsEnabled(previousStatistics)
	redisqueue.SetEnabled(true)
	redisqueue.SetUsageStatisticsEnabled(true)
	redisqueue.PopOldest(10000)
	stop := redisqueue.ObserveUsage(s.Consume)
	defer stop()
	publishThroughBuiltin(t, fixtureRecord("provider-enabled", time.Now()))
	redisqueue.SetUsageStatisticsEnabled(false)
	publishThroughBuiltin(t, fixtureRecord("statistics-disabled", time.Now()))
	redisqueue.SetUsageStatisticsEnabled(true)
	publishThroughBuiltin(t, fixtureRecord("provider-reenabled", time.Now()))
	// Persistence observes a copy. The original queue remains available to its
	// normal management/Home consumers instead of being popped by this addon.
	queued := redisqueue.PopOldest(100)
	if len(queued) != 2 {
		t.Fatalf("passive observer stole or changed provider queue: got %d", len(queued))
	}
	for _, payload := range queued {
		if strings.Contains(string(payload), "statistics-disabled") {
			t.Fatal("builtin statistics gate bypassed")
		}
	}
	redisqueue.SetEnabled(false)
	publishThroughBuiltin(t, fixtureRecord("provider-disabled", time.Now()))
	redisqueue.SetEnabled(true)
	publishThroughBuiltin(t, fixtureRecord("provider-restored", time.Now()))
	flushFixture(t, s)
	page, err := s.Events(ctx, Filter{}, 100, 0)
	if err != nil || page.Total != 3 {
		t.Fatalf("original provider gates: %#v %v", page, err)
	}
	stop()
	publishThroughBuiltin(t, fixtureRecord("after-detach", time.Now()))
	page, err = s.Events(ctx, Filter{}, 100, 0)
	if err != nil || page.Total != 3 {
		t.Fatalf("detached observer still consumed: %#v %v", page, err)
	}

	redisqueue.SetUsageStatisticsEnabled(false)
	if _, err = s.SetPrice(ctx, fixturePrice()); err != nil {
		t.Fatal(err)
	}
	used := 25.0
	if err = s.mergeQuota(ctx, quota.Snapshot{Provider: "codex", Account: fixtureAccount, Source: quota.SourceHeaders, ObservedAt: time.Now().UTC(), Windows: []quota.Window{{ID: "primary", UsedPercent: &used}}}); err != nil {
		t.Fatal(err)
	}
	quotas, err := s.Quotas(ctx)
	if err != nil || len(quotas) != 1 {
		t.Fatalf("disabled usage blocked independent quota merge: %#v %v", quotas, err)
	}
}

func TestConsumerJSONValidationMissingGenerateAndFailureStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, invalid := range []string{`not-json`, `{}`, `{"execution_id":"id"}`, `{"timestamp":"2026-09-01T00:00:00Z"}`} {
		if _, err := normalizeUsage([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid provider output: %s", invalid)
		}
	}
	r := fixtureRecord("missing-generate", time.Now())
	r.Failed = true
	r.Fail.StatusCode = 429
	var value map[string]any
	if err := json.Unmarshal(fixturePayload(r), &value); err != nil {
		t.Fatal(err)
	}
	delete(value, "generate")
	value["client_ip"] = "private-client-ip"
	value["prompt"] = "private-prompt-body"
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	s.Consume(ctx, payload)
	s.Consume(ctx, payload)
	flushFixture(t, s)
	page, err := s.Events(ctx, Filter{}, 100, 0)
	if err != nil || page.Total != 1 {
		t.Fatalf("missing generate/dedup: %#v %v", page, err)
	}
	e := page.Events[0]
	if !e.Failed || e.StatusCode != 429 || len(e.KeyID) != 64 || e.KeyID == r.APIKey || e.TotalTokens != 1300 {
		t.Fatalf("provider fields lost: %#v", e)
	}
	// The account fact is persisted deliberately; the deleted identity fields are
	// neither carried on the wire nor stored.
	if e.Account != fixtureAccount || e.AccountKind != "email" {
		t.Fatalf("account facts lost: %#v", e)
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{r.APIKey, "sk-private-error-key", "private-client-ip", "private-prompt-body", "auth_index", "access_token_sha256"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("event contains ignored input %q", secret)
		}
	}
	// The raw failure body is replaced by its sanitized provider message.
	if !strings.Contains(string(encoded), `"error_text":"rate_limit_error: slow down, key [redacted] rejected"`) {
		t.Fatalf("event lost the sanitized failure message: %s", encoded)
	}
}

// A failure message is shown on hover, so it must stay a short provider reason
// with no credential in it: the journal is plaintext and the PostgreSQL payload is
// queryable.
func TestConsumeKeepsOnlySanitizedBoundedFailureMessage(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		expect string
	}{
		{name: "structured-error", body: `{"error":{"type":"invalid_request_error","message":"max_tokens too large"}}`, expect: "invalid_request_error: max_tokens too large"},
		{name: "structured-message", body: `{"message":"upstream unavailable"}`, expect: "upstream unavailable"},
		{name: "plain-text", body: "connection reset by peer", expect: "connection reset by peer"},
		{name: "labelled-secret", body: `{"error":{"message":"bad key: access_token=eyJhbGciOiJIUzI1NiJ9.invalid.sig"}}`, expect: "bad key: access_token=[redacted]"},
		{name: "unlabelled-secret", body: `{"error":{"message":"sent sk-ant-oat01-secretvalue1234567890"}}`, expect: "sent [redacted]"},
		{name: "hex-secret", body: `{"error":{"message":"fingerprint 9f8e7d6c5b4a39281706f5e4d3c2b1a0f9e8d7c6b5a4938271605f4e3d2c1b0a"}}`, expect: "fingerprint [redacted]"},
		{name: "control-characters", body: "line one\n\r\t line two", expect: "line one line two"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := openTestStore(t)
			r := fixtureRecord("sanitize", time.Now().Add(-time.Minute))
			r.Failed, r.Fail.StatusCode, r.Fail.Body = true, 400, test.body
			if err := s.recordFixture(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			events, err := s.Events(context.Background(), Filter{}, 10, 0)
			if err != nil || len(events.Events) != 1 || events.Events[0].ErrorText != test.expect {
				t.Fatalf("sanitized message = %q, want %q (%v)", events.Events[0].ErrorText, test.expect, err)
			}
		})
	}
	s := openTestStore(t)
	r := fixtureRecord("sanitize-bound", time.Now().Add(-time.Minute))
	r.Failed, r.Fail.StatusCode, r.Fail.Body = true, 500, strings.Repeat("é", 1000)
	if err := s.recordFixture(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(context.Background(), Filter{}, 10, 0)
	if err != nil || len(events.Events) != 1 {
		t.Fatalf("bounded message missing: %v", err)
	}
	message := events.Events[0].ErrorText
	if len(message) > maxErrorTextBytes+len("…") || !utf8.ValidString(message) || !strings.HasSuffix(message, "…") {
		t.Fatalf("message not bounded at a rune boundary: %d bytes %q", len(message), message)
	}
	// NUL cannot be stored in PostgreSQL text/JSONB and must not abort accounting.
	nul := fixtureRecord("sanitize-nul", time.Now().Add(-time.Minute))
	var envelope map[string]any
	if err := json.Unmarshal(fixturePayload(nul), &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["fail"] = map[string]any{"status_code": 500, "body": "bad\x00request"}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	s.Consume(context.Background(), payload)
	flushFixture(t, s)
	page, err := s.Events(context.Background(), Filter{}, 10, 0)
	if err != nil || page.Total != 2 {
		t.Fatalf("NUL failure rejected accounting: %#v %v", page, err)
	}
	if got := page.Events[0].ErrorText; got != "bad�request" {
		t.Fatalf("NUL failure message = %q", got)
	}
}

func TestConsumeNormalizesOnlyKnownProviderHeaders(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := fixtureRecord("header-provider", time.Now().Add(-time.Minute))
	r.Provider = "claude"
	r.ResponseHeaders = http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.4"}, "Authorization": []string{"private-header"}, "Set-Cookie": []string{"private-cookie"}}
	s.Consume(ctx, fixturePayload(r))
	flushFixture(t, s)
	quotas, err := s.Quotas(ctx)
	if err != nil || len(quotas) != 1 || quotas[0].Account != fixtureAccount {
		t.Fatalf("quota extraction failed: %#v %v", quotas, err)
	}
	encoded, err := json.Marshal(quotas)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("raw response headers persisted")
	}
	// A provider with no recognizable headers is recorded as usage only; it can
	// never add a quota row under the account the request ran against.
	r.RequestID = "unknown-provider"
	r.Provider = "unknown"
	s.Consume(ctx, fixturePayload(r))
	flushFixture(t, s)
	quotas, err = s.Quotas(ctx)
	if err != nil || len(quotas) != 1 {
		t.Fatalf("unknown provider accepted quota headers: %#v %v", quotas, err)
	}
	page, err := s.Events(ctx, Filter{}, 10, 0)
	if err != nil || page.Total != 2 {
		t.Fatalf("header normalization dropped accounting: %#v %v", page, err)
	}
}
