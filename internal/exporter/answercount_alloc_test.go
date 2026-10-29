package exporter

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// discardWriter is a ResponseWriter that keeps nothing and allocates
// nothing, so that what an answer through it allocates is the middleware's.
type discardWriter struct{ header http.Header }

func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(int)             {}

// Counting the bytes of an answer sent as it is allocates nothing: the
// writer that counts them is taken from a pool and given back once the
// answer is counted, where one was made for every answer, a small
// allocation on every scrape of /probe, the self-metrics and the static
// targets. At level 0 an answer allocates nothing at all, and at another
// level, to a client that does not take gzip, what the handler, saying
// that the answer varies with Accept-Encoding and reading that header
// allocate, and nothing more.
// The bytes are counted all the same.
func TestCountingAnAnswerSentAsItIsAllocatesNothing(t *testing.T) {
	body := []byte("up 1\n")
	answer := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
		_, _ = w.Write(body)
	}
	for _, level := range []int{0, DefaultGzipLevel} {
		var counter answerCounter
		handler := newAnswerCompression(level).wrap(&counter, answer)
		r := httptest.NewRequest(http.MethodGet, "/probe", nil)
		w := &discardWriter{header: http.Header{}}
		// What the answer allocates without the counting: the handler,
		// and at a level that compresses, the Vary header and the reading
		// of Accept-Encoding.
		alone, _ := alloctest.Allocations(100, func() {
			clear(w.header)
			if level > 0 {
				w.Header().Add("Vary", "Accept-Encoding")
				_ = acceptsGzip(r.Header.Get("Accept-Encoding"))
			}
			answer(w, r)
		})
		run := func() {
			clear(w.header)
			handler(w, r)
		}
		if got := alloctest.AllocsAtMost(100, alone, run); got > alone {
			t.Errorf("level %d: an answer sent as it is allocates %v times through the middleware, and %v without it", level, got, alone)
		}
		if level == 0 && alone != 0 {
			t.Errorf("level 0: the answer alone allocates %v times, want none", alone)
		}
		if written, sent := counter.written.Load(), counter.sent.Load(); written == 0 || written != sent || written%uint64(2*len(body)) != 0 {
			t.Errorf("level %d: %d bytes written and %d sent are counted, want a whole number of answers of %d bytes each way", level, written, sent, 2*len(body))
		}
	}
}
