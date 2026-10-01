package usagepersist

import (
	"bytes"
	"compress/gzip"
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
)

// readMiddlewareManagementHTML shares the reduced upstream document fixture with
// API integration tests. The navigation asset does not recognize upstream
// builds, so any HTML document with a script or body anchor would do.
func readMiddlewareManagementHTML(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "api", "testdata", "management-upstream.html")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reduced management fixture: %v", err)
	}
	return string(data)
}

func middlewareRequest(t *testing.T, handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestManagementMiddlewareHTMLInjectsRealFileAndRepairsHeaders(t *testing.T) {
	cfg := &config.Config{}
	engine := gin.New()
	engine.Use(ManagementNavMiddleware(func() *config.Config { return cfg }))
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
	if response.Code != 200 || !strings.Contains(response.Body.String(), "data-cpa-stats-nav") || !strings.Contains(response.Body.String(), `<body><div id="root"></div></body>`) {
		t.Fatalf("navigation asset missing: %d, %s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-CPA-Stats-Nav") != "enabled" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("ETag") != "" || response.Header().Get("Last-Modified") != "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("modified entity headers stale: %v", response.Header())
	}
	original, errRead := os.ReadFile(path)
	if errRead != nil || string(original) != readMiddlewareManagementHTML(t) {
		t.Fatal("source asset changed")
	}
}

func TestManagementMiddlewareHTMLPreUpgradeConditionalCacheGetsFullRepresentation(t *testing.T) {
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
			after.Use(ManagementNavMiddleware(func() *config.Config { return &config.Config{} }))
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
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "data-cpa-stats-nav") {
				t.Fatalf("old cached page was not replaced with full augmented HTML: %d", response.Code)
			}
			if response.Header().Get("X-CPA-Stats-Nav") != "enabled" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
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
					engine.Use(ManagementNavMiddleware(func() *config.Config { return &config.Config{} }))
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
	engine := gin.New()
	engine.Use(ManagementNavMiddleware(func() *config.Config { return &config.Config{} }))
	engine.GET("/management.html", func(c *gin.Context) {
		c.Header("ETag", `"source"`)
		http.ServeContent(c.Writer, c.Request, "management.html", time.Unix(1700000000, 0), strings.NewReader(readMiddlewareManagementHTML(t)))
	})
	request := httptest.NewRequest(http.MethodGet, "/management.html", nil)
	request.Header.Set("Range", "bytes=0-9,30-49")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	mediaType, params, errType := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if errType != nil || mediaType != "multipart/byteranges" || response.Code != http.StatusPartialContent || response.Header().Get("ETag") != `"source"` || response.Header().Get("X-CPA-Stats-Nav") != "" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
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
			engine.Use(ManagementNavMiddleware(func() *config.Config { return &config.Config{} }))
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
			if response.Body.String() != readMiddlewareManagementHTML(t) || response.Header().Get("ETag") != `"source"` || response.Header().Get("X-CPA-Stats-Nav") != "" || !response.Flushed || !reflect.DeepEqual(request.Header, originalHeaders) {
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
	engine := gin.New()
	engine.Use(ManagementNavMiddleware(func() *config.Config { return &config.Config{} }))
	engine.GET("/management.html", func(c *gin.Context) {
		c.Header("Content-Encoding", "gzip")
		c.Header("ETag", `"compressed-source"`)
		c.Header("Content-Length", strconv.Itoa(encoded.Len()))
		c.Data(http.StatusOK, "text/html", encoded.Bytes())
	})
	response := middlewareRequest(t, engine, http.MethodGet, "/management.html", nil)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), encoded.Bytes()) || response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Content-Length") != strconv.Itoa(encoded.Len()) || response.Header().Get("ETag") != `"compressed-source"` || response.Header().Get("X-CPA-Stats-Nav") != "" {
		t.Fatal("encoded response bytes or representation headers changed")
	}
}

func TestManagementMiddlewareHTMLFailOpen(t *testing.T) {
	for _, test := range []struct {
		name                      string
		status                    int
		body, encoding, navHeader string
		flush, disabled, home     bool
	}{
		{name: "unknown", status: 200, body: "<html>unknown upstream</html>"},
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
			cfg := &config.Config{}
			cfg.RemoteManagement.DisableControlPanel = test.disabled
			cfg.Home.Enabled = test.home
			engine := gin.New()
			engine.Use(ManagementNavMiddleware(func() *config.Config { return cfg }))
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
			if response.Code != test.status || response.Body.String() != test.body || response.Header().Get("ETag") != `"unchanged"` || response.Header().Get("X-CPA-Stats-Nav") != test.navHeader {
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
	if response.Code != 200 || response.Body.String() != "123456789tail" || response.Header().Get("X-Before") != "preserved" || response.Header().Get("X-Late") != "" || response.Header().Get("X-CPA-Stats-Nav") != "" {
		t.Fatalf("overflow changed original writer behavior: %d %v %q", response.Code, response.Header(), response.Body.String())
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
	engine := gin.New()
	engine.Use(ManagementNavMiddleware(func() *config.Config { return &config.Config{} }))
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
	if errBody != nil || response.StatusCode != 200 || len(body) <= len(original) || !bytes.Contains(body, []byte("data-cpa-stats-nav")) || response.Header.Get("X-CPA-Stats-Nav") != "enabled" || response.ContentLength != int64(len(body)) {
		t.Fatalf("real HTTP HTML buffer failed: status=%d bytes=%d original=%d length=%d err=%v", response.StatusCode, len(body), len(original), response.ContentLength, errBody)
	}
}

// TestManagementMiddlewareSkipsObservationUntilIdentityIsPublished pins the
// boundary a "refresh quota" click actually depends on. The original handler always
// performs the provider call and returns its own response; this add-on only copies
// the captured evidence, and only for a credential it can already name. Publication
// happens when the add-on identity endpoint is read, so a refresh before that read
// is skipped silently rather than guessed at.
