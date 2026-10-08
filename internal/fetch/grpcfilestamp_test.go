//go:build !select_request_types || request_type_grpc

package fetch

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// oldPartOf is what the old stamp (oldFilesStamp) said of the files of a
// stamp: each file's record with its identity and permissions left out.
func oldPartOf(stamp string) string {
	records := strings.Split(stamp, "|")
	for i, record := range records {
		if fields := strings.Split(record, ":"); len(fields) == 5 {
			records[i] = strings.Join(fields[:3], ":")
		}
	}
	return strings.Join(records, "|")
}

// watchStamp is the stamp of the configuration's watch (config's
// filesStamp), copied: which file each path leads to after its symbolic
// links, with its time, size and permissions, or that there is none.
func watchStamp(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		b.WriteByte(0)
		st, err := os.Stat(path)
		if err != nil {
			b.WriteString("missing\n")
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			b.WriteString(resolved)
		}
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(st.Size(), 10))
		b.WriteByte(0)
		b.WriteString(st.Mode().String())
		b.WriteByte('\n')
	}
	return b.String()
}

// stampFixture is a file reached in each way a descriptor file is: a plain
// file; a symbolic link to a file, which is swapped by pointing it at
// another; a file under a directory reached through a link, which is swapped
// by pointing that link at another directory; and the Kubernetes shape, a
// link x.proto to ..data/x.proto, where ..data, a link to the directory of
// the version in force, is what is swapped.
type stampFixture struct {
	t     *testing.T
	dir   string
	paths []string
	// targets are, for each path, the file it leads to now; versions counts
	// the files written, so each has a name and a time of its own.
	targets  []string
	versions int
	times    time.Time
}

func newStampFixture(t *testing.T) *stampFixture {
	f := &stampFixture{t: t, dir: t.TempDir(), times: time.Unix(1_700_000_000, 0)}
	plain := filepath.Join(f.dir, "etc", "exporter", "protos", "acme", "queue", "v1", "plain.proto")
	f.must(os.MkdirAll(filepath.Dir(plain), 0o750))
	f.must(os.WriteFile(plain, []byte("plain-0"), 0o600))
	linked := filepath.Join(f.dir, "linked.proto")
	f.must(os.Symlink(f.version("linked", "linked-0"), linked))
	dirLink := filepath.Join(f.dir, "current")
	underLink := filepath.Join(dirLink, "under.proto")
	f.must(os.Symlink(filepath.Dir(f.version("under", "under-0")), dirLink))
	protos := filepath.Join(f.dir, "protos")
	f.must(os.MkdirAll(protos, 0o750))
	kube := filepath.Join(protos, "x.proto")
	version := f.version("kube", "kube-0")
	f.must(os.Symlink(filepath.Dir(version), filepath.Join(protos, "..data")))
	f.must(os.Symlink(filepath.Join("..data", "x.proto"), kube))
	f.paths = []string{plain, linked, underLink, kube}
	for _, path := range f.paths {
		target, err := filepath.EvalSymlinks(path)
		f.must(err)
		f.targets = append(f.targets, target)
	}
	return f
}

func (f *stampFixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

// version writes content as a new version of a file, in a directory of its
// own, as Kubernetes writes each version of a ConfigMap, and returns its
// path; the file under a directory link and the Kubernetes file are always
// named as the link expects them.
func (f *stampFixture) version(kind, content string) string {
	f.versions++
	name := map[string]string{"linked": "linked.proto", "under": "under.proto", "kube": "x.proto"}[kind]
	path := filepath.Join(f.dir, fmt.Sprintf("..v%d", f.versions), name)
	f.must(os.MkdirAll(filepath.Dir(path), 0o750))
	f.must(os.WriteFile(path, []byte(content), 0o600))
	return path
}

// nextTime is a modification time no file had.
func (f *stampFixture) nextTime() time.Time {
	f.times = f.times.Add(time.Second)
	return f.times
}

// replaceLink points the link at link to target, as Kubernetes does: a new
// link renamed over the old one.
func (f *stampFixture) replaceLink(link, target string) {
	f.must(os.Symlink(target, link+".tmp"))
	f.must(os.Rename(link+".tmp", link))
}

// stampEvent is what can happen to a file, and whether, after it, the path
// leads to another file or the file has other permissions.
type stampEvent struct {
	name string
	// apply changes the i-th path and reports whether it leads to another
	// file or the file's permissions changed, which the old stamp missed
	// when the time and size stayed; ok is false when the event does not
	// apply to that path as it is now.
	apply func(f *stampFixture, i int) (another, ok bool)
}

var stampEvents = []stampEvent{
	{"edit with a new time", func(f *stampFixture, i int) (bool, bool) {
		if _, err := os.Stat(f.targets[i]); err != nil {
			return false, false
		}
		f.must(os.WriteFile(f.targets[i], []byte("edited-"+strconv.Itoa(f.versions)), 0o600))
		f.versions++
		at := f.nextTime()
		f.must(os.Chtimes(f.targets[i], at, at))
		return false, true
	}},
	{"edit keeping time and size", func(f *stampFixture, i int) (bool, bool) {
		st, err := os.Stat(f.targets[i])
		if err != nil {
			return false, false
		}
		raw, err := os.ReadFile(f.targets[i])
		f.must(err)
		for j := range raw {
			raw[j] ^= 1
		}
		file, err := os.OpenFile(f.targets[i], os.O_WRONLY, 0)
		f.must(err)
		_, err = file.Write(raw)
		f.must(err)
		f.must(file.Close())
		f.must(os.Chtimes(f.targets[i], st.ModTime(), st.ModTime()))
		return false, true
	}},
	{"rename over with a new time", func(f *stampFixture, i int) (bool, bool) {
		if _, err := os.Stat(f.targets[i]); err != nil {
			return false, false
		}
		f.must(os.WriteFile(f.targets[i]+".new", []byte("renamed"), 0o600))
		at := f.nextTime()
		f.must(os.Chtimes(f.targets[i]+".new", at, at))
		f.must(os.Rename(f.targets[i]+".new", f.targets[i]))
		return true, true
	}},
	{"swap to a file of the same time and size", func(f *stampFixture, i int) (bool, bool) {
		st, err := os.Stat(f.paths[i])
		if err != nil || i == 0 {
			return false, false
		}
		raw, err := os.ReadFile(f.paths[i])
		f.must(err)
		kind := []string{"", "linked", "under", "kube"}[i]
		next := f.version(kind, string(raw))
		f.must(os.Chmod(next, st.Mode().Perm()))
		f.must(os.Chtimes(next, st.ModTime(), st.ModTime()))
		switch kind {
		case "linked":
			f.replaceLink(f.paths[i], next)
		case "under":
			f.replaceLink(filepath.Dir(f.paths[i]), filepath.Dir(next))
		case "kube":
			f.replaceLink(filepath.Join(filepath.Dir(f.paths[i]), "..data"), filepath.Dir(next))
		}
		f.targets[i] = next
		return true, true
	}},
	{"chmod", func(f *stampFixture, i int) (bool, bool) {
		st, err := os.Stat(f.targets[i])
		if err != nil {
			return false, false
		}
		f.must(os.Chmod(f.targets[i], st.Mode().Perm()^0o044))
		return true, true
	}},
	{"delete", func(f *stampFixture, i int) (bool, bool) {
		if _, err := os.Stat(f.targets[i]); err != nil {
			return false, false
		}
		f.must(os.Remove(f.targets[i]))
		return false, true
	}},
	{"recreate", func(f *stampFixture, i int) (bool, bool) {
		if _, err := os.Stat(f.targets[i]); err == nil {
			return false, false
		}
		f.must(os.WriteFile(f.targets[i], []byte("recreated"), 0o600))
		at := f.nextTime()
		f.must(os.Chtimes(f.targets[i], at, at))
		return true, true
	}},
}

// Over generated sequences of what happens to descriptor files, reached in
// each of the ways stampFixture has, the stamp of the check at each call
// tells a change whenever the old stamp did, and besides exactly when the
// path came to lead to another file or the file's permissions changed: a
// swap of the file's link or of a directory link on its way, as Kubernetes
// swaps ..data, to a file of the same time and size, and a chmod. It tells
// every change the configuration's watch tells, and what it says of the
// old stamp's fields is what the old stamp said.
func TestTheStampOfDescriptorFilesSeesWhatTheWatchSees(t *testing.T) {
	steps := alloctest.UnlessRaced(600, 150)
	random := rand.New(rand.NewPCG(42, 7))
	f := newStampFixture(t)
	seen := map[string]int{}
	for step := 0; step < steps; {
		i := random.IntN(len(f.paths))
		event := stampEvents[random.IntN(len(stampEvents))]
		path := []string{f.paths[i]}
		old, now, watch := oldFilesStamp(path), filesStamp(path), watchStamp(path)
		another, ok := event.apply(f, i)
		if !ok {
			continue
		}
		step++
		seen[event.name]++
		oldChanged := oldFilesStamp(path) != old
		changed := filesStamp(path) != now
		watchChanged := watchStamp(path) != watch
		if want := oldChanged || another; changed != want {
			t.Errorf("step %d, %s of %s: the stamp tells a change %v, want %v (the old stamp %v)", step, event.name, f.paths[i], changed, want, oldChanged)
		}
		if watchChanged && !changed {
			t.Errorf("step %d, %s of %s: the watch tells a change the stamp does not", step, event.name, f.paths[i])
		}
		if got := oldPartOf(filesStamp(path)); got != oldFilesStamp(path) {
			t.Errorf("step %d: the stamp says %q of the old fields, and the old stamp %q", step, got, oldFilesStamp(path))
		}
	}
	for _, event := range stampEvents {
		if seen[event.name] == 0 {
			t.Errorf("%s never happened", event.name)
		}
	}
}

// kubeProtos lays .proto files out as Kubernetes mounts a ConfigMap, each a
// link protos/name to ..data/name, ..data a link to the directory of the
// version in force, and returns the path of each by name. publish writes a
// new version and swaps ..data to it.
func kubeProtos(t *testing.T, files map[string]string) (paths map[string]string, publish func(files map[string]string)) {
	t.Helper()
	dir := t.TempDir()
	protos := filepath.Join(dir, "protos")
	versions := 0
	write := func(files map[string]string) string {
		versions++
		version := filepath.Join(protos, fmt.Sprintf("..2026_10_08_00_00_0%d", versions))
		for name, content := range files {
			path := filepath.Join(version, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			// Every version of a file has the same time, so only which
			// file the path leads to tells them apart.
			if err := os.Chtimes(path, protoTimes, protoTimes); err != nil {
				t.Fatal(err)
			}
		}
		return filepath.Base(version)
	}
	link := func(target, path string) {
		if err := os.Symlink(target, path+".tmp"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(protos, 0o750); err != nil {
		t.Fatal(err)
	}
	link(write(files), filepath.Join(protos, "..data"))
	paths = map[string]string{}
	for name := range files {
		paths[name] = filepath.Join(protos, name)
		link(filepath.Join("..data", name), paths[name])
	}
	return paths, func(files map[string]string) { link(write(files), filepath.Join(protos, "..data")) }
}

// A collector's .proto files mounted from a ConfigMap, and published anew
// by swapping ..data to files of the same time and size: the next call
// compiles the new files, so it finds the method the new version has and
// not the one it no longer has, and the call after it compiles nothing.
func TestADataSwapToFilesOfTheSameTimeAndSizeIsCompiledAtTheNextCall(t *testing.T) {
	const (
		service = "syntax = \"proto3\";\npackage k.api;\nimport \"types.proto\";\nservice S { rpc Get(k.Request) returns (k.Request); }\n"
		renamed = "syntax = \"proto3\";\npackage k.api;\nimport \"types.proto\";\nservice S { rpc Put(k.Request) returns (k.Request); }\n"
		types   = "syntax = \"proto3\";\npackage k;\nmessage Request { string queue = 1; }\n"
	)
	paths, publish := kubeProtos(t, map[string]string{"svc.proto": service, "types.proto": types})
	root := filepath.Dir(paths["svc.proto"])
	collector := func(rpc string) *model.Collector {
		return &model.Collector{Name: "kube", Request: model.RequestConfig{
			Type: RequestTypeGRPC, RPC: "k.api.S/" + rpc, Descriptors: descriptorsProto,
			ProtoFiles: []string{paths["svc.proto"]}, ProtoImportPaths: []string{root},
		}}
	}
	if _, err := staticMethod(collector("Get")); err != nil {
		t.Fatal(err)
	}
	if _, err := staticMethod(collector("Put")); err == nil {
		t.Fatal("the first version has a method Put")
	}
	publish(map[string]string{"svc.proto": renamed, "types.proto": types})
	if n := compiles(func() {
		if _, err := staticMethod(collector("Put")); err != nil {
			t.Errorf("the call after the swap does not find the new method: %v", err)
		}
	}); n != 1 {
		t.Errorf("the call after the swap compiled %d times, want once", n)
	}
	if n := compiles(func() {
		if _, err := staticMethod(collector("Get")); err == nil {
			t.Error("the call after the swap still finds the method the new version removed")
		}
	}); n != 0 {
		t.Errorf("a call after that compiled %d times", n)
	}
}

// The same for a FileDescriptorSet: swapped to one of the same time and
// size, it is read again at the next call, and not at the one after.
func TestADataSwapOfAProtosetIsReadAtTheNextCall(t *testing.T) {
	raw, err := os.ReadFile(writeProtosetOf(t))
	if err != nil {
		t.Fatal(err)
	}
	paths, publish := kubeProtos(t, map[string]string{"queue.pb": string(raw)})
	reads := countDescriptorReads(t)
	c := &model.Collector{Name: "kube", Request: model.RequestConfig{
		Type: RequestTypeGRPC, RPC: "acme.queue.v1.QueueService/GetStats", Descriptors: descriptorsProtoset, ProtosetFile: paths["queue.pb"],
	}}
	if _, err := staticMethod(c); err != nil {
		t.Fatal(err)
	}
	publish(map[string]string{"queue.pb": string(raw)})
	before := reads()
	if _, err := staticMethod(c); err != nil {
		t.Fatal(err)
	}
	if _, err := staticMethod(c); err != nil {
		t.Fatal(err)
	}
	if n := reads() - before; n != 1 {
		t.Errorf("the two calls after the swap read the set %d times, want once", n)
	}
}

// countDescriptorReads counts the descriptor files read from here on.
func countDescriptorReads(t *testing.T) func() int {
	t.Helper()
	var n atomic.Int64
	descriptorFileRead = func(string) { n.Add(1) }
	t.Cleanup(func() { descriptorFileRead = nil })
	return func() int { return int(n.Load()) }
}

// writeProtosetOf writes the queue service's FileDescriptorSet in a
// directory of its own.
func writeProtosetOf(t *testing.T) string {
	t.Helper()
	return grpctest.WriteProtoset(t, filepath.Join(t.TempDir(), "queue.pb"), false)
}

// A chmod of a collector's descriptor file, which leaves its time and size,
// is seen at the next call: the files are read again, so a file that is no
// longer readable fails that call as an unreadable file always has. Run as
// root, as the tests may be, a chmod takes nothing from what can be read, so
// what the test can see there is that the files were read again; run as
// another user, it also sees the call fail.
func TestAChmodOfADescriptorFileIsSeenAtTheNextCall(t *testing.T) {
	dir := t.TempDir()
	svc := writeProto(t, dir, importingService, serviceSource)
	types := writeProto(t, dir, importedTypes, typesSource)
	writeProto(t, dir, importedBase, baseSource)
	c := importingCollector(dir, dir)
	if _, err := staticMethod(c); err != nil {
		t.Fatal(err)
	}
	// An imported file as much as a named one.
	for _, path := range []string{types, svc} {
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		var err error
		if n := compiles(func() { _, err = staticMethod(c) }); n != 1 {
			t.Errorf("the call after a chmod of %s compiled %d times, want once", path, n)
		}
		if os.Geteuid() != 0 && (err == nil || !strings.Contains(err.Error(), "permission denied")) {
			t.Errorf("the call after %s became unreadable gave %v", path, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if n := compiles(func() { _, err = staticMethod(c) }); n != 1 || err != nil {
			t.Errorf("the call after %s became readable again compiled %d times and gave %v", path, n, err)
		}
	}

	set := writeProtosetOf(t)
	reads := countDescriptorReads(t)
	ps := &model.Collector{Name: "set", Request: model.RequestConfig{
		Type: RequestTypeGRPC, RPC: "acme.queue.v1.QueueService/GetStats", Descriptors: descriptorsProtoset, ProtosetFile: set,
	}}
	if _, err := staticMethod(ps); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(set, 0); err != nil {
		t.Fatal(err)
	}
	before := reads()
	_, err := staticMethod(ps)
	if n := reads() - before; n != 1 {
		t.Errorf("the call after a chmod of the protoset read it %d times, want once", n)
	}
	if os.Geteuid() != 0 && (err == nil || !strings.Contains(err.Error(), "reading protoset_file: ") || !strings.Contains(err.Error(), "permission denied")) {
		t.Errorf("the call after the protoset became unreadable gave %v", err)
	}
}

// The check at each call, of files that did not change, reads nothing and
// allocates no more than the old one did over the same files, each a link
// through ..data as Kubernetes mounts them: telling which file a path leads
// to costs no allocation, and no stat but the one the old check made.
func TestTheQuietCheckOfDescriptorFilesAllocatesNoMoreThanBefore(t *testing.T) {
	if alloctest.RaceDetector {
		t.Skip("the race detector allocates on its own")
	}
	for _, n := range []int{1, 10, 50} {
		paths := stampBenchFiles(t, t.TempDir(), "kubernetes", n)
		sets := &fileSets{slots: map[string]*fileSetSlot{}}
		files := new(protoregistry.Files)
		reads := 0
		read := func() (*protoregistry.Files, []string, string, error) {
			reads++
			return files, paths, filesStamp(paths), nil
		}
		if _, err := sets.getStamped("set", read); err != nil {
			t.Fatal(err)
		}
		old, _ := alloctest.Allocations(100, func() { _ = oldFilesStamp(paths) })
		if now := alloctest.AllocsAtMost(100, old, func() { _ = filesStamp(paths) }); now > old {
			t.Errorf("%d files: the stamp allocates %.0f times, and the old one %.0f", n, now, old)
		}
		if now := alloctest.AllocsAtMost(100, old, func() { _, _ = sets.getStamped("set", read) }); now > old {
			t.Errorf("%d files: the check at a call allocates %.0f times, the old stamp alone %.0f", n, now, old)
		}
		if reads != 1 {
			t.Errorf("%d files: the set was read %d times", n, reads)
		}
	}
}
