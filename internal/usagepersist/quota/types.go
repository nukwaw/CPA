// Package quota extracts only known quota fields from upstream responses.
// It never retains response bodies, request data, or arbitrary HTTP headers.
package quota

import "time"

const (
	SourceHeaders = "response_headers"
	SourceAPICall = "management_api"
	SourceFetch   = "management_fetch"
	maxWindows    = 128
	maxText       = 256
)

// Identity identifies a credential without retaining its tokens or auth metadata.
type Identity struct {
	Provider             string `json:"provider"`
	AuthIndex            string `json:"auth_index"`
	CredentialGeneration string `json:"credential_generation"`
	Revision             string `json:"-"`
}

// Snapshot is safe to persist. Missing numeric values stay absent, not zero.
// Windows carry their own observation times because headers can update only a
// subset of the windows returned by a management quota refresh.
type Snapshot struct {
	Provider             string    `json:"provider"`
	AuthIndex            string    `json:"auth_index"`
	CredentialGeneration string    `json:"credential_generation"`
	Revision             string    `json:"-"`
	Source               string    `json:"source"`
	ObservedAt           time.Time `json:"observed_at"`
	Plan                 string    `json:"plan,omitempty"`
	TierName             string    `json:"tier_name,omitempty"`
	TierID               string    `json:"tier_id,omitempty"`
	Windows              []Window  `json:"windows"`
	Credits              *Credits  `json:"credits,omitempty"`
	Summary              []Metric  `json:"summary,omitempty"`
}

// Window represents a measured quota bucket. Percentages are 0..100 units;
// provider overage above 100 is retained rather than presented as fresh quota.
type Window struct {
	ID               string     `json:"id"`
	Label            string     `json:"label,omitempty"`
	Group            string     `json:"group,omitempty"`
	Model            string     `json:"model,omitempty"`
	Unit             string     `json:"unit,omitempty"`
	UsedPercent      *float64   `json:"used_percent,omitempty"`
	RemainingPercent *float64   `json:"remaining_percent,omitempty"`
	Used             *float64   `json:"used,omitempty"`
	Limit            *float64   `json:"limit,omitempty"`
	Remaining        *float64   `json:"remaining,omitempty"`
	WindowSeconds    *int64     `json:"window_seconds,omitempty"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
	Allowed          *bool      `json:"allowed,omitempty"`
	LimitReached     *bool      `json:"limit_reached,omitempty"`
	ObservedAt       time.Time  `json:"observed_at"`
	Source           string     `json:"source"`
}

// Metric is a bounded numeric summary from the existing plugin quota contract.
type Metric struct {
	Key      string  `json:"key"`
	Label    string  `json:"label,omitempty"`
	Value    float64 `json:"value"`
	Unit     string  `json:"unit,omitempty"`
	Format   string  `json:"format,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

// Credits contains only Codex's documented-in-runtime quota credit scalars.
type Credits struct {
	HasCredits *bool    `json:"has_credits,omitempty"`
	Unlimited  *bool    `json:"unlimited,omitempty"`
	Balance    *float64 `json:"balance,omitempty"`
}

// Merge preserves unobserved windows and ignores out-of-order observations for
// the same window. Callers must serialize read/merge/write for each identity.
// Reset operations should delete the snapshot rather than merge an empty one.
func Merge(previous, incoming Snapshot) Snapshot {
	if previous.Provider != incoming.Provider || previous.AuthIndex != incoming.AuthIndex || previous.CredentialGeneration != incoming.CredentialGeneration || previous.ObservedAt.IsZero() {
		return cloneSnapshot(incoming)
	}
	result := cloneSnapshot(previous)
	if !incoming.ObservedAt.Before(previous.ObservedAt) {
		result.ObservedAt, result.Source = incoming.ObservedAt, incoming.Source
		if incoming.Plan != "" {
			result.Plan = incoming.Plan
		}
		if incoming.TierName != "" {
			result.TierName = incoming.TierName
		}
		if incoming.TierID != "" {
			result.TierID = incoming.TierID
		}
		if incoming.Summary != nil {
			result.Summary = append([]Metric(nil), incoming.Summary...)
		}
		if incoming.Credits != nil {
			result.Credits = cloneCredits(incoming.Credits)
		}
	}
	positions := make(map[string]int, len(result.Windows))
	for i := range result.Windows {
		positions[result.Windows[i].ID] = i
	}
	for _, window := range incoming.Windows {
		if i, exists := positions[window.ID]; exists {
			if !window.ObservedAt.Before(result.Windows[i].ObservedAt) {
				result.Windows[i] = mergeWindow(result.Windows[i], window)
			}
		} else if len(result.Windows) < maxWindows {
			positions[window.ID] = len(result.Windows)
			result.Windows = append(result.Windows, cloneWindow(window))
		}
	}
	return result
}

// mergeWindow carries forward stable display metadata omitted from a measured
// header update. An omitted reset is retained only while it is still in the
// future. ObservedAt dates the new measurement, not the retained metadata.
// Transient flags and old usage amounts are never carried into a new sample.
func mergeWindow(previous, incoming Window) Window {
	result := cloneWindow(incoming)
	if result.WindowSeconds == nil {
		result.WindowSeconds = clonePtr(previous.WindowSeconds)
	}
	if result.Label == "" || result.Label == "primary" || result.Label == "secondary" {
		if previous.Label != "" {
			result.Label = previous.Label
		}
	}
	if result.Group == "" {
		result.Group = previous.Group
	}
	if result.Model == "" {
		result.Model = previous.Model
	}
	if result.Unit == "" {
		result.Unit = previous.Unit
	}
	if result.ResetAt == nil && previous.ResetAt != nil && previous.ResetAt.After(incoming.ObservedAt) {
		result.ResetAt = clonePtr(previous.ResetAt)
	}
	return result
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	windows := make([]Window, len(snapshot.Windows))
	for i, window := range snapshot.Windows {
		windows[i] = cloneWindow(window)
	}
	snapshot.Windows = windows
	snapshot.Summary = append([]Metric(nil), snapshot.Summary...)
	snapshot.Credits = cloneCredits(snapshot.Credits)
	return snapshot
}

func cloneWindow(window Window) Window {
	window.UsedPercent = clonePtr(window.UsedPercent)
	window.RemainingPercent = clonePtr(window.RemainingPercent)
	window.Used = clonePtr(window.Used)
	window.Limit = clonePtr(window.Limit)
	window.Remaining = clonePtr(window.Remaining)
	window.WindowSeconds = clonePtr(window.WindowSeconds)
	window.ResetAt = clonePtr(window.ResetAt)
	window.Allowed = clonePtr(window.Allowed)
	window.LimitReached = clonePtr(window.LimitReached)
	return window
}

func cloneCredits(credits *Credits) *Credits {
	if credits == nil {
		return nil
	}
	return &Credits{HasCredits: clonePtr(credits.HasCredits), Unlimited: clonePtr(credits.Unlimited), Balance: clonePtr(credits.Balance)}
}

func clonePtr[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func ptr[T any](value T) *T { return &value }
