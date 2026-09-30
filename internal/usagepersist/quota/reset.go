package quota

import (
	"net/url"
	"strings"
)

// IsAPICallReset recognizes the existing Codex reset-credit consume response.
// It performs no request and retains no response content. A successful status
// alone is insufficient: keeper's protocol requires code=reset and windows_reset.
func IsAPICallReset(identity Identity, rawURL string, statusCode int, body []byte) bool {
	provider, _, _, valid := identityFacts(identity)
	if !valid || provider != "codex" || statusCode < 200 || statusCode >= 300 || len(body) > maxBodyBytes {
		return false
	}
	endpoint, errURL := url.Parse(rawURL)
	if errURL != nil || endpoint.Scheme != "https" || endpoint.User != nil || strings.ToLower(endpoint.Hostname()) != "chatgpt.com" || (endpoint.Port() != "" && endpoint.Port() != "443") || endpoint.Path != "/backend-api/wham/rate-limit-reset-credits/consume" {
		return false
	}
	object := decodeObject(body)
	return textField(object, "code") == "reset" && intField(object, "windows_reset", "windowsReset") != nil
}
