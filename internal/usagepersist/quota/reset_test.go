package quota

import "testing"

func TestIsAPICallResetRequiresKnownSuccessfulConsume(t *testing.T) {
	identity := parserIdentity("codex", "a")
	endpoint := "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
	for _, body := range []string{`{"code":"reset","windows_reset":2}`, `{"code":"reset","windowsReset":0}`} {
		if !IsAPICallReset(identity, endpoint, 200, []byte(body)) {
			t.Fatalf("valid consume response not recognized: %s", body)
		}
	}
	for _, body := range []string{`{}`, `not-json`, `{"code":"reset"}`, `{"code":"failed","windows_reset":2}`, `{"code":"reset","windows_reset":null}`, `{"code":"reset","windows_reset":-1}`} {
		if IsAPICallReset(identity, endpoint, 200, []byte(body)) {
			t.Fatalf("invalid consume response accepted: %s", body)
		}
	}
	body := []byte(`{"code":"reset","windows_reset":2}`)
	for _, invalidURL := range []string{"http://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", "https://evil.example/backend-api/wham/rate-limit-reset-credits/consume", "https://chatgpt.com/backend-api/wham/usage", "https://user:secret@chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"} {
		if IsAPICallReset(identity, invalidURL, 200, body) {
			t.Fatalf("invalid endpoint accepted: %s", invalidURL)
		}
	}
	if IsAPICallReset(parserIdentity("claude", "a"), endpoint, 200, body) || IsAPICallReset(identity, endpoint, 400, body) || IsAPICallReset(parserIdentity("codex", ""), endpoint, 200, body) {
		t.Fatal("reset accepted for unrelated provider, error status, or missing identity")
	}
}
