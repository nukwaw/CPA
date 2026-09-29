package usagepersist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	// Bound dispatcher CPU/allocation work, including ignored JSON fields. These
	// are admission limits, not collection settings or configurable queue sizes.
	maxUsagePayloadBytes = 256 << 10
	maxUsageTextBytes    = 1024
)

// providerUsage is only a transient decoder. Keys and headers must be hashed or
// normalized before admission; neither this object nor the payload is queued.
// Source, endpoints, failure bodies, client metadata and legacy token fields are
// deliberately absent. Only the built-in provider's canonical breakdown is used.
type providerUsage struct {
	ExecutionID       string    `json:"execution_id"`
	Timestamp         time.Time `json:"timestamp"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Alias             string    `json:"alias"`
	AuthIndex         string    `json:"auth_index"`
	APIKey            string    `json:"api_key"`
	AccessTokenSHA256 string    `json:"access_token_sha256"`
	LatencyMS         int64     `json:"latency_ms"`
	TTFTMS            int64     `json:"ttft_ms"`
	Failed            bool      `json:"failed"`
	Generate          *bool     `json:"generate"`
	Stream            bool      `json:"stream"`
	Fail              struct {
		StatusCode int `json:"status_code"`
	} `json:"fail"`
	TokenBreakdown  usage.TokenBreakdown `json:"token_breakdown"`
	ResponseHeaders http.Header          `json:"response_headers"`
}

// Consume is bounded, nonblocking admission of a passive copy emitted AFTER the
// built-in provider's existing gates and queue publication. It performs no I/O,
// never waits for queue space and never retains the payload or request context.
// Persistence runs on the Store's independent, lifecycle-cancellable worker.
// Health counters report invalid inputs, overflow, shutdown drops and failures.
func (s *Store) Consume(_ context.Context, payload []byte) {
	if s == nil {
		return
	}
	item, err := normalizeUsage(payload)
	if err != nil {
		s.validationFailures.Add(1)
		s.recordDrop(1)
		return
	}
	if item == nil { // Non-generation records are not accounting events.
		return
	}
	// Credential-manager locks may cover core storage I/O. Keep producer-supplied
	// quota evidence untouched here; the independent worker verifies it later.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.workerStopped || s.workerCtx.Err() != nil {
		s.recordDrop(1)
		return
	}
	// This lock protects only lifecycle/admission bookkeeping, never backend I/O.
	// Worker completion takes the same lock, after admission has been counted.
	select {
	case s.queue <- *item:
		s.accepted++
		s.pendingEvents.Add(1)
	default:
		s.queueOverflows.Add(1)
		s.recordDrop(1)
	}
}

// bindQueuedUsageQuota runs only on the independent persistence worker, after
// accounting insertion. A live source lookup can wait on core credential/storage
// locks; it must never run in Consume or an original management handler.
// Generation still comes only from source-time token evidence. In particular an
// old token hash never acquires today's account/project/org selector, and Codex's
// account-scoped header samples remain unsupported by the current producer.
func (s *Store) bindQueuedUsageQuota(ctx context.Context, item *queuedUsage) {
	if item.Quota == nil {
		return
	}
	if ctx.Err() != nil || item.Quota.CredentialGeneration == "" || item.AccessTokenSHA256 == "" {
		item.Quota = nil
		s.skippedQuotaHeaders.Add(1)
		return
	}
	binding, ok := s.quotaBinding(item.Quota.Provider, item.Quota.AuthIndex, true)
	if ctx.Err() != nil || !ok || !binding.TokenScoped || binding.AccessTokenSHA256 != item.AccessTokenSHA256 || binding.CredentialGeneration != item.Quota.CredentialGeneration {
		item.Quota = nil
		s.skippedQuotaHeaders.Add(1)
		return
	}
	item.Quota.Revision = binding.Revision
}

func normalizeUsage(payload []byte) (*queuedUsage, error) {
	if len(payload) == 0 || len(payload) > maxUsagePayloadBytes {
		return nil, errors.New("built-in usage JSON exceeds admission bounds")
	}
	var record providerUsage
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, errors.New("invalid built-in usage JSON")
	}
	if record.Generate != nil && !*record.Generate {
		return nil, nil
	}
	event, err := eventFromProvider(record)
	if err != nil {
		return nil, err
	}
	item := &queuedUsage{Event: event}
	if validTokenHash(record.AccessTokenSHA256) {
		item.AccessTokenSHA256 = strings.Clone(record.AccessTokenSHA256)
	}
	// The provider has no header-observation timestamp. Approximate completion
	// time without allowing a malicious latency to overflow time.Duration.
	latency := min(max(record.LatencyMS, 0), int64(math.MaxInt64/time.Millisecond))
	observedAt := event.RequestedAt.Add(time.Duration(latency) * time.Millisecond)
	if snapshot, ok := quota.ParseHeaders(quota.Identity{Provider: event.Provider, AuthIndex: event.AuthIndex}, record.ResponseHeaders, observedAt); ok {
		if snapshot.Provider == "claude" && item.AccessTokenSHA256 != "" {
			snapshot.CredentialGeneration = TokenQuotaGeneration(snapshot.Provider, item.AccessTokenSHA256)
		}
		item.Quota = &snapshot
	}
	return item, nil
}

func eventFromProvider(record providerUsage) (Event, error) {
	if strings.TrimSpace(record.ExecutionID) == "" || record.Timestamp.IsZero() {
		return Event{}, errors.New("built-in usage execution ID and timestamp are required")
	}
	b := record.TokenBreakdown
	if !b.Valid() {
		return Event{}, errors.New("missing or invalid canonical usage token breakdown")
	}
	e := Event{ID: strings.TrimSpace(record.ExecutionID), RequestedAt: record.Timestamp.UTC(),
		Provider: record.Provider, Model: record.Model, Alias: record.Alias, AuthIndex: record.AuthIndex,
		InputTokens: b.Input.TotalTokens, OutputTokens: b.Output.TotalTokens,
		ReasoningTokens: b.Output.ReasoningTokens, CacheReadTokens: b.Input.CacheReadTokens,
		CacheWriteTokens: b.Input.CacheWriteTokens, TotalTokens: b.TotalTokens,
		UnclassifiedTokens: b.UnclassifiedTokens, AccountingQuality: string(b.Quality),
		LatencyMS: float64(max(record.LatencyMS, 0)), TTFTMS: float64(max(record.TTFTMS, 0)),
		Failed: record.Failed, StatusCode: record.Fail.StatusCode, Stream: record.Stream,
	}
	// PostgreSQL text/JSONB cannot represent NUL. Keep accounting with a visible
	// replacement, rejecting overlong fields instead of silently merging IDs.
	for _, field := range []*string{&e.ID, &e.Provider, &e.Model, &e.Alias, &e.AuthIndex} {
		*field = strings.ReplaceAll(*field, "\x00", "�")
		if len(*field) > maxUsageTextBytes {
			return Event{}, errors.New("built-in usage field exceeds admission bounds")
		}
	}
	if record.APIKey != "" {
		hash := sha256.Sum256([]byte(record.APIKey))
		e.KeyID = hex.EncodeToString(hash[:])
	}
	return e, nil
}
