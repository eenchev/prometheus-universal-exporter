package exporter

import (
	"compress/gzip"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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

// AnswerWriteTimeout is how long a client has to read an answer, from the
// moment the exporter starts writing it.
//
// Nothing else bounds the write. An answer can be megabytes — a passed-through
// exposition, the static targets, the verbose self-metrics, a debug report —
// and a client that asks for one and stops reading would hold its handler,
// and the rendered answer with it, for as long as it kept the connection
// open: the server has no write timeout (lifecycle.go), and a probe's budget
// ends with the trip to the target. Prometheus reads an answer as fast as the
// network carries it, and gives up at its scrape timeout in any case.
const AnswerWriteTimeout = 30 * time.Second

// answerWriter sets the connection's write deadline when the first byte of an
// answer is written, status line included, so the time a handler took to make
// the answer is not counted against the client reading it.
type answerWriter struct {
	http.ResponseWriter
	timeout time.Duration
	started bool
	// dropped is called once, when a write fails because the deadline
	// passed.
	dropped func()
	failed  bool
}

func (w *answerWriter) start() {
	if w.started {
		return
	}
	w.started = true
	// A writer without a connection, as a test's recorder is, has no
	// deadline to set and nothing that could block.
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout))
}

func (w *answerWriter) WriteHeader(code int) {
	w.start()
	w.ResponseWriter.WriteHeader(code)
}

func (w *answerWriter) Write(p []byte) (int, error) {
	w.start()
	n, err := w.ResponseWriter.Write(p)
	if err != nil && !w.failed && errors.Is(err, os.ErrDeadlineExceeded) {
		w.failed = true
		w.dropped()
	}
	return n, err
}

// Unwrap lets http.ResponseController reach the connection through this
// writer.
func (w *answerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// writeBounded gives the client of every answer next writes
// AnswerWriteTimeout to read it. A client that has not is dropped: the write
// fails, net/http closes the connection, which cannot carry another answer
// after half of this one, and the handler returns. It is no failure of the
// exporter's or of a target's, so it is logged at debug level only. The
// deadline is the answer's alone: net/http clears it before the connection's
// next request.
func (s *Server) writeBounded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&answerWriter{ResponseWriter: w, timeout: s.answerWriteTimeout, dropped: func() {
			s.logger.Debug("a client did not read its answer in time; its connection is closed", "path", r.URL.Path, "remote_address", r.RemoteAddr, "write_timeout", s.answerWriteTimeout.String())
		}}, r)
	})
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
