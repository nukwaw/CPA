package usagepersist

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaIdentityCoreSettingsAreNonSelectors(t *testing.T) {
	// Shapes mirror core overrides, request-scoped error rules, file model aliases,
	// and the original management field patcher, not telemetry compatibility keys.
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
				if !valid || binding.CredentialGeneration != original.CredentialGeneration {
					t.Fatalf("supported setting %s revoked/changed identity", key)
				}
			}
			for _, key := range []string{"account_id", "organization_id", "project_id", "base_url"} {
				changed := auth.Clone()
				changed.Metadata[key] = "different-selector"
				binding, valid := ProjectQuotaBinding(changed)
				if !valid || binding.CredentialGeneration == original.CredentialGeneration {
					t.Fatalf("selector %s disappeared into settings allowlist", key)
				}
			}
			for _, key := range []string{"unknown_setting", "account-selector", "request-retry-compat", "headers"} {
				changed := auth.Clone()
				changed.Metadata[key] = map[string]any{"account_id": "different-selector"}
				if _, valid := ProjectQuotaBinding(changed); valid {
					t.Fatalf("unaudited metadata %s accepted", key)
				}
			}
		})
	}
}

func TestQuotaIdentityCoreSettingsPreserveRevisionFences(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := quotaFixtureAuth("claude", "index", "same.json", "secret")
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	source := NewQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
	original := source.QuotaBindings(false)[0]
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
		bindings := source.QuotaBindings(true)
		if len(bindings) != 1 || bindings[0].CredentialGeneration != original.CredentialGeneration || bindings[0].Revision == original.Revision {
			t.Fatalf("core normalized setting %s changed generation or reused revision", key)
		}
		original = bindings[0]
	}
}
