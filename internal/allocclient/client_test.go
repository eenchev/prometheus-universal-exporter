package allocclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/allocapi"
)

// fakeAllocator is the target allocator as the client sees it: it answers
// heartbeats with the generation it holds, and bundle requests with the
// bundle it holds, holding a request that names that generation until it
// holds another or the request ends.
type fakeAllocator struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	bundle allocapi.Bundle
	// changed is closed and replaced when the bundle changes.
	changed chan struct{}
	// failBundle, when set, is the status bundle requests are answered.
	failBundle int
	// noWait answers a request naming the generation held 304 at once.
	noWait bool
	// heartbeats and bundleRequests are what the allocator was sent.
	heartbeats     chan sentHeartbeat
	bundleRequests chan *http.Request
}

// sentHeartbeat is a heartbeat with the request that carried it.
type sentHeartbeat struct {
	allocapi.Heartbeat
	host, authorization string
}

func newFakeAllocator(t *testing.T) *fakeAllocator {
	t.Helper()
	a := &fakeAllocator{t: t, changed: make(chan struct{}), heartbeats: make(chan sentHeartbeat, 256), bundleRequests: make(chan *http.Request, 256)}
	a.server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.server.Close)
	return a
}

func (a *fakeAllocator) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case allocapi.HeartbeatPath:
		var hb allocapi.Heartbeat
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&hb) != nil {
			http.Error(w, "not a heartbeat", http.StatusBadRequest)
			return
		}
		select {
		case a.heartbeats <- sentHeartbeat{Heartbeat: hb, host: r.Host, authorization: r.Header.Get("Authorization")}:
		case <-r.Context().Done():
			return
		}
		a.mu.Lock()
		generation := a.bundle.Generation
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(allocapi.HeartbeatReply{Generation: generation})
	case allocapi.BundlePath:
		select {
		case a.bundleRequests <- r.Clone(context.Background()):
		case <-r.Context().Done():
			return
		}
		for {
			a.mu.Lock()
			b, changed, fail, noWait := a.bundle, a.changed, a.failBundle, a.noWait
			a.mu.Unlock()
			switch {
			case fail != 0:
				http.Error(w, "the allocator is not ready", fail)
				return
			case b.Generation == "":
			case r.Header.Get("If-None-Match") != b.Generation:
				_ = json.NewEncoder(w).Encode(b)
				return
			case noWait:
				w.WriteHeader(http.StatusNotModified)
				return
			}
			select {
			case <-changed:
			case <-r.Context().Done():
				return
			}
		}
	default:
		http.NotFound(w, r)
	}
}

// serveFiles makes files the bundle the allocator holds, and returns its
// generation.
func (a *fakeAllocator) serveFiles(files map[string]string) string {
	b := allocapi.Bundle{Files: files}
	b.Seal()
	a.serveBundle(b)
	return b.Generation
}

// serveBundle makes b, sealed or not, the bundle the allocator holds.
func (a *fakeAllocator) serveBundle(b allocapi.Bundle) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bundle = b
	close(a.changed)
	a.changed = make(chan struct{})
}

// nextHeartbeat is the next heartbeat the allocator hears.
func (a *fakeAllocator) nextHeartbeat(t *testing.T) sentHeartbeat {
	t.Helper()
	select {
	case hb := <-a.heartbeats:
		return hb
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for a heartbeat")
	}
	return sentHeartbeat{}
}

// heartbeatWhere is the first heartbeat from now on that ok accepts.
func (a *fakeAllocator) heartbeatWhere(t *testing.T, what string, ok func(sentHeartbeat) bool) sentHeartbeat {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case hb := <-a.heartbeats:
			if ok(hb) {
				return hb
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a heartbeat %s", what)
		}
	}
}

// nextBundleRequest is the next bundle request the allocator is sent.
func (a *fakeAllocator) nextBundleRequest(t *testing.T) *http.Request {
	t.Helper()
	select {
	case r := <-a.bundleRequests:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for a bundle request")
	}
	return nil
}

// bundleRequestNaming waits for a bundle request whose If-None-Match names
// generation.
func (a *fakeAllocator) bundleRequestNaming(t *testing.T, generation string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case r := <-a.bundleRequests:
			if r.Header.Get("If-None-Match") == generation {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a bundle request naming %s", generation)
		}
	}
}

// fakeManager is the configuration manager as the client uses it. Its
// reload reads <dir>/config.yaml through the cache directory's link, under
// its lock, as the manager's does, and refuses a configuration refuse
// accepts; check is CheckReplacement's verdict.
type fakeManager struct {
	dir     string
	mu      sync.Mutex
	check   func(configPath string) error
	refuse  func(config string) bool
	inForce string
	reloads []string
}

func (f *fakeManager) CheckReplacement(configPath, _ string) error {
	if _, err := os.ReadFile(configPath); err != nil { // #nosec G304 -- a test's file
		return err
	}
	if f.check != nil {
		return f.check(configPath)
	}
	return nil
}

func (f *fakeManager) ReloadReplacing(trigger string, replace func() (func() error, error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	restore, err := replace()
	if err != nil {
		return err
	}
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(f.dir, allocapi.ConfigFile))
		return string(b)
	}
	got := read()
	f.reloads = append(f.reloads, trigger+": "+got)
	if f.refuse != nil && f.refuse(got) {
		if err := restore(); err != nil {
			return err
		}
		f.inForce = read()
		return errors.New("the reload refused " + got)
	}
	f.inForce = got
	return nil
}

func (f *fakeManager) state() (inForce string, reloads []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inForce, append([]string(nil), f.reloads...)
}

// testClient is a client of a fake allocator, with its events.
type testClient struct {
	*Client
	manager *fakeManager
	events  chan Event
	dir     string
}

// startClient runs a client of a with the options adjust changes, until the
// test ends. Its heartbeats are an hour apart: a test has those it sends
// at once, at its start and after a bundle, and asks for others.
func startClient(t *testing.T, a *fakeAllocator, adjust func(*Options)) *testClient {
	t.Helper()
	dir := t.TempDir()
	c := newTestClient(t, a, dir, adjust)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	// The events not waited for are drained, so a client that has more to
	// tell than the test read stops too.
	t.Cleanup(func() {
		cancel()
		for {
			select {
			case <-done:
				return
			case <-c.events:
			}
		}
	})
	return c
}

// newTestClient is startClient without running the client.
func newTestClient(t *testing.T, a *fakeAllocator, dir string, adjust func(*Options)) *testClient {
	t.Helper()
	m := &fakeManager{dir: dir}
	events := make(chan Event, 256)
	o := Options{
		URL: a.server.URL, CacheDir: dir, Member: "pod-0", Advertise: "10.0.0.7:8080", Manager: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), HeartbeatInterval: time.Hour,
		MinBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		Event: func(e Event) { events <- e },
	}
	if adjust != nil {
		adjust(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &testClient{Client: c, manager: m, events: events, dir: dir}
}

// waitFor is the next event of kind, the others before it passed over.
func (c *testClient) waitFor(t *testing.T, kind string) Event {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case e := <-c.events:
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s event", kind)
		}
	}
}

// read is the file at name in the cache directory, through its links, or
// "-" when there is none.
func (c *testClient) read(name string) string {
	b, err := os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(name))) // #nosec G304 -- a test's file
	if err != nil {
		return "-"
	}
	return string(b)
}

// generations lists the generation directories in the cache directory.
func generations(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), genPrefix) {
			out = append(out, strings.TrimPrefix(e.Name(), genPrefix))
		}
	}
	return out
}

// The first bundle is written into the cache directory, 0600 files in 0700
// directories, the manager reloads it with the trigger "allocator", and the
// heartbeat after it says its generation is in force.
func TestTheFirstBundleIsPutInForceAndReportedInTheNextHeartbeat(t *testing.T) {
	a := newFakeAllocator(t)
	c := startClient(t, a, nil)
	first := a.nextHeartbeat(t)
	if first.Member != "pod-0" || first.Advertise != "10.0.0.7:8080" || first.Generation != "" || first.ShuttingDown || first.Refused != nil {
		t.Fatalf("the first heartbeat: %+v", first.Heartbeat)
	}
	if r := a.nextBundleRequest(t); r.URL.Query().Get("member") != "pod-0" || r.URL.Query().Get("wait") != "30s" || r.Header.Get("If-None-Match") != "" {
		t.Fatalf("the first bundle request: %s %v", r.URL, r.Header)
	}
	generation := a.serveFiles(map[string]string{"config.yaml": "collectors: first\n", "collectors/a.yaml": "a\n", "static-targets.yaml": "targets\n"})
	if e := c.waitFor(t, EventInstalled); e.Generation != generation {
		t.Fatalf("installed %q, want %q", e.Generation, generation)
	}
	if inForce, reloads := c.manager.state(); inForce != "collectors: first\n" || len(reloads) != 1 || reloads[0] != "allocator: collectors: first\n" {
		t.Fatalf("in force %q after %q", inForce, reloads)
	}
	if got := c.read("collectors/a.yaml") + c.read("static-targets.yaml"); got != "a\ntargets\n" {
		t.Errorf("the bundle's other files read %q", got)
	}
	hb := a.heartbeatWhere(t, "with the generation", func(hb sentHeartbeat) bool { return hb.Generation != "" })
	if hb.Generation != generation || hb.Refused != nil {
		t.Errorf("the heartbeat after the bundle: %+v", hb.Heartbeat)
	}
	if st := c.Status(); st.Generation != generation || st.LastSync.IsZero() || st.Refused != 0 {
		t.Errorf("status %+v", st)
	}
	err := filepath.WalkDir(filepath.Join(c.dir, genPrefix+generation), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if want := os.FileMode(0o600); d.IsDir() {
			if info.Mode().Perm() != 0o700 {
				t.Errorf("%s is %v, want 0700", path, info.Mode().Perm())
			}
		} else if info.Mode().Perm() != want {
			t.Errorf("%s is %v, want %v", path, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A new generation replaces the files of the one in force: a file it no
// longer has is gone from the cache directory, the directory of the old
// generation is removed, and the bundle request after it names it, so the
// allocator holds the request until it has another.
func TestANewGenerationReplacesTheFilesOfTheOld(t *testing.T) {
	a := newFakeAllocator(t)
	c := startClient(t, a, nil)
	first := a.serveFiles(map[string]string{"config.yaml": "one\n", "static-targets.yaml": "targets\n", "old/a.yaml": "a\n"})
	c.waitFor(t, EventInstalled)
	second := a.serveFiles(map[string]string{"config.yaml": "two\n", "new/b.yaml": "b\n"})
	if e := c.waitFor(t, EventInstalled); e.Generation != second {
		t.Fatalf("installed %q, want %q", e.Generation, second)
	}
	if got := c.read("config.yaml") + c.read("static-targets.yaml") + c.read("old/a.yaml") + c.read("new/b.yaml"); got != "two\n--b\n" {
		t.Errorf("after the second bundle the files read %q", got)
	}
	if got := generations(t, c.dir); len(got) != 1 || got[0] != second {
		t.Errorf("generation directories %v, want only %s", got, second)
	}
	if inForce, _ := c.manager.state(); inForce != "two\n" {
		t.Errorf("in force %q", inForce)
	}
	for {
		r := a.nextBundleRequest(t)
		if r.Header.Get("If-None-Match") == second {
			break
		}
		if got := r.Header.Get("If-None-Match"); got != "" && got != first {
			t.Fatalf("a bundle request named %q", got)
		}
	}
	if CachedGeneration(c.dir) != second {
		t.Errorf("the cache directory holds %q", CachedGeneration(c.dir))
	}
}

// A bundle the manager's check refuses is not put in place: the files in
// force stay, its directory is removed, the refusal is counted and reported
// in the next heartbeat, and the next bundle request names the refused
// generation, so it is not fetched again until the allocator has another.
// A later bundle that loads clears the refusal.
func TestABundleTheCheckRefusesKeepsTheFilesInForceAndIsReported(t *testing.T) {
	a := newFakeAllocator(t)
	c := startClient(t, a, nil)
	c.manager.check = func(configPath string) error {
		if b, _ := os.ReadFile(configPath); string(b) == "broken\n" { // #nosec G304 -- a test's file
			return errors.New("collectors must not be empty")
		}
		return nil
	}
	good := a.serveFiles(map[string]string{"config.yaml": "good\n"})
	c.waitFor(t, EventInstalled)
	bad := a.serveFiles(map[string]string{"config.yaml": "broken\n", "extra.yaml": "x\n"})
	if e := c.waitFor(t, EventRefused); e.Generation != bad || !strings.Contains(e.Err.Error(), "collectors must not be empty") {
		t.Fatalf("refused: %+v", e)
	}
	if got := c.read("config.yaml") + c.read("extra.yaml"); got != "good\n-" {
		t.Errorf("after the refusal the files read %q", got)
	}
	if got := generations(t, c.dir); len(got) != 1 || got[0] != good {
		t.Errorf("generation directories %v, want only %s", got, good)
	}
	hb := a.heartbeatWhere(t, "with the refusal", func(hb sentHeartbeat) bool { return hb.Refused != nil })
	if hb.Generation != good || hb.Refused.Generation != bad || !strings.Contains(hb.Refused.Error, "collectors must not be empty") {
		t.Errorf("the heartbeat after the refusal: %+v %+v", hb.Heartbeat, hb.Refused)
	}
	if st := c.Status(); st.Generation != good || st.Refused != 1 {
		t.Errorf("status %+v", st)
	}
	a.bundleRequestNaming(t, bad)
	if _, reloads := c.manager.state(); len(reloads) != 1 {
		t.Errorf("the refused bundle was reloaded: %q", reloads)
	}
	fixed := a.serveFiles(map[string]string{"config.yaml": "fixed\n"})
	c.waitFor(t, EventInstalled)
	hb = a.heartbeatWhere(t, "with the fixed bundle", func(hb sentHeartbeat) bool { return hb.Generation == fixed })
	if hb.Refused != nil {
		t.Errorf("the refusal is still reported: %+v", hb.Refused)
	}
}

// A bundle the reload refuses, although the check let it through, is swapped
// back: the files of the generation before are in place again, its
// directory is kept and the refused one's removed, and the refusal is
// reported.
func TestABundleTheReloadRefusesIsSwappedBack(t *testing.T) {
	a := newFakeAllocator(t)
	c := startClient(t, a, nil)
	c.manager.refuse = func(config string) bool { return config == "refused\n" }
	good := a.serveFiles(map[string]string{"config.yaml": "good\n", "keep/a.yaml": "a\n"})
	c.waitFor(t, EventInstalled)
	bad := a.serveFiles(map[string]string{"config.yaml": "refused\n", "other.yaml": "o\n"})
	if e := c.waitFor(t, EventRefused); e.Generation != bad {
		t.Fatalf("refused %+v", e)
	}
	if got := c.read("config.yaml") + c.read("keep/a.yaml") + c.read("other.yaml"); got != "good\na\n-" {
		t.Errorf("after the refused reload the files read %q", got)
	}
	if CachedGeneration(c.dir) != good {
		t.Errorf("the cache directory holds %q, want %q", CachedGeneration(c.dir), good)
	}
	if got := generations(t, c.dir); len(got) != 1 || got[0] != good {
		t.Errorf("generation directories %v", got)
	}
	if inForce, reloads := c.manager.state(); inForce != "good\n" || len(reloads) != 2 {
		t.Errorf("in force %q after %q", inForce, reloads)
	}
	if hb := a.heartbeatWhere(t, "with the refusal", func(hb sentHeartbeat) bool { return hb.Refused != nil }); hb.Refused.Generation != bad || hb.Generation != good {
		t.Errorf("heartbeat %+v", hb.Heartbeat)
	}
}

// A bundle whose files could leave the cache directory, or take a name the
// cache directory keeps for itself, that has no configuration, or whose
// generation is not its files' digest, is refused before anything is
// written.
func TestABundleThatCouldLeaveTheCacheDirectoryIsRefusedUnwritten(t *testing.T) {
	for name, b := range map[string]allocapi.Bundle{
		"a parent":            {Files: map[string]string{"config.yaml": "c\n", "../escape.yaml": "x"}},
		"an absolute path":    {Files: map[string]string{"config.yaml": "c\n", "/etc/escape.yaml": "x"}},
		"the cache's own":     {Files: map[string]string{"config.yaml": "c\n", "..data/config.yaml": "x"}},
		"a generation's name": {Files: map[string]string{"config.yaml": "c\n", "..gen-0/x": "x"}},
		"no configuration":    {Files: map[string]string{"collectors/a.yaml": "x"}},
		"a wrong generation":  {Generation: "0123456789abcdef0123456789abcdef", Files: map[string]string{"config.yaml": "c\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			a := newFakeAllocator(t)
			c := startClient(t, a, nil)
			if b.Generation == "" {
				b.Seal()
			}
			a.serveBundle(b)
			c.waitFor(t, EventRefused)
			entries, err := os.ReadDir(c.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("the cache directory holds %d entries after the refusal", len(entries))
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(c.dir), "escape.yaml")); err == nil {
				t.Error("a file was written outside the cache directory")
			}
		})
	}
}

// Whoever reads the cache directory finds the files of one generation: at
// every change the swap makes, the files read through the cache directory's
// links, and through ..data, are all of the generation before or all of the
// one after, and the configuration is always there.
func TestAReaderNeverFindsFilesOfTwoGenerations(t *testing.T) {
	a := newFakeAllocator(t)
	dir := t.TempDir()
	names := []string{"config.yaml", "shared/a.yaml", "only-1.yaml", "only-2.yaml", "only-3.yaml"}
	bundle := func(n int) *allocapi.Bundle {
		g := strconv.Itoa(n)
		b := &allocapi.Bundle{Files: map[string]string{"config.yaml": g, "shared/a.yaml": g, "only-" + g + ".yaml": g}}
		b.Seal()
		return b
	}
	var checked int
	var c *testClient
	var installed bool
	c = newTestClient(t, a, dir, func(o *Options) {
		o.Step = func(change string) {
			if !installed {
				return
			}
			checked++
			seen := map[string]bool{}
			for _, root := range []string{dir, filepath.Join(dir, dataLink)} {
				for _, name := range names {
					if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))); err == nil { // #nosec G304 -- a test's file
						seen[string(b)] = true
					} else if name == "config.yaml" {
						t.Errorf("after %q the configuration cannot be read: %v", change, err)
					}
				}
			}
			if len(seen) != 1 {
				t.Errorf("after %q the cache directory holds files of generations %v", change, seen)
			}
		}
	})
	for n := 1; n <= 3; n++ {
		if refusal, err := c.apply(bundle(n)); refusal != nil || err != nil {
			t.Fatal(refusal, err)
		}
		installed = true
		if got := c.read("config.yaml"); got != strconv.Itoa(n) {
			t.Fatalf("generation %d: the configuration reads %q", n, got)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d changes were looked at", checked)
	}
}

// redirect is a transport that records each request's address and Host
// header, and sends it to the one allocator there is.
type redirect struct {
	to   string
	mu   sync.Mutex
	sent []string
}

func (r *redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.sent = append(r.sent, req.Method+" "+req.URL.Host+" Host:"+req.Host)
	r.mu.Unlock()
	req = req.Clone(req.Context())
	req.URL.Host = r.to
	return http.DefaultTransport.RoundTrip(req)
}

// A heartbeat goes to every address the allocator URL's host name resolves
// to, at the URL's port, each with the URL's host as its Host header, so two
// copies of the allocator behind one name both hear it; the bundle request
// goes to the URL as it is.
func TestHeartbeatsGoToEveryAddressTheHostResolvesTo(t *testing.T) {
	a := newFakeAllocator(t)
	to := strings.TrimPrefix(a.server.URL, "http://")
	transport := &redirect{to: to}
	var asked []string
	c := newTestClient(t, a, t.TempDir(), func(o *Options) {
		o.URL = "http://pue-allocator.monitoring.svc:8080"
		o.Transport = transport
		o.Resolve = func(_ context.Context, host string) ([]string, error) {
			asked = append(asked, host)
			return []string{"10.1.0.4", "10.1.0.5", "fd00::6"}, nil
		}
	})
	c.heartbeat(context.Background())
	got := map[string]bool{}
	for range 3 {
		hb := a.nextHeartbeat(t)
		got[hb.host] = true
	}
	if len(got) != 1 || !got["pue-allocator.monitoring.svc:8080"] {
		t.Errorf("Host headers %v", got)
	}
	if len(asked) != 1 || asked[0] != "pue-allocator.monitoring.svc" {
		t.Errorf("resolved %v", asked)
	}
	want := map[string]bool{"POST 10.1.0.4:8080 Host:pue-allocator.monitoring.svc:8080": true, "POST 10.1.0.5:8080 Host:pue-allocator.monitoring.svc:8080": true, "POST [fd00::6]:8080 Host:pue-allocator.monitoring.svc:8080": true}
	transport.mu.Lock()
	sent := append([]string(nil), transport.sent...)
	transport.mu.Unlock()
	if len(sent) != 3 {
		t.Fatalf("sent %v", sent)
	}
	for _, s := range sent {
		if !want[s] {
			t.Errorf("sent %q", s)
		}
	}
	transport.mu.Lock()
	transport.sent = nil
	transport.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = c.fetch(ctx) }()
	a.nextBundleRequest(t)
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.sent) != 1 || transport.sent[0] != "GET pue-allocator.monitoring.svc:8080 Host:pue-allocator.monitoring.svc:8080" {
		t.Errorf("the bundle request went %v", transport.sent)
	}
}

// An allocator URL whose host is an IP address is heartbeated there alone,
// without a lookup.
func TestAnIPHostIsHeartbeatedAlone(t *testing.T) {
	a := newFakeAllocator(t)
	c := newTestClient(t, a, t.TempDir(), func(o *Options) {
		o.Resolve = func(context.Context, string) ([]string, error) {
			t.Error("an IP address was looked up")
			return nil, errors.New("no")
		}
	})
	c.heartbeat(context.Background())
	if hb := a.nextHeartbeat(t); hb.host != strings.TrimPrefix(a.server.URL, "http://") {
		t.Errorf("Host %q", hb.host)
	}
	select {
	case hb := <-a.heartbeats:
		t.Errorf("a second heartbeat: %+v", hb)
	default:
	}
}

// With --allocator.token-file every request carries its token as a bearer
// token, read again for each, so a rotated token is sent from the next
// request on; without it none carries an Authorization header.
func TestTheBearerTokenIsReadAgainForEveryRequest(t *testing.T) {
	a := newFakeAllocator(t)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, a, t.TempDir(), func(o *Options) { o.TokenFile = token })
	c.heartbeat(context.Background())
	if hb := a.nextHeartbeat(t); hb.authorization != "Bearer first" {
		t.Errorf("Authorization %q", hb.authorization)
	}
	if err := os.WriteFile(token, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.heartbeat(context.Background())
	if hb := a.nextHeartbeat(t); hb.authorization != "Bearer second" {
		t.Errorf("after the rotation Authorization %q", hb.authorization)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = c.fetch(ctx) }()
	if r := a.nextBundleRequest(t); r.Header.Get("Authorization") != "Bearer second" {
		t.Errorf("the bundle request's Authorization %q", r.Header.Get("Authorization"))
	}
	plain := newTestClient(t, a, t.TempDir(), nil)
	plain.heartbeat(context.Background())
	if hb := a.nextHeartbeat(t); hb.authorization != "" {
		t.Errorf("without a token file Authorization %q", hb.authorization)
	}
}

// ShuttingDown sends a heartbeat saying so at once, before it returns, and
// every heartbeat after it says so too.
func TestShuttingDownHeartbeatsAtOnce(t *testing.T) {
	a := newFakeAllocator(t)
	c := startClient(t, a, nil)
	if hb := a.nextHeartbeat(t); hb.ShuttingDown {
		t.Fatal("the first heartbeat says the exporter is shutting down")
	}
	c.ShuttingDown(context.Background())
	select {
	case hb := <-a.heartbeats:
		if !hb.ShuttingDown || hb.Member != "pod-0" {
			t.Errorf("the heartbeat at the shutdown: %+v", hb.Heartbeat)
		}
	default:
		t.Fatal("ShuttingDown returned before its heartbeat was heard")
	}
	c.heartbeat(context.Background())
	if hb := a.nextHeartbeat(t); !hb.ShuttingDown {
		t.Error("a heartbeat after ShuttingDown does not say so")
	}
}

// An allocator that answers a bundle request without a change at once, as
// one that does not hold requests would, is not asked again at once: each
// next request waits the least backoff, so the loop never spins.
func TestAnAnswerWithoutAChangeIsNotAskedAgainAtOnce(t *testing.T) {
	a := newFakeAllocator(t)
	a.noWait = true
	const least = 20 * time.Millisecond
	unchanged := make(chan time.Time, 256)
	startClient(t, a, func(o *Options) {
		o.MinBackoff = least
		o.Event = func(e Event) {
			if e.Kind == EventUnchanged {
				unchanged <- time.Now()
			}
		}
	})
	a.serveFiles(map[string]string{"config.yaml": "c\n"})
	next := func() time.Time {
		select {
		case at := <-unchanged:
			return at
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for an answer without a change")
		}
		return time.Time{}
	}
	last := next()
	for range 4 {
		at := next()
		if gap := at.Sub(last); gap < least {
			t.Errorf("an answer without a change was asked for again after %s", gap)
		}
		last = at
	}
}

// A bundle request that fails, and a bundle that cannot be written into the
// cache directory, are counted by stage and tried again.
func TestFailuresAreCountedByStageAndTriedAgain(t *testing.T) {
	a := newFakeAllocator(t)
	a.failBundle = http.StatusServiceUnavailable
	c := startClient(t, a, nil)
	c.waitFor(t, EventError)
	c.waitFor(t, EventError)
	if st := c.Status(); st.SyncErrors[StageFetch] < 2 || st.SyncErrors[StageWrite] != 0 {
		t.Errorf("status %+v", st)
	}
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := newFakeAllocator(t)
	b.serveFiles(map[string]string{"config.yaml": "c\n"})
	w := startClient(t, b, func(o *Options) { o.CacheDir = notADirectory })
	w.waitFor(t, EventError)
	w.waitFor(t, EventError)
	if st := w.Status(); st.SyncErrors[StageWrite] < 2 || st.Generation != "" {
		t.Errorf("status %+v", st)
	}
	down := newTestClient(t, a, t.TempDir(), func(o *Options) { o.URL = "http://127.0.0.1:1" })
	down.heartbeat(context.Background())
	if st := down.Status(); st.SyncErrors[StageHeartbeat] != 1 {
		t.Errorf("status after a heartbeat nobody heard: %+v", st)
	}
}

// A client made with the generation found in the cache directory, as a
// restart is, says it in its first heartbeat and names it in its first
// bundle request, so the allocator holds it rather than sending the same
// bundle again.
func TestARestartResumesFromTheCacheDirectory(t *testing.T) {
	a := newFakeAllocator(t)
	c := startClient(t, a, nil)
	generation := a.serveFiles(map[string]string{"config.yaml": "c\n"})
	c.waitFor(t, EventInstalled)
	again := newTestClient(t, a, c.dir, func(o *Options) { o.Generation = CachedGeneration(c.dir) })
	if again.Status().Generation != generation {
		t.Fatalf("resumed with %q, want %q", again.Status().Generation, generation)
	}
	again.heartbeat(context.Background())
	if hb := a.heartbeatWhere(t, "from the restart", func(hb sentHeartbeat) bool { return hb.Generation == generation }); hb.Refused != nil {
		t.Errorf("heartbeat %+v", hb.Heartbeat)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = again.fetch(ctx) }()
	a.bundleRequestNaming(t, generation)
}

// --allocator.url is an http URL with a host and nothing after its path.
func TestTheAllocatorURLMustBeAPlainHTTPURL(t *testing.T) {
	for _, ok := range []string{"http://pue-allocator:8080", "http://10.0.0.1:8080/prefix", "http://[fd00::1]:8080"} {
		if _, err := ParseURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "pue-allocator:8080", "https://pue-allocator", "http://", "http://u:p@a:1", "http://a:1/?x=1", "http://a:1/#f", "ftp://a"} {
		if _, err := ParseURL(bad); err == nil || !strings.Contains(err.Error(), "--allocator.url") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
