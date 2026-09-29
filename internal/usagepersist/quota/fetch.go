package quota

import (
	"math"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ParseFetch normalizes successful plugin/declarative quota fetches using the
// existing SDK schema. It does not take request metadata or provider secrets.
func ParseFetch(identity Identity, response pluginapi.QuotaFetchResponse, observedAt time.Time) (Snapshot, bool) {
	snapshot, valid := newSnapshot(identity, SourceFetch, observedAt)
	if !valid {
		return Snapshot{}, false
	}
	if response.Subscription != nil {
		snapshot.Plan = safeText(response.Subscription.Plan)
		snapshot.TierName = safeText(response.Subscription.TierName)
		snapshot.TierID = safeText(response.Subscription.TierID)
	}
	for groupIndex, group := range response.Groups {
		if groupIndex >= maxWindows {
			break
		}
		groupName := safeText(group.DisplayName)
		groupID := identifier(groupName)
		if groupID == "" {
			groupID = strconv.Itoa(groupIndex)
		}
		for index, bucket := range group.Buckets {
			if index >= maxWindows {
				break
			}
			remaining := bucket.RemainingFraction
			if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 1 {
				continue
			}
			label := safeText(bucket.Window)
			bucketID := identifier(label)
			if bucketID == "" {
				bucketID = strconv.Itoa(index)
			}
			addWindow(&snapshot, Window{ID: "fetch:" + groupID + ":" + bucketID, Label: label, Group: groupName, RemainingPercent: ptr(remaining * 100), ResetAt: resetTime(bucket.ResetTime)})
		}
	}
	for index, metric := range response.Summary {
		if index >= maxWindows {
			break
		}
		key := safeText(metric.Key)
		if key == "" || math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) {
			continue
		}
		format := safeText(metric.Format)
		if format != "number" && format != "currency" {
			format = ""
		}
		snapshot.Summary = append(snapshot.Summary, Metric{Key: key, Label: safeText(metric.Label), Value: metric.Value, Unit: safeText(metric.Unit), Format: format, Currency: safeText(metric.Currency)})
	}
	return finish(snapshot)
}
