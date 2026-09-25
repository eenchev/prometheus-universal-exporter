package exporter

import (
	"compress/gzip"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
)

// Prometheus asks for gzip on every scrape, and a probe answer can be large: a
// passed-through target's whole exposition, or the verbose self-metrics with a
// series per request. The answers of /probe, the self-metrics path and the
// static targets path are gzipped for a client that accepts it, as Prometheus's own client library
// does; an answer is text that typically shrinks to a tenth. Other
// endpoints answer a line or two, and /health and /ready are read by kubelets
// that do not ask.

// acceptsGzip reports whether an Accept-Encoding header allows gzip: named,
// or covered by *, with a quality above zero. gzip or x-gzip named decides,
// whatever * says, as RFC 9110 has a coding named override *: gzip;q=0, *
// refuses gzip.
func acceptsGzip(header string) bool {
	named, wildcard := -1.0, -1.0
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding != "gzip" && coding != "x-gzip" && coding != "*" {
			continue
		}
		quality := 1.0
		for _, param := range strings.Split(params, ";") {
			name, value, found := strings.Cut(strings.TrimSpace(param), "=")
			if found && strings.EqualFold(strings.TrimSpace(name), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = q
				}
			}
		}
		if coding == "*" {
			wildcard = max(wildcard, quality)
		} else {
			named = max(named, quality)
		}
	}
	if named >= 0 {
		return named > 0
	}
	return wildcard > 0
}

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// gzipResponseWriter compresses what a handler writes.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	// bodyless is an answer that may not have a body, 204 or 304, which is
	// passed on as it is.
	bodyless bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.bodyless = code == http.StatusNoContent || code == http.StatusNotModified
		if !w.bodyless {
			h := w.Header()
			h.Del("Content-Length")
			h.Set("Content-Encoding", "gzip")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		// net/http would sniff the type from the compressed bytes, so it is
		// sniffed here from the plain ones.
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(p))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.bodyless {
		return w.ResponseWriter.Write(p)
	}
	return w.gz.Write(p)
}

// compressed gzips a handler's answer for a client that accepts gzip.
func compressed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next(w, r)
			return
		}
		gz := gzipWriters.Get().(*gzip.Writer)
		gz.Reset(w)
		cw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			if cw.wroteHeader && !cw.bodyless {
				_ = gz.Close()
			}
			gz.Reset(nil)
			gzipWriters.Put(gz)
		}()
		next(cw, r)
	}
}

func (s *Server) basicAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := s.manager.Get().Web.BasicAuth
		if auth == nil || !auth.Enabled {
			next(w, r)
			return
		}
		wantUser, wantPassword, err := config.WebAuthCredentials(auth)
		if err != nil {
			s.logger.Error("the exporter's Basic Auth credential cannot be read; refusing the request", "path", r.URL.Path, "error", err)
			http.Error(w, "the exporter's credential cannot be read", http.StatusInternalServerError)
			return
		}
		username, password, ok := r.BasicAuth()
		// Both comparisons run whatever the first found, so the time taken
		// does not say which one failed.
		userOK := subtle.ConstantTimeCompare([]byte(username), []byte(wantUser))
		passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(wantPassword))
		if !ok || userOK&passwordOK != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="prometheus-universal-exporter"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
