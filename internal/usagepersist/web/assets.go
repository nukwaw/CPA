// Package web embeds the dependency-free statistics UI and the navigation asset
// that links the management sidebar to it.
package web

import (
	"bytes"
	"embed"
	"regexp"
)

// AssetsPrefix is the canonical URL prefix for the embedded statistics assets.
const AssetsPrefix = "/stats-assets"

//go:embed stats.html stats.css stats-core.js stats.js management-nav.js
var assets embed.FS

// Asset returns a known embedded asset, its MIME type and whether it exists.
// It never resolves arbitrary filesystem paths.
func Asset(name string) ([]byte, string, bool) {
	contentTypes := map[string]string{
		"stats.html":        "text/html; charset=utf-8",
		"stats.css":         "text/css; charset=utf-8",
		"stats-core.js":     "text/javascript; charset=utf-8",
		"stats.js":          "text/javascript; charset=utf-8",
		"management-nav.js": "text/javascript; charset=utf-8",
	}
	contentType, ok := contentTypes[name]
	if !ok {
		return nil, "", false
	}
	data, err := assets.ReadFile(name)
	return data, contentType, err == nil
}

// navMarker marks the navigation asset that links the management sidebar to the
// embedded statistics dashboard.
const navMarker = "data-cpa-stats-nav"

var (
	scriptTag = regexp.MustCompile(`(?i)<script[\s>]`)
	bodyEnd   = regexp.MustCompile(`(?i)</body\s*>`)
)

// InjectManagementNav adds the navigation asset to a management document. The
// asset is deliberately markup-tolerant: it leaves an unrecognized sidebar
// unchanged and the dashboard stays reachable by URL, so injection never needs
// to recognize the upstream build. The source file on disk is never changed.
//
// The script is placed before the first original script so the sidebar entry
// appears as early as possible; a document without scripts receives it before
// </body>. A document with neither anchor is served unchanged.
func InjectManagementNav(data []byte) ([]byte, bool) {
	if bytes.Contains(data, []byte(navMarker)) {
		return data, true
	}
	anchor := scriptTag.FindIndex(data)
	if anchor == nil {
		anchor = bodyEnd.FindIndex(data)
	}
	if anchor == nil {
		return data, false
	}
	asset := `<script ` + navMarker + ` src=".` + AssetsPrefix + `/management-nav.js"></script>`
	var result bytes.Buffer
	result.Grow(len(data) + len(asset))
	result.Write(data[:anchor[0]])
	result.WriteString(asset)
	result.Write(data[anchor[0]:])
	return result.Bytes(), true
}
