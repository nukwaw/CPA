package usagepersist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	// Bound dispatcher CPU/allocation work, including ignored JSON fields. These
	// are admission limits, not collection settings or configurable queue sizes.
	maxUsagePayloadBytes = 256 << 10
	maxUsageTextBytes    = 1024
	// maxErrorTextBytes bounds the sanitized provider message kept for display.
	maxErrorTextBytes = 300
	// maxAccountFactBytes bounds the account label persisted for grouping.
	maxAccountFactBytes = 256
)

// providerUsage is only a transient decoder. Keys and headers must be hashed or
// normalized before admission; neither this object nor the payload is queued.
// Source, endpoints, client metadata and legacy token fields are deliberately
// absent. A failure body is kept only as the sanitized message below. Only the
// built-in provider's canonical breakdown is used.
type providerUsage struct {
	ExecutionID         string    `json:"execution_id"`
	Timestamp           time.Time `json:"timestamp"`
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	Alias               string    `json:"alias"`
	ResponseModel       string    `json:"response_model"`
	ServiceTier         string    `json:"service_tier"`
	ResponseServiceTier string    `json:"response_service_tier"`
	APIKey              string    `json:"api_key"`
	// Account facts are stamped by the producer, where the live credential is
	// known. They group usage by (provider, account); they are not credentials.
	Account     string `json:"account"`
	AccountKind string `json:"account_kind"`
	LatencyMS   int64  `json:"latency_ms"`
	TTFTMS      int64  `json:"ttft_ms"`
	Failed      bool   `json:"failed"`
	Generate    *bool  `json:"generate"`
	Stream      bool   `json:"stream"`
	Fail        struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
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
	// The provider has no header-observation timestamp. Approximate completion
	// time without allowing a malicious latency to overflow time.Duration.
	latency := min(max(record.LatencyMS, 0), int64(math.MaxInt64/time.Millisecond))
	observedAt := event.RequestedAt.Add(time.Duration(latency) * time.Millisecond)
	// The header observation carries the same account facts as its usage record:
	// the producer stamps them where the live credential is known, so attribute
	// here without another lookup.
	identity := quota.Identity{Provider: event.Provider, Account: event.Account, AccountKind: event.AccountKind}
	if snapshot, ok := quota.ParseHeaders(identity, record.ResponseHeaders, observedAt); ok {
		item.Quota = &snapshot
	}
	return item, nil
}

// sanitizeAccountFact bounds the account label used to group usage. It is a fact
// recorded at the producer, never a credential, and it never becomes a storage key
// component without the same validation the projection applies.
func sanitizeAccountFact(value string) string {
	return sanitizeUsageText(value, maxAccountFactBytes)
}

// sanitizeAccountKind keeps only a kind we can name ourselves, so a producer
// cannot label an arbitrary credential property as the account.
func sanitizeAccountKind(value string) string {
	value = strings.TrimSpace(value)
	if value == "email" || value == "device_id" {
		return value
	}
	return ""
}

func sanitizeUsageText(value string, limit int) string {
	return truncateUTF8(normalizeUsageText(value), limit)
}

// normalizeUsageText trims and replaces NUL, which neither the plaintext journal
// nor PostgreSQL text/JSONB can represent.
func normalizeUsageText(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "\x00", "�")
}

// truncateUTF8 cuts at a rune boundary so stored text stays valid UTF-8.
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// sanitizeErrorText keeps a short, human-readable reason for a failed request.
// The raw body is never stored: the message is taken from a structured provider
// error when one parses, and every token-like substring is replaced before the
// result is bounded, so the plaintext journal and PostgreSQL payload stay free of
// credentials even when a provider echoes one back.
func sanitizeErrorText(body string) string {
	message := providerErrorMessage(body)
	if message == "" {
		return ""
	}
	message = errorRedactions.ReplaceAllString(message, "$1[redacted]")
	message = secretRun.ReplaceAllString(message, "[redacted]")
	message = normalizeUsageText(strings.Join(strings.Fields(message), " "))
	if message == "" {
		return ""
	}
	if len(message) > maxErrorTextBytes {
		message = truncateUTF8(message, maxErrorTextBytes) + "…"
	}
	return message
}

// providerErrorMessage prefers the structured error object providers return, so a
// wrapper around the JSON payload cannot smuggle unrelated text into the stored
// message.
func providerErrorMessage(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	var decoded struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(body), &decoded) == nil {
		if message := strings.TrimSpace(decoded.Error.Message); message != "" {
			if kind := strings.TrimSpace(decoded.Error.Type); kind != "" {
				return kind + ": " + message
			}
			return message
		}
		if message := strings.TrimSpace(decoded.Message); message != "" {
			return message
		}
	}
	return body
}

var (
	// errorRedactions covers labelled secrets in the common provider spellings.
	errorRedactions = regexp.MustCompile(`(?i)((?:api[-_ ]?key|access[-_ ]?token|refresh[-_ ]?token|id[-_ ]?token|session[-_ ]?token|authorization|bearer|cookie|secret|password)\s*["']?\s*[:=]\s*["']?\s*)([^\s"',;)]{4,})`)
	// secretRun covers unlabelled high-entropy credentials such as sk-... keys,
	// JSON Web Tokens and long hexadecimal or base64 runs.
	secretRun = regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9_-]{6,}|eyJ[a-z0-9_-]{8,}(?:\.[a-z0-9_-]+){1,2}|[a-f0-9]{32,}|[a-z0-9+/]{40,}={0,2})\b`)
)

func eventFromProvider(record providerUsage) (Event, error) {
	if strings.TrimSpace(record.ExecutionID) == "" || record.Timestamp.IsZero() {
		return Event{}, errors.New("built-in usage execution ID and timestamp are required")
	}
	b := record.TokenBreakdown
	if !b.Valid() {
		return Event{}, errors.New("missing or invalid canonical usage token breakdown")
	}
	e := Event{ID: strings.TrimSpace(record.ExecutionID), RequestedAt: record.Timestamp.UTC(),
		Provider: record.Provider, Model: record.Model, Alias: record.Alias,
		Account: sanitizeAccountFact(record.Account), AccountKind: sanitizeAccountKind(record.AccountKind),
		ResponseModel: sanitizeUsageText(record.ResponseModel, maxUsageTextBytes),
		ServiceTier:   sanitizeUsageText(record.ServiceTier, maxUsageTextBytes),
		// Only a sanitized provider message is retained, never the raw body.
		ResponseServiceTier: sanitizeUsageText(record.ResponseServiceTier, maxUsageTextBytes),
		ErrorText:           sanitizeErrorText(record.Fail.Body),
		InputTokens:         b.Input.TotalTokens, OutputTokens: b.Output.TotalTokens,
		ReasoningTokens: b.Output.ReasoningTokens, CacheReadTokens: b.Input.CacheReadTokens,
		CacheWriteTokens: b.Input.CacheWriteTokens, TotalTokens: b.TotalTokens,
		UnclassifiedTokens: b.UnclassifiedTokens, AccountingQuality: string(b.Quality),
		LatencyMS: float64(max(record.LatencyMS, 0)), TTFTMS: float64(max(record.TTFTMS, 0)),
		Failed: record.Failed, StatusCode: record.Fail.StatusCode, Stream: record.Stream,
	}
	// PostgreSQL text/JSONB cannot represent NUL. Keep accounting with a visible
	// replacement, rejecting overlong fields instead of silently merging IDs.
	for _, field := range []*string{&e.ID, &e.Provider, &e.Model, &e.Alias, &e.Account, &e.AccountKind} {
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
