package quota

import (
	"math"
	"net/http"
	"strings"
	"time"
)

// ParseHeaders consumes usage.Record.ResponseHeaders, including Codex websocket
// events already translated by executor/helps into the same header format.
// Only measured Codex and Claude account quota windows are recognized. Generic
// request/token rate-limit headers and Retry-After are not account balances.
func ParseHeaders(identity Identity, headers http.Header, observedAt time.Time) (Snapshot, bool) {
	snapshot, valid := newSnapshot(identity, SourceHeaders, observedAt)
	if !valid || (snapshot.Provider != "codex" && snapshot.Provider != "claude") {
		return Snapshot{}, false
	}
	values := make(map[string]string)
	for _, key := range sortedKeys(headers) {
		name := strings.ToLower(strings.TrimSpace(key))
		if !strings.HasPrefix(name, "x-codex-") && !strings.HasPrefix(name, "anthropic-ratelimit-unified-") {
			continue
		}
		if len(name) > maxText {
			continue
		}
		headerValues := headers[key]
		if len(headerValues) > 0 {
			if value := safeText(headerValues[len(headerValues)-1]); value != "" {
				values[name] = value
			}
		}
	}
	if snapshot.Provider == "codex" {
		parseCodexHeaders(&snapshot, values)
	} else {
		parseClaudeHeaders(&snapshot, values)
	}
	return finish(snapshot)
}

func parseCodexHeaders(snapshot *Snapshot, values map[string]string) {
	snapshot.Plan = values["x-codex-plan-type"]
	credits := &Credits{
		HasCredits: boolean(values["x-codex-credits-has-credits"]),
		Unlimited:  boolean(values["x-codex-credits-unlimited"]),
		Balance:    number(values["x-codex-credits-balance"]),
	}
	if credits.HasCredits != nil || credits.Unlimited != nil || credits.Balance != nil {
		snapshot.Credits = credits
	}
	activeLimit := codexAlias(values["x-codex-active-limit"])
	// Named additional windows may be echoed into the unnamespaced headers.
	// Never overwrite the main quota with that compatibility projection.
	mainAllowed := values["x-codex-active-limit"] == "" || activeLimit == "premium"
	for _, key := range sortedKeys(values) {
		if !strings.HasPrefix(key, "x-codex-") || !strings.HasSuffix(key, "-limit-name") {
			continue
		}
		group := strings.TrimSuffix(strings.TrimPrefix(key, "x-codex-"), "-limit-name")
		if group == "" {
			continue
		}
		name := values[key]
		nameID := identifier(name)
		if nameID == "" {
			continue
		}
		if activeLimit != "" && (activeLimit == codexAlias(group) || activeLimit == codexAlias(name)) {
			mainAllowed = false
		}
		parseCodexHeaderGroup(snapshot, values, "x-codex-"+group+"-", "additional:"+nameID+":", name)
	}
	if mainAllowed {
		parseCodexHeaderGroup(snapshot, values, "x-codex-", "", "")
	}
	parseCodexHeaderGroup(snapshot, values, "x-codex-code-review-", "code_review:", "Code Review")
}

func codexAlias(value string) string {
	alias := strings.ReplaceAll(identifier(value), "-", "")
	alias = strings.TrimPrefix(alias, "additional")
	return strings.TrimPrefix(alias, "codex")
}

func parseCodexHeaderGroup(snapshot *Snapshot, values map[string]string, prefix, idPrefix, group string) {
	for _, role := range []string{"primary", "secondary"} {
		windowPrefix := prefix + role + "-"
		used := number(values[windowPrefix+"used-percent"])
		if used == nil {
			continue
		}
		window := Window{ID: idPrefix + role, Group: group, UsedPercent: used, Allowed: boolean(values[prefix+"allowed"]), LimitReached: boolean(values[prefix+"limit-reached"])}
		if minutes := integer(values[windowPrefix+"window-minutes"]); minutes != nil && *minutes > 0 && *minutes <= math.MaxInt64/60 {
			window.WindowSeconds = ptr(*minutes * 60)
		}
		window.ResetAt = resetTime(values[windowPrefix+"reset-at"])
		if window.ResetAt == nil {
			window.ResetAt = relativeReset(snapshot.ObservedAt, integer(values[windowPrefix+"reset-after-seconds"]))
		}
		window.Label = windowLabel(role, window.WindowSeconds)
		addWindow(snapshot, window)
	}
}

func parseClaudeHeaders(snapshot *Snapshot, values map[string]string) {
	for _, entry := range []struct {
		key, id, label string
		seconds        int64
	}{{"5h", "five_hour", "5h", 18000}, {"7d", "seven_day", "Weekly", 604800}} {
		prefix := "anthropic-ratelimit-unified-" + entry.key + "-"
		fraction := number(values[prefix+"utilization"])
		if fraction == nil {
			continue
		}
		used := nonnegative(*fraction * 100)
		if used == nil {
			continue
		}
		window := Window{ID: entry.id, Label: entry.label, WindowSeconds: ptr(entry.seconds), UsedPercent: used, ResetAt: resetTime(values[prefix+"reset"])}
		switch values[prefix+"status"] {
		case "allowed", "allowed_warning":
			window.Allowed = ptr(true)
			window.LimitReached = ptr(false)
		case "rejected":
			window.Allowed = ptr(false)
			window.LimitReached = ptr(true)
		}
		addWindow(snapshot, window)
	}
}
