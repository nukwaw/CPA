package usagepersist

// manualSourceProof contains only operation selector digests. It is an add-on
// display-operation precondition, never credential material, never identity and
// never an inference hook.
type manualSourceProof struct {
	Version        int               `json:"v"`
	SelectorHashes map[string]string `json:"selector_hashes"`
}

type publicQuotaBinding struct {
	QuotaBinding
	ManualSourceProof *manualSourceProof `json:"manual_source_proof,omitempty"`
}

// quotaBindingPublic publishes the binding plus, when the provider has operation
// selectors, the digests a manual quota fetch must match. Publishing a proof for a
// credential that cannot serve the call is harmless: the call itself fails.
func quotaBindingPublic(binding QuotaBinding) publicQuotaBinding {
	public := publicQuotaBinding{QuotaBinding: binding}
	if len(quotaOperationSelectors[binding.Provider]) == 0 {
		return public
	}
	selectors := make(map[string]string, len(binding.SelectorHashes))
	for key, digest := range binding.SelectorHashes {
		if !operationSelectorKnown(binding.Provider, key) || !validTokenHash(digest) {
			return public
		}
		selectors[key] = digest
	}
	public.ManualSourceProof = &manualSourceProof{Version: 1, SelectorHashes: selectors}
	return public
}
