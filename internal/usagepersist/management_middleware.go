package usagepersist

import (
	"bufio"
	"bytes"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	usageweb "github.com/router-for-me/CLIProxyAPI/v8/internal/usagepersist/web"
	log "github.com/sirupsen/logrus"
)

const middlewareHTMLLimit = 16 << 20

// ManagementNavMiddleware augments only the management document with the
// statistics navigation asset. It needs no store and never recognizes the
// upstream build: the asset tolerates unknown sidebar markup, and the dashboard
// stays reachable by URL. Encoded, partial, flushed, and oversized responses
// keep their original bytes.
func ManagementNavMiddleware(currentConfig func() *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || c.Request.URL == nil || c.Request.Method != http.MethodGet || c.Request.URL.Path != "/management.html" || !middlewarePanelEnabled(currentConfig) {
			c.Next()
			return
		}
		// Source-file validators do not describe the augmented representation.
		// A browser may still hold a pre-upgrade page without the asset, so
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
	}
}

func middlewarePanelEnabled(currentConfig func() *config.Config) bool {
	if currentConfig == nil {
		return false
	}
	cfg := currentConfig()
	return cfg != nil && !cfg.Home.Enabled && !cfg.RemoteManagement.DisableControlPanel
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
		result, injected := usageweb.InjectManagementNav(writer.body.Bytes())
		if injected {
			writer.body.Reset()
			_, _ = writer.body.Write(result)
			writer.header.Set("X-CPA-Stats-Nav", "enabled")
			writer.header.Set("Content-Length", strconv.Itoa(len(result)))
			writer.header.Set("Cache-Control", "no-store")
			writer.header.Set("Pragma", "no-cache")
			for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest", "Accept-Ranges"} {
				writer.header.Del(name)
			}
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
