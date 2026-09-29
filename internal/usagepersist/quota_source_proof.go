package usagepersist

// manualSourceProof contains only domain selector digests. It is an add-on
// display-operation precondition, never credential material or an inference hook.
type manualSourceProof struct {
	Version        int               `json:"v"`
	SelectorHashes map[string]string `json:"selector_hashes"`
}

type publicQuotaBinding struct {
	QuotaBinding
	ManualSourceProof *manualSourceProof `json:"manual_source_proof,omitempty"`
}

func quotaBindingPublic(binding QuotaBinding) publicQuotaBinding {
	public := publicQuotaBinding{QuotaBinding: binding}
	if binding.APITokenSHA256 == "" {
		return public
	}
	allowed := map[string]bool{}
	switch binding.Provider {
	case "codex":
		allowed["account_id"] = true
	case "antigravity":
		allowed["project_id"] = true
	case "xai":
		allowed["sub"], allowed["user_id"] = true, true
	default:
		return public
	}
	selectors := make(map[string]string, len(binding.SelectorHashes))
	for key, digest := range binding.SelectorHashes {
		if !allowed[key] || !validTokenHash(digest) {
			return public
		}
		selectors[key] = digest
	}
	public.ManualSourceProof = &manualSourceProof{Version: 1, SelectorHashes: selectors}
	return public
}
