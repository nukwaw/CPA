package web

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const managementFixture = `<!doctype html><html><head><script type="module" crossorigin>console.log('panel')</script></head><body><div id="root"></div></body></html>`

func TestAssetAllowlist(t *testing.T) {
	for _, name := range []string{"stats.html", "stats.css", "stats-core.js", "stats.js", "management-nav.js"} {
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

func TestInjectManagementNav(t *testing.T) {
	result, ok := InjectManagementNav([]byte(managementFixture))
	if !ok {
		t.Fatal("management document was not augmented")
	}
	asset := `<script ` + navMarker + ` src=".` + AssetsPrefix + `/management-nav.js"></script>`
	if bytes.Count(result, []byte(asset)) != 1 {
		t.Fatal("navigation asset does not use the canonical asset path and marker")
	}
	if bytes.Index(result, []byte("management-nav.js")) > bytes.Index(result, []byte(`type="module"`)) {
		t.Fatal("navigation asset must load before the inline module")
	}
	if !bytes.Contains(result, []byte(`<body><div id="root"></div></body>`)) {
		t.Fatal("original management body was changed")
	}
	second, secondOK := InjectManagementNav(result)
	if !secondOK || !bytes.Equal(result, second) {
		t.Fatal("injection is not idempotent")
	}
}

// Injection never recognizes the upstream build: unknown or future markup is
// still augmented, because the asset itself tolerates an unfamiliar sidebar.
func TestInjectManagementNavDoesNotRecognizeBundles(t *testing.T) {
	for _, fixture := range []string{
		`<html><head><script type="module">console.log('unknown')</script></head><body></body></html>`,
		`<HTML><HEAD><SCRIPT TYPE="module">future()</SCRIPT></HEAD><BODY></BODY></HTML>`,
		strings.Replace(managementFixture, `type="module"`, `type="module" src="external.js"`, 1),
		managementFixture + managementFixture,
	} {
		result, ok := InjectManagementNav([]byte(fixture))
		if !ok || !bytes.Contains(result, []byte(navMarker)) {
			t.Fatalf("valid HTML document was not augmented: %.60s", fixture)
		}
	}
}

func TestInjectManagementNavAnchors(t *testing.T) {
	// A document without scripts receives the asset before </body>.
	fixture := `<!doctype html><html><body><div id="root"></div></body></html>`
	result, ok := InjectManagementNav([]byte(fixture))
	if !ok {
		t.Fatal("scriptless document was not augmented")
	}
	if bytes.Index(result, []byte(navMarker)) > bytes.Index(result, []byte("</body>")) {
		t.Fatal("navigation asset must load before the body ends")
	}
	// A document with neither anchor is served unchanged.
	plain := []byte(`plain text`)
	result, ok = InjectManagementNav(plain)
	if ok || !bytes.Equal(result, plain) {
		t.Fatal("non-HTML document must remain unchanged")
	}
}

func TestJavaScriptDoesNotUseHTMLSinks(t *testing.T) {
	for _, name := range []string{"stats.js", "stats-core.js", "management-nav.js"} {
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
	result, ok := InjectManagementNav(data)
	if !ok {
		t.Fatal("the actual upstream document has no injection anchor")
	}
	if len(result) <= len(data) || bytes.Count(result, []byte(navMarker)) != 1 {
		t.Fatal("invalid navigation asset injection")
	}
	if bytes.Index(result, []byte(navMarker)) > bytes.Index(result, []byte(`type="module"`)) {
		t.Fatal("navigation asset must load before the original module")
	}
}
