package web

import (
	"bytes"
	"regexp"
	"strings"
)

// This is the actual Vite helper/store sequence at the verified source commit.
// Only lexical identifiers may vary. Checking the whole sequence also verifies
// every setter uses resolveUpdater, the original clear's filename/global guards,
// and capture/commit reference this exact store. New generated shapes fail closed.
const quotaHelpersTemplate = `var @updater=(@value,@previous)=>typeof @value==` + "`function`" + `?@value(@previous):@value,@quota=@create(@set=>({cacheGeneration:0,fileGenerations:{},antigravityQuota:{},claudeQuota:{},codexQuota:{},devinQuota:{},kimiQuota:{},metaQuota:{},xaiQuota:{},setAntigravityQuota:@input=>@set(@state=>({antigravityQuota:@updater(@input,@state.antigravityQuota)})),setClaudeQuota:@input=>@set(@state=>({claudeQuota:@updater(@input,@state.claudeQuota)})),setCodexQuota:@input=>@set(@state=>({codexQuota:@updater(@input,@state.codexQuota)})),setDevinQuota:@input=>@set(@state=>({devinQuota:@updater(@input,@state.devinQuota)})),setKimiQuota:@input=>@set(@state=>({kimiQuota:@updater(@input,@state.kimiQuota)})),setMetaQuota:@input=>@set(@state=>({metaQuota:@updater(@input,@state.metaQuota)})),setXaiQuota:@input=>@set(@state=>({xaiQuota:@updater(@input,@state.xaiQuota)})),clearQuotaCache:@input=>@set(@state=>{if(@input){if(@input.length===0)return @state;let @files={...@state.fileGenerations};@input.forEach(@item=>{@files[@item]=(@files[@item]??0)+1});let @names=new Set(@input),@omit=@cache=>{let @keys=Object.keys(@cache).filter(@key=>@names.has(@filename(@key)));if(@keys.length===0)return @cache;let @copy={...@cache};return @keys.forEach(@key=>delete @copy[@key]),@copy};return{fileGenerations:@files,antigravityQuota:@omit(@state.antigravityQuota),claudeQuota:@omit(@state.claudeQuota),codexQuota:@omit(@state.codexQuota),devinQuota:@omit(@state.devinQuota),kimiQuota:@omit(@state.kimiQuota),metaQuota:@omit(@state.metaQuota),xaiQuota:@omit(@state.xaiQuota)}}return{cacheGeneration:@state.cacheGeneration+1,fileGenerations:{},antigravityQuota:{},claudeQuota:{},codexQuota:{},devinQuota:{},kimiQuota:{},metaQuota:{},xaiQuota:{}}})})),@capture=@name=>{let{cacheGeneration:@global,fileGenerations:@perfile}=@quota.getState();return{cacheGeneration:@global,fileGenerations:@perfile,name:@name}},@commit=(@generation,@callback,@scope=@generation.name)=>{let @current=@quota.getState();if(@current.cacheGeneration!==@generation.cacheGeneration)return!1;if(@scope!==void 0){if((@current.fileGenerations[@scope]??0)!==(@generation.fileGenerations[@scope]??0))return!1}else if(@current.fileGenerations!==@generation.fileGenerations)return!1;return @callback(),!0}`

var helperSlot = regexp.MustCompile(`@[A-Za-z][A-Za-z0-9]*`)

func lexicalPattern(template string) (*regexp.Regexp, []string) {
	var pattern strings.Builder
	var slots []string
	last := 0
	for _, loc := range helperSlot.FindAllStringIndex(template, -1) {
		pattern.WriteString(regexp.QuoteMeta(template[last:loc[0]]))
		pattern.WriteString(`([A-Za-z_$][A-Za-z0-9_$]*)`)
		slots = append(slots, template[loc[0]:loc[1]])
		last = loc[1]
	}
	pattern.WriteString(regexp.QuoteMeta(template[last:]))
	return regexp.MustCompile(pattern.String()), slots
}

var verifiedHelpers, helperSlots = lexicalPattern(quotaHelpersTemplate)

type helperEdit struct {
	start, end int
	wrapper    string
	projector  string
}

func findHelperEdits(body []byte, quota string) ([]helperEdit, bool) {
	matches := verifiedHelpers.FindAllSubmatchIndex(body, -1)
	if len(matches) != 1 {
		return nil, false
	}
	match := matches[0]
	bindings := make(map[string]string)
	for i, slot := range helperSlots {
		value := string(body[match[2*i+2]:match[2*i+3]])
		if previous, exists := bindings[slot]; exists && previous != value {
			return nil, false
		}
		bindings[slot] = value
	}
	if bindings["@quota"] != quota {
		return nil, false
	}
	for _, slot := range []string{"@updater", "@capture", "@commit"} {
		name := regexp.QuoteMeta(bindings[slot])
		initializer := regexp.MustCompile(`(?:var\s+|let\s+|const\s+|,)` + name + `\s*=`)
		if len(initializer.FindAllIndex(body, -1)) != 1 {
			return nil, false
		}
	}
	// The native clear uses this verified filename helper (Devin's NUL suffix).
	// Verify the helper without depending on its minified parameter/separator names.
	filePattern := regexp.MustCompile(`function ` + regexp.QuoteMeta(bindings["@filename"]) + `\(([A-Za-z_$][A-Za-z0-9_$]*)\)\{let ([A-Za-z_$][A-Za-z0-9_$]*)=([A-Za-z_$][A-Za-z0-9_$]*)\.indexOf\(([A-Za-z_$][A-Za-z0-9_$]*)\);return ([A-Za-z_$][A-Za-z0-9_$]*)===-1\?([A-Za-z_$][A-Za-z0-9_$]*):([A-Za-z_$][A-Za-z0-9_$]*)\.slice\(0,([A-Za-z_$][A-Za-z0-9_$]*)\)\}`)
	files := filePattern.FindAllSubmatch(body, -1)
	if len(files) != 1 {
		return nil, false
	}
	f := files[0]
	if !bytes.Equal(f[1], f[3]) || !bytes.Equal(f[1], f[6]) || !bytes.Equal(f[1], f[7]) || !bytes.Equal(f[2], f[5]) || !bytes.Equal(f[2], f[8]) {
		return nil, false
	}
	separator := regexp.MustCompile(`(?:var |let |const |,)` + regexp.QuoteMeta(string(f[4])) + "=(`\\\\0`|\"\\\\0\"|'\\\\0')" + `(?:,|;)`)
	if len(separator.FindAllIndex(body, -1)) != 1 {
		return nil, false
	}
	start := match[0]
	text := string(body[start:match[1]])
	updater := strings.Index(text, bindings["@updater"]+"=") + len(bindings["@updater"]) + 1
	quotaStart := strings.Index(text, ","+quota+"=")
	capture := strings.Index(text, ","+bindings["@capture"]+"=") + len(bindings["@capture"]) + 2
	commitStart := strings.Index(text, ","+bindings["@commit"]+"=")
	commit := commitStart + len(bindings["@commit"]) + 2
	return []helperEdit{{start + updater, start + quotaStart, "wrapUpdater", ""}, {start + capture, start + commitStart, "wrapCapture", ""}, {start + commit, match[1], "wrapCommit", ""}}, true
}

func writeWrappedHelpers(result *bytes.Buffer, body []byte, edits []helperEdit) {
	last := 0
	for _, edit := range edits {
		result.Write(body[last:edit.start])
		result.WriteString("(window.CPAQuotaPersistence?." + edit.wrapper + "||(f=>f))(")
		result.Write(body[edit.start:edit.end])
		if edit.projector != "" {
			result.WriteByte(',')
			result.WriteString(edit.projector)
		}
		result.WriteByte(')')
		last = edit.end
	}
	result.Write(body[last:])
}
