package usagepersist

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestManualSourceProofPublishesSelectorsForProviderWithOperationSelectors pins
// the published-since-removal rule: a provider that has operation selectors
// publishes a proof even when the credential exposes no selector value. The
// manual fetch then fails closed against an empty digest set rather than being
// silently trusted, so publishing it is harmless.
func TestManualSourceProofPublishesSelectorsForProviderWithOperationSelectors(t *testing.T) {
	// A codex credential that publishes its account selector gives the browser a
	// proof of exactly that digest.
	bound := QuotaBinding{Provider: "codex", Key: "account.json", Account: "same@example.invalid", AccountKind: "email",
		AuthIndex: "index", SelectorHashes: map[string]string{"account_id": quotaHash("private-account-id")}}
	public := quotaBindingPublic(bound)
	if public.ManualSourceProof == nil || public.ManualSourceProof.Version != 1 || public.ManualSourceProof.SelectorHashes["account_id"] != quotaHash("private-account-id") {
		t.Fatal("known source selector proof is missing")
	}
	// The proof is published whenever the provider has operation selectors, even
	// when the credential exposes no selector value to verify.
	for _, provider := range []string{"codex", "antigravity", "xai"} {
		binding := QuotaBinding{Provider: provider, Key: "file.json", AuthIndex: "index", SelectorHashes: map[string]string{}}
		if quotaBindingPublic(binding).ManualSourceProof == nil {
			t.Fatalf("provider with operation selectors published no proof: %s", provider)
		}
	}
	// A provider without operation selectors has nothing to prove.
	for _, provider := range []string{"claude", "kimi", "meta", "devin"} {
		binding := QuotaBinding{Provider: provider, Key: "file.json", AuthIndex: "index", SelectorHashes: map[string]string{}}
		if quotaBindingPublic(binding).ManualSourceProof != nil {
			t.Fatalf("provider without operation selectors advertised a proof: %s", provider)
		}
	}
}

// TestManualSourceProofOnlyExposesBoundSelectorDigests keeps the secret-free
// assertion: the published document carries the account fact deliberately, and
// never a credential, an auth file, a removed fence, or a raw selector value.
func TestManualSourceProofOnlyExposesBoundSelectorDigests(t *testing.T) {
	binding := QuotaBinding{Provider: "codex", Key: "account.json", Account: "same@example.invalid", AccountKind: "email",
		AuthIndex: "index", SelectorHashes: map[string]string{"account_id": quotaHash("private-account-id")}}
	public := quotaBindingPublic(binding)
	data, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	// The account fact is deliberately published so the dashboard can group by it.
	if !strings.Contains(string(data), `"account":"same@example.invalid"`) {
		t.Fatalf("published binding lost its account fact: %s", data)
	}
	for _, secret := range []string{"private-token", "private-account-id", "AccessTokenSHA256", "APITokenSHA256", "credential_generation", "revision", "lifetime"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("identity response leaked private proof material: %s", secret)
		}
	}
	// The published proof is a copy: editing it must not alias the internal map.
	public.ManualSourceProof.SelectorHashes["account_id"] = "changed"
	if binding.SelectorHashes["account_id"] != quotaHash("private-account-id") {
		t.Fatal("public proof aliases the internal selector map")
	}
}

// TestManualSourceProofRefusesUnverifiableSelectorDigests is the fail-closed
// direction: a proof that does not describe the credential must never be
// published as trustworthy.
func TestManualSourceProofRefusesUnverifiableSelectorDigests(t *testing.T) {
	for _, binding := range []QuotaBinding{
		// A selector this provider does not define.
		{Provider: "codex", AuthIndex: "index", SelectorHashes: map[string]string{"project_id": quotaHash("p")}},
		// A selector value that is not a digest at all.
		{Provider: "antigravity", AuthIndex: "index", SelectorHashes: map[string]string{"project_id": "not-a-digest"}},
		{Provider: "antigravity", AuthIndex: "index", SelectorHashes: map[string]string{"project_id": strings.Repeat("A", 64)}},
		// Unbounded selector set.
		{Provider: "codex", AuthIndex: "index", SelectorHashes: map[string]string{"account_id": quotaHash("a"), "account_2": quotaHash("b"), "account_3": quotaHash("c"), "account_4": quotaHash("d"), "account_5": quotaHash("e")}},
	} {
		if quotaBindingPublic(binding).ManualSourceProof != nil {
			t.Fatalf("unproved source advertised eligible selectors: %s", binding.Provider)
		}
	}
	for provider, selector := range map[string]string{"codex": "account_id", "antigravity": "project_id", "xai": "sub"} {
		binding := QuotaBinding{Provider: provider, AuthIndex: "index", SelectorHashes: map[string]string{selector: quotaHash("value")}}
		if quotaBindingPublic(binding).ManualSourceProof == nil {
			t.Fatalf("known source missing proof: %s", provider)
		}
	}
}
