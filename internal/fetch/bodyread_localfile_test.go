//go:build !select_request_types || request_type_localfile

package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A file is read whole into a buffer of its size and held to the limit as
// before: a file up to the limit is the response's body, to the byte, an
// empty one an empty body, and one a byte over the limit is refused with the
// message it always had.
func TestAFileIsReadWholeIntoABufferOfItsSize(t *testing.T) {
	root := t.TempDir()
	text := strings.Repeat("value 1\n", 5000)
	large := string(bodyBytes()[:3<<20+17])
	testutil.WriteIn(t, root, "text.prom", text)
	testutil.WriteIn(t, root, "large.bin", large)
	testutil.WriteIn(t, root, "empty.prom", "")
	for name, test := range map[string]struct {
		file    string
		limit   int64
		want    string
		wantErr string
	}{
		"a file":            {file: "text.prom", want: text},
		"at the limit":      {file: "text.prom", limit: 40000, want: text},
		"over the limit":    {file: "text.prom", limit: 39999, wantErr: "file " + filepath.Join(root, "text.prom") + ": response size exceeds limit 39999"},
		"megabytes":         {file: "large.bin", want: large},
		"an empty file":     {file: "empty.prom", want: ""},
		"the largest limit": {file: "text.prom", limit: math.MaxInt64, want: text},
	} {
		c := fileCollector("files", root, test.file)
		c.Request.MaxResponseBytes = model.ByteSize(test.limit)
		response, err := fetchLocalFile(context.Background(), "", validated(t, c), RequestOverrides{}, nil)
		if test.wantErr != "" {
			if err == nil || err.Error() != test.wantErr || !errors.Is(err, model.ErrLimitExceeded) {
				t.Errorf("%s: err=%v, want %q", name, err, test.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if response.Body == nil || string(response.Body) != test.want {
			t.Errorf("%s: a body of %d bytes (nil: %v), want the file's %d", name, len(response.Body), response.Body == nil, len(test.want))
		}
		// The buffer is the file's size and the byte in which its end is
		// found, not a larger one grown to fit. An empty file gives no
		// size to go by.
		if len(test.want) > 0 && cap(response.Body) > len(test.want)+1 {
			t.Errorf("%s: a buffer of %d bytes for a file of %d", name, cap(response.Body), len(test.want))
		}
	}
}

// A file whose size says nothing of its content, as those of /proc, which
// all give a size of 0, is read to its end like an answer of unknown length.
func TestAFileOfNoStatedSizeIsReadToItsEnd(t *testing.T) {
	want, err := os.ReadFile("/proc/self/stat")
	if err != nil || len(want) == 0 {
		t.Skipf("no /proc here: %v", err)
	}
	root, err := os.OpenRoot("/proc/self")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	body, info, err := readOpenedFile(root, "stat", "/proc/self/stat", defaultResponseLimit)
	if err != nil || info.Size() != 0 {
		t.Fatalf("size=%d err=%v, want a size of 0 and no error", info.Size(), err)
	}
	// The numbers in it move between two reads; its start, the process
	// and its name, does not.
	if start := want[:bytes.IndexByte(want, ')')+1]; !bytes.HasPrefix(body, start) {
		t.Fatalf("read %q, want it to start with %q", body, start)
	}
}

// The files of a directory are read the same way, each into a buffer of its
// size, an empty one among them.
func TestTheFilesOfADirectoryAreReadIntoBuffersOfTheirSizes(t *testing.T) {
	root := t.TempDir()
	want := map[string]string{"empty.prom": ""}
	for i, size := range []int{1, 100, testBodyStep, testBodyStep + 1, 300000} {
		want[fmt.Sprintf("f%d.prom", i)] = string(bodyBytes()[:size])
	}
	for name, body := range want {
		testutil.WriteIn(t, root, name, body)
	}
	response, err := FetchCollector(context.Background(), "", validated(t, dirCollector("dir", root, "*.prom")), RequestOverrides{}, nil)
	if err != nil || response.Directory == nil || len(response.Directory.Files) != len(want) {
		t.Fatalf("response=%+v err=%v, want %d files", response, err, len(want))
	}
	for _, file := range response.Directory.Files {
		switch {
		case file.Err != nil:
			t.Errorf("%s: %v", file.Name, file.Err)
		case file.Response.Body == nil || string(file.Response.Body) != want[file.Name]:
			t.Errorf("%s: a body of %d bytes (nil: %v), want the file's %d", file.Name, len(file.Response.Body), file.Response.Body == nil, len(want[file.Name]))
		case len(want[file.Name]) > 0 && cap(file.Response.Body) > len(want[file.Name])+1:
			t.Errorf("%s: a buffer of %d bytes for a file of %d", file.Name, cap(file.Response.Body), len(want[file.Name]))
		}
	}
}

// Reading a file of a megabyte allocates little more than the megabyte: its
// buffer, once, and what opening and checking the file takes. Read with
// io.ReadAll it was 2.1 MiB; the bound is between the two. It is a bound on
// the bytes and not on the number of allocations, which opening a file under
// the race detector changes.
func TestReadingAMegabyteFileAllocatesItOnce(t *testing.T) {
	root := t.TempDir()
	body := bodyBytes()[:megabyteBody]
	testutil.WriteIn(t, root, "large.bin", string(body))
	size, _ := allocatedPerRun(func() {
		read, _, err := readLocalFile(root, "large.bin", defaultResponseLimit)
		if err != nil || len(read) != len(body) {
			t.Fatalf("%d bytes, err=%v", len(read), err)
		}
	})
	if size > 3<<19 {
		t.Errorf("reading a file of %d bytes allocated %d bytes, want little more than the file", len(body), size)
	}
}
