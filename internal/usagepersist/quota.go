package usagepersist

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const quotaNamespace = "quota_snapshots"

// One row per account makes normalized observations, UI state and reset cutoffs
// atomic across both local goroutines and PostgreSQL replicas.
type quotaState struct {
	Snapshot  *quota.Snapshot            `json:"snapshot,omitempty"`
	ResetAt   time.Time                  `json:"reset_at,omitempty"`
	UIEntries map[string]QuotaCacheEntry `json:"ui_entries,omitempty"`
}

// quotaStoreKey is the durable key for one account's quota state. Identity is
// (provider, account); a credential that exposes no account property is grouped by
// provider alone rather than guessed at.
func quotaStoreKey(provider, account string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if account = strings.TrimSpace(account); account == "" {
		return provider
	}
	return provider + ":" + account
}

// mergeQuota stores one observation under its account. Stale-write protection is
// observation-time ordering only: there is no generation or revision to fence on,
// so a replayed or out-of-order sample never overwrites newer state.
func (s *Store) mergeQuota(ctx context.Context, incoming quota.Snapshot) error {
	if strings.TrimSpace(incoming.Provider) == "" {
		return nil
	}
	accepted := false
	err := s.store.MutateCache(ctx, quotaNamespace, quotaStoreKey(incoming.Provider, incoming.Account), func(raw json.RawMessage) (json.RawMessage, error) {
		var state quotaState
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
		}
		if !state.ResetAt.IsZero() && !incoming.ObservedAt.After(state.ResetAt) {
			return raw, nil
		}
		var previous quota.Snapshot
		if state.Snapshot != nil {
			previous = *state.Snapshot
		}
		merged := quota.Merge(previous, incoming)
		state.Snapshot = &merged
		accepted = true
		return json.Marshal(state)
	})
	if err != nil {
		return err
	}
	if accepted {
		// Bounded observation history for the dashboard. It is written after the
		// durable merge, in its own atomic row mutation, and can never fail or
		// delay the observation beyond that extra metadata write.
		s.recordQuotaHistory(ctx, incoming)
	}
	return nil
}

// ObserveAPICall normalizes a bounded, known management response before
// nonblocking admission. Neither persistence nor request-context retention occurs
// in the handler chain; Flush provides an explicit persistence-attempt barrier.
func (s *Store) ObserveAPICall(_ context.Context, binding QuotaBinding, rawURL string, status int, headers http.Header, body []byte) {
	observedAt := time.Now().UTC()
	if quota.IsAPICallReset(binding.quotaIdentity(), rawURL, status, body) {
		s.observeQuotaResetAt(binding, observedAt)
		return
	}
	snapshot, ok := quota.ParseAPICall(binding.quotaIdentity(), rawURL, status, headers, body, observedAt)
	if ok {
		s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &snapshot})
	}
}

// ObserveQuotaFetch shares the bounded worker with usage and reset observations.
// The plugin response is normalized on receipt, never retained as queued work.
func (s *Store) ObserveQuotaFetch(_ context.Context, binding QuotaBinding, response pluginapi.QuotaFetchResponse) {
	snapshot, ok := quota.ParseFetch(binding.quotaIdentity(), response, time.Now().UTC())
	if ok {
		s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &snapshot})
	}
}

// ObserveQuotaReset admits a successful original-handler reset without waiting for
// optional storage. A reset only advances the durable cutoff for its account; it
// carries no credential fence.
func (s *Store) ObserveQuotaReset(_ context.Context, binding QuotaBinding) {
	s.observeQuotaResetAt(binding, time.Now().UTC())
}

func (b QuotaBinding) quotaIdentity() quota.Identity {
	return quota.Identity{Provider: b.Provider, Account: b.Account, AccountKind: b.AccountKind}
}

func (s *Store) observeQuotaResetAt(binding QuotaBinding, observedAt time.Time) {
	if s == nil || strings.TrimSpace(binding.Provider) == "" {
		return
	}
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaResetBarrier, Reset: &queuedQuotaReset{Provider: binding.Provider, Account: binding.Account, ObservedAt: observedAt}})
}

// resetBoundQuotaAt discards stored quota state older than the reset. It is keyed
// by account, so a reset can never lower the cutoff of another account.
func (s *Store) resetBoundQuotaAt(ctx context.Context, reset *queuedQuotaReset) error {
	if reset == nil || strings.TrimSpace(reset.Provider) == "" {
		return nil
	}
	observedAt := reset.ObservedAt
	return s.store.MutateCache(ctx, quotaNamespace, quotaStoreKey(reset.Provider, reset.Account), func(raw json.RawMessage) (json.RawMessage, error) {
		var state quotaState
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
		}
		// An older reset may arrive after a newer one via concurrent admission, a
		// synchronous administrator, or another replica. Never lower the durable
		// cutoff or discard observations made after this reset.
		if !observedAt.After(state.ResetAt) {
			return raw, nil
		}
		state.ResetAt = observedAt
		if state.Snapshot != nil {
			if !state.Snapshot.ObservedAt.After(observedAt) {
				state.Snapshot = nil
			} else {
				windows := make([]quota.Window, 0, len(state.Snapshot.Windows))
				for _, window := range state.Snapshot.Windows {
					if window.ObservedAt.After(observedAt) {
						windows = append(windows, window)
					}
				}
				state.Snapshot.Windows = windows
			}
		}
		for key, entry := range state.UIEntries {
			if !entry.ObservedAt.After(observedAt) {
				delete(state.UIEntries, key)
			}
		}
		return json.Marshal(state)
	})
}

// ResetQuota is the synchronous direct storage API. Original HTTP handler
// observers must use ObserveQuotaReset instead so optional storage cannot stall
// an otherwise successful reset response.
func (s *Store) ResetQuota(ctx context.Context, provider, account string) error {
	if s == nil {
		return nil
	}
	if err := s.begin(); err != nil {
		return err
	}
	defer s.active.Done()
	return s.resetBoundQuotaAt(ctx, &queuedQuotaReset{Provider: provider, Account: account, ObservedAt: time.Now().UTC()})
}

func (s *Store) Quotas(ctx context.Context) ([]quota.Snapshot, error) {
	values, err := s.ListCache(ctx, quotaNamespace)
	if err != nil {
		return nil, err
	}
	states, err := cacheDecode[quotaState](values)
	if err != nil {
		return nil, err
	}
	result := make([]quota.Snapshot, 0, len(states))
	for _, key := range sortedKeys(states) {
		if snapshot := states[key].Snapshot; snapshot != nil {
			result = append(result, *snapshot)
		}
	}
	return result, nil
}

func (s *Store) quotaHTTP(c *gin.Context) {
	snapshots, err := s.Quotas(c.Request.Context())
	respond(c, gin.H{"snapshots": snapshots, "generated_at": time.Now().UTC()}, err)
}
