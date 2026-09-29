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

// One row per identity makes normalized observations, UI state, and reset barriers
// atomic across both local goroutines and PostgreSQL replicas.
type quotaState struct {
	CredentialGeneration string                     `json:"credential_generation"`
	Snapshot             *quota.Snapshot            `json:"snapshot,omitempty"`
	ResetAt              time.Time                  `json:"reset_at,omitempty"`
	UIEntries            map[string]QuotaCacheEntry `json:"ui_entries,omitempty"`
}

func quotaKey(provider, authIndex string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + ":" + authIndex
}

func (s *Store) mergeQuota(ctx context.Context, incoming quota.Snapshot) error {
	if incoming.Revision == "" || !s.validQuotaIdentity(incoming.Provider, incoming.AuthIndex, incoming.CredentialGeneration, incoming.Revision, true) {
		return nil
	}
	return s.store.MutateCache(ctx, quotaNamespace, quotaKey(incoming.Provider, incoming.AuthIndex), func(raw json.RawMessage) (json.RawMessage, error) {
		// Recheck after acquiring the atomic row lock, including queued waits.
		if !s.validQuotaIdentity(incoming.Provider, incoming.AuthIndex, incoming.CredentialGeneration, incoming.Revision, true) {
			return raw, nil
		}
		var state quotaState
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
		}
		if state.CredentialGeneration != incoming.CredentialGeneration {
			state = quotaState{CredentialGeneration: incoming.CredentialGeneration}
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
		return json.Marshal(state)
	})
}

// ObserveAPICall normalizes a bounded, known management response before
// nonblocking admission. Neither persistence nor request-context retention occurs
// in the handler chain; Flush provides an explicit persistence-attempt barrier.
func (s *Store) ObserveAPICall(_ context.Context, provider, authIndex, rawURL string, status int, headers http.Header, body []byte, bindings ...QuotaBinding) {
	binding, valid := s.observationBinding(provider, authIndex, bindings)
	if !valid {
		return
	}
	observedAt := time.Now().UTC()
	identity := binding.quotaIdentity()
	if quota.IsAPICallReset(identity, rawURL, status, body) {
		s.observeBoundQuotaResetAt(binding, observedAt, false)
		return
	}
	snapshot, ok := quota.ParseAPICall(identity, rawURL, status, headers, body, observedAt)
	if ok {
		s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &snapshot})
	}
}

// ObserveQuotaFetch shares the bounded worker with usage and reset observations.
// The plugin response is normalized on receipt, never retained as queued work.
func (s *Store) ObserveQuotaFetch(_ context.Context, provider, authIndex string, response pluginapi.QuotaFetchResponse, bindings ...QuotaBinding) {
	binding, valid := s.observationBinding(provider, authIndex, bindings)
	if !valid {
		return
	}
	snapshot, ok := quota.ParseFetch(binding.quotaIdentity(), response, time.Now().UTC())
	if ok {
		s.enqueueManagement(queuedUsage{Kind: queuedQuotaObservation, Quota: &snapshot})
	}
}

// ObserveQuotaReset admits a successful original-handler reset without waiting
// for optional storage. The durable cutoff is its receipt time, not the worker's
// later execution time. Like other observations, queue overflow drops and counts
// this reset; only admitted resets are covered by Flush and the durable barrier.
func (s *Store) ObserveQuotaReset(_ context.Context, provider, authIndex string, bindings ...QuotaBinding) {
	binding, valid := s.observationBinding(provider, authIndex, bindings)
	if valid {
		s.observeBoundQuotaResetAt(binding, time.Now().UTC(), false)
	}
}

func (s *Store) observationBinding(provider, index string, bindings []QuotaBinding) (QuotaBinding, bool) {
	if s == nil || len(bindings) != 1 {
		return QuotaBinding{}, false
	}
	binding := bindings[0]
	valid := binding.Provider == strings.ToLower(strings.TrimSpace(provider)) && binding.AuthIndex == strings.TrimSpace(index) && binding.Revision != "" && s.validQuotaIdentity(binding.Provider, binding.AuthIndex, binding.CredentialGeneration, binding.Revision, false)
	return binding, valid
}
func (b QuotaBinding) quotaIdentity() quota.Identity {
	return quota.Identity{Provider: b.Provider, AuthIndex: b.AuthIndex, CredentialGeneration: b.CredentialGeneration, Revision: b.Revision}
}
func (s *Store) observeBoundQuotaResetAt(binding QuotaBinding, observedAt time.Time, allowAdvance bool) {
	reset := &queuedQuotaReset{Provider: binding.Provider, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, Revision: binding.Revision, Lifetime: binding.Lifetime, RuntimeGeneration: binding.RuntimeGeneration, AllowResetAdvance: allowAdvance, ObservedAt: observedAt}
	if !s.validQuotaReset(reset, false) {
		return
	}
	s.enqueueManagement(queuedUsage{Kind: queuedQuotaResetBarrier, Reset: reset})
}
func (s *Store) validQuotaReset(reset *queuedQuotaReset, disk bool) bool {
	if reset == nil || reset.CredentialGeneration == "" || reset.Revision == "" {
		return false
	}
	current, ok := s.quotaBinding(reset.Provider, reset.AuthIndex, disk)
	return ok && current.CredentialGeneration == reset.CredentialGeneration && (current.Revision == reset.Revision || (reset.AllowResetAdvance && current.Lifetime == reset.Lifetime && current.RuntimeGeneration > 0 && current.RuntimeGeneration-1 == reset.RuntimeGeneration))
}

// ResetQuota is the synchronous direct storage API. Original HTTP handler
// observers must use ObserveQuotaReset instead so optional storage cannot stall
// an otherwise successful reset response.
func (s *Store) ResetQuota(ctx context.Context, provider, authIndex string) error {
	if s == nil {
		return nil
	}
	if err := s.begin(); err != nil {
		return err
	}
	defer s.active.Done()
	return s.resetQuota(ctx, provider, authIndex)
}
func (s *Store) resetQuota(ctx context.Context, provider, authIndex string) error {
	return s.resetQuotaAt(ctx, provider, authIndex, time.Now().UTC())
}

func (s *Store) resetQuotaAt(ctx context.Context, provider, authIndex string, observedAt time.Time) error {
	binding, ok := s.quotaBinding(provider, authIndex, true)
	if !ok {
		return ErrQuotaIdentity
	}
	return s.resetBoundQuotaAt(ctx, &queuedQuotaReset{Provider: binding.Provider, AuthIndex: binding.AuthIndex, CredentialGeneration: binding.CredentialGeneration, Revision: binding.Revision, ObservedAt: observedAt})
}
func (s *Store) resetBoundQuotaAt(ctx context.Context, reset *queuedQuotaReset) error {
	if !s.validQuotaReset(reset, true) {
		return nil
	}
	observedAt := reset.ObservedAt
	return s.store.MutateCache(ctx, quotaNamespace, quotaKey(reset.Provider, reset.AuthIndex), func(raw json.RawMessage) (json.RawMessage, error) {
		if !s.validQuotaReset(reset, true) {
			return raw, nil
		}
		var state quotaState
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
		}
		if state.CredentialGeneration != reset.CredentialGeneration {
			state = quotaState{CredentialGeneration: reset.CredentialGeneration}
		}
		// An older reset may arrive after a newer one via concurrent admission,
		// a synchronous administrator, or another replica. Never lower the
		// durable cutoff or discard observations made after this reset.
		if !observedAt.After(state.ResetAt) {
			return raw, nil
		}
		state.ResetAt = observedAt
		if state.Snapshot != nil {
			if !state.Snapshot.ObservedAt.After(observedAt) {
				state.Snapshot = nil
			} else {
				windows := state.Snapshot.Windows[:0]
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
func (s *Store) Quotas(ctx context.Context) ([]quota.Snapshot, error) {
	values, err := s.ListCache(ctx, quotaNamespace)
	if err != nil {
		return nil, err
	}
	states, err := cacheDecode[quotaState](values)
	if err != nil {
		return nil, err
	}
	bindings := s.quotaIdentitySnapshot(true)
	result := make([]quota.Snapshot, 0, len(states))
	for _, key := range sortedKeys(states) {
		state := states[key]
		if snapshot := state.Snapshot; snapshot != nil && state.CredentialGeneration == snapshot.CredentialGeneration && bindings.valid(snapshot.Provider, snapshot.AuthIndex, snapshot.CredentialGeneration, "") {
			result = append(result, *snapshot)
		}
	}
	return result, nil
}
func (s *Store) quotaHTTP(c *gin.Context) {
	snapshots, err := s.Quotas(c.Request.Context())
	respond(c, gin.H{"snapshots": snapshots, "generated_at": time.Now().UTC()}, err)
}
