package usagepersist

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	log "github.com/sirupsen/logrus"
)

// quotaHistoryNamespace stores the bounded quota observation time series behind
// the /quota/summary endpoint. It is diagnostic state: recording is best-effort
// and an unusable history backend must never fail an accepted observation.
const quotaHistoryNamespace = "quota_history"

// quotaHistoryLimit bounds one provider/auth-index row. The row is rewritten for
// every accepted observation, so the bound keeps each write small and its cost
// independent of how long a credential has been in use.
const quotaHistoryLimit = 400

// maxQuotaIdentityText mirrors the existing bound applied to provider and
// auth-index identity text elsewhere in this package.
const maxQuotaIdentityText = 256

// quotaHistoryEntry is one accepted normalized observation. It intentionally
// carries only a timestamp, its provenance and window utilization percentages:
// never tokens, hashes, account labels, emails, URLs or provider payloads.
type quotaHistoryEntry struct {
	ObservedAt time.Time          `json:"observed_at"`
	Source     string             `json:"source"`
	Windows    map[string]float64 `json:"windows"`
}

// quotaHistoryRow is keyed by quotaStoreKey(provider, account), so each account
// owns its own history row and a different account can never inherit it.
type quotaHistoryRow struct {
	Observations []quotaHistoryEntry `json:"observations"`
}

// quotaHistoryWindows keeps only the window ids and percentages a chart can use.
// Providers that report grouped buckets have no windows; those observations are
// skipped instead of inventing values.
func quotaHistoryWindows(snapshot quota.Snapshot) map[string]float64 {
	windows := make(map[string]float64, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		if window.ID == "" || window.UsedPercent == nil {
			continue
		}
		value := *window.UsedPercent
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		windows[window.ID] = value
	}
	if len(windows) == 0 {
		return nil
	}
	return windows
}

// recordQuotaHistory appends one accepted observation through the same
// backend-agnostic cache namespace API as the other metadata rows. The read,
// dedupe, bound and write happen inside one atomic mutation, so concurrent
// observations of the same identity cannot interleave or drop each other's
// entries. Errors are counted and logged, never propagated.
func (s *Store) recordQuotaHistory(ctx context.Context, snapshot quota.Snapshot) {
	if snapshot.ObservedAt.IsZero() {
		return
	}
	windows := quotaHistoryWindows(snapshot)
	if len(windows) == 0 {
		return
	}
	entry := quotaHistoryEntry{ObservedAt: snapshot.ObservedAt.UTC(), Source: snapshot.Source, Windows: windows}
	err := s.store.MutateCache(ctx, quotaHistoryNamespace, quotaStoreKey(snapshot.Provider, snapshot.Account), func(raw json.RawMessage) (json.RawMessage, error) {
		var row quotaHistoryRow
		if len(raw) > 0 {
			if errUnmarshal := json.Unmarshal(raw, &row); errUnmarshal != nil {
				// History is derived data: an unreadable row is replaced, never
				// allowed to fail the observation that triggered this write.
				row = quotaHistoryRow{}
			}
		}
		row.Observations = appendQuotaHistory(row.Observations, entry)
		return json.Marshal(row)
	})
	if err != nil {
		s.quotaHistoryFailures.Add(1)
		// Backend errors can contain credentials or raw upstream data.
		log.Warn("usage persistence: failed to record quota history")
	}
}

// appendQuotaHistory inserts the entry in ascending observation order, skips a
// repeat of the same (observed_at, source) pair, and keeps only the newest
// quotaHistoryLimit entries.
func appendQuotaHistory(observations []quotaHistoryEntry, entry quotaHistoryEntry) []quotaHistoryEntry {
	for _, existing := range observations {
		if existing.Source == entry.Source && existing.ObservedAt.Equal(entry.ObservedAt) {
			return observations
		}
	}
	index := sort.Search(len(observations), func(i int) bool { return !observations[i].ObservedAt.Before(entry.ObservedAt) })
	// Rebuild rather than shift in place: the row is bounded by quotaHistoryLimit, and a
	// fresh slice keeps the previous backing array untouched for any reader.
	next := make([]quotaHistoryEntry, 0, len(observations)+1)
	next = append(next, observations[:index]...)
	next = append(next, entry)
	next = append(next, observations[index:]...)
	if len(next) > quotaHistoryLimit {
		next = next[len(next)-quotaHistoryLimit:]
	}
	return next
}

// quotaHistory loads the row for one (provider, account) pair. An account with no
// history is not an error: it yields the zero row.
func (s *Store) quotaHistory(ctx context.Context, provider, account string) (quotaHistoryRow, error) {
	values, err := s.ListCache(ctx, quotaHistoryNamespace)
	if err != nil {
		return quotaHistoryRow{}, err
	}
	raw := values[quotaStoreKey(provider, account)]
	if len(raw) == 0 {
		return quotaHistoryRow{}, nil
	}
	var row quotaHistoryRow
	if err = json.Unmarshal(raw, &row); err != nil {
		return quotaHistoryRow{}, fmt.Errorf("decode quota history: %w", err)
	}
	return row, nil
}
