//go:build !select_request_types || request_type_localfile

package fetch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// How reads behave when the file changes under them, grows, or the filesystem
// stops answering. afterLocalFileRead stands in for the writer or the stalled
// filesystem.

const promFile = "# HELP app_jobs_total Jobs run.\n# TYPE app_jobs_total counter\napp_jobs_total{queue=\"default\"} 7\n"

// onFileRead runs hook after every file read until the test ends.
func onFileRead(t *testing.T, hook func(full string)) {
	t.Helper()
	afterLocalFileRead.Store(&hook)
	t.Cleanup(func() { afterLocalFileRead.Store(nil) })
}

// validated fills in c's request defaults, as loading the configuration does.
func validated(t *testing.T, c model.Collector) *model.Collector {
	t.Helper()
	if err := ValidateRequest(&c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// A file that changes under the read is read again; one that keeps changing
// is refused, rather than exporting a torn write.
func TestLocalFileChangedWhileRead(t *testing.T) {
	root := t.TempDir()
	file := testutil.WriteIn(t, root, "app.prom", promFile)
	c := validated(t, fileCollector("files", root, "app.prom"))

	reads := 0
	onFileRead(t, func(string) {
		reads++
		if reads == 1 {
			// The writer rewrites the file in place during the first read.
			testutil.WriteIn(t, root, "app.prom", strings.Replace(promFile, "7", "8", 1)+"# more\n")
		}
	})
	resp, err := fetchLocalFile(context.Background(), "", c, RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), "} 8") {
		t.Fatalf("body=%q", resp.Body)
	}
	if reads != 2 {
		t.Fatalf("read %d times, want 2", reads)
	}

	onFileRead(t, func(string) {
		reads++
		later := time.Now().Add(time.Duration(reads) * time.Second)
		if err := os.Chtimes(file, later, later); err != nil {
			t.Error(err)
		}
	})
	_, err = fetchLocalFile(context.Background(), "", c, RequestOverrides{}, nil)
	if err == nil || !strings.Contains(err.Error(), "changed while it was read") || !strings.Contains(err.Error(), "rename it into place") {
		t.Fatalf("err=%v", err)
	}
}

// A read that does not return in time — a hung network filesystem — does not
// hold the scrape past its deadline.
func TestLocalFileTimeout(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "app.prom", promFile)
	c := validated(t, fileCollector("files", root, "app.prom"))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	onFileRead(t, func(string) { <-release })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := fetchLocalFile(ctx, "", c, RequestOverrides{}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the read took %s", elapsed)
	}
}

// Reads left running on a filesystem that has stopped answering are capped per
// collector: once the cap of reads has been abandoned by probes that gave up
// on them, a read fails at once instead of adding another goroutine, and the
// places come back as the reads return.
func TestLocalFileAbandonedReadsAreCapped(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= localFileMaxAbandonedReads; i++ {
		testutil.WriteIn(t, root, "f"+strconv.Itoa(i)+".prom", promFile)
	}
	stuck := validated(t, fileCollector("stuck", root, ""))
	release := make(chan struct{})
	onFileRead(t, func(string) { <-release })
	read := func(file string, timeout time.Duration) (*HTTPResponse, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return fetchLocalFile(ctx, file, stuck, RequestOverrides{}, nil)
	}

	// Different files, as probes that do not share one read would ask for.
	for i := 0; i < localFileMaxAbandonedReads; i++ {
		if _, err := read("f"+strconv.Itoa(i)+".prom", 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read %d: err=%v", i, err)
		}
	}
	if got := localFileReads.abandoned("stuck"); got != localFileMaxAbandonedReads {
		t.Fatalf("abandoned=%d, want %d", got, localFileMaxAbandonedReads)
	}
	start := time.Now()
	if _, err := read("f"+strconv.Itoa(localFileMaxAbandonedReads)+".prom", 5*time.Second); err == nil || !strings.Contains(err.Error(), "already has 4 file reads that have not returned") || !strings.Contains(err.Error(), "the filesystem under request.root is not answering") {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a read over the cap waited instead of failing at once")
	}
	// The cap is per collector.
	if got := localFileReads.abandoned("other"); got != 0 {
		t.Fatalf("other collector abandoned=%d", got)
	}

	close(release)
	waitForNoAbandonedReads(t, "stuck")
	afterLocalFileRead.Store(nil)
	resp, err := read("f0.prom", 5*time.Second)
	if err != nil || !strings.Contains(string(resp.Body), "} 7") {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

// waitForNoAbandonedReads waits until every abandoned read of a collector has
// returned.
func waitForNoAbandonedReads(t *testing.T, collector string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for localFileReads.abandoned(collector) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("abandoned reads never returned: %d", localFileReads.abandoned(collector))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// readsHeldTogether holds every file read until want of them are in progress
// at the same moment, and reports whether that many ever were: the proof that
// the reads ran side by side rather than one after another.
func readsHeldTogether(t *testing.T, want int) (together func() bool) {
	t.Helper()
	var mu sync.Mutex
	held := 0
	all := make(chan struct{})
	onFileRead(t, func(string) {
		mu.Lock()
		held++
		if held == want {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(5 * time.Second):
		}
	})
	return func() bool {
		select {
		case <-all:
			return true
		default:
			return false
		}
	}
}

// Probes of a healthy filesystem are never refused for being many: reads whose
// probe is still waiting for them do not count against the cap, which is for
// the reads a probe has given up on. Three times the cap of different files
// are read at the same moment, and every probe is answered.
func TestLocalFileConcurrentReadsOfAHealthyFilesystemAreNotRefused(t *testing.T) {
	root := t.TempDir()
	const probes = 3 * localFileMaxAbandonedReads
	for i := range probes {
		testutil.WriteIn(t, root, "f"+strconv.Itoa(i)+".prom", promFile)
	}
	c := validated(t, fileCollector("busy", root, ""))
	together := readsHeldTogether(t, probes)
	errs := make(chan error, probes)
	for i := range probes {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resp, err := fetchLocalFile(ctx, "f"+strconv.Itoa(i)+".prom", c, RequestOverrides{}, nil)
			if err == nil && !strings.Contains(string(resp.Body), "} 7") {
				err = errors.New("the file was not answered: " + string(resp.Body))
			}
			errs <- err
		}()
	}
	for range probes {
		if err := <-errs; err != nil {
			t.Errorf("a probe of a healthy filesystem failed: %v", err)
		}
	}
	if !together() {
		t.Errorf("the %d reads were never in progress together", probes)
	}
	// A read that returned to a waiting probe was never abandoned.
	if got := localFileReads.abandoned("busy"); got != 0 {
		t.Errorf("abandoned=%d after every read returned to its probe", got)
	}
}

// The same holds for a collector reading directories: each directory read is
// one read, many at once are all answered, and directory reads their probes
// gave up on are capped as file reads are.
func TestLocalDirectoryReadsCountOnlyWhenAbandoned(t *testing.T) {
	root := t.TempDir()
	const probes = 3 * localFileMaxAbandonedReads
	for i := range probes {
		testutil.WriteIn(t, filepath.Join(root, "d"+strconv.Itoa(i)), "app.prom", promFile)
	}
	c := validated(t, dirCollector("busydir", root, "*.prom"))
	read := func(dir string, timeout time.Duration) (*HTTPResponse, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return fetchLocalFile(ctx, dir, c, RequestOverrides{}, nil)
	}
	together := readsHeldTogether(t, probes)
	errs := make(chan error, probes)
	for i := range probes {
		go func() {
			resp, err := read("d"+strconv.Itoa(i), 10*time.Second)
			switch {
			case err != nil:
			case resp.Directory == nil || len(resp.Directory.Files) != 1:
				err = errors.New("the directory's one file was not answered")
			default:
				err = resp.Directory.Files[0].Err
			}
			errs <- err
		}()
	}
	for range probes {
		if err := <-errs; err != nil {
			t.Errorf("a probe of a healthy filesystem failed: %v", err)
		}
	}
	if !together() {
		t.Errorf("the %d directory reads were never in progress together", probes)
	}
	if got := localFileReads.abandoned("busydir"); got != 0 {
		t.Fatalf("abandoned=%d after every read returned to its probe", got)
	}

	// The filesystem stops answering: each probe's deadline ends with its
	// directory read still running, and the cap is reached.
	release := make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	onFileRead(t, func(string) { <-release })
	for i := range localFileMaxAbandonedReads {
		resp, err := read("d"+strconv.Itoa(i), 200*time.Millisecond)
		if err != nil || !errors.Is(fileErrors(t, resp)["app.prom"], context.DeadlineExceeded) {
			t.Fatalf("read %d: resp=%v err=%v", i, resp, err)
		}
	}
	if got := localFileReads.abandoned("busydir"); got != localFileMaxAbandonedReads {
		t.Fatalf("abandoned=%d, want %d", got, localFileMaxAbandonedReads)
	}
	start := time.Now()
	if _, err := read("d"+strconv.Itoa(localFileMaxAbandonedReads), 5*time.Second); err == nil || !strings.Contains(err.Error(), "already has 4 file reads that have not returned") {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a directory read over the cap waited instead of failing at once")
	}
}

// fileErrors maps each file of a directory read to its error, nil for one
// read.
func fileErrors(t *testing.T, resp *HTTPResponse) map[string]error {
	t.Helper()
	if resp.Directory == nil {
		t.Fatal("not a directory read")
	}
	errs := map[string]error{}
	for _, f := range resp.Directory.Files {
		errs[f.Name] = f.Err
	}
	return errs
}

// A file over the per-file limit, or past the scrape's total, is refused
// without being read, and fails alone.
func TestLocalDirectorySizeLimits(t *testing.T) {
	root := t.TempDir()
	small := "# TYPE v gauge\nv 1\n"
	testutil.WriteIn(t, root, "a.prom", small)
	testutil.WriteIn(t, root, "b-huge.prom", small+strings.Repeat("# padding\n", 1000))
	testutil.WriteIn(t, root, "c.prom", small)
	testutil.WriteIn(t, root, "d.prom", small)
	var mu sync.Mutex
	read := map[string]bool{}
	onFileRead(t, func(full string) {
		mu.Lock()
		defer mu.Unlock()
		read[filepath.Base(full)] = true
	})
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxResponseBytes = 1000
	c.Request.MaxTotalBytes = model.ByteSize(3*len(small) - 1)
	resp, err := fetchLocalFile(context.Background(), "", validated(t, c), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errs := fileErrors(t, resp)
	if errs["a.prom"] != nil || errs["c.prom"] != nil {
		t.Fatalf("errs=%v", errs)
	}
	for file, want := range map[string]string{"b-huge.prom": "more than the collector's limit of 1000 for one file", "d.prom": "past request.max_total_bytes"} {
		if err := errs[file]; err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, model.ErrLimitExceeded) {
			t.Errorf("%s: err=%v, want a limit error saying %q", file, err, want)
		} else if same := model.SameFailureText(err); same == err.Error() || !strings.Contains(same, want) {
			// The sizes measured are no part of what the failure is to
			// the log; the file and the limit are.
			t.Errorf("%s: the failure %v is recognised by %q", file, err, same)
		}
	}
	// Neither of the two sizes the total's failure measured is: the file's,
	// and that of the files taken before it, a.prom and c.prom.
	own, others := fmt.Sprintf("is %d bytes", len(small)), fmt.Sprintf("after %d bytes of other files", 2*len(small))
	if err := errs["d.prom"]; err != nil {
		if same := model.SameFailureText(err); !strings.Contains(err.Error(), own) || !strings.Contains(err.Error(), others) || strings.Contains(same, own) || strings.Contains(same, others) {
			t.Errorf("the failure %v, which says %q and %q, is recognised by %q, want by neither size", err, own, others, same)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if read["b-huge.prom"] || read["d.prom"] {
		t.Errorf("refused files were read: %v", read)
	}
}

// A file the read has not finished by the scrape's deadline fails alone; what
// was read is answered.
func TestLocalDirectoryAnswersWhatWasReadByTheDeadline(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.prom", "b.prom", "slow.prom"} {
		testutil.WriteIn(t, root, name, "# TYPE v gauge\nv 1\n")
	}
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	onFileRead(t, func(full string) {
		if filepath.Base(full) == "slow.prom" {
			<-hold
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, err := fetchLocalFile(ctx, "", validated(t, dirCollector("dir", root, "*.prom")), RequestOverrides{}, nil)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the read took %s", took)
	}
	if err != nil {
		t.Fatal(err)
	}
	errs := fileErrors(t, resp)
	if len(errs) != 3 || errs["a.prom"] != nil || errs["b.prom"] != nil {
		t.Fatalf("errs=%v", errs)
	}
	if err := errs["slow.prom"]; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the file still being read: err=%v", err)
	}
	// The read says it was cut short, so the answer is not cached.
	if !resp.Directory.CutShort {
		t.Fatal("a read the deadline ended is not marked cut short")
	}
	release()
	afterLocalFileRead.Store(nil)
	whole, err := fetchLocalFile(context.Background(), "", validated(t, dirCollector("dir", root, "*.prom")), RequestOverrides{}, nil)
	if err != nil || whole.Directory.CutShort {
		t.Fatalf("a whole read: err=%v, cut short=%v", err, whole != nil && whole.Directory.CutShort)
	}
}

// Files are read several at a time, never more than the workers, and answered
// in name order whatever order the reads finished in.
func TestLocalDirectoryReadsFilesConcurrently(t *testing.T) {
	root := t.TempDir()
	for i := range 10 {
		testutil.WriteIn(t, root, "f"+strconv.Itoa(i)+".prom", "# TYPE v gauge\nv "+strconv.Itoa(i)+"\n")
	}
	var mu sync.Mutex
	current, peak := 0, 0
	onFileRead(t, func(string) {
		mu.Lock()
		current++
		peak = max(peak, current)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		current--
		mu.Unlock()
	})
	resp, err := fetchLocalFile(context.Background(), "", validated(t, dirCollector("dir", root, "*.prom")), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	files := resp.Directory.Files
	if len(files) != 10 {
		t.Fatalf("%d files", len(files))
	}
	for i, f := range files {
		if f.Err != nil {
			t.Fatalf("%s: %v", f.Name, f.Err)
		}
		if i > 0 && files[i-1].Name > f.Name {
			t.Fatalf("not in name order: %s before %s", files[i-1].Name, f.Name)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if peak < 2 || peak > localFileDirectoryWorkers {
		t.Fatalf("%d files were read at once, want between 2 and %d", peak, localFileDirectoryWorkers)
	}
}

// A file that grows after its size was taken, past what the total leaves it,
// fails alone, so the scrape never reads more than max_total_bytes.
func TestLocalDirectoryTotalHoldsWhenAFileGrows(t *testing.T) {
	root := t.TempDir()
	small := "# TYPE v gauge\nv 1\n"
	testutil.WriteIn(t, root, "a.prom", small)
	grow := testutil.WriteIn(t, root, "b.prom", small)
	c := dirCollector("dir", root, "*.prom")
	c.Request.MaxTotalBytes = model.ByteSize(2*len(small) + 4)
	var once sync.Once
	// Growing b.prom during its own read makes the read start again, and
	// find it larger than its share.
	onFileRead(t, func(full string) {
		if filepath.Base(full) == "b.prom" {
			once.Do(func() {
				if err := os.WriteFile(grow, []byte(small+strings.Repeat("# grown\n", 20)), 0o600); err != nil {
					t.Error(err)
				}
			})
		}
	})
	resp, err := fetchLocalFile(context.Background(), "", validated(t, c), RequestOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	errs := fileErrors(t, resp)
	if errs["a.prom"] != nil {
		t.Fatalf("a.prom: %v", errs["a.prom"])
	}
	if err := errs["b.prom"]; err == nil || !strings.Contains(err.Error(), "grew while the directory was read") {
		t.Fatalf("b.prom: err=%v", err)
	}
	if resp.Directory.Bytes > int64(c.Request.MaxTotalBytes) {
		t.Fatalf("read %d bytes, past the total of %d", resp.Directory.Bytes, c.Request.MaxTotalBytes)
	}
}
