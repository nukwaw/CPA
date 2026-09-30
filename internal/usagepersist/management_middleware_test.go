package usagepersist

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const middlewareQuotaURL = "https://chatgpt.com/backend-api/wham/usage"

// readMiddlewareManagementHTML shares the pinned, syntax-valid source excerpt
// with API integration tests. It includes selector declarations and references,
// not only the store/helper subset deliberately rejected by strict recognition.
func readMiddlewareManagementHTML(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "api", "testdata", "management-upstream.html")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reduced management fixture: %v", err)
	}
	return string(data)
}

func middlewareTestStore(t *testing.T) *Store {
	t.Helper()
	store, errOpen := Open(context.Background(), Options{DataDir: t.TempDir()})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(context.Background()); errClose != nil {
			t.Error(errClose)
		}
	})
	wpBindManagementFixtures(t, store)
	return store
}

func middlewareResolver(index string) (string, bool) { return "codex", index == "account" }

func middlewareQuotaEnvelope(t *testing.T, padding string) []byte {
	t.Helper()
	data, errJSON := json.Marshal(map[string]any{
		"status_code": 200,
		"header":      map[string][]string{"Set-Cookie": {"private-cookie"}},
		"body":        `{"rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000}},"token":"private-body-token"}`,
		"unrelated":   padding,
	})
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	return data
}

func middlewareRequest(t *testing.T, handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func middlewareQuotaCount(t *testing.T, store *Store) int {
	t.Helper()
	flushFixture(t, store)
	snapshots, errRead := store.Quotas(context.Background())
	if errRead != nil {
		t.Fatal(errRead)
	}
	return len(snapshots)
}

func TestManagementMiddlewarePreservesAPICallBytesAndObservesQuota(t *testing.T) {
	for _, path := range []string{"/v0/management/api-call", "/v8/management/requests/api-call"} {
		t.Run(path, func(t *testing.T) {
			store := middlewareTestStore(t)
			engine := gin.New()
			engine.Use(store.ManagementMiddleware(middlewareResolver, nil))
			input := []byte(` {"authIndex":"account","url":"` + middlewareQuotaURL + `","header":{"Authorization":"Bearer $TOKEN$"},"data":"{}"} `)
			output := middlewareQuotaEnvelope(t, "")
			engine.POST(path, func(c *gin.Context) {
				got, errRead := io.ReadAll(c.Request.Body)
				if errRead != nil || !bytes.Equal(got, input) {
					t.Errorf("request body changed: %q, %v", got, errRead)
				}
				c.Header("Content-Type", "application/json")
				c.Header("X-Original", "preserved")
				c.Status(http.StatusOK)
				_, _ = c.Writer.WriteString(string(output[:10]))
				_, _ = c.Writer.Write(output[10:])
			})
			response := middlewareRequest(t, engine, http.MethodPost, path, input)
			if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), output) || response.Header().Get("X-Original") != "preserved" {
				t.Fatalf("original response altered: %d, %q", response.Code, response.Body.String())
			}
			flushFixture(t, store)
			snapshots, errRead := store.Quotas(context.Background())
			if errRead != nil || len(snapshots) != 1 || len(snapshots[0].Windows) != 1 || *snapshots[0].Windows[0].UsedPercent != 25 {
				t.Fatalf("quota observation missing: %+v, %v", snapshots, errRead)
			}
			serialized, _ := json.Marshal(snapshots)
			for _, secret := range []string{"request-secret", "private-body-token", "private-cookie", middlewareQuotaURL} {
				if strings.Contains(string(serialized), secret) {
					t.Fatalf("persisted raw response/request data: %s", serialized)
				}
			}
		})
	}
}

func TestManagementMiddlewareIgnoresFailuresUnknownAndOversizedCaptures(t *testing.T) {
	input := []byte(`{"auth_index":"account","url":"` + middlewareQuotaURL + `","header":{"Authorization":"Bearer $TOKEN$"}}`)
	output := middlewareQuotaEnvelope(t, "")
	for _, test := range []struct {
		name, path    string
		status        int
		input, output []byte
		abort         bool
		encoding      string
	}{
		{name: "unauthorized", path: "/v0/management/api-call", status: 401, input: input, output: output, abort: true},
		{name: "forbidden", path: "/v0/management/api-call", status: 403, input: input, output: output, abort: true},
		{name: "handler-error", path: "/v0/management/api-call", status: 500, input: input, output: output},
		{name: "unknown-route", path: "/v0/management/arbitrary", status: 200, input: input, output: output},
		{name: "unknown-credential", path: "/v0/management/api-call", status: 200, input: []byte(`{"auth_index":"unknown","url":"` + middlewareQuotaURL + `","header":{"Authorization":"Bearer $TOKEN$"}}`), output: output},
		{name: "unknown-envelope", path: "/v0/management/api-call", status: 200, input: input, output: []byte(`{"message":"success"}`)},
		{name: "invalid-json", path: "/v0/management/api-call", status: 200, input: input, output: []byte("not-json")},
		{name: "encoded-json", path: "/v0/management/api-call", status: 200, input: input, output: output, encoding: "gzip"},
		{name: "request-cap", path: "/v0/management/api-call", status: 200, input: append(append([]byte(nil), input...), bytes.Repeat([]byte(" "), middlewareRequestLimit)...), output: output},
		{name: "response-cap", path: "/v0/management/api-call", status: 200, input: input, output: middlewareQuotaEnvelope(t, strings.Repeat("x", middlewareResponseLimit))},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := middlewareTestStore(t)
			engine := gin.New()
			engine.Use(store.ManagementMiddleware(middlewareResolver, nil))
			engine.POST(test.path, func(c *gin.Context) {
				got, errRead := io.ReadAll(c.Request.Body)
				if errRead != nil || !bytes.Equal(got, test.input) {
					t.Errorf("cap/error truncated input: got=%d want=%d err=%v", len(got), len(test.input), errRead)
				}
				if test.encoding != "" {
					c.Header("Content-Encoding", test.encoding)
				}
				if test.abort {
					c.Abort()
				}
				c.Data(test.status, "application/json", test.output)
			})
			response := middlewareRequest(t, engine, http.MethodPost, test.path, test.input)
			if response.Code != test.status || !bytes.Equal(response.Body.Bytes(), test.output) {
				t.Fatalf("cap/error altered output: status=%d bytes=%d", response.Code, response.Body.Len())
			}
			if got := middlewareQuotaCount(t, store); got != 0 {
				t.Fatalf("unexpected %d observations", got)
			}
		})
	}
}

func TestManagementMiddlewareFetchPreservesMissingFraction(t *testing.T) {
	for _, test := range []struct{ method, path, request string }{
		{http.MethodPost, "/v0/management/quota/fetch", `{"AuthIndex":"account"}`},
		{http.MethodPost, "/v8/management/credentials/quota/fetch", `{"auth_index":"account"}`},
		{http.MethodGet, "/v0/management/plugins/custom/quota?auth_index=account", ""},
		{http.MethodGet, "/v8/management/plugins/custom/quota?authIndex=account", ""},
	} {
		t.Run(test.path, func(t *testing.T) {
			store := middlewareTestStore(t)
			engine := gin.New()
			engine.Use(store.ManagementMiddleware(middlewareResolver, nil))
			output := []byte(`{"groups":[{"displayName":"Plan","buckets":[{"window":"missing"},{"window":"unknown","remainingFraction":null},{"window":"monthly","remainingFraction":0}]}]}`)
			engine.Handle(test.method, strings.Split(test.path, "?")[0], func(c *gin.Context) {
				_, _ = io.Copy(io.Discard, c.Request.Body)
				c.Data(200, "application/json", output)
			})
			response := middlewareRequest(t, engine, test.method, test.path, []byte(test.request))
			if !bytes.Equal(response.Body.Bytes(), output) {
				t.Fatal("fetch output changed")
			}
			flushFixture(t, store)
			snapshots, errRead := store.Quotas(context.Background())
			if errRead != nil || len(snapshots) != 1 || len(snapshots[0].Windows) != 1 || snapshots[0].Windows[0].Label != "monthly" || *snapshots[0].Windows[0].RemainingPercent != 0 {
				t.Fatalf("missing value fabricated exhausted quota: %+v, %v", snapshots, errRead)
			}
		})
	}
}

func TestManagementMiddlewareResetRequiresConfirmedSuccessfulResponse(t *testing.T) {
	for _, test := range []struct {
		name, method, path, response string
		status                       int
		removed                      bool
	}{
		{"routing", http.MethodPost, "/v0/management/reset-quota", `{"status":"ok","auth_index":"account"}`, 200, true},
		{"credential", http.MethodPost, "/v8/management/credentials/quota/reset", `{"status":"ok","auth_index":"account"}`, 200, true},
		{"plugin", http.MethodDelete, "/v0/management/plugins/custom/quota?auth_index=account", `{"status":"ok","auth_index":"account"}`, 200, true},
		{"plugin-action", http.MethodPost, "/v0/management/plugins/custom/quota/reset", `{"status":"ok","auth_index":"account"}`, 200, true},
		{"failed", http.MethodPost, "/v0/management/quota/reset", `{"status":"ok","auth_index":"account"}`, 500, false},
		{"unconfirmed", http.MethodPost, "/v0/management/quota/reset", `{"message":"success"}`, 200, false},
		{"wrong-identity", http.MethodPost, "/v0/management/quota/reset", `{"status":"ok","auth_index":"other"}`, 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := middlewareTestStore(t)
			wpObserveAPICall(store, context.Background(), "codex", "account", middlewareQuotaURL, 200, nil, []byte(`{"rate_limit":{"primary_window":{"used_percent":25}}}`))
			engine := gin.New()
			engine.Use(store.ManagementMiddleware(middlewareResolver, nil))
			engine.Handle(test.method, strings.Split(test.path, "?")[0], func(c *gin.Context) {
				_, _ = io.Copy(io.Discard, c.Request.Body)
				c.Data(test.status, "application/json", []byte(test.response))
			})
			response := middlewareRequest(t, engine, test.method, test.path, []byte(`{"auth_index":"account"}`))
			if response.Code != test.status || response.Body.String() != test.response {
				t.Fatal("reset response changed")
			}
			if removed := middlewareQuotaCount(t, store) == 0; removed != test.removed {
				t.Fatalf("removed=%v want=%v", removed, test.removed)
			}
		})
	}
}

func TestManagementMiddlewareHTMLInjectsRealFileAndRepairsHeaders(t *testing.T) {
	store := middlewareTestStore(t)
	cfg := &config.Config{}
	engine := gin.New()
	engine.Use(store.ManagementMiddleware(nil, func() *config.Config { return cfg }))
	path := filepath.Join(t.TempDir(), "management.html")
	if errWrite := os.WriteFile(path, []byte(readMiddlewareManagementHTML(t)), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	engine.GET("/management.html", func(c *gin.Context) {
		if c.Query("safe-mode") != "configure" {
			t.Error("query changed")
		}
		c.Header("ETag", `"original"`)
		c.Header("Cache-Control", "public,max-age=3600")
		c.File(path)
	})
	response := middlewareRequest(t, engine, http.MethodGet, "/management.html?safe-mode=configure", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "CPAQuotaPersistence.attach") || !strings.Contains(response.Body.String(), `<body><div id="root"></div></body>`) {
		t.Fatalf("original file adapter missing: %d, %s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-CPA-Quota-Persistence") != "enabled" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("ETag") != "" || response.Header().Get("Last-Modified") != "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("modified entity headers stale: %v", response.Header())
	}
	original, errRead := os.ReadFile(path)
	if errRead != nil || string(original) != readMiddlewareManagementHTML(t) {
		t.Fatal("source asset changed")
	}
}

func TestManagementMiddlewareHTMLPreUpgradeConditionalCacheGetsFullRepresentation(t *testing.T) {
	store := middlewareTestStore(t)
	path := filepath.Join(t.TempDir(), "management.html")
	if errWrite := os.WriteFile(path, []byte(readMiddlewareManagementHTML(t)), 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
	modified := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	if errTimes := os.Chtimes(path, modified, modified); errTimes != nil {
		t.Fatal(errTimes)
	}
	serve := func(c *gin.Context) {
		c.Header("ETag", `"source-file"`)
		c.Header("Cache-Control", "public, max-age=3600")
		c.Header("Content-MD5", "source-checksum")
		c.Header("Digest", "source-digest")
		c.File(path)
	}
	// Obtain the validators from a real pre-upgrade static-file response, and
	// prove the unchanged source would actually answer a conditional GET 304.
	before := gin.New()
	before.GET("/management.html", serve)
	cached := middlewareRequest(t, before, http.MethodGet, "/management.html", nil)
	if cached.Code != http.StatusOK || cached.Body.String() != readMiddlewareManagementHTML(t) || cached.Header().Get("Last-Modified") == "" {
		t.Fatal("pre-upgrade file fixture did not provide a cacheable representation")
	}
	for _, test := range []struct {
		name    string
		headers http.Header
	}{
		{"last-modified", http.Header{"If-Modified-Since": {cached.Header().Get("Last-Modified")}}},
		{"etag", http.Header{"If-None-Match": {cached.Header().Get("ETag")}}},
		{"both", http.Header{"If-Modified-Since": {cached.Header().Get("Last-Modified")}, "If-None-Match": {cached.Header().Get("ETag")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/management.html?safe-mode=configure", strings.NewReader("untouched GET body"))
			request.Header = test.headers.Clone()
			revalidated := httptest.NewRecorder()
			before.ServeHTTP(revalidated, request)
			if revalidated.Code != http.StatusNotModified || revalidated.Body.Len() != 0 {
				t.Fatalf("fixture did not reproduce pre-upgrade 304: %d", revalidated.Code)
			}
			request.Header.Set("X-Original", "preserved")
			originalHeaders, originalBody, originalURL := request.Header.Clone(), request.Body, request.URL
			after := gin.New()
			after.Use(func(c *gin.Context) {
				c.Next()
				if c.Request != request || c.Request.Body != originalBody || c.Request.URL != originalURL {
					t.Error("middleware did not restore the caller's exact request")
				}
			})
			after.Use(store.ManagementMiddleware(nil, func() *config.Config { return &config.Config{} }))
			after.GET("/management.html", func(c *gin.Context) {
				if c.Request == request || c.Request.Header.Get("If-None-Match") != "" || c.Request.Header.Get("If-Modified-Since") != "" || c.Request.Header.Get("X-Original") != "preserved" || c.Request.Body != originalBody || c.Request.URL.RawQuery != "safe-mode=configure" {
					t.Error("local handler did not receive an isolated full-representation request")
				}
				if !reflect.DeepEqual(request.Header, originalHeaders) {
					t.Error("original headers were mutated while the clone was active")
				}
				serve(c)
			})
			response := httptest.NewRecorder()
			after.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "CPAQuotaPersistence.attach") {
				t.Fatalf("old cached page was not replaced with full augmented HTML: %d", response.Code)
			}
			if response.Header().Get("X-CPA-Quota-Persistence") != "enabled" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
				t.Fatalf("new representation has invalid cache/length headers: %v", response.Header())
			}
			for _, header := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest", "Accept-Ranges"} {
				if response.Header().Get(header) != "" {
					t.Errorf("source validator %s survived injection", header)
				}
			}
			body, errRead := io.ReadAll(originalBody)
			if errRead != nil || string(body) != "untouched GET body" || !reflect.DeepEqual(request.Header, originalHeaders) {
				t.Fatal("conditional request headers or body changed")
			}
		})
	}
}

func TestManagementMiddlewareHTMLRangeAndPreconditionsPreserveSourceBytes(t *testing.T) {
	store := middlewareTestStore(t)
	modified := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, test := range []struct {
		name   string
		header http.Header
		status int
	}{
		{"range", http.Header{"Range": {"bytes=10-39"}}, http.StatusPartialContent},
		{"range-with-old-validator", http.Header{"Range": {"bytes=10-39"}, "If-None-Match": {`"original"`}}, http.StatusPartialContent},
		{"if-range-etag", http.Header{"Range": {"bytes=10-39"}, "If-Range": {`"original"`}}, http.StatusPartialContent},
		{"if-range-date", http.Header{"Range": {"bytes=10-39"}, "If-Range": {modified.Format(http.TimeFormat)}}, http.StatusPartialContent},
		{"invalid-range", http.Header{"Range": {"bytes=999999-"}}, http.StatusRequestedRangeNotSatisfiable},
		{"if-match", http.Header{"If-Match": {`"not-current"`}}, http.StatusPreconditionFailed},
		{"if-unmodified-since", http.Header{"If-Unmodified-Since": {modified.Add(-time.Hour).Format(http.TimeFormat)}}, http.StatusPreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			makeEngine := func(augmented bool) *gin.Engine {
				engine := gin.New()
				if augmented {
					engine.Use(store.ManagementMiddleware(nil, func() *config.Config { return &config.Config{} }))
				}
				engine.GET("/management.html", func(c *gin.Context) {
					c.Header("ETag", `"original"`)
					http.ServeContent(c.Writer, c.Request, "management.html", modified, strings.NewReader(readMiddlewareManagementHTML(t)))
				})
				return engine
			}
			request := httptest.NewRequest(http.MethodGet, "/management.html", nil)
			request.Header = test.header.Clone()
			response := httptest.NewRecorder()
			makeEngine(true).ServeHTTP(response, request)
			baselineRequest := request.Clone(request.Context())
			baselineRequest.Header.Del("If-None-Match")
			baseline := httptest.NewRecorder()
			makeEngine(false).ServeHTTP(baseline, baselineRequest)
			if response.Code != test.status || response.Code != baseline.Code || !bytes.Equal(response.Body.Bytes(), baseline.Body.Bytes()) || !reflect.DeepEqual(response.Header(), baseline.Header()) || !reflect.DeepEqual(request.Header, test.header) {
				t.Fatalf("range/precondition changed: got %d %v (%d bytes), baseline %d %v (%d bytes)", response.Code, response.Header(), response.Body.Len(), baseline.Code, baseline.Header(), baseline.Body.Len())
			}
		})
	}
}

func TestManagementMiddlewareHTMLMultipartRangesRemainSourceBytes(t *testing.T) {
	store := middlewareTestStore(t)
	engine := gin.New()
	engine.Use(store.ManagementMiddleware(nil, func() *config.Config { return &config.Config{} }))
	engine.GET("/management.html", func(c *gin.Context) {
		c.Header("ETag", `"source"`)
		http.ServeContent(c.Writer, c.Request, "management.html", time.Unix(1700000000, 0), strings.NewReader(readMiddlewareManagementHTML(t)))
	})
	request := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	request.Header.Set("Range", "bytes=0-9,30-49")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	mediaType, params, errType := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if errType != nil || mediaType != "multipart/byteranges" || response.Code != http.StatusPartialContent || response.Header().Get("ETag") != `"source"` || response.Header().Get("X-CPA-Quota-Persistence") != "" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("multipart representation changed: %d %v", response.Code, response.Header())
	}
	reader := multipart.NewReader(bytes.NewReader(response.Body.Bytes()), params["boundary"])
	for _, window := range [][2]int{{0, 10}, {30, 50}} {
		part, errPart := reader.NextPart()
		if errPart != nil {
			t.Fatal(errPart)
		}
		body, errRead := io.ReadAll(part)
		if errRead != nil || string(body) != readMiddlewareManagementHTML(t)[window[0]:window[1]] {
			t.Fatal("multipart range bytes changed")
		}
		if errClose := part.Close(); errClose != nil {
			t.Fatal(errClose)
		}
	}
	if _, errPart := reader.NextPart(); errPart != io.EOF {
		t.Fatalf("unexpected extra multipart range: %v", errPart)
	}
}

func TestManagementMiddlewareHTMLScopeDoesNotTouchOtherRequests(t *testing.T) {
	store := middlewareTestStore(t)
	for _, test := range []struct{ method, path string }{
		{http.MethodPost, "/management.html"}, {http.MethodHead, "/management.html"},
		{http.MethodGet, "/management.html/extra"}, {http.MethodGet, "/other.html"},
		{http.MethodPost, "/v1/chat/completions"}, {http.MethodPost, "/v1/responses"},
		{http.MethodGet, "/v1/responses"},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path+"?stream=true", strings.NewReader("unchanged request body"))
			request.Header = http.Header{"If-None-Match": {`"source"`}, "If-Modified-Since": {"Fri, 02 Jan 2026 03:04:05 GMT"}, "Accept-Encoding": {"gzip"}, "Range": {"bytes=0-10"}, "Upgrade": {"websocket"}, "Connection": {"upgrade"}}
			originalHeaders, originalBody := request.Header.Clone(), request.Body
			engine := gin.New()
			engine.Use(store.ManagementMiddleware(nil, func() *config.Config { return &config.Config{} }))
			engine.Handle(test.method, test.path, func(c *gin.Context) {
				if c.Request != request || c.Request.Body != originalBody || !reflect.DeepEqual(c.Request.Header, originalHeaders) {
					t.Error("request outside GET management.html was wrapped or changed")
				}
				if _, wrapped := c.Writer.(*middlewareHTMLWriter); wrapped {
					t.Error("non-document writer was intercepted")
				}
				body, errRead := io.ReadAll(c.Request.Body)
				if errRead != nil || string(body) != "unchanged request body" {
					t.Error("non-document body changed")
				}
				c.Header("Content-Type", "text/html")
				c.Header("ETag", `"source"`)
				_, _ = c.Writer.WriteString(readMiddlewareManagementHTML(t))
				c.Writer.Flush()
			})
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			if response.Body.String() != readMiddlewareManagementHTML(t) || response.Header().Get("ETag") != `"source"` || response.Header().Get("X-CPA-Quota-Persistence") != "" || !response.Flushed || !reflect.DeepEqual(request.Header, originalHeaders) {
				t.Fatal("non-document response bytes/headers/streaming changed")
			}
		})
	}
}

func TestManagementMiddlewareHTMLCompressedBytesRemainUnchanged(t *testing.T) {
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	if _, errWrite := compressor.Write([]byte(readMiddlewareManagementHTML(t))); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := compressor.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	store := middlewareTestStore(t)
	engine := gin.New()
	engine.Use(store.ManagementMiddleware(nil, func() *config.Config { return &config.Config{} }))
	engine.GET("/management.html", func(c *gin.Context) {
		c.Header("Content-Encoding", "gzip")
		c.Header("ETag", `"compressed-source"`)
		c.Header("Content-Length", strconv.Itoa(encoded.Len()))
		c.Data(http.StatusOK, "text/html", encoded.Bytes())
	})
	response := middlewareRequest(t, engine, http.MethodGet, "/management.html", nil)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), encoded.Bytes()) || response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Content-Length") != strconv.Itoa(encoded.Len()) || response.Header().Get("ETag") != `"compressed-source"` || response.Header().Get("X-CPA-Quota-Persistence") != "" {
		t.Fatal("encoded response bytes or representation headers changed")
	}
}

func TestManagementMiddlewareHTMLFailOpen(t *testing.T) {
	for _, test := range []struct {
		name                   string
		status                 int
		body, encoding, bridge string
		flush, disabled, home  bool
	}{
		{name: "unknown", status: 200, body: "<html>unknown upstream</html>", bridge: "unsupported"},
		{name: "partial", status: 206, body: readMiddlewareManagementHTML(t)},
		{name: "not-modified", status: 304},
		{name: "denied", status: 403, body: readMiddlewareManagementHTML(t)},
		{name: "server-error", status: 500, body: readMiddlewareManagementHTML(t)},
		{name: "encoded", status: 200, body: readMiddlewareManagementHTML(t), encoding: "gzip"},
		{name: "flush", status: 200, body: readMiddlewareManagementHTML(t), flush: true},
		{name: "disabled-panel", status: 200, body: readMiddlewareManagementHTML(t), disabled: true},
		{name: "home-mode", status: 200, body: readMiddlewareManagementHTML(t), home: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := middlewareTestStore(t)
			cfg := &config.Config{}
			cfg.RemoteManagement.DisableControlPanel = test.disabled
			cfg.Home.Enabled = test.home
			engine := gin.New()
			engine.Use(store.ManagementMiddleware(nil, func() *config.Config { return cfg }))
			engine.GET("/management.html", func(c *gin.Context) {
				c.Header("Content-Type", "text/html")
				c.Header("ETag", `"unchanged"`)
				if test.encoding != "" {
					c.Header("Content-Encoding", test.encoding)
				}
				c.Status(test.status)
				if test.flush {
					c.Writer.Flush()
				}
				if test.body != "" {
					_, _ = c.Writer.WriteString(test.body)
				}
			})
			response := middlewareRequest(t, engine, http.MethodGet, "/management.html", nil)
			if response.Code != test.status || response.Body.String() != test.body || response.Header().Get("ETag") != `"unchanged"` || response.Header().Get("X-CPA-Quota-Persistence") != test.bridge {
				t.Fatalf("fallback changed original: status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestManagementHTMLBufferOverflowAndWriteSemantics(t *testing.T) {
	engine := gin.New()
	engine.GET("/overflow", func(c *gin.Context) {
		writer := newMiddlewareHTMLWriter(c.Writer, 8)
		c.Writer = writer
		c.Header("Content-Type", "text/html")
		c.Header("X-Before", "preserved")
		writer.WriteHeader(200)
		_, _ = writer.WriteString("1234")
		if !writer.Written() || writer.Size() != 4 || writer.Status() != 200 {
			t.Error("buffered writer state is wrong")
		}
		c.Header("X-Late", "must-not-appear")
		writer.WriteHeader(500)
		_, _ = writer.WriteString("56789")
		_, _ = writer.WriteString("tail")
		writer.finish(true)
	})
	response := middlewareRequest(t, engine, http.MethodGet, "/overflow", nil)
	if response.Code != 200 || response.Body.String() != "123456789tail" || response.Header().Get("X-Before") != "preserved" || response.Header().Get("X-Late") != "" || response.Header().Get("X-CPA-Quota-Persistence") != "" {
		t.Fatalf("overflow changed original writer behavior: %d %v %q", response.Code, response.Header(), response.Body.String())
	}
}

func TestManagementMiddlewareDoesNotAttributeAnonymousAPICallByQuery(t *testing.T) {
	store := middlewareTestStore(t)
	engine := gin.New()
	engine.Use(store.ManagementMiddleware(middlewareResolver, nil))
	output := middlewareQuotaEnvelope(t, "")
	engine.POST("/v0/management/api-call", func(c *gin.Context) {
		_, _ = io.Copy(io.Discard, c.Request.Body)
		c.Data(200, "application/json", output)
	})
	response := middlewareRequest(t, engine, http.MethodPost, "/v0/management/api-call?auth_index=account", []byte(`{"url":"`+middlewareQuotaURL+`","header":{"Authorization":"Bearer $TOKEN$"}}`))
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), output) || middlewareQuotaCount(t, store) != 0 {
		t.Fatal("anonymous API-call was incorrectly attributed using ignored query input")
	}
}

func TestManagementFetchSnakeCaseAndMissingValues(t *testing.T) {
	response, valid := middlewareFetchResponse([]byte(`{"groups":[{"buckets":[{"remaining_fraction":0.75},{"remainingFraction":0,"remaining_fraction":0.8},{"remainingFraction":null}]}]}`))
	if !valid || len(response.Groups) != 1 || len(response.Groups[0].Buckets) != 2 || response.Groups[0].Buckets[0].RemainingFraction != 0.75 || response.Groups[0].Buckets[1].RemainingFraction != 0 {
		t.Fatalf("fraction presence or alias precedence lost: %+v", response)
	}
}

func TestManagementFetchSummaryValuePresence(t *testing.T) {
	response, valid := middlewareFetchResponse([]byte(`{"summary":[{"key":"missing"},{"key":"null","value":null},{"key":"zero","value":0},{"key":"measured","value":12.5}]}`))
	if !valid || len(response.Summary) != 2 || response.Summary[0].Key != "zero" || response.Summary[0].Value != 0 || response.Summary[1].Value != 12.5 {
		t.Fatalf("summary missing value was fabricated as zero: %+v", response.Summary)
	}
}

func TestManagementMiddlewareActualHTMLBuild(t *testing.T) {
	path := os.Getenv("CPA_MANAGEMENT_FIXTURE")
	if path == "" {
		t.Skip("set CPA_MANAGEMENT_FIXTURE to the actual compiled upstream management HTML")
	}
	original, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	store := middlewareTestStore(t)
	engine := gin.New()
	engine.Use(store.ManagementMiddleware(nil, func() *config.Config { return &config.Config{} }))
	engine.GET("/management.html", func(c *gin.Context) { c.File(path) })
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatal(errStat)
	}
	request, errRequest := http.NewRequest(http.MethodGet, server.URL+"/management.html?safe-mode=configure", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	request.Header.Set("If-Modified-Since", info.ModTime().UTC().Format(http.TimeFormat))
	response, errGet := server.Client().Do(request)
	if errGet != nil {
		t.Fatal(errGet)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	body, errBody := io.ReadAll(response.Body)
	if errBody != nil || response.StatusCode != 200 || len(body) <= len(original) || !bytes.Contains(body, []byte("CPAQuotaPersistence.attach")) || response.Header.Get("X-CPA-Quota-Persistence") != "enabled" || response.ContentLength != int64(len(body)) {
		t.Fatalf("real HTTP HTML buffer failed: status=%d bytes=%d original=%d length=%d err=%v", response.StatusCode, len(body), len(original), response.ContentLength, errBody)
	}
}

// TestManagementMiddlewareSkipsObservationUntilIdentityIsPublished pins the
// boundary a "refresh quota" click actually depends on. The original handler always
// performs the provider call and returns its own response; this add-on only copies
// the captured evidence, and only for a credential it can already name. Publication
// happens when the add-on identity endpoint is read, so a refresh before that read
// is skipped silently rather than guessed at.
func TestManagementMiddlewareSkipsObservationUntilIdentityIsPublished(t *testing.T) {
	input := []byte(`{"auth_index":"account","url":"` + middlewareQuotaURL + `","header":{"Authorization":"Bearer $TOKEN$"},"data":"{}"}`)
	output := middlewareQuotaEnvelope(t, "")
	serve := func(t *testing.T, store *Store) (*httptest.ResponseRecorder, *Store) {
		t.Helper()
		engine := gin.New()
		engine.Use(store.ManagementMiddleware(middlewareResolver, nil))
		engine.POST("/v0/management/api-call", func(c *gin.Context) {
			// The capture is a passive tee: it fills only as the original handler
			// reads, exactly like the real handler binding its JSON body.
			_, _ = io.ReadAll(c.Request.Body)
			c.Header("Content-Type", "application/json")
			c.Status(http.StatusOK)
			_, _ = c.Writer.Write(output)
		})
		return middlewareRequest(t, engine, http.MethodPost, "/v0/management/api-call", input), store
	}

	t.Run("unprimed", func(t *testing.T) {
		store := openTestStore(t)
		// A source is bound, but nothing has read identities yet: no advisory
		// binding copy exists, which is the state after a fresh start.
		manager := coreauth.NewManager(nil, nil, nil)
		if _, errRegister := manager.Register(context.Background(), wpQuotaAuth("codex", "account", "account.json", "fixture-secret")); errRegister != nil {
			t.Fatal(errRegister)
		}
		store.BindQuotaIdentitySource(NewQuotaIdentitySource(func() *coreauth.Manager { return manager }))
		if len(store.publishedQuotaBindings()) != 0 {
			t.Fatal("fixture unexpectedly published bindings before any identity read")
		}
		response, store := serve(t, store)
		// The original response is returned byte-for-byte; only the copy is skipped.
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), output) {
			t.Fatalf("original response altered: %d, %q", response.Code, response.Body.String())
		}
		if count := middlewareQuotaCount(t, store); count != 0 {
			t.Fatalf("quota state recorded without published identity: %d", count)
		}
	})

	t.Run("primed", func(t *testing.T) {
		store := middlewareTestStore(t) // publishes an advisory binding copy
		if len(store.publishedQuotaBindings()) == 0 {
			t.Fatal("fixture did not publish bindings")
		}
		response, store := serve(t, store)
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), output) {
			t.Fatalf("original response altered: %d, %q", response.Code, response.Body.String())
		}
		if count := middlewareQuotaCount(t, store); count != 1 {
			t.Fatalf("primed observation was not recorded: %d", count)
		}
	})
}

func TestManagementMiddlewareRouteAllowlist(t *testing.T) {
	for _, path := range []string{"/v0/management/plugins/p/quota/other", "/v0/management/plugins/p/quota/reset/extra", "/v1/management/api-call", "/v0/management/quota/providers", "/v0/management/auth-files", "/v0/management/stats/quota/cache"} {
		if got := middlewareQuotaRoute(http.MethodPost, path); got != "" {
			t.Fatalf("unknown route %q classified %q", path, got)
		}
	}
	if middlewareQuotaRoute(http.MethodGet, "/v0/management/api-call") != "" {
		t.Fatal("unknown method observed")
	}
}
