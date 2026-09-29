package api

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUsageQuotaIdentityCoreDiskSettingsAndAliases(t *testing.T) {
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
				directory := t.TempDir()
				path := filepath.Join(directory, "same.json")
				document := map[string]any{"type": "claude", "access_token": "secret", spelling: setting.value}
				data, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
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
				cfg := &config.Config{AuthDir: directory}
				source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager }, func() *config.Config { return cfg })
				memory := source.QuotaBindings(false)
				disk := source.QuotaBindings(true)
				if len(memory) != 1 || len(disk) != 1 || memory[0].CredentialGeneration != disk[0].CredentialGeneration || memory[0].Revision != disk[0].Revision {
					t.Fatal("supported core disk setting revoked runtime identity")
				}
				if binding, ok := source.(usagepersist.TargetedQuotaIdentitySource).QuotaBinding("claude", before.Index, true); !ok || binding.Revision != disk[0].Revision {
					t.Fatal("targeted disk check disagreed with normalized snapshot")
				}
				after, _ := manager.GetByID(auth.ID)
				unchanged, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(data, unchanged) || !reflect.DeepEqual(before, after) {
					t.Fatal("add-on normalization mutated the core credential or file")
				}
				// A genuine account selector still revokes a stale runtime binding,
				// even when ordinary settings and legacy aliases are present.
				document["organization_id"] = "different-account"
				data, _ = json.Marshal(document)
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if len(source.QuotaBindings(true)) != 0 || len(source.QuotaBindings(false)) != 1 {
					t.Fatal("selector change was ignored or memory-only path read disk")
				}
			})
		}
	}
}

func TestUsageQuotaIdentityCredentialAliasPrecedenceAndSelectors(t *testing.T) {
	for _, changed := range []string{"api-key", "base-url", "canonical-api-key", "canonical-base-url", "unknown", "custom-headers"} {
		t.Run(changed, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "same.json")
			document := map[string]any{"type": "codex", "api-key": "secret", "base-url": "https://quota.invalid/A", "excluded-models": []any{"model-*"}}
			if changed == "canonical-api-key" || changed == "canonical-base-url" {
				document["api_key"] = "secret"
				document["api-key"] = "ignored-secret"
				document["base_url"] = "https://quota.invalid/A"
				document["base-url"] = "https://quota.invalid/ignored"
			}
			write := func() {
				t.Helper()
				data, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write()
			data, _ := json.Marshal(document)
			var metadata map[string]any
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{ID: "same.json", FileName: "same.json", Provider: "codex", Metadata: metadata, Attributes: map[string]string{coreauth.AttributePath: path, coreauth.AttributeSourceBackend: coreauth.AuthSourceFile}}
			if _, err := manager.Register(ctx, auth); err != nil {
				t.Fatal(err)
			}
			source := newUsageQuotaIdentitySource(func() *coreauth.Manager { return manager }, nil)
			if len(source.QuotaBindings(true)) != 1 {
				t.Fatal("genuine credential/selector aliases or canonical precedence disagreed with core")
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
			}
			write()
			if len(source.QuotaBindings(true)) != 0 {
				t.Fatal("changed selector/credential or unknown metadata accepted")
			}
			if _, ok := source.(usagepersist.TargetedQuotaIdentitySource).QuotaBinding("codex", auth.Index, true); ok {
				t.Fatal("targeted locked check accepted changed disk identity")
			}
		})
	}
}
