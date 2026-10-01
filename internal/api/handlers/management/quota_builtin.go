package management

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	kimiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// builtinQuotaEndpoint describes one provider's own quota endpoint: how the
// management quota handler fetches it (through the same credential-aware probe
// machinery the api-call endpoint uses) and how the response normalizes into
// the shared quota contract. This is the server-side counterpart of the
// endpoint knowledge the control panel carries, so the management quota
// handler works for builtin providers without a plugin.
type builtinQuotaEndpoint struct {
	method    string
	url       string
	headers   map[string]string
	data      string
	normalize func(body []byte) (pluginapi.QuotaFetchResponse, error)
}

// builtinQuotaEndpoints is a package-level table so tests can point endpoints
// at fixture servers.
//
// Maintenance: each entry records where its request and response shape was
// confirmed. When a provider changes its endpoint, update the single entry
// and its test; the normalizers ignore unknown fields and fail loudly on a
// changed shape, so a stale entry never yields wrong quota. Escape hatches
// that need no code change: a credential-level quota_probe overrides these
// entries per credential, and a plugin quota provider takes precedence.
var builtinQuotaEndpoints = map[string]builtinQuotaEndpoint{
	// Shape confirmed against the control panel bundle (chatgpt.com
	// backend-api/wham/usage, codex CLI user agent) and the usage keeper.
	"codex": {
		method: http.MethodGet,
		url:    "https://chatgpt.com/backend-api/wham/usage",
		headers: map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"User-Agent":    "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal",
		},
		normalize: normalizeCodexUsage,
	},
	// Shape confirmed against the control panel bundle and the Claude Code
	// OAuth usage endpoint (anthropic-beta: oauth-2025-04-20).
	"claude": {
		method: http.MethodGet,
		url:    "https://api.anthropic.com/api/oauth/usage",
		headers: map[string]string{
			"Authorization":  "Bearer $TOKEN$",
			"Content-Type":   "application/json",
			"anthropic-beta": "oauth-2025-04-20",
		},
		normalize: normalizeClaudeUsage,
	},
	// Shape confirmed against the usage keeper's retrieveUserQuota call.
	"gemini-cli": {
		method: http.MethodPost,
		url:    "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota",
		headers: map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
		},
		normalize: normalizeGeminiUsage,
	},
	// Shape confirmed against the control panel bundle and the usage keeper's
	// retrieveUserQuotaSummary call (bucket fractions as 0..1 or "25%").
	"antigravity": {
		method: http.MethodPost,
		url:    "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
		headers: map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"User-Agent":    "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)",
		},
		data:      "{}",
		normalize: normalizeAntigravityUsage,
	},
	// Shape confirmed against the control panel bundle
	// (api.kimi.com / api.kimi.ai /coding/v1/usages, Authorization only).
	"kimi": {
		// url is resolved per credential (domain or custom base_url).
		method:    http.MethodGet,
		headers:   map[string]string{"Authorization": "Bearer $TOKEN$"},
		normalize: normalizeKimiUsages,
	},
	// Shape confirmed against the control panel bundle (billing?format=credits).
	// Client version headers follow xaiClientVersionValue in the xAI executor;
	// bump together when cli-chat-proxy raises its minimum (HTTP 426).
	"xai": {
		method: http.MethodGet,
		url:    "https://cli-chat-proxy.grok.com/v1/billing?format=credits",
		headers: map[string]string{
			"Authorization":         "Bearer $TOKEN$",
			"Accept":                "*/*",
			"User-Agent":            "grok-pager/1.0.44 grok-shell/1.0.44 (macos; aarch64)",
			"X-XAI-Token-Auth":      "xai-grok-cli",
			"x-grok-client-version": "1.0.44",
		},
		normalize: normalizeXAIBilling,
	},
}

// builtinQuotaEndpointFor resolves the credential's provider to a builtin
// quota endpoint. Kimi is resolved per credential because its domain (and
// optional custom base_url) selects the host; the table entry supplies the
// headers and normalizer.
func builtinQuotaEndpointFor(auth *coreauth.Auth) (builtinQuotaEndpoint, bool) {
	if auth == nil {
		return builtinQuotaEndpoint{}, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "kimi", "kimi-ai", "kimi.ai", "kimi.com":
		endpoint := builtinQuotaEndpoints["kimi"]
		endpoint.url = builtinKimiUsagesURL(auth)
		return endpoint, true
	}
	endpoint, ok := builtinQuotaEndpoints[provider]
	return endpoint, ok
}

// builtinKimiUsagesURL resolves the usages endpoint for a Kimi credential,
// honoring a custom base_url (trimmed of any trailing /v1) and otherwise the
// credential's domain.
func builtinKimiUsagesURL(auth *coreauth.Auth) string {
	base := ""
	if auth.Attributes != nil {
		base = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if base == "" {
		base = kimiauth.ResolveKimiAPIBaseURL(kimiauth.ResolveKimiDomainFromAuth(auth))
	}
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + "/v1/usages"
}

// fetchBuiltinQuota fetches one builtin provider endpoint through the shared
// probe request machinery and normalizes the body with the endpoint's parser.
func (h *Handler) fetchBuiltinQuota(c *gin.Context, auth *coreauth.Auth, endpoint builtinQuotaEndpoint) (pluginapi.QuotaFetchResponse, error) {
	probe := map[string]any{
		"url":    endpoint.url,
		"method": endpoint.method,
		"header": headersToAny(endpoint.headers),
	}
	if endpoint.data != "" {
		probe["data"] = endpoint.data
	}
	body, serverOffsetMs, _, errRun := h.runQuotaProbeRequest(c, auth, probe)
	if errRun != nil {
		return pluginapi.QuotaFetchResponse{}, errRun
	}
	quotaResp, errNormalize := endpoint.normalize(body)
	if errNormalize != nil {
		return pluginapi.QuotaFetchResponse{}, errNormalize
	}
	if quotaResp.ServerTimeOffsetMs == 0 {
		quotaResp.ServerTimeOffsetMs = serverOffsetMs
	}
	return quotaResp, nil
}

func headersToAny(headers map[string]string) map[string]any {
	out := make(map[string]any, len(headers))
	for key, value := range headers {
		out[key] = value
	}
	return out
}

// remainingFraction converts a used percentage (0..100) into a remaining
// fraction (0..1), rejecting invalid inputs.
func remainingFraction(usedPercent float64) (float64, bool) {
	if math.IsNaN(usedPercent) || math.IsInf(usedPercent, 0) || usedPercent < 0 {
		return 0, false
	}
	remaining := 1 - usedPercent/100
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

// ratioFraction converts remaining/total amounts into a remaining fraction.
func ratioFraction(remaining, total float64) (float64, bool) {
	if math.IsNaN(remaining) || math.IsNaN(total) || math.IsInf(remaining, 0) || math.IsInf(total, 0) || total <= 0 || remaining < 0 {
		return 0, false
	}
	return remaining / total, true
}

// numericFraction reads a remaining fraction that upstreams encode as a 0..1
// number, a numeric string, or a percent-suffixed string ("25%").
func numericFraction(value gjson.Result) (float64, bool) {
	if !value.Exists() {
		return 0, false
	}
	raw := strings.TrimSpace(value.String())
	percent := strings.HasSuffix(raw, "%")
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	var parsed float64
	switch {
	case value.Type == gjson.Number && !percent:
		parsed = value.Float()
	default:
		if raw == "" {
			return 0, false
		}
		var errParse error
		parsed, errParse = strconv.ParseFloat(raw, 64)
		if errParse != nil {
			return 0, false
		}
	}
	if percent {
		parsed = parsed / 100
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 1 {
		return 0, false
	}
	return parsed, true
}

func quotaResponseValid(quotaResp pluginapi.QuotaFetchResponse) error {
	for _, group := range quotaResp.Groups {
		if len(group.Buckets) > 0 {
			return nil
		}
	}
	if quotaResp.Subscription != nil && strings.TrimSpace(quotaResp.Subscription.Plan) != "" {
		return nil
	}
	if len(quotaResp.Summary) > 0 {
		return nil
	}
	return fmt.Errorf("upstream response does not match the provider's quota shape")
}

// normalizeKimiUsages parses {"usage":{"limit","remaining"},"limits":[{"name",
// "title","detail":{"limit","remaining"},"resetAt"}]} from /coding/v1/usages.
func normalizeKimiUsages(body []byte) (pluginapi.QuotaFetchResponse, error) {
	root := gjson.ParseBytes(body)
	group := pluginapi.QuotaGroup{DisplayName: "Kimi"}
	usage := root.Get("usage")
	if fraction, ok := ratioFraction(usage.Get("remaining").Float(), usage.Get("limit").Float()); ok && usage.Get("remaining").Exists() {
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: "overall", RemainingFraction: fraction, ResetTime: usage.Get("resetAt").String()})
	}
	for _, limit := range root.Get("limits").Array() {
		detail := limit.Get("detail")
		fraction, ok := ratioFraction(detail.Get("remaining").Float(), detail.Get("limit").Float())
		if !ok || !detail.Get("remaining").Exists() {
			continue
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
			Window:            limit.Get("name").String(),
			RemainingFraction: fraction,
			ResetTime:         limit.Get("resetAt").String(),
			Description:       limit.Get("title").String(),
		})
	}
	quotaResp := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{group}}
	return quotaResp, quotaResponseValid(quotaResp)
}

// normalizeClaudeUsage parses the Anthropic OAuth usage windows: each named
// window carries a used "utilization" percentage and a "resets_at" time.
func normalizeClaudeUsage(body []byte) (pluginapi.QuotaFetchResponse, error) {
	root := gjson.ParseBytes(body)
	group := pluginapi.QuotaGroup{DisplayName: "Claude"}
	windows := []struct{ id, label string }{
		{"five_hour", "5h"}, {"seven_day", "Weekly"},
		{"seven_day_oauth_apps", "7d OAuth Apps"}, {"seven_day_opus", "7d Opus"},
		{"seven_day_sonnet", "7d Sonnet"}, {"seven_day_cowork", "7d Cowork"},
		{"iguana_necktie", "Iguana Necktie"},
	}
	for _, entry := range windows {
		bucket := root.Get(entry.id)
		fraction, ok := remainingFraction(bucket.Get("utilization").Float())
		if !ok || !bucket.Get("utilization").Exists() {
			continue
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: entry.label, RemainingFraction: fraction, ResetTime: bucket.Get("resets_at").String()})
	}
	if extra := root.Get("extra_usage"); extra.Exists() {
		if fraction, ok := remainingFraction(extra.Get("utilization").Float()); ok && extra.Get("utilization").Exists() {
			group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: "Extra Usage", RemainingFraction: fraction})
		}
	}
	quotaResp := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{group}}
	return quotaResp, quotaResponseValid(quotaResp)
}

// normalizeCodexUsage parses the wham/usage payload: plan, primary/secondary
// rate-limit windows with used percentages, named additional limits, and the
// credits balance.
func normalizeCodexUsage(body []byte) (pluginapi.QuotaFetchResponse, error) {
	root := gjson.ParseBytes(body)
	quotaResp := pluginapi.QuotaFetchResponse{}
	if plan := strings.TrimSpace(root.Get("plan_type").String()); plan != "" {
		quotaResp.Subscription = &pluginapi.QuotaSubscription{Plan: plan}
	}
	group := pluginapi.QuotaGroup{DisplayName: "Codex"}
	rateLimit := root.Get("rate_limit")
	for _, role := range []string{"primary", "secondary"} {
		window := rateLimit.Get(role + "_window")
		fraction, ok := remainingFraction(window.Get("used_percent").Float())
		if !ok || !window.Get("used_percent").Exists() {
			continue
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: role, RemainingFraction: fraction, ResetTime: window.Get("reset_at").String()})
	}
	for _, additional := range root.Get("additional_rate_limits").Array() {
		name := strings.TrimSpace(additional.Get("limit_name").String())
		if name == "" {
			continue
		}
		for _, role := range []string{"primary", "secondary"} {
			window := additional.Get("rate_limit").Get(role + "_window")
			fraction, ok := remainingFraction(window.Get("used_percent").Float())
			if !ok || !window.Get("used_percent").Exists() {
				continue
			}
			group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: name + " " + role, RemainingFraction: fraction, ResetTime: window.Get("reset_at").String()})
		}
	}
	if len(group.Buckets) > 0 {
		quotaResp.Groups = []pluginapi.QuotaGroup{group}
	}
	if balance := root.Get("credits.balance"); balance.Exists() && balance.Type == gjson.Number {
		quotaResp.Summary = append(quotaResp.Summary, pluginapi.QuotaMetric{Key: "credits_balance", Label: "Credits balance", Value: balance.Float()})
	}
	return quotaResp, quotaResponseValid(quotaResp)
}

// normalizeGeminiUsage parses the Cloud Code retrieveUserQuota buckets, which
// already carry a remainingFraction per model/token type.
func normalizeGeminiUsage(body []byte) (pluginapi.QuotaFetchResponse, error) {
	root := gjson.ParseBytes(body)
	group := pluginapi.QuotaGroup{DisplayName: "Gemini"}
	for _, bucket := range root.Get("buckets").Array() {
		fraction, ok := numericFraction(bucket.Get("remainingFraction"))
		if !ok {
			continue
		}
		name := strings.TrimSpace(strings.Trim(strings.Join([]string{bucket.Get("modelId").String(), bucket.Get("tokenType").String()}, " "), " "))
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: name, RemainingFraction: fraction, ResetTime: bucket.Get("resetTime").String()})
	}
	quotaResp := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{group}}
	return quotaResp, quotaResponseValid(quotaResp)
}

// normalizeAntigravityUsage parses retrieveUserQuotaSummary groups; bucket
// fractions arrive as 0..1 numbers or percent-suffixed strings.
func normalizeAntigravityUsage(body []byte) (pluginapi.QuotaFetchResponse, error) {
	root := gjson.ParseBytes(body)
	if body_ := root.Get("body"); body_.Exists() && body_.IsObject() {
		root = body_
	}
	var groups []pluginapi.QuotaGroup
	for _, rawGroup := range root.Get("groups").Array() {
		group := pluginapi.QuotaGroup{DisplayName: rawGroup.Get("displayName").String()}
		for _, bucket := range rawGroup.Get("buckets").Array() {
			fraction, ok := numericFraction(bucket.Get("remainingFraction"))
			if !ok {
				continue
			}
			group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{
				Window:            bucket.Get("window").String(),
				RemainingFraction: fraction,
				ResetTime:         bucket.Get("resetTime").String(),
				Description:       bucket.Get("displayName").String(),
			})
		}
		if len(group.Buckets) > 0 {
			groups = append(groups, group)
		}
	}
	quotaResp := pluginapi.QuotaFetchResponse{Groups: groups}
	return quotaResp, quotaResponseValid(quotaResp)
}

// normalizeXAIBilling parses the cli-chat-proxy credits billing payload: a
// weekly credit usage percentage plus per-product usage percentages.
func normalizeXAIBilling(body []byte) (pluginapi.QuotaFetchResponse, error) {
	root := gjson.ParseBytes(body)
	if body_ := root.Get("body"); body_.Exists() && body_.IsObject() {
		root = body_
	}
	config := root.Get("config")
	group := pluginapi.QuotaGroup{DisplayName: "xAI"}
	if fraction, ok := remainingFraction(config.Get("creditUsagePercent").Float()); ok && config.Get("creditUsagePercent").Exists() {
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: "weekly", RemainingFraction: fraction, ResetTime: config.Get("currentPeriod.end").String()})
	}
	for _, product := range config.Get("productUsage").Array() {
		fraction, ok := remainingFraction(product.Get("usagePercent").Float())
		if !ok || !product.Get("usagePercent").Exists() {
			continue
		}
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: product.Get("product").String(), RemainingFraction: fraction})
	}
	quotaResp := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{group}}
	return quotaResp, quotaResponseValid(quotaResp)
}
