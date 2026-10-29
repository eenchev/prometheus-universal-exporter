package exporter

import (
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
// does; an answer is text that typically shrinks to a tenth. --web.gzip-level
// sets the level, or turns compression off. Other
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

// DefaultGzipLevel is the level of --web.gzip-level when it is not given:
// gzip.BestSpeed. A probe's answer is compressed on every scrape and read
// once, so the time spent compressing it is spent again on the next scrape,
// while the bytes it saves are a few percent of a level-6 answer: in the
// probe benchmarks (probe_bench_test.go) level 1 compressed in a fifth or less
// of level 6's time, and its answers were at most about a tenth larger.
const DefaultGzipLevel = gzip.BestSpeed

// ValidateGzipLevel refuses a --web.gzip-level that is not 0, for answers
// sent uncompressed, or a gzip level from 1 to 9.
func ValidateGzipLevel(level int) error {
	if level < 0 || level > gzip.BestCompression {
		return fmt.Errorf("--web.gzip-level must be from 0, which answers uncompressed, to 9, got %d", level)
	}
	return nil
}

// answerCompression gzips answers at one level. Its writers are pooled: a
// gzip.Writer holds several hundred kilobytes of tables, and one made for
// every answer was most of what compressing it allocated. Reset keeps a
// writer's level, so every writer in the pool has the one it was made with.
type answerCompression struct {
	// level is from 1 to 9, or 0 for answers sent as they are.
	level   int
	writers sync.Pool
	// counting are the writers that count the bytes of an answer sent as
	// it is (countingResponseWriter), pooled too: one made for every such
	// answer was an allocation of every scrape that compressing nothing
	// did not otherwise make.
	counting sync.Pool
}

// newAnswerCompression makes the compression of --web.gzip-level, which
// ValidateGzipLevel has accepted.
func newAnswerCompression(level int) *answerCompression {
	c := &answerCompression{level: level}
	c.counting.New = func() any { return new(countingResponseWriter) }
	c.writers.New = func() any {
		gz, err := gzip.NewWriterLevel(nil, level)
		if err != nil {
			// Only a level outside 1 to 9 is refused, and the flag's
			// was checked at startup.
			panic(err)
		}
		return gz
	}
	return c
}

// SetGzipLevel sets the gzip level of /probe's, the self-metrics' and the
// static targets' answers, from --web.gzip-level: 1 to 9, or 0 to answer
// uncompressed whatever the client accepts. It is set before Handler.
func (s *Server) SetGzipLevel(level int) { s.compression = newAnswerCompression(level) }

// gzipResponseWriter compresses what a handler writes.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	// bodyless is an answer that may not have a body, 204 or 304, which is
	// passed on as it is.
	bodyless bool
	// written counts what the handler wrote, and sent what went on to the
	// client, the compressed bytes gz writes through sent included.
	written int64
	sent    sentBytes
}

// sentBytes counts what is written through it to the client. It is a field
// of gzipResponseWriter, which hands gz its address, so counting what gzip
// writes allocates nothing more.
type sentBytes struct {
	w io.Writer
	n int64
}

func (s *sentBytes) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.n += int64(n)
	return n, err
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
		n, err := w.sent.Write(p)
		w.written += int64(n)
		return n, err
	}
	n, err := w.gz.Write(p)
	w.written += int64(n)
	return n, err
}

// countingResponseWriter counts what a handler writes to an answer sent as
// it is. It is taken from answerCompression.counting for the answer and
// given back once the answer is counted, the handler having returned.
type countingResponseWriter struct {
	http.ResponseWriter
	n int64
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the connection through this
// writer.
func (w *countingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// wrap gzips a handler's answer for a client that accepts gzip. At level 0
// every answer is sent as it is, which every client accepts, Prometheus
// included; it does not vary with Accept-Encoding, and does not say it does.
//
// It counts every answer in counter, compressed or not, error and refusal
// alike: what the handler wrote, and what was written on to the client. The
// two are counted in the writer the answer has to itself, and added to
// counter once the answer is done, gzip's last bytes included, so a write
// takes no lock and costs no atomic operation; and the self-metrics, which
// are rendered before they are written, show their own answer in the next
// one.
func (c *answerCompression) wrap(counter *answerCounter, next http.HandlerFunc) http.HandlerFunc {
	plain := func(w http.ResponseWriter, r *http.Request) {
		cw := c.counting.Get().(*countingResponseWriter)
		cw.ResponseWriter, cw.n = w, 0
		defer func() {
			counter.add(cw.n, cw.n)
			// Given back without the answer's writer, which the pool
			// would otherwise keep alive.
			*cw = countingResponseWriter{}
			c.counting.Put(cw)
		}()
		next(cw, r)
	}
	if c.level == 0 {
		return plain
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			plain(w, r)
			return
		}
		gz := c.writers.Get().(*gzip.Writer)
		cw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		cw.sent.w = w
		gz.Reset(&cw.sent)
		defer func() {
			if cw.wroteHeader && !cw.bodyless {
				_ = gz.Close()
			}
			gz.Reset(nil)
			c.writers.Put(gz)
			counter.add(cw.written, cw.sent.n)
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

// credentialsMatch reports whether a presented username and password are
// the exporter's own, comparing their SHA-256 digests with want's through
// compare, subtle.ConstantTimeCompare but in a test. Digests are all of one
// length, so the comparison takes the same time whatever the length of what
// was presented and of the credential, which comparing the credentials
// themselves gave away; and both are compared whatever the first found, so
// the time taken does not say which one was wrong.
func credentialsMatch(compare func(x, y []byte) int, username, password string, want config.WebAuthDigest) bool {
	presentedUser := sha256.Sum256([]byte(username))
	presentedPassword := sha256.Sum256([]byte(password))
	userOK := compare(presentedUser[:], want.Username[:])
	passwordOK := compare(presentedPassword[:], want.Password[:])
	return userOK&passwordOK == 1
}

// basicAuthMiddleware authenticates every endpoint but /health and /ready
// with web.basic_auth, and refuses them all with 503 while the manager is
// awaiting its first configuration in allocator mode (allocatormode.go).
// web.basic_auth is part of the bundle, so until one is in force the
// exporter does not know whether it should ask for a credential, and the
// empty configuration it holds asks for none: the landing page, the
// collectors page, the self-metrics, a pasted debug run and /-/reload were
// served to anyone while the allocator could not be reached. Refusing them
// cannot let in a request the bundle would refuse.
func (s *Server) basicAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.awaitingConfiguration(w) {
			return
		}
		auth := s.manager.Get().Web.BasicAuth
		if auth == nil || !auth.Enabled {
			next(w, r)
			return
		}
		want, err := config.WebAuthDigests(auth)
		if err != nil {
			// Logged as a repeated failure is (failurelog.go): every
			// request fails on the file, unauthenticated ones and
			// Prometheus's scrapes alike, and a line for each buried the
			// log. The first is an error, the repeats are at debug
			// level, and the first request that reads the file again
			// logs the recovery.
			s.failures.failed(s.logger, slog.LevelError, webAuthKey, "the exporter's Basic Auth credential cannot be read; refusing every request", "web_auth", err, "path", r.URL.Path)
			http.Error(w, "the exporter's credential cannot be read", http.StatusInternalServerError)
			return
		}
		s.failures.recovered(s.logger, webAuthKey, "the exporter's Basic Auth credential can be read again")
		username, password, ok := r.BasicAuth()
		if matched := credentialsMatch(subtle.ConstantTimeCompare, username, password, want); !ok || !matched {
			w.Header().Set("WWW-Authenticate", `Basic realm="prometheus-universal-exporter"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
