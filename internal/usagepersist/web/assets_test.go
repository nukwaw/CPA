package web

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A reduced, syntax-valid fixture of the initializers in the actual Vite build
// of upstream commit 4530da271ba2e89810d4dccebc57f3091afa590a. Store names are not
// part of the contract: the second test exercises different minified bindings.
const nativeManagementFixture = `<!doctype html><html><head><script type="module" crossorigin>
var ym="\0";function Sm(e){let t=e.indexOf(ym);return t===-1?e:e.slice(0,t)}var Cm=(e,t)=>typeof e==` + "`function`" + `?e(t):e,wm=Sc(e=>({cacheGeneration:0,fileGenerations:{},antigravityQuota:{},claudeQuota:{},codexQuota:{},devinQuota:{},kimiQuota:{},metaQuota:{},xaiQuota:{},setAntigravityQuota:t=>e(e=>({antigravityQuota:Cm(t,e.antigravityQuota)})),setClaudeQuota:t=>e(e=>({claudeQuota:Cm(t,e.claudeQuota)})),setCodexQuota:t=>e(e=>({codexQuota:Cm(t,e.codexQuota)})),setDevinQuota:t=>e(e=>({devinQuota:Cm(t,e.devinQuota)})),setKimiQuota:t=>e(e=>({kimiQuota:Cm(t,e.kimiQuota)})),setMetaQuota:t=>e(e=>({metaQuota:Cm(t,e.metaQuota)})),setXaiQuota:t=>e(e=>({xaiQuota:Cm(t,e.xaiQuota)})),clearQuotaCache:t=>e(e=>{if(t){if(t.length===0)return e;let n={...e.fileGenerations};t.forEach(e=>{n[e]=(n[e]??0)+1});let r=new Set(t),i=e=>{let t=Object.keys(e).filter(e=>r.has(Sm(e)));if(t.length===0)return e;let n={...e};return t.forEach(e=>delete n[e]),n};return{fileGenerations:n,antigravityQuota:i(e.antigravityQuota),claudeQuota:i(e.claudeQuota),codexQuota:i(e.codexQuota),devinQuota:i(e.devinQuota),kimiQuota:i(e.kimiQuota),metaQuota:i(e.metaQuota),xaiQuota:i(e.xaiQuota)}}return{cacheGeneration:e.cacheGeneration+1,fileGenerations:{},antigravityQuota:{},claudeQuota:{},codexQuota:{},devinQuota:{},kimiQuota:{},metaQuota:{},xaiQuota:{}}})})),Tm=e=>{let{cacheGeneration:t,fileGenerations:n}=wm.getState();return{cacheGeneration:t,fileGenerations:n,name:e}},Em=(e,t,n=e.name)=>{let r=wm.getState();if(r.cacheGeneration!==e.cacheGeneration)return!1;if(n!==void 0){if((r.fileGenerations[n]??0)!==(e.fileGenerations[n]??0))return!1}else if(r.fileGenerations!==e.fileGenerations)return!1;return t(),!0},Dm=null,Om=Sc()(Jc((e,t)=>({isAuthenticated:!1,apiBase:"",managementKey:"",rememberPassword:!1,connectionStatus:"disconnected",restoreSession:()=>{},logout:()=>{}})));
</script></head><body><div id="root"></div></body></html>`

func TestAssetAllowlist(t *testing.T) {
	for _, name := range []string{"stats.html", "stats.css", "stats-core.js", "stats.js", "management-bridge.js", "management-nav.js"} {
		data, contentType, ok := Asset(name)
		if !ok || len(data) == 0 || !strings.Contains(contentType, "charset=utf-8") {
			t.Fatalf("missing asset %s", name)
		}
	}
	for _, name := range []string{"../stats.html", "/stats.html", "assets.go", "missing.js", "test\\stats.html"} {
		if _, _, ok := Asset(name); ok {
			t.Errorf("unexpected asset path allowed: %q", name)
		}
	}
}

func TestStatisticsDocumentUsesCanonicalAssetPrefix(t *testing.T) {
	data, _, ok := Asset("stats.html")
	if !ok || AssetsPrefix != "/stats-assets" {
		t.Fatal("canonical assets prefix is missing")
	}
	for _, name := range []string{"stats.css", "stats-core.js", "stats.js"} {
		if !bytes.Contains(data, []byte(`".`+AssetsPrefix+`/`+name+`"`)) {
			t.Errorf("statistics document does not reference canonical asset %s", name)
		}
	}
}

func TestInjectRecognizedManagementModule(t *testing.T) {
	for _, fixture := range []string{managementFixture, strings.NewReplacer("wm", "renamedQuota", "Om", "renamedAuth", "Cm", "renamedUpdater", "Tm", "renamedCapture", "Em", "renamedCommit", "Sm", "renamedFilename", "ym", "renamedSeparator").Replace(managementFixture)} {
		result, ok := InjectManagementHTML([]byte(fixture))
		if !ok {
			t.Fatal("known module was not recognized")
		}
		if !bytes.Contains(result, []byte(`<script data-cpa-quota-persistence src=".`+AssetsPrefix+`/management-bridge.js"></script>`)) {
			t.Fatal("bridge does not use the canonical asset path and marker")
		}
		if !bytes.Contains(result, []byte(`<script data-cpa-stats-nav src=".`+AssetsPrefix+`/management-nav.js"></script>`)) {
			t.Fatal("navigation asset does not use the canonical asset path and marker")
		}
		if bytes.Index(result, []byte("management-bridge.js")) > bytes.Index(result, []byte(`type="module"`)) {
			t.Fatal("bridge must load before the inline module")
		}
		if bytes.Index(result, []byte("management-nav.js")) > bytes.Index(result, []byte(`type="module"`)) {
			t.Fatal("navigation asset must load before the inline module")
		}
		adapter := bytes.Index(result, []byte("CPAQuotaPersistence.attach"))
		lastScriptEnd := bytes.LastIndex(result, []byte("</script>"))
		if adapter < 0 || adapter > lastScriptEnd {
			t.Fatal("adapter must execute in the store module's lexical scope")
		}
		if !bytes.Contains(result, []byte(`<body><div id="root"></div></body>`)) {
			t.Fatal("original management body was changed")
		}
		for _, helper := range []string{"wrapCapture", "wrapCommit", "wrapUpdater"} {
			if bytes.Count(result, []byte("CPAQuotaPersistence?."+helper)) != 1 {
				t.Fatalf("expected one original helper wrapper: %s", helper)
			}
		}
		verifyCompiledHelpers(t, []byte(fixture), result)
		verifyCompiledSources(t, []byte(fixture))
		second, secondOK := InjectManagementHTML(result)
		if !secondOK || !bytes.Equal(result, second) {
			t.Fatal("injection is not idempotent")
		}
	}
}

func TestInjectFailsClosedForUnknownOrAmbiguousBundle(t *testing.T) {
	fixtures := []string{
		`<html><script type="module">console.log('unknown')</script></html>`,
		strings.Replace(managementFixture, "metaQuota:{}", "futureQuota:{}", 1),
		strings.Replace(managementFixture, `type="module"`, `type="module" src="external.js"`, 1),
		strings.Replace(managementFixture, "clearQuotaCache:", "newClearQuotaCache:", 1),
		strings.Replace(managementFixture, "n=e.name", "n=undefined", 1),
		strings.Replace(managementFixture, "r.fileGenerations[n]??0", "r.fileGenerations[n]??1", 1),
		strings.Replace(managementFixture, "r.cacheGeneration!==e.cacheGeneration", "false", 1),
		strings.Replace(managementFixture, "return t(),!0", "return!0", 1),
		strings.Replace(managementFixture, "name:e}},Em=", "name:e,extra:true}},Em=", 1),
		strings.Replace(managementFixture, "fileGenerations:n,name:e", "fileGenerations:{},name:e", 1),
		strings.Replace(managementFixture, "codexQuota:Cm(t,e.codexQuota)", "codexQuota:t", 1),
		strings.Replace(managementFixture, "n[e]=(n[e]??0)+1", "n[e]=0", 1),
		strings.Replace(managementFixture, "r.has(Sm(e))", "r.has(e)", 1),
		strings.Replace(managementFixture, `ym="\0"`, `ym="-"`, 1),
		strings.Replace(managementFixture, "return t===-1?e:e.slice(0,t)", "return e", 1),
		managementFixture + managementFixture,
	}
	for _, fixture := range fixtures {
		result, ok := InjectManagementHTML([]byte(fixture))
		if ok || string(result) != fixture {
			t.Fatal("unrecognized bundle must remain unchanged")
		}
	}
}

func TestJavaScriptDoesNotUseHTMLSinks(t *testing.T) {
	for _, name := range []string{"stats.js", "stats-core.js", "management-bridge.js", "management-nav.js"} {
		data, _, _ := Asset(name)
		for _, unsafe := range []string{".innerHTML", ".outerHTML", "insertAdjacentHTML", "document.write(", "eval("} {
			if bytes.Contains(data, []byte(unsafe)) {
				t.Errorf("unsafe DOM sink %q in %s", unsafe, name)
			}
		}
	}
}

// This optional integration check runs against a real single-file upstream build
// without vendoring its SPA or adding dependencies to the server repository.
func TestActualManagementBuild(t *testing.T) {
	path := os.Getenv("CPA_MANAGEMENT_FIXTURE")
	if path == "" {
		t.Skip("set CPA_MANAGEMENT_FIXTURE to a compiled upstream index.html")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := InjectManagementHTML(data)
	if !ok || !bytes.Contains(result, []byte("CPAQuotaPersistence.attach")) {
		t.Fatal("the actual upstream bundle was not recognized")
	}
	if len(result) <= len(data) || bytes.Count(result, []byte(bridgeMarker)) != 1 || bytes.Count(result, []byte(navMarker)) != 1 {
		t.Fatal("invalid compiled-asset injection")
	}
	verifyCompiledHelpers(t, data, result)
}

func verifyCompiledHelpers(t *testing.T, original, injected []byte) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute compiled native helper semantics")
	}
	var body []byte
	for _, match := range scriptElement.FindAllSubmatch(injected, -1) {
		if moduleType.Match(match[1]) && !scriptSource.Match(match[1]) {
			body = match[2]
		}
	}
	check := exec.Command(node, "--input-type=module", "--check")
	check.Stdin = bytes.NewReader(body)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("injected module is not valid JavaScript: %v\n%s", err, output)
	}
	match := verifiedHelpers.FindSubmatchIndex(original)
	if match == nil {
		t.Fatal("missing verified native helper sequence")
	}
	names := make(map[string]string)
	for i, slot := range helperSlots {
		names[strings.TrimPrefix(slot, "@")] = string(original[match[2*i+2]:match[2*i+3]])
	}
	source := original[match[0]:match[1]]
	// Extract the same native sequence with its three wrappers, not loader/body
	// rewrites; stripping only wrapper delimiters recovers exact original bytes.
	start := bytes.Index(body, []byte("var "+names["updater"]+"="))
	if start < 0 {
		t.Fatal("cannot locate wrapped updater")
	}
	endMarker := []byte("return " + names["callback"] + "(),!0})")
	end := bytes.Index(body[start:], endMarker)
	if end < 0 {
		t.Fatal("cannot locate wrapped helper sequence")
	}
	wrapped := body[start : start+end+len(endMarker)]
	unwrapped := string(wrapped)
	for _, wrapper := range []string{"wrapUpdater", "wrapCapture", "wrapCommit"} {
		unwrapped = strings.Replace(unwrapped, "(window.CPAQuotaPersistence?."+wrapper+"||(f=>f))(", "", 1)
	}
	unwrapped = strings.Replace(unwrapped, "),"+names["quota"]+"=", ","+names["quota"]+"=", 1)
	unwrapped = strings.Replace(unwrapped, "),"+names["commit"]+"=", ","+names["commit"]+"=", 1)
	unwrapped = strings.TrimSuffix(unwrapped, ")")
	if unwrapped != string(source) {
		t.Fatal("original native helper/store bodies were changed")
	}
	bridge, _, _ := Asset("management-bridge.js")
	payload, err := json.Marshal(map[string]any{"original": string(source), "wrapped": string(wrapped), "names": names, "bridge": string(bridge)})
	if err != nil {
		t.Fatal(err)
	}
	run := exec.Command(node, "compiled_helpers_test.cjs")
	run.Stdin = bytes.NewReader(payload)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("compiled native helpers violated semantics/provenance: %v\n%s", err, output)
	}
}
