// Package web embeds the dependency-free statistics UI and management quota adapter.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"regexp"
	"sort"
)

// AssetsPrefix is the canonical URL prefix for the embedded statistics assets.
const AssetsPrefix = "/stats-assets"

//go:embed stats.html stats.css stats-core.js stats.js management-bridge.js management-nav.js
var assets embed.FS

// Asset returns a known embedded asset, its MIME type and whether it exists.
// It never resolves arbitrary filesystem paths.
func Asset(name string) ([]byte, string, bool) {
	contentTypes := map[string]string{
		"stats.html":           "text/html; charset=utf-8",
		"stats.css":            "text/css; charset=utf-8",
		"stats-core.js":        "text/javascript; charset=utf-8",
		"stats.js":             "text/javascript; charset=utf-8",
		"management-bridge.js": "text/javascript; charset=utf-8",
		"management-nav.js":    "text/javascript; charset=utf-8",
	}
	contentType, ok := contentTypes[name]
	if !ok {
		return nil, "", false
	}
	data, err := assets.ReadFile(name)
	return data, contentType, err == nil
}

var (
	scriptElement = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	moduleType    = regexp.MustCompile(`(?i)\btype\s*=\s*["']module["']`)
	scriptSource  = regexp.MustCompile(`(?i)\bsrc\s*=`)
	// The initial state and adjacent setters come from the actual upstream store,
	// not inferred DOM structure. Unknown/minifier-changed shapes fail closed.
	quotaBinding = regexp.MustCompile(`(?:var\s+|let\s+|const\s+|,)([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*[A-Za-z_$][A-Za-z0-9_$]*\(\s*\(?[A-Za-z_$][A-Za-z0-9_$]*\)?\s*=>\s*\(\s*\{\s*cacheGeneration\s*:\s*0\s*,\s*fileGenerations\s*:\s*\{\s*\}\s*,\s*antigravityQuota\s*:\s*\{\s*\}\s*,\s*claudeQuota\s*:\s*\{\s*\}\s*,\s*codexQuota\s*:\s*\{\s*\}\s*,\s*devinQuota\s*:\s*\{\s*\}\s*,\s*kimiQuota\s*:\s*\{\s*\}\s*,\s*metaQuota\s*:\s*\{\s*\}\s*,\s*xaiQuota\s*:\s*\{\s*\}\s*,\s*setAntigravityQuota\s*:`)
	authBinding  = regexp.MustCompile("(?:var\\s+|let\\s+|const\\s+|,)([A-Za-z_$][A-Za-z0-9_$]*)\\s*=\\s*[A-Za-z_$][A-Za-z0-9_$]*\\(\\)\\(\\s*[A-Za-z_$][A-Za-z0-9_$]*\\(\\s*\\([A-Za-z_$][A-Za-z0-9_$]*\\s*,\\s*[A-Za-z_$][A-Za-z0-9_$]*\\)\\s*=>\\s*\\(\\s*\\{\\s*isAuthenticated\\s*:\\s*(?:!1|false)\\s*,\\s*apiBase\\s*:\\s*(?:``|\"\"|'')\\s*,\\s*managementKey\\s*:\\s*(?:``|\"\"|'')\\s*,\\s*rememberPassword\\s*:\\s*(?:!1|false)")
)

const bridgeMarker = "data-cpa-quota-persistence"

// navMarker marks the separate navigation asset that links the management
// sidebar to the embedded statistics dashboard.
const navMarker = "data-cpa-stats-nav"

// InjectManagementHTML adds a separate bridge asset and an explicit same-module
// store adapter to a recognized upstream management bundle. The source file on
// disk is never changed. On unknown versions, callers should log a compatibility
// warning and serve the unchanged original rather than replace the quota UI.
//
// Verified against upstream source commit 4530da271ba2e89810d4dccebc57f3091afa590a.
func InjectManagementHTML(data []byte) ([]byte, bool) {
	if bytes.Contains(data, []byte(bridgeMarker)) && bytes.Contains(data, []byte(navMarker)) {
		return data, true
	}
	matches := scriptElement.FindAllSubmatchIndex(data, -1)
	type binding struct {
		start, bodyStart, end int
		quota, auth           string
		edits                 []helperEdit
	}
	var found []binding
	for _, match := range matches {
		attrs := data[match[2]:match[3]]
		if !moduleType.Match(attrs) || scriptSource.Match(attrs) {
			continue
		}
		body := data[match[4]:match[5]]
		quota := quotaBinding.FindAllSubmatch(body, -1)
		auth := authBinding.FindAllSubmatch(body, -1)
		if len(quota) != 1 || len(auth) != 1 {
			continue
		}
		// Check the complete quota mutation and auth lifecycle surface as well.
		valid := true
		for _, name := range []string{"setClaudeQuota:", "setCodexQuota:", "setDevinQuota:", "setKimiQuota:", "setMetaQuota:", "setXaiQuota:", "clearQuotaCache:", "restoreSession:", "connectionStatus:", "logout:"} {
			if !bytes.Contains(body, []byte(name)) {
				valid = false
				break
			}
		}
		edits, helpersOK := findHelperEdits(body, string(quota[0][1]))
		sources, sourcesOK := findSourceEdits(body, string(quota[0][1]))
		edits = append(edits, sources...)
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		for i := 1; i < len(edits); i++ {
			if edits[i-1].end > edits[i].start {
				sourcesOK = false
			}
		}
		if valid && helpersOK && sourcesOK {
			found = append(found, binding{match[0], match[4], match[5], string(quota[0][1]), string(auth[0][1]), edits})
		}
	}
	if len(found) != 1 {
		return data, false
	}
	item := found[0]
	asset := `<script ` + bridgeMarker + ` src=".` + AssetsPrefix + `/management-bridge.js"></script>` +
		`<script ` + navMarker + ` src=".` + AssetsPrefix + `/management-nav.js"></script>`
	adapter := fmt.Sprintf("\n;try{window.CPAQuotaPersistence&&window.CPAQuotaPersistence.attach({quotaStore:%s,authStore:%s});}catch{console.warn('CPA quota persistence could not attach; original quota UI remains available.');}\n", item.quota, item.auth)
	var result bytes.Buffer
	result.Grow(len(data) + len(asset) + len(adapter) + 64)
	result.Write(data[:item.start])
	result.WriteString(asset)
	result.Write(data[item.start:item.bodyStart])
	writeWrappedHelpers(&result, data[item.bodyStart:item.end], item.edits)
	result.WriteString(adapter)
	result.Write(data[item.end:])
	return result.Bytes(), true
}
