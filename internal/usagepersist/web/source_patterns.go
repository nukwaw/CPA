package web

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
)

type sourcePattern struct {
	pattern  *regexp.Regexp
	anchored *regexp.Regexp
	slots    []string
}

var verifiedSources = func() map[string]sourcePattern {
	patterns := make(map[string]sourcePattern)
	for name, template := range sourceTemplates {
		pattern, slots := lexicalPattern(template)
		patterns[name] = sourcePattern{pattern, regexp.MustCompile("^" + pattern.String()), slots}
	}
	return patterns
}()

// Source selectors are checked at their actual lexical callsites, before config
// and registry capture. All templates must agree on shared lexical references.
// Missing, duplicate, partial, or changed coverage serves the original document.
func matchSources(body []byte, quota string) (map[string]string, map[string][]int, bool) {
	bindings := map[string]string{"@quota": quota}
	found := make(map[string][]int)
	pending := make(map[string]bool)
	for name := range verifiedSources {
		pending[name] = true
	}
	for len(pending) > 0 {
		progress := false
		for name := range pending {
			verified := verifiedSources[name]
			var candidates [][]int
			if name == "registry" {
				candidates = verified.pattern.FindAllSubmatchIndex(body, -1)
			} else if symbol := bindings["@"+name]; symbol != "" {
				// Follow actual lexical references out from the verified registry.
				// Avoid rescanning a multi-megabyte SPA with every large regexp.
				marker := symbol + "="
				if strings.HasPrefix(sourceTemplates[name], "function ") {
					marker = "function " + symbol + "("
				}
				for offset := 0; offset < len(body); {
					index := bytes.Index(body[offset:], []byte(marker))
					if index < 0 {
						break
					}
					start := offset + index
					offset = start + len(marker)
					if start > 0 && strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_$", rune(body[start-1])) {
						continue
					}
					if match := verified.anchored.FindSubmatchIndex(body[start:]); match != nil {
						for i := range match {
							match[i] += start
						}
						candidates = append(candidates, match)
					}
				}
			} else {
				continue
			}
			var compatible [][]int
			for _, match := range candidates {
				local := make(map[string]string)
				valid := true
				for i, slot := range verifiedSources[name].slots {
					value := string(body[match[2*i+2]:match[2*i+3]])
					if previous, exists := bindings[slot]; exists && previous != value {
						valid = false
					}
					if previous, exists := local[slot]; exists && previous != value {
						valid = false
					}
					local[slot] = value
				}
				if valid {
					compatible = append(compatible, match)
				}
			}
			if len(compatible) == 0 {
				return nil, nil, false
			}
			if len(compatible) != 1 {
				continue
			}
			match := compatible[0]
			for i, slot := range verifiedSources[name].slots {
				bindings[slot] = string(body[match[2*i+2]:match[2*i+3]])
			}
			found[name] = match
			delete(pending, name)
			progress = true
		}
		if !progress {
			return nil, nil, false
		}
	}
	return bindings, found, true
}

var sourceDeclarations = regexp.MustCompile(`(?:var\s+|let\s+|const\s+|,)([A-Za-z_$][A-Za-z0-9_$]*)\s*=`)
var sourceFunctions = regexp.MustCompile(`function\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
var sourceAssignments = regexp.MustCompile(`([A-Za-z_$][A-Za-z0-9_$]*)\s*=`)

func findSourceEdits(body []byte, quota string) ([]helperEdit, bool) {
	bindings, locations, ok := matchSources(body, quota)
	if !ok {
		return nil, false
	}
	declarations, functions, assignments := make(map[string]int), make(map[string]int), make(map[string]int)
	for _, match := range sourceAssignments.FindAllSubmatchIndex(body, -1) {
		if match[1] < len(body) && (body[match[1]] == '=' || body[match[1]] == '>') {
			continue
		}
		assignments[string(body[match[2]:match[3]])]++
	}
	for _, match := range sourceDeclarations.FindAllSubmatch(body, -1) {
		declarations[string(match[1])]++
	}
	for _, match := range sourceFunctions.FindAllSubmatch(body, -1) {
		functions[string(match[1])]++
	}
	for name, template := range sourceTemplates {
		counts, expectedAssignments := declarations, 1
		if strings.HasPrefix(template, "function ") {
			counts, expectedAssignments = functions, 0
		}
		if counts[bindings["@"+name]] != 1 || assignments[bindings["@"+name]] != expectedAssignments {
			return nil, false
		}
	}
	for _, pair := range [][2]string{{"fetch", "codexconfig"}, {"header", "fetch"}, {"project", "agconfig"}, {"xaiheader", "xaiconfig"}, {"devinfetch", "devinconfig"}, {"agconfig", "registry"}, {"claudeconfig", "registry"}, {"codexconfig", "registry"}, {"devinconfig", "registry"}, {"kimiconfig", "registry"}, {"metaconfig", "registry"}, {"xaiconfig", "registry"}} {
		if locations[pair[0]][1] > locations[pair[1]][0] {
			return nil, false
		}
	}
	file := "__cpaManualFile"
	for {
		collision := false
		for _, value := range bindings {
			if value == file {
				collision = true
			}
		}
		if !collision {
			break
		}
		file += "_"
	}
	projector := func(provider, suffix string, camelFirst bool) string {
		index := file + ".auth_index??" + file + ".authIndex"
		if camelFirst {
			index = file + ".authIndex??" + file + ".auth_index"
		}
		return file + "=>({provider:\"" + provider + "\",name:" + file + ".name,index:" + bindings["@normalize"] + "(" + index + ")" + suffix + "})"
	}
	var edits []helperEdit
	initializer := func(name, projection string) {
		loc := locations[name]
		start := loc[0] + len(bindings["@"+name]) + 1
		edits = append(edits, helperEdit{start, loc[1], "wrapQuotaSource", projection})
	}
	initializer("fetch", projector("codex", ",selectors:{account_id:"+bindings["@account"]+"("+file+")}", false))
	initializer("project", projector("antigravity", ",mode:\"async-result\"", false))
	initializer("xaiheader", projector("xai", ",mode:\"result\"", false))
	initializer("devinfetch", projector("devin", "", true))
	for _, provider := range []string{"claude", "kimi", "xai"} {
		loc := locations[provider+"config"]
		text := body[loc[0]:loc[1]]
		start, end := bytes.Index(text, []byte("fetchQuota:")), bytes.Index(text, []byte(",storeSelector:"))
		if start < 0 || end <= start {
			return nil, false
		}
		edits = append(edits, helperEdit{loc[0] + start + len("fetchQuota:"), loc[0] + end, "wrapQuotaSource", projector(provider, "", false)})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	return edits, true
}
