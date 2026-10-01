package quota

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

func newSnapshot(identity Identity, source string, observedAt time.Time) (Snapshot, bool) {
	provider, account, accountKind, valid := identityFacts(identity)
	if !valid || observedAt.IsZero() {
		return Snapshot{}, false
	}
	return Snapshot{Provider: provider, Account: account, AccountKind: accountKind, Source: source, ObservedAt: observedAt.UTC(), Windows: []Window{}}, true
}

// identityFacts validates and normalizes the caller-supplied account facts.
// The provider must be present and well formed. The account property is
// optional: a credential without one is still recorded, grouped by provider
// alone. A non-empty account must stay bounded and free of control characters
// so that persisted facts cannot carry injected content.
func identityFacts(identity Identity) (provider, account, accountKind string, valid bool) {
	provider = strings.ToLower(safeText(identity.Provider))
	if provider == "" {
		return "", "", "", false
	}
	account, ok := accountFact(identity.Account)
	if !ok {
		return "", "", "", false
	}
	return provider, account, safeText(identity.AccountKind), true
}

// accountFact bounds an optional account property. Oversized or
// control-character-bearing values are rejected instead of silently dropped,
// because an invalid account must never be recorded as a different credential.
func accountFact(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	if len(value) > maxText {
		return "", false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", false
		}
	}
	return value, true
}

func finish(snapshot Snapshot) (Snapshot, bool) {
	if len(snapshot.Windows) == 0 && snapshot.Credits == nil && len(snapshot.Summary) == 0 {
		return Snapshot{}, false
	}
	return snapshot, true
}

func addWindow(snapshot *Snapshot, window Window) {
	if len(snapshot.Windows) >= maxWindows || window.ID == "" {
		return
	}
	if window.UsedPercent == nil && window.RemainingPercent == nil && window.Used == nil && window.Limit == nil && window.Remaining == nil {
		return
	}
	if window.Used == nil && window.Limit != nil && window.Remaining != nil {
		window.Used = nonnegative(*window.Limit - *window.Remaining)
	}
	if window.Remaining == nil && window.Limit != nil && window.Used != nil {
		window.Remaining = nonnegative(math.Max(0, *window.Limit-*window.Used))
	}
	if window.UsedPercent == nil && window.Used != nil && window.Limit != nil && *window.Limit > 0 {
		window.UsedPercent = nonnegative(*window.Used / *window.Limit * 100)
	}
	if window.UsedPercent == nil && window.RemainingPercent != nil {
		window.UsedPercent = nonnegative(math.Max(0, 100-*window.RemainingPercent))
	}
	if window.RemainingPercent == nil && window.UsedPercent != nil {
		window.RemainingPercent = nonnegative(math.Max(0, 100-*window.UsedPercent))
	}
	window.ObservedAt, window.Source = snapshot.ObservedAt, snapshot.Source
	for i := range snapshot.Windows {
		if snapshot.Windows[i].ID == window.ID {
			snapshot.Windows[i] = window
			return
		}
	}
	snapshot.Windows = append(snapshot.Windows, window)
}

func safeText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxText {
		return ""
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return ""
		}
	}
	return value
}

func identifier(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(safeText(value)) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			result.WriteRune(character)
		} else if result.Len() > 0 && !strings.HasSuffix(result.String(), "-") {
			result.WriteByte('-')
		}
	}
	return strings.Trim(result.String(), "-")
}

func nonnegative(value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return nil
	}
	return ptr(value)
}

func number(value string) *float64 {
	parsed, errParse := strconv.ParseFloat(safeText(value), 64)
	if errParse != nil {
		return nil
	}
	return nonnegative(parsed)
}

func integer(value string) *int64 {
	parsed, errParse := strconv.ParseInt(safeText(value), 10, 64)
	if errParse != nil || parsed < 0 {
		return nil
	}
	return &parsed
}

func boolean(value string) *bool {
	switch strings.ToLower(safeText(value)) {
	case "true", "1":
		return ptr(true)
	case "false", "0":
		return ptr(false)
	default:
		return nil
	}
}

func resetTime(value string) *time.Time {
	value = safeText(value)
	if parsed, errParse := time.Parse(time.RFC3339Nano, value); errParse == nil && parsed.Year() > 0 {
		return ptr(parsed.UTC())
	}
	if seconds := integer(value); seconds != nil && *seconds > 0 && *seconds <= 253402300799 {
		return ptr(time.Unix(*seconds, 0).UTC())
	}
	return nil
}

func relativeReset(observed time.Time, seconds *int64) *time.Time {
	if seconds == nil || *seconds > math.MaxInt64/int64(time.Second) {
		return nil
	}
	reset := observed.Add(time.Duration(*seconds) * time.Second).UTC()
	if reset.Year() < 1 || reset.Year() > 9999 {
		return nil
	}
	return &reset
}

func sortedKeys[T any](object map[string]T) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func windowLabel(role string, seconds *int64) string {
	if seconds != nil {
		switch *seconds {
		case 18000:
			return "5h"
		case 604800:
			return "Weekly"
		case 2592000, 2628000:
			return "Monthly"
		}
	}
	return role
}
