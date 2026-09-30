package usagepersist

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// TestQuotaIdentityCoreSettingsAreNonSelectors pins the fail-closed metadata
// contract. Durable identity is (provider, account) and nothing else, so an
// ordinary management setting must never move a credential to a different account.
// Shapes here mirror core overrides, request-scoped error rules, file model
// aliases, and the original management field patcher.
func TestQuotaIdentityCoreSettingsAreNonSelectors(t *testing.T) {
	settings := map[string]any{
		"request_retry":         2,
		"request_scoped_errors": []config.RequestScopedErrorRule{{Status: 429}},
		"disable_cooling":       true,
		"model_aliases":         []any{map[string]any{"name": "upstream", "alias": "public", "force-mapping": true}},
		"websockets":            true,
		"tool_prefix_disabled":  true,
		"excluded_models":       []any{"model-*"},
		"proxy_url":             "http://proxy.invalid:8080",
		"fingerprint_profile":   "chrome",
		"prefix":                "team",
		"priority":              1,
		"weight":                100,
		"note":                  "ordinary management setting",
		"timestamp":             "2025-01-02T03:04:05Z",
	}
	for _, provider := range []string{"codex", "claude", "antigravity", "devin", "kimi", "meta", "xai"} {
		t.Run(provider, func(t *testing.T) {
			auth := quotaFixtureAuth(provider, "index", "same.json", "secret")
			original, ok := ProjectQuotaBinding(auth)
			if !ok {
				t.Fatal("baseline projection failed")
			}
			for key, value := range settings {
				auth.Metadata[key] = value
				binding, valid := ProjectQuotaBinding(auth)
				if !valid || !sameAccountIdentity(binding, original) {
					t.Fatalf("supported setting %s moved the credential to another account", key)
				}
			}
			// Other account-ish metadata is tolerated as a fact in real credentials
			// (codex publishes account_id, antigravity project_id), but it is NOT
			// identity: only the provider's own account property is. A change here
			// therefore must not detach the credential from its saved state.
			for _, key := range []string{"account_id", "organization_id", "project_id", "base_url"} {
				changed := auth.Clone()
				changed.Metadata[key] = "different-value"
				binding, valid := ProjectQuotaBinding(changed)
				if !valid {
					t.Fatalf("metadata %s must be tolerated on a real credential", key)
				}
				if !sameAccountIdentity(binding, original) {
					t.Fatalf("metadata %s must not change durable identity", key)
				}
			}
			// An unaudited key is still refused outright rather than ignored, because
			// an unrecognized field could change which account serves a request.
			for _, key := range []string{"unknown_setting", "account-selector", "request-retry-compat"} {
				changed := auth.Clone()
				changed.Metadata[key] = map[string]any{"account_id": "different-value"}
				if _, valid := ProjectQuotaBinding(changed); valid {
					t.Fatalf("unaudited metadata %s accepted", key)
				}
			}
			// A custom header can replace authorization or select an account behind
			// the projected facts, so it is refused.
			changed := auth.Clone()
			changed.Attributes = map[string]string{"header:Authorization": "Bearer attacker"}
			if _, valid := ProjectQuotaBinding(changed); valid {
				t.Fatal("custom header attribute accepted as a binding")
			}
		})
	}
}

// TestQuotaIdentityCoreSettingsKeepAccountFactsStable replaces the former
// revision-fence test. Revisions no longer exist, so the surviving guarantee is
// that repeated projections through the live manager report the same account facts
// no matter how often ordinary settings are rewritten.
func TestQuotaIdentityCoreSettingsKeepAccountFactsStable(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("claude", "index", "same.json", "secret")
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager })
	original := source.QuotaBindings()[0]
	for _, key := range []string{"request-retry", "request-scoped-errors", "disable-cooling", "model-aliases", "tool-prefix-disabled", "excluded-models", "fingerprint-profile", "proxy-url"} {
		current, _ := manager.GetByID(auth.ID)
		var value any = true
		switch key {
		case "request-retry":
			value = 2
		case "request-scoped-errors":
			value = []any{map[string]any{"status": 429}}
		case "model-aliases":
			value = []any{map[string]any{"name": "upstream", "alias": "public"}}
		case "excluded-models":
			value = []any{"model-*"}
		case "fingerprint-profile":
			value = "chrome"
		case "proxy-url":
			value = "http://proxy.invalid:8080"
		}
		current.Metadata[key] = value
		if _, err := manager.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
		bindings := source.QuotaBindings()
		if len(bindings) != 1 || !sameAccountIdentity(bindings[0], original) {
			t.Fatalf("core normalized setting %s changed the account facts", key)
		}
	}
}

// sameAccountIdentity compares only what is durable identity plus the credential
// slot that addresses the live entry. Tokens, revisions and generations are gone.
func sameAccountIdentity(a, b QuotaBinding) bool {
	return a.Provider == b.Provider && a.Key == b.Key && a.Account == b.Account && a.AccountKind == b.AccountKind
}
