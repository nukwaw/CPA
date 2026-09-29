package web

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

func withManualSources(fixture string) string {
	data, err := os.ReadFile("manual_sources_fixture.json")
	if err != nil {
		panic(err)
	}
	var source struct {
		Order []string
		Nodes map[string]string
	}
	if err := json.Unmarshal(data, &source); err != nil {
		panic(err)
	}
	var code strings.Builder
	for _, name := range source.Order {
		node := source.Nodes[name]
		if !strings.HasPrefix(node, "function ") {
			code.WriteString("var ")
		}
		code.WriteString(node)
		code.WriteByte(';')
	}
	return strings.Replace(fixture, "</script>", code.String()+"</script>", 1)
}

var managementFixture = withManualSources(nativeManagementFixture)

func TestManualSourcePatternsFailClosed(t *testing.T) {
	mutations := [][2]string{
		{"fetchQuota:gk", "fetchQuota:otherFetch"},
		{"await vk(e,t),gk(e,t)", "await vk(e,t),otherFetch(e,t)"},
		{"a=d_(e),o=fk(e)", "a=d_(e),o=otherHeader(e)"},
		{"let t=d_(e),n={...Ag}", "let t=otherAccount(e),n={...Ag}"},
		{"e.auth_index??e.authIndex", "e.authIndex??e.auth_index"},
		{"let r=await pO(e)", "let r=await otherProject(e)"},
		{"let r=Ik(e)", "let r=otherUser(e)"},
		{"t[`x-userid`]=n", "t[`x-userid`]=e.user_id"},
		{"let a=n.name.trim(),o=Jg(n.authIndex??n.auth_index)", "let a=n.name.trim(),o=Jg(n.auth_index??n.authIndex)"},
		{"codex:{...yk,Body:Ck}", "codex:{...otherConfig,Body:Ck}"},
		{"Ag={Authorization:`Bearer $TOKEN$`", "Ag={Authorization:`Bearer literal`"},
	}
	for _, mutation := range mutations {
		fixture := strings.Replace(managementFixture, mutation[0], mutation[1], 1)
		if fixture == managementFixture {
			t.Fatalf("missing mutation %q", mutation[0])
		}
		if result, ok := InjectManagementHTML([]byte(fixture)); ok || string(result) != fixture {
			t.Fatalf("accepted partial selector coverage: %q", mutation[0])
		}
	}
	for _, suffix := range []string{"var gk=()=>{};", "var pO=()=>{};", "var yk={};", "function d_(){}", "gk=()=>{};", "d_=()=>null;"} {
		fixture := strings.Replace(managementFixture, "</script>", suffix+"</script>", 1)
		if result, ok := InjectManagementHTML([]byte(fixture)); ok || string(result) != fixture {
			t.Fatalf("accepted duplicate source %q", suffix)
		}
	}
	if _, ok := InjectManagementHTML([]byte(nativeManagementFixture)); ok {
		t.Fatal("accepted missing selector coverage")
	}
}

func TestManualSourcesRenameLexicalSlots(t *testing.T) {
	body := []byte(managementFixture)
	// Rename every non-local lexical reference using the captured positions, not
	// string replacement (which would corrupt property names or string literals).
	type replacement struct {
		start, end int
		text       string
	}
	var edits []replacement
	seen := make(map[int]bool)
	for _, pattern := range verifiedSources {
		match := pattern.pattern.FindSubmatchIndex(body)
		if match == nil {
			t.Fatal("missing source template")
		}
		for i, slot := range pattern.slots {
			if strings.Contains(slot, "local") || slot == "@quota" {
				continue
			}
			start, end := match[2*i+2], match[2*i+3]
			if !seen[start] {
				edits = append(edits, replacement{start, end, "renamed" + slot[1:]})
				seen[start] = true
			}
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, edit := range edits {
		body = append(append(append([]byte{}, body[:edit.start]...), []byte(edit.text)...), body[edit.end:]...)
	}
	result, ok := InjectManagementHTML(body)
	if !ok {
		t.Fatal("lexical names became an ABI")
	}
	verifyCompiledHelpers(t, body, result)
	verifyCompiledSources(t, body)
}

func TestActualSourceShapes(t *testing.T) {
	path := os.Getenv("CPA_MANAGEMENT_FIXTURE")
	if path == "" {
		t.Skip("fixture required")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	quota := quotaBinding.FindSubmatch(body)
	if quota == nil {
		t.Fatal("missing store")
	}
	if _, _, ok := matchSources(body, string(quota[1])); !ok {
		t.Fatal("incomplete linked source proof")
	}
	if _, ok := findSourceEdits(body, string(quota[1])); !ok {
		t.Fatal("invalid source declaration/order")
	}
	verifyCompiledSources(t, body)
}

func verifyCompiledSources(t *testing.T, original []byte) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node required")
	}
	var body []byte
	for _, match := range scriptElement.FindAllSubmatch(original, -1) {
		if moduleType.Match(match[1]) && !scriptSource.Match(match[1]) {
			body = match[2]
		}
	}
	quota := quotaBinding.FindSubmatch(body)
	if quota == nil {
		t.Fatal("missing quota binding")
	}
	edits, ok := findSourceEdits(body, string(quota[1]))
	if !ok {
		t.Fatal("missing verified source coverage")
	}
	var wrapped bytes.Buffer
	writeWrappedHelpers(&wrapped, body, edits)
	// Execute only AST-verified provider nodes and helpers, not the full React app.
	// Fetch APIs / display-only parsers are stubbed; selector/index functions are
	// the exact compiled bodies matched from the actual fixture under test.
	nodes := make(map[string]string)
	names := make(map[string]string)
	_, matches, matched := matchSources(body, string(quota[1]))
	if !matched {
		t.Fatal("missing linked source shapes")
	}
	for name, pattern := range verifiedSources {
		match := matches[name]
		for i, slot := range pattern.slots {
			names[slot[1:]] = string(body[match[2*i+2]:match[2*i+3]])
		}
		loc := match[:2]
		var local []helperEdit
		for _, edit := range edits {
			if edit.start >= loc[0] && edit.end <= loc[1] {
				edit.start -= loc[0]
				edit.end -= loc[0]
				local = append(local, edit)
			}
		}
		var result bytes.Buffer
		writeWrappedHelpers(&result, body[loc[0]:loc[1]], local)
		nodes[name] = result.String()
	}
	bridge, _, _ := Asset("management-bridge.js")
	payload, err := json.Marshal(map[string]any{"nodes": nodes, "names": names, "bridge": string(bridge)})
	if err != nil {
		t.Fatal(err)
	}
	run := exec.Command(node, "compiled_sources_test.cjs")
	run.Stdin = bytes.NewReader(payload)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("compiled source proof violated: %v\n%s", err, output)
	}
}
