package usagepersist

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	usageweb "github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/web"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const (
	middlewareRequestLimit  = 1 << 20
	middlewareResponseLimit = 4 << 20
	middlewareHTMLLimit     = 16 << 20
)

// ManagementMiddleware observes the existing authenticated management pipeline;
// it neither authorizes requests nor replaces handlers. Only bounded, successful
// known quota responses are normalized. Captured raw bytes are never persisted.
// The optional HTML adapter preserves the original page on unsupported bundles,
// encoded/partial responses, flushes, and oversized responses.
func (s *Store) ManagementMiddleware(_ func(string) (string, bool), currentConfig func() *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/management.html" && middlewarePanelEnabled(currentConfig) {
			// Source-file validators do not describe the augmented representation.
			// A browser may still hold a pre-upgrade page without the adapter, so
			// obtain the full document rather than revalidating that old entity.
			// Clone only this local GET; the caller's request/header/body objects
			// remain untouched, as do range and other precondition semantics.
			originalRequest := c.Request
			c.Request = originalRequest.Clone(originalRequest.Context())
			c.Request.Header.Del("If-None-Match")
			c.Request.Header.Del("If-Modified-Since")
			defer func() { c.Request = originalRequest }()
			original := c.Writer
			writer := newMiddlewareHTMLWriter(original, middlewareHTMLLimit)
			c.Writer = writer
			defer func() {
				// If a downstream handler panics after writing, preserve its
				// original partial response before the outer recovery resumes.
				if writer.Written() && !writer.passthrough {
					_ = writer.forward()
				}
				c.Writer = original
			}()
			c.Next()
			writer.finish(!c.IsAborted())
			return
		}
		kind := middlewareQuotaRoute(c.Request.Method, c.Request.URL.Path)
		if kind == "" {
			c.Next()
			return
		}
		// Only take an immediate immutable publication. Live manager "reads" may
		// wait on core storage under its lock, so neither side of c.Next may query
		// that manager. Missing/stale proof is harmless: skip or queue the captured
		// evidence, and let the isolated worker authoritatively reject stale work.
		startBindings := s.publishedQuotaBindings()
		if len(startBindings) == 0 {
			c.Next()
			return
		}
		requestCapture := &middlewareBoundedBuffer{limit: middlewareRequestLimit}
		if c.Request.Body != nil {
			originalBody := c.Request.Body
			c.Request.Body = &middlewareBodyTee{ReadCloser: originalBody, capture: requestCapture}
			defer func() { c.Request.Body = originalBody }()
		}
		responseCapture := &middlewareBoundedBuffer{limit: middlewareResponseLimit}
		original := c.Writer
		c.Writer = &middlewareResponseTee{ResponseWriter: original, capture: responseCapture}
		defer func() { c.Writer = original }()
		c.Next()
		if c.IsAborted() || original.Status() < 200 || original.Status() >= 300 || requestCapture.overflow || responseCapture.overflow || !middlewareIdentityEncoding(original.Header()) {
			return
		}
		s.observeMiddlewareQuota(c, kind, requestCapture.Bytes(), responseCapture.Bytes(), startBindings)
	}
}

func middlewarePanelEnabled(currentConfig func() *config.Config) bool {
	if currentConfig == nil {
		return false
	}
	cfg := currentConfig()
	return cfg != nil && !cfg.Home.Enabled && !cfg.RemoteManagement.DisableControlPanel
}

func middlewareQuotaRoute(method, path string) string {
	if method == http.MethodPost {
		switch path {
		case "/v0/management/api-call", "/v8/management/requests/api-call":
			return "api-call"
		case "/v0/management/quota/fetch", "/v8/management/credentials/quota/fetch":
			return "fetch"
		case "/v0/management/reset-quota", "/v0/management/quota/reset", "/v8/management/routing/cooldown/reset", "/v8/management/credentials/quota/reset":
			return "reset"
		}
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 5 || (parts[0] != "v0" && parts[0] != "v8") || parts[1] != "management" || parts[2] != "plugins" || parts[3] == "" || parts[4] != "quota" {
		return ""
	}
	if len(parts) == 5 {
		switch method {
		case http.MethodGet, http.MethodPost:
			return "fetch"
		case http.MethodDelete:
			return "reset"
		}
	}
	if len(parts) == 6 && parts[0] == "v0" && parts[5] == "reset" && method == http.MethodPost {
		return "reset"
	}
	return ""
}

func (s *Store) observeMiddlewareQuota(c *gin.Context, kind string, requestBytes, responseBytes []byte, startBindings []QuotaBinding) {
	var request struct {
		AuthIndex      string            `json:"auth_index"`
		AuthIndexCamel string            `json:"authIndex"`
		AuthIndexUpper string            `json:"AuthIndex"`
		URL            string            `json:"url"`
		Header         map[string]string `json:"header"`
		Data           string            `json:"data"`
		Provider       string            `json:"provider"`
	}
	if len(bytes.TrimSpace(requestBytes)) > 0 && json.Unmarshal(requestBytes, &request) != nil {
		return
	}
	index := middlewareFirstString(request.AuthIndex, request.AuthIndexCamel, request.AuthIndexUpper)
	pluginPath := strings.HasPrefix(c.Request.URL.Path, "/v0/management/plugins/") || strings.HasPrefix(c.Request.URL.Path, "/v8/management/plugins/")
	if pluginPath && (c.Request.Method == http.MethodGet || kind == "reset") {
		// Only these existing handlers accept query identity, with query taking
		// precedence. API-call does not: never attribute its anonymous request
		// using an auth_index that the original handler ignored.
		if queryIndex := middlewareFirstString(c.Query("auth_index"), c.Query("authIndex")); queryIndex != "" {
			index = queryIndex
		}
	}
	if index == "" || len(index) > 256 {
		return
	}
	var binding QuotaBinding
	for _, candidate := range startBindings {
		if candidate.AuthIndex == index {
			if binding.AuthIndex != "" {
				return
			}
			binding = candidate
		}
	}
	if binding.AuthIndex == "" || (request.Provider != "" && request.Provider != binding.Provider) {
		return
	}
	for _, alias := range []string{request.AuthIndex, request.AuthIndexCamel, request.AuthIndexUpper} {
		if alias != "" && strings.TrimSpace(alias) != index {
			return
		}
	}
	ctx := c.Request.Context()
	switch kind {
	case "api-call":
		if !quotaAPICallProof(binding, request.URL, request.Header, request.Data) {
			return
		}
		var response struct {
			StatusCode int         `json:"status_code"`
			Header     http.Header `json:"header"`
			Body       string      `json:"body"`
		}
		if request.URL == "" || json.Unmarshal(responseBytes, &response) != nil || response.StatusCode < 100 || response.StatusCode > 599 {
			return
		}
		s.ObserveAPICall(ctx, binding, request.URL, response.StatusCode, response.Header, []byte(response.Body))
	case "fetch":
		response, valid := middlewareFetchResponse(responseBytes)
		if valid {
			s.ObserveQuotaFetch(ctx, binding, response)
		}
	case "reset":
		var response struct {
			Status    string `json:"status"`
			AuthIndex string `json:"auth_index"`
		}
		if json.Unmarshal(responseBytes, &response) != nil || response.Status != "ok" || response.AuthIndex != index {
			return
		}
		s.ObserveQuotaReset(ctx, binding)
	}
}

func middlewareFirstString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func middlewareFetchResponse(data []byte) (pluginapi.QuotaFetchResponse, bool) {
	var response pluginapi.QuotaFetchResponse
	if json.Unmarshal(data, &response) != nil {
		return response, false
	}
	// The SDK's value-type numbers default to zero. Preserve missing-field
	// semantics at this JSON boundary instead of inventing measured quota.
	var presence struct {
		Groups []struct {
			Buckets []map[string]json.RawMessage `json:"buckets"`
		} `json:"groups"`
		Summary []map[string]json.RawMessage `json:"summary"`
	}
	if json.Unmarshal(data, &presence) != nil {
		return pluginapi.QuotaFetchResponse{}, false
	}
	for i := range response.Groups {
		buckets := response.Groups[i].Buckets[:0]
		if i < len(presence.Groups) {
			for j, bucket := range response.Groups[i].Buckets {
				if j >= len(presence.Groups[i].Buckets) {
					continue
				}
				raw := presence.Groups[i].Buckets[j]
				var fraction *float64
				_ = json.Unmarshal(raw["remainingFraction"], &fraction)
				if fraction == nil {
					_ = json.Unmarshal(raw["remaining_fraction"], &fraction)
				}
				if fraction != nil {
					bucket.RemainingFraction = *fraction
					buckets = append(buckets, bucket)
				}
			}
		}
		response.Groups[i].Buckets = buckets
	}
	summary := response.Summary[:0]
	for i, metric := range response.Summary {
		if i >= len(presence.Summary) {
			continue
		}
		var value *float64
		if json.Unmarshal(presence.Summary[i]["value"], &value) == nil && value != nil {
			metric.Value = *value
			summary = append(summary, metric)
		}
	}
	response.Summary = summary
	return response, true
}

// middlewareBoundedBuffer drops its entire observation when the cap is exceeded,
// but always accepts the original byte count so teeing cannot affect I/O.
type middlewareBoundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (capture *middlewareBoundedBuffer) Write(data []byte) (int, error) {
	if !capture.overflow {
		if len(data) > capture.limit-capture.Len() {
			capture.Reset()
			capture.overflow = true
		} else {
			_, _ = capture.Buffer.Write(data)
		}
	}
	return len(data), nil
}

type middlewareBodyTee struct {
	io.ReadCloser
	capture *middlewareBoundedBuffer
}

func (body *middlewareBodyTee) Read(data []byte) (int, error) {
	n, errRead := body.ReadCloser.Read(data)
	_, _ = body.capture.Write(data[:n])
	return n, errRead
}

type middlewareResponseTee struct {
	gin.ResponseWriter
	capture *middlewareBoundedBuffer
}

func (writer *middlewareResponseTee) Write(data []byte) (int, error) {
	n, errWrite := writer.ResponseWriter.Write(data)
	_, _ = writer.capture.Write(data[:n])
	return n, errWrite
}

func (writer *middlewareResponseTee) WriteString(data string) (int, error) {
	n, errWrite := writer.ResponseWriter.WriteString(data)
	_, _ = writer.capture.Write([]byte(data[:n]))
	return n, errWrite
}

func middlewareIdentityEncoding(header http.Header) bool {
	encoding := strings.TrimSpace(header.Get("Content-Encoding"))
	return encoding == "" || strings.EqualFold(encoding, "identity")
}

// middlewareHTMLWriter buffers only the management document. Flush, hijacking,
// non-200 statuses and overflow switch permanently to the original writer.
// Headers are snapshotted on the first logical write just like net/http.
type middlewareHTMLWriter struct {
	gin.ResponseWriter
	body        bytes.Buffer
	limit       int
	status      int
	size        int
	passthrough bool
	header      http.Header
}

func newMiddlewareHTMLWriter(writer gin.ResponseWriter, limit int) *middlewareHTMLWriter {
	return &middlewareHTMLWriter{ResponseWriter: writer, limit: limit, status: writer.Status(), size: -1, passthrough: writer.Written()}
}

func (writer *middlewareHTMLWriter) Status() int {
	if writer.passthrough {
		return writer.ResponseWriter.Status()
	}
	return writer.status
}

func (writer *middlewareHTMLWriter) Size() int {
	if writer.passthrough {
		return writer.ResponseWriter.Size()
	}
	return writer.size
}

func (writer *middlewareHTMLWriter) Written() bool { return writer.Size() >= 0 }

func (writer *middlewareHTMLWriter) WriteHeader(status int) {
	if writer.passthrough {
		writer.ResponseWriter.WriteHeader(status)
		return
	}
	if !writer.Written() && status > 0 {
		writer.status = status
	}
}

func (writer *middlewareHTMLWriter) WriteHeaderNow() {
	if writer.passthrough {
		writer.ResponseWriter.WriteHeaderNow()
		return
	}
	if writer.size < 0 {
		writer.size = 0
		writer.header = writer.Header().Clone()
	}
}

func (writer *middlewareHTMLWriter) Write(data []byte) (int, error) {
	if writer.passthrough {
		return writer.ResponseWriter.Write(data)
	}
	writer.WriteHeaderNow()
	if writer.status != http.StatusOK || len(data) > writer.limit-writer.body.Len() || !middlewareIdentityEncoding(writer.header) || writer.header.Get("Trailer") != "" || writer.header.Get("Content-Range") != "" {
		if errFlush := writer.forward(); errFlush != nil {
			return 0, errFlush
		}
		return writer.ResponseWriter.Write(data)
	}
	n, errWrite := writer.body.Write(data)
	writer.size += n
	return n, errWrite
}

func (writer *middlewareHTMLWriter) WriteString(data string) (int, error) {
	return writer.Write([]byte(data))
}

func (writer *middlewareHTMLWriter) Flush() {
	_ = writer.forward()
	writer.ResponseWriter.Flush()
}

func (writer *middlewareHTMLWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if errFlush := writer.forward(); errFlush != nil {
		return nil, nil, errFlush
	}
	return writer.ResponseWriter.Hijack()
}

func (writer *middlewareHTMLWriter) forward() error {
	if writer.passthrough {
		return nil
	}
	writer.passthrough = true
	if writer.header != nil {
		replaceMiddlewareHeader(writer.Header(), writer.header)
	}
	writer.ResponseWriter.WriteHeader(writer.status)
	if writer.body.Len() > 0 {
		_, errWrite := writer.ResponseWriter.Write(writer.body.Bytes())
		writer.body.Reset()
		return errWrite
	}
	if writer.size >= 0 {
		writer.ResponseWriter.WriteHeaderNow()
	}
	return nil
}

func (writer *middlewareHTMLWriter) finish(allowInjection bool) {
	if writer.passthrough {
		return
	}
	writer.WriteHeaderNow()
	contentType := writer.header.Get("Content-Type")
	if contentType == "" && writer.body.Len() > 0 {
		contentType = http.DetectContentType(writer.body.Bytes())
	}
	mediaType, _, errMediaType := mime.ParseMediaType(contentType)
	if allowInjection && writer.status == http.StatusOK && errMediaType == nil && mediaType == "text/html" && middlewareIdentityEncoding(writer.header) {
		result, hooked := usageweb.InjectManagementHTML(writer.body.Bytes())
		if hooked {
			writer.body.Reset()
			_, _ = writer.body.Write(result)
			writer.header.Set("X-CPA-Quota-Persistence", "enabled")
			writer.header.Set("Content-Length", strconv.Itoa(len(result)))
			writer.header.Set("Cache-Control", "no-store")
			writer.header.Set("Pragma", "no-cache")
			for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest", "Accept-Ranges"} {
				writer.header.Del(name)
			}
		} else {
			writer.header.Set("X-CPA-Quota-Persistence", "unsupported")
		}
	}
	if errWrite := writer.forward(); errWrite != nil {
		log.Debug("usage persistence: management document write failed")
	}
}

func replaceMiddlewareHeader(target, source http.Header) {
	for name := range target {
		delete(target, name)
	}
	for name, values := range source {
		target[name] = append([]string(nil), values...)
	}
}
