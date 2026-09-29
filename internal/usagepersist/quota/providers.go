package quota

import (
	"math"
	"strconv"
	"strings"
)

// Provider response fields below come from the existing keeper quota parsers:
// codex/claude OAuth usage, Gemini quota buckets, Antigravity grouped quota,
// Kimi usage detail, and xAI billing. Unknown fields are never copied.
func parseCodexBody(snapshot *Snapshot, object jsonObject) {
	snapshot.Plan = textField(object, "plan_type", "planType")
	parseCodexBodyGroup(snapshot, objectField(object, "rate_limit", "rateLimit"), "", "")
	parseCodexBodyGroup(snapshot, objectField(object, "code_review_rate_limit", "codeReviewRateLimit"), "code_review:", "Code Review")
	for _, raw := range arrayField(object, "additional_rate_limits", "additionalRateLimits") {
		additional := decodeObject(raw)
		name := textField(additional, "limit_name", "limitName")
		if name == "" {
			name = textField(additional, "metered_feature", "meteredFeature")
		}
		if id := identifier(name); id != "" {
			parseCodexBodyGroup(snapshot, objectField(additional, "rate_limit", "rateLimit"), "additional:"+id+":", name)
		}
	}
	creditObject := objectField(object, "credits")
	credits := &Credits{HasCredits: boolField(creditObject, "has_credits", "hasCredits"), Unlimited: boolField(creditObject, "unlimited"), Balance: numberField(creditObject, "balance")}
	if credits.HasCredits != nil || credits.Unlimited != nil || credits.Balance != nil {
		snapshot.Credits = credits
	}
}

func parseCodexBodyGroup(snapshot *Snapshot, groupObject jsonObject, idPrefix, group string) {
	for _, role := range []string{"primary", "secondary"} {
		object := objectField(groupObject, role+"_window", role+"Window")
		used := numberField(object, "used_percent", "usedPercent")
		if used == nil {
			continue
		}
		window := Window{ID: idPrefix + role, Group: group, UsedPercent: used, Allowed: boolField(groupObject, "allowed"), LimitReached: boolField(groupObject, "limit_reached", "limitReached")}
		if seconds := intField(object, "limit_window_seconds", "limitWindowSeconds"); seconds != nil && *seconds > 0 {
			window.WindowSeconds = seconds
		}
		window.ResetAt = resetTime(textField(object, "reset_at", "resetAt"))
		if window.ResetAt == nil {
			window.ResetAt = relativeReset(snapshot.ObservedAt, intField(object, "reset_after_seconds", "resetAfterSeconds"))
		}
		window.Label = windowLabel(role, window.WindowSeconds)
		addWindow(snapshot, window)
	}
}

func parseClaudeBody(snapshot *Snapshot, object jsonObject) {
	// The current management UI maps weekly_scoped Fable limits to the legacy
	// iguana_necktie row, preferring an explicitly active limit.
	var fable jsonObject
	for _, raw := range arrayField(object, "limits") {
		limit := decodeObject(raw)
		model := objectField(objectField(limit, "scope"), "model")
		name := strings.ToLower(textField(model, "display_name"))
		if textField(limit, "kind") != "weekly_scoped" || (name != "fable" && name != "fable 5") || numberField(limit, "percent") == nil {
			continue
		}
		if fable == nil {
			fable = limit
		}
		if active := boolField(limit, "is_active"); active != nil && *active {
			fable = limit
			break
		}
	}
	for _, entry := range []struct{ id, camel, label string }{
		{"five_hour", "fiveHour", "5h"}, {"seven_day", "sevenDay", "Weekly"},
		{"seven_day_oauth_apps", "sevenDayOauthApps", "7d OAuth Apps"},
		{"seven_day_opus", "sevenDayOpus", "7d Opus"}, {"seven_day_sonnet", "sevenDaySonnet", "7d Sonnet"},
		{"seven_day_cowork", "sevenDayCowork", "7d Cowork"}, {"iguana_necktie", "iguanaNecktie", "Iguana Necktie"},
	} {
		bucket := objectField(object, entry.id, entry.camel)
		window := Window{ID: entry.id, Label: entry.label, UsedPercent: numberField(bucket, "utilization"), ResetAt: resetTime(textField(bucket, "resets_at", "resetsAt"))}
		if entry.id == "iguana_necktie" && fable != nil {
			window.Label, window.Model = "7d Fable", "fable"
			window.UsedPercent = numberField(fable, "percent")
			window.ResetAt = resetTime(textField(fable, "resets_at"))
			window.WindowSeconds = ptr(int64(604800))
		}
		if entry.id == "five_hour" {
			window.WindowSeconds = ptr(int64(18000))
		} else if strings.HasPrefix(entry.id, "seven_day") {
			window.WindowSeconds = ptr(int64(604800))
		}
		addWindow(snapshot, window)
	}
	if extra := objectField(object, "extra_usage", "extraUsage"); extra != nil {
		addWindow(snapshot, Window{ID: "extra_usage", Label: "Extra Usage", Unit: "credits", Allowed: boolField(extra, "is_enabled", "isEnabled"), Used: numberField(extra, "used_credits", "usedCredits"), Limit: numberField(extra, "monthly_limit", "monthlyLimit"), UsedPercent: numberField(extra, "utilization")})
	}
}

func parseGeminiBody(snapshot *Snapshot, object jsonObject) {
	for index, raw := range arrayField(object, "buckets") {
		bucket := decodeObject(raw)
		model := textField(bucket, "modelId", "model_id")
		tokenType := textField(bucket, "tokenType", "token_type")
		id := identifier(model) + ":" + identifier(tokenType)
		if id == ":" {
			id = strconv.Itoa(index)
		}
		window := Window{ID: "bucket:" + id, Model: model, Label: model, Unit: tokenType, Remaining: numberField(bucket, "remainingAmount", "remaining_amount"), ResetAt: resetTime(textField(bucket, "resetTime", "reset_time"))}
		if fraction := numberField(bucket, "remainingFraction", "remaining_fraction"); fraction != nil {
			window.RemainingPercent = nonnegative(*fraction * 100)
		}
		addWindow(snapshot, window)
	}
}

func parseAntigravityBody(snapshot *Snapshot, object jsonObject) {
	if objectField(object, "body") != nil {
		object = objectField(object, "body")
	}
	for groupIndex, rawGroup := range arrayField(object, "groups") {
		group := decodeObject(rawGroup)
		groupName := textField(group, "displayName", "display_name")
		groupID := identifier(groupName)
		if groupID == "" {
			groupID = strconv.Itoa(groupIndex)
		}
		for bucketIndex, rawBucket := range arrayField(group, "buckets") {
			bucket := decodeObject(rawBucket)
			windowName := textField(bucket, "window")
			bucketID := identifier(textField(bucket, "bucketId", "bucket_id"))
			if bucketID == "" {
				bucketID = identifier(windowName)
			}
			if bucketID == "" {
				bucketID = strconv.Itoa(bucketIndex)
			}
			window := Window{ID: "group:" + groupID + ":" + bucketID, Group: groupName, Label: textField(bucket, "displayName", "display_name"), ResetAt: resetTime(textField(bucket, "resetTime", "reset_time"))}
			if window.Label == "" {
				window.Label = windowName
			}
			fraction := textField(bucket, "remainingFraction", "remaining_fraction")
			if strings.HasSuffix(fraction, "%") {
				window.RemainingPercent = number(strings.TrimSpace(strings.TrimSuffix(fraction, "%")))
			} else if value := number(fraction); value != nil {
				window.RemainingPercent = nonnegative(*value * 100)
			}
			switch strings.ToLower(windowName) {
			case "5h", "five-hour", "five_hour":
				window.WindowSeconds = ptr(int64(18000))
			case "weekly", "week":
				window.WindowSeconds = ptr(int64(604800))
			}
			addWindow(snapshot, window)
		}
	}
}

func parseKimiBody(snapshot *Snapshot, object jsonObject) {
	if usage := objectField(object, "usage"); usage != nil {
		addKimiWindow(snapshot, "usage", "Usage", usage, nil)
	}
	for index, raw := range arrayField(object, "limits") {
		limit := decodeObject(raw)
		id := identifier(textField(limit, "name"))
		if id == "" {
			id = strconv.Itoa(index)
		}
		label := textField(limit, "title", "name")
		addKimiWindow(snapshot, "limits:"+id, label, limit, objectField(limit, "detail"))
	}
}

func addKimiWindow(snapshot *Snapshot, id, label string, object, detail jsonObject) {
	values := object
	if field(detail, "used", "limit", "remaining") != nil {
		values = detail
	}
	window := Window{ID: id, Label: label, Used: numberField(values, "used"), Limit: numberField(values, "limit"), Remaining: numberField(values, "remaining")}
	window.ResetAt = resetTime(textField(object, "resetAt", "reset_at", "resetTime", "reset_time"))
	if window.ResetAt == nil {
		window.ResetAt = resetTime(textField(detail, "resetAt", "reset_at", "resetTime", "reset_time"))
	}
	if window.ResetAt == nil {
		seconds := intField(object, "resetIn", "reset_in", "ttl")
		if seconds == nil {
			seconds = intField(detail, "resetIn", "reset_in", "ttl")
		}
		window.ResetAt = relativeReset(snapshot.ObservedAt, seconds)
	}
	windowObject := objectField(object, "window")
	if windowObject == nil {
		windowObject = object
	}
	duration := intField(windowObject, "duration")
	var multiplier int64
	switch strings.ToLower(textField(windowObject, "timeUnit", "time_unit")) {
	case "second", "seconds":
		multiplier = 1
	case "minute", "minutes":
		multiplier = 60
	case "hour", "hours":
		multiplier = 3600
	case "day", "days":
		multiplier = 86400
	case "week", "weeks":
		multiplier = 604800
	}
	if duration != nil && *duration > 0 && multiplier > 0 && *duration <= math.MaxInt64/multiplier {
		window.WindowSeconds = ptr(*duration * multiplier)
	}
	addWindow(snapshot, window)
}

func parseXAIBody(snapshot *Snapshot, object jsonObject, credits bool) {
	if objectField(object, "body") != nil {
		object = objectField(object, "body")
	}
	config := objectField(object, "config")
	if config == nil {
		return
	}
	if credits {
		period := objectField(config, "currentPeriod", "current_period")
		window := Window{ID: "billing.weekly", Label: "Weekly", UsedPercent: numberField(config, "creditUsagePercent", "credit_usage_percent"), ResetAt: resetTime(textField(period, "end")), WindowSeconds: ptr(int64(604800))}
		addWindow(snapshot, window)
		for _, raw := range arrayField(config, "productUsage", "product_usage") {
			product := decodeObject(raw)
			name := textField(product, "product")
			if id := identifier(name); id != "" {
				addWindow(snapshot, Window{ID: "billing.product:" + id, Label: name, Model: name, UsedPercent: numberField(product, "usagePercent", "usage_percent")})
			}
		}
		return
	}
	limit := moneyField(config, "monthlyLimit", "monthly_limit")
	used := moneyField(config, "used")
	onDemandUsed := moneyField(config, "onDemandUsed", "on_demand_used")
	if limit != nil && used != nil {
		if onDemandUsed == nil {
			onDemandUsed = nonnegative(math.Max(0, *used-*limit))
		}
		used = nonnegative(math.Min(*used, *limit))
	}
	reset := resetTime(textField(config, "billingPeriodEnd", "billing_period_end"))
	addWindow(snapshot, Window{ID: "billing.monthly", Label: "Monthly Spend", Unit: "usd_cents", Used: used, Limit: limit, ResetAt: reset})
	if cap := moneyField(config, "onDemandCap", "on_demand_cap"); cap != nil && *cap > 0 {
		addWindow(snapshot, Window{ID: "billing.on_demand", Label: "Pay-as-you-go", Unit: "usd_cents", Used: onDemandUsed, Limit: cap, ResetAt: reset})
	}
}

func moneyField(object jsonObject, names ...string) *float64 {
	if nested := objectField(object, names...); nested != nil {
		return numberField(nested, "val")
	}
	return numberField(object, names...)
}
