package usagepersist

import (
	"context"
	"encoding/json"
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

// ObserveQuotaFetch normalizes a successful management quota fetch before
// nonblocking admission. The caller (the management handler's observer)
// supplies the account facts of the live credential it already holds, so no
// credential lookup happens here. Flush provides an explicit
// persistence-attempt barrier.
func (s *Store) ObserveQuotaFetch(_ context.Context, provider, account, accountKind string, response pluginapi.QuotaFetchResponse) {
	snapshot, ok := quota.ParseFetch(quota.Identity{Provider: provider, Account: account, AccountKind: accountKind}, response, time.Now().UTC())
	if ok {
		s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &snapshot})
	}
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
