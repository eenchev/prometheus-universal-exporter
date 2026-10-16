//go:build !select_request_types || request_type_http

package allocator

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log the server's goroutines write and the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) count(text string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), text)
}

// The token file is read again when it changes, so a rotated token is the
// one required from the next request and the old one is refused. A file
// that becomes empty, or goes, refuses every request, a bearer of nothing
// too, and says so in the log once for as long as the reason stays the
// same; a token written back is required again, and the log says the file
// can be used again. A token file that is empty or missing at the start is
// refused.
func TestARotatedTokenFileTakesEffectWithoutARestart(t *testing.T) {
	p := writeInputs(t, testConfig, staticTargets(2), testProbes)
	a, _ := settledAllocator(t, mustLoad(t, p), "r1")
	path := filepath.Join(t.TempDir(), "token")
	write := func(text string) {
		t.Helper()
		// Each write is a second later, as a Secret's rotation is, so the
		// change is seen whatever the file system's time resolution.
		var at time.Time
		if st, err := os.Stat(path); err == nil {
			at = st.ModTime().Add(time.Second)
		} else {
			at = time.Now()
		}
		tmp := path + ".new"
		if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(tmp, at, at); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	write("first\n")
	logs := &lockedBuffer{}
	f, err := NewTokenFile(path, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.HandlerReading(f))
	t.Cleanup(server.Close)
	status := func(token string) int {
		t.Helper()
		resp, _ := get(t, server.URL+"/metrics", "Authorization", "Bearer "+token)
		return resp.StatusCode
	}
	expect := func(when string, want map[string]int) {
		t.Helper()
		for token, code := range want {
			if got := status(token); got != code {
				t.Errorf("%s, the token %q: got %d, want %d", when, token, got, code)
			}
		}
	}
	expect("at the start", map[string]int{"first": 200, "second": 401, "": 401})
	write("second\n")
	expect("rotated", map[string]int{"first": 401, "second": 200})
	// The same size, a later time.
	write("secon2\n")
	expect("rotated to a token of the same length", map[string]int{"second": 401, "secon2": 200})

	write(" \n")
	expect("emptied", map[string]int{"secon2": 401, "": 401, " ": 401})
	expect("still empty", map[string]int{"secon2": 401})
	if n := logs.count("the token file is empty"); n != 1 {
		t.Errorf("the empty token file was logged %d times, want once", n)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expect("removed", map[string]int{"secon2": 401, "": 401})
	expect("still removed", map[string]int{"secon2": 401})
	if n := logs.count("the token file cannot be read"); n != 1 {
		t.Errorf("the missing token file was logged %d times, want once", n)
	}
	write("third\n")
	expect("written back", map[string]int{"third": 200, "secon2": 401})
	if n := logs.count("the token file can be used again"); n != 1 {
		t.Errorf("the token file's return was logged %d times, want once", n)
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), writeFile(t, " \n")} {
		if _, err := NewTokenFile(path, nil); err == nil {
			t.Errorf("a token file %s that holds no token was taken at the start", path)
		}
	}
	if resp, _ := get(t, server.URL+"/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %s", resp.Status)
	}
}

// writeFile writes text into a file of its own and returns its path.
func writeFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
