package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Unrelated credential settings and legacy aliases must not disturb the account
// facts the live projection publishes: identity is (provider, account) only.
func TestUsageQuotaIdentityCoreSettingsAndAliasesDoNotChangeIdentity(t *testing.T) {
	settings := []struct {
		canonical, alias string
		value            any
	}{
		{"request_retry", "request-retry", 2},
		{"request_scoped_errors", "request-scoped-errors", []any{map[string]any{"status": 429, "match": []any{"busy"}, "action": "continue"}}},
		{"disable_cooling", "disable-cooling", true},
		{"model_aliases", "model-aliases", []any{map[string]any{"name": "upstream", "alias": "public", "force-mapping": true}}},
		{"excluded_models", "excluded-models", []any{"private-*"}},
		{"fingerprint_profile", "fingerprint-profile", "chrome"},
		{"proxy_url", "proxy-url", "http://proxy.invalid:8080"},
		{"tool_prefix_disabled", "tool-prefix-disabled", true},
		{"websockets", "", true},
	}
	for _, setting := range settings {
		for _, spelling := range []string{setting.canonical, setting.alias} {
			if spelling == "" {
				continue
			}
			t.Run(spelling, func(t *testing.T) {
				ctx := context.Background()
				document := map[string]any{"type": "claude", "access_token": "secret", "email": "same@example.invalid", spelling: setting.value}
				data, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				var runtimeMetadata map[string]any
				if err = json.Unmarshal(data, &runtimeMetadata); err != nil {
					t.Fatal(err)
				}
				auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "claude", Metadata: runtimeMetadata, Attributes: map[string]string{coreauth.AttributePath: "same.json", coreauth.AttributeSourceBackend: coreauth.AuthSourceFile}}
				manager := coreauth.NewManager(nil, nil, nil)
				if _, err = manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
				before, _ := manager.GetByID(auth.ID)
				snapshot := *before
				source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager })
				bindings := source.QuotaBindings()
				if len(bindings) != 1 {
					t.Fatalf("supported core setting revoked runtime identity: %+v", bindings)
				}
				if bindings[0].Provider != "claude" || bindings[0].Account != "same@example.invalid" || bindings[0].AccountKind != "email" {
					t.Fatalf("projected facts = %+v", bindings[0])
				}
				if strings.Contains(bindings[0].Key, "secret") || len(bindings[0].SelectorHashes) != 0 {
					t.Fatal("projection leaked a credential or invented selectors")
				}
				// Projection reads the live credential; it must never mutate it.
				after, _ := manager.GetByID(auth.ID)
				if !reflect.DeepEqual(snapshot.Metadata, after.Metadata) || !reflect.DeepEqual(snapshot.Attributes, after.Attributes) {
					t.Fatal("add-on normalization mutated the core credential")
				}
				// The account property is the only field that re-keys identity.
				before.Metadata["email"] = "different@example.invalid"
				if _, err = manager.Update(ctx, before); err != nil {
					t.Fatal(err)
				}
				changed := source.QuotaBindings()
				if len(changed) != 1 || changed[0].Account != "different@example.invalid" {
					t.Fatalf("account change was not projected: %+v", changed)
				}
			})
		}
	}
}

// Credential secrets, base URLs and operation selectors are not identity, so
// changing them must not re-key an account, while unaudited metadata and custom
// headers fail closed: they could select a different account than the projection
// describes, so the credential is refused rather than silently trusted.
func TestUsageQuotaIdentityIgnoresCredentialFieldsAndFollowsAccount(t *testing.T) {
	for _, changed := range []string{"api-key", "base-url", "canonical-api-key", "canonical-base-url", "account-selector", "unknown", "custom-headers"} {
		t.Run(changed, func(t *testing.T) {
			refused := changed == "unknown" || changed == "custom-headers"
			ctx := context.Background()
			document := map[string]any{"type": "codex", "api-key": "secret", "base-url": "https://quota.invalid/A", "email": "same@example.invalid", "excluded-models": []any{"model-*"}}
			if changed == "canonical-api-key" || changed == "canonical-base-url" {
				document["api_key"] = "secret"
				document["api-key"] = "ignored-secret"
				document["base_url"] = "https://quota.invalid/A"
				document["base-url"] = "https://quota.invalid/ignored"
			}
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "codex", Metadata: metadata}
			if _, err := manager.Register(ctx, auth); err != nil {
				t.Fatal(err)
			}
			source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager })
			bindings := source.QuotaBindings()
			if len(bindings) != 1 || bindings[0].Account != "same@example.invalid" || bindings[0].AccountKind != "email" {
				t.Fatalf("genuine credential aliases changed identity: %+v", bindings)
			}
			switch changed {
			case "api-key":
				document["api-key"] = "new-secret"
			case "base-url":
				document["base-url"] = "https://quota.invalid/B"
			case "canonical-api-key":
				document["api_key"] = "new-secret"
			case "canonical-base-url":
				document["base_url"] = "https://quota.invalid/B"
			case "unknown":
				document["unknown_account_selector"] = "B"
			case "custom-headers":
				document["headers"] = map[string]any{"Chatgpt-Account-Id": "B"}
			case "account-selector":
				// An operation selector is not the account property.
				document["account_id"] = "B"
			}
			if err := json.Unmarshal(mustJSON(t, document), &metadata); err != nil {
				t.Fatal(err)
			}
			auth.Metadata = metadata
			if _, err := manager.Update(ctx, auth); err != nil {
				t.Fatal(err)
			}
			after := source.QuotaBindings()
			if refused {
				if len(after) != 0 {
					t.Fatalf("unaudited metadata was trusted as a credential: %+v", after)
				}
				return
			}
			if len(after) != 1 || after[0].Account != "same@example.invalid" || after[0].AccountKind != "email" {
				t.Fatalf("non-account field re-keyed or broke the credential: %+v", after)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
