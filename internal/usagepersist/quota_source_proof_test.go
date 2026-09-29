package usagepersist

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManualSourceProofOnlyExposesBoundSelectorDigests(t *testing.T) {
	binding := QuotaBinding{Provider: "codex", Key: "account.json", AuthIndex: "index", CredentialGeneration: "generation", Revision: "revision", APITokenSHA256: quotaHash("private-token"), AccessTokenSHA256: quotaHash("private-token"), SelectorHashes: map[string]string{"account_id": quotaHash("private-account-id")}}
	public := quotaBindingPublic(binding)
	if public.ManualSourceProof == nil || public.ManualSourceProof.Version != 1 || public.ManualSourceProof.SelectorHashes["account_id"] != quotaHash("private-account-id") {
		t.Fatal("known source selector proof is missing")
	}
	data, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-token", "private-account-id", binding.APITokenSHA256, "AccessTokenSHA256", "APITokenSHA256", "RuntimeGeneration", "Lifetime"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("identity response leaked private proof material: %s", secret)
		}
	}
	public.ManualSourceProof.SelectorHashes["account_id"] = "changed"
	if binding.SelectorHashes["account_id"] != quotaHash("private-account-id") {
		t.Fatal("public proof aliases the internal selector map")
	}
}

func TestManualSourceProofRefusesUnverifiedResolverOrSelector(t *testing.T) {
	for _, binding := range []QuotaBinding{
		{Provider: "codex", SelectorHashes: map[string]string{"account_id": quotaHash("a")}},
		{Provider: "codex", APITokenSHA256: quotaHash("token"), SelectorHashes: map[string]string{"project_id": quotaHash("p")}},
		{Provider: "antigravity", APITokenSHA256: quotaHash("token"), SelectorHashes: map[string]string{"project_id": "not-a-digest"}},
		{Provider: "meta", APITokenSHA256: quotaHash("token"), SelectorHashes: map[string]string{}},
	} {
		if quotaBindingPublic(binding).ManualSourceProof != nil {
			t.Fatalf("unproved source advertised eligible selectors: %s", binding.Provider)
		}
	}
	for provider, selector := range map[string]string{"codex": "account_id", "antigravity": "project_id", "xai": "sub"} {
		binding := QuotaBinding{Provider: provider, APITokenSHA256: quotaHash("token"), SelectorHashes: map[string]string{selector: quotaHash("value")}}
		if quotaBindingPublic(binding).ManualSourceProof == nil {
			t.Fatalf("known source missing proof: %s", provider)
		}
	}
}
