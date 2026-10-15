//go:build !select_request_types || request_type_sshfile

package fetch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/sshtest"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// newSSHFixtureOptions is newSSHFixture with the server's options changed by
// change.
func newSSHFixtureOptions(t *testing.T, change func(*sshtest.Options)) *sshFixture {
	t.Helper()
	f := &sshFixture{root: t.TempDir(), dir: t.TempDir()}
	var key ssh.PublicKey
	f.keyFile, key = sshtest.WriteClientKey(t, f.dir, "id_ed25519", "")
	f.passwordFile = sshtest.WriteFile(t, f.dir, "password", sshPassword+"\n")
	options := sshtest.Options{Password: sshPassword, AuthorizedKeys: []ssh.PublicKey{key}}
	change(&options)
	f.server = sshtest.Start(t, options)
	f.knownHosts = f.server.WriteKnownHosts(t, f.dir)
	t.Cleanup(sshConns.closeAll)
	return f
}

// connect makes the fixture's connection, with a scrape reading a file,
// and returns it.
func (f *sshFixture) connect(t *testing.T) *sshConn {
	t.Helper()
	testutil.WriteIn(t, f.root, "connect.prom", sshPromFile)
	if _, err := f.fetch(f.collector(t, "connect", "connect.prom"), RequestOverrides{}); err != nil {
		t.Fatal(err)
	}
	return keptConn(f.server.Addr)
}

// listAll lists a directory to its end, seven entries at a time, so that
// the batches of the host are handed out across calls.
func listAll(d interface {
	ReadDir(n int) ([]fs.DirEntry, error)
}) ([]fs.DirEntry, error) {
	var all []fs.DirEntry
	for {
		entries, err := d.ReadDir(7)
		all = append(all, entries...)
		if errors.Is(err, io.EOF) {
			return all, nil
		}
		if err != nil {
			return all, err
		}
	}
}

// oracleSFTPDir is sftpDir as it was before listings were read in batches:
// pkg/sftp's Client.ReadDir, the whole listing at the first ReadDir. It is
// the oracle of the tests that the batches list what it listed.
type oracleSFTPDir struct {
	client  *sftp.Client
	path    string
	info    fs.FileInfo
	listed  bool
	entries []fs.DirEntry
}

func (d *oracleSFTPDir) Stat() (fs.FileInfo, error) { return d.info, nil }

func (d *oracleSFTPDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.listed {
		infos, err := d.client.ReadDir(d.path)
		if err != nil {
			return nil, err
		}
		d.listed = true
		d.entries = make([]fs.DirEntry, len(infos))
		for i, info := range infos {
			d.entries[i] = fs.FileInfoToDirEntry(info)
		}
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(d.entries))
	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}

func (d *oracleSFTPDir) Close() error { return nil }

// oracleSFTPSource is an sftpSource whose directories are oracleSFTPDirs.
type oracleSFTPSource struct{ *sftpSource }

func (s oracleSFTPSource) OpenDir(name string) (sourceDir, error) {
	p, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	info, err := s.client.Stat(p)
	if err != nil {
		return nil, err
	}
	return &oracleSFTPDir{client: s.client, path: p, info: info}, nil
}

// A directory far larger than what a scrape lists is listed only as far as
// the bound of ten times max_files entries, and at least 1000: the host is
// asked for the batches up to it and no more, so it sends at most one batch
// past it (128 entries, the batch of pkg/sftp's server), and the scrape
// allocates what that many entries cost, not what the directory's do. Before,
// the whole directory was listed at every scrape.
func TestSSHFileListsADirectoryOnlyAsFarAsItsBound(t *testing.T) {
	f := newSSHFixture(t)
	size := alloctest.UnlessRaced(50_000, 20_000)
	for i := range size {
		if err := os.WriteFile(filepath.Join(f.root, fmt.Sprintf("m%06d.prom", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := f.collector(t, "huge", "", func(r *model.RequestConfig) {
		r.Files = []string{"*.prom"}
		r.MaxFiles = 1
	})
	limit := maxListedEntries(c)
	const serverBatch = 128
	resp, err := f.fetch(c, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if read := resp.Directory; read.Listed != limit || !read.Truncated || len(read.Files) != 1 {
		t.Fatalf("listed %d, truncated %v, %d files; want %d, true, 1", read.Listed, read.Truncated, len(read.Files), limit)
	}
	if sent := f.server.Listed(); sent > limit+serverBatch {
		t.Errorf("the host sent %d entries for a listing bound at %d; want at most one batch of %d past it", sent, limit, serverBatch)
	}
	// What a scrape allocates, the host's side in the same process with it,
	// which costs about 3 KB for each entry it sends (pkg/sftp's server
	// looks up the owner of each for its long name): about 4 MB for the
	// entries up to the bound, 60 MB and more for the directory's.
	_, bytes := alloctest.Once(1, func() {
		if _, err := f.fetch(c, RequestOverrides{}); err != nil {
			t.Error(err)
		}
	})
	const most = 12 << 20
	if bytes > most {
		t.Errorf("a scrape allocated %d bytes, more than %d", bytes, most)
	}
}

// sftpNames are the names the directories of the differential test are made
// of: spaces, dots, accents and other scripts.
var sftpNames = []string{"a", "Z", "0", " ", ".", "-", "_", "é", "日本", "ü", "prom"}

// generateDirectory fills dir with n entries of random names: files of
// random sizes and modification times, directories, empty, and symbolic
// links to a file, to a directory, and to nothing.
func generateDirectory(t *testing.T, rng *rand.Rand, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for made := 0; made < n; {
		var b strings.Builder
		for range 1 + rng.IntN(10) {
			b.WriteString(sftpNames[rng.IntN(len(sftpNames))])
		}
		name := b.String()
		p := filepath.Join(dir, name)
		if name == "." || name == ".." {
			continue
		}
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		var err error
		switch rng.IntN(8) {
		case 0:
			err = os.Mkdir(p, 0o700)
		case 1:
			err = os.Symlink("..", p)
		case 2:
			err = os.Symlink("missing", p)
		case 3:
			err = os.Symlink(filepath.Join(dir, "target"), p)
		default:
			if err = os.WriteFile(p, []byte(strings.Repeat("x", rng.IntN(3000))), 0o600); err == nil {
				at := time.Unix(946684800+rng.Int64N(1<<30), 0)
				err = os.Chtimes(p, at, at)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		made++
	}
}

// A directory is listed in batches as pkg/sftp listed it whole: the same
// entries in the same order, each with the same name, type, mode, size,
// modification time and attributes, . and .. left out, over generated
// directories of every kind of entry and of sizes across the server's
// batches; and a directory read through it answers exactly what one through
// pkg/sftp's listing answered, the files read, refused, skipped and left out
// alike, a listing past its bound among them.
func TestSFTPListerListsWhatPkgSFTPListed(t *testing.T) {
	f := newSSHFixture(t)
	entry := f.connect(t)
	rng := rand.New(rand.NewPCG(60, 134))
	sizes := []int{0, 1, 2, 127, 128, 129, 300, alloctest.UnlessRaced(1100, 1050)}
	for i, size := range sizes {
		dir := filepath.Join(f.root, fmt.Sprintf("gen%d", i))
		generateDirectory(t, rng, dir, size)
		testutil.WriteIn(t, dir, "target", "the target")
		want, err := entry.sftp.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		lister := newSFTPLister(entry.client, dir)
		got, err := listAll(lister)
		_ = lister.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d entries, want %d", dir, len(got), len(want))
		}
		for j, e := range got {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			w := want[j]
			if e.Name() != w.Name() || e.IsDir() != w.IsDir() || e.Type() != w.Mode().Type() || info.Mode() != w.Mode() || info.Size() != w.Size() || !info.ModTime().Equal(w.ModTime()) || !reflect.DeepEqual(info.Sys(), w.Sys()) {
				t.Errorf("%s entry %d: %q %v %d %v %+v, want %q %v %d %v %+v", dir, j, e.Name(), info.Mode(), info.Size(), info.ModTime(), info.Sys(), w.Name(), w.Mode(), w.Size(), w.ModTime(), w.Sys())
			}
		}
		maxFiles := 1 + rng.IntN(40)
		if i == len(sizes)-1 {
			maxFiles = 3
		}
		c := f.collector(t, "gen", "", func(r *model.RequestConfig) {
			r.Root = dir
			r.Files = []string{"*", ".*"}
			r.MaxFiles = maxFiles
		})
		readWith := func(src fileSource, s *sftpSource) *DirectoryRead {
			progress := &directoryProgress{}
			if err := s.run(func() error { return readSourceDirectory(src, c, "", progress) }); err != nil {
				t.Fatal(err)
			}
			return progress.snapshot(nil)
		}
		batches := &sftpSource{client: entry.sftp, conn: entry.client, root: dir}
		oracle := oracleSFTPSource{&sftpSource{client: entry.sftp, root: dir}}
		gotRead, wantRead := readWith(batches, batches), readWith(oracle, oracle.sftpSource)
		if i == len(sizes)-1 && !wantRead.Truncated {
			t.Errorf("%s: the listing of %d entries was not cut at its bound", dir, size)
		}
		if g, w := describeDirectoryRead(gotRead), describeDirectoryRead(wantRead); g != w {
			t.Errorf("%s with max_files %d read\n%s\nwant\n%s", dir, maxFiles, g, w)
		}
	}
}

// describeDirectoryRead is everything a directory read answers, as text.
func describeDirectoryRead(read *DirectoryRead) string {
	var b strings.Builder
	fmt.Fprintf(&b, "path %s matched %d skipped %q bytes %d listed %d truncated %v cut %v\n", read.Path, read.Matched, read.Skipped, read.Bytes, read.Listed, read.Truncated, read.CutShort)
	for _, file := range read.Files {
		fmt.Fprintf(&b, "%q %v err=%v", file.Name, file.ModTime.UTC(), file.Err)
		if r := file.Response; r != nil {
			fmt.Fprintf(&b, " %d %v %v %q %s", r.StatusCode, r.NoStatus, r.Headers, r.Body, r.Collector)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// sftpScript answers the requests of a fake SFTP server: the bytes to send
// for a request of type typ and id, the rest of it body, and whether to end
// the session after them. nil answers as serveSFTPScript says.
type sftpScript func(typ byte, id uint32, body []byte) (answer []byte, end bool)

// serveSFTPScript serves an SFTP session by script, until the session ends
// or the script ends it. Unless the script answers them, INIT is answered
// with version 3, OPENDIR with a handle, CLOSE with OK and READDIR with the
// end of the directory.
func serveSFTPScript(channel io.ReadWriter, script sftpScript) {
	for {
		var head [4]byte
		if _, err := io.ReadFull(channel, head[:]); err != nil {
			return
		}
		b := make([]byte, binary.BigEndian.Uint32(head[:]))
		if _, err := io.ReadFull(channel, b); err != nil || len(b) < 5 {
			return
		}
		typ, id, body := b[0], binary.BigEndian.Uint32(b[1:]), b[5:]
		if typ == sftpPacketInit {
			id, body = 0, b[1:]
		}
		answer, end := script(typ, id, body)
		if answer == nil {
			switch typ {
			case sftpPacketInit:
				answer = sftpTestPacket(sftpPacketVersion, sftpU32(3))
			case sftpPacketOpenDir:
				answer = sftpTestPacket(sftpPacketHandle, sftpU32(id), sshString("h"))
			case sftpPacketClose:
				answer = sftpStatusPacket(id, 0, "")
			case sftpPacketReadDir:
				answer = sftpStatusPacket(id, sftpStatusEOF, "")
			}
		}
		if _, err := channel.Write(answer); err != nil || end {
			return
		}
	}
}

func sftpU32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// sftpTestPacket is a packet of type typ made of parts.
func sftpTestPacket(typ byte, parts ...[]byte) []byte {
	body := slices.Concat(parts...)
	return slices.Concat(sftpU32(uint32(1+len(body))), []byte{typ}, body)
}

func sftpStatusPacket(id, code uint32, msg string) []byte {
	return sftpTestPacket(sftpPacketStatus, sftpU32(id), sftpU32(code), sshString(msg), sshString("en"))
}

// sftpTestEntry is an entry of a NAME packet: a regular file of size bytes,
// with its permissions and times.
func sftpTestEntry(name string, size uint64) []byte {
	return slices.Concat(sshString(name), sshString("-rw-r--r-- "+name), sftpU32(sftpAttrSize|sftpAttrPermissions|sftpAttrACModTime), binary.BigEndian.AppendUint64(nil, size), sftpU32(0o100644), sftpU32(1), sftpU32(2))
}

// sftpNamePacket is a NAME packet answering id with count and entries.
func sftpNamePacket(id, count uint32, entries ...[]byte) []byte {
	return sftpTestPacket(sftpPacketName, sftpU32(id), sftpU32(count), slices.Concat(entries...))
}

// scriptedSSHFixture is a fixture whose server serves every SFTP session
// but the connection's own by the script script holds.
func scriptedSSHFixture(t *testing.T, change func(*sshtest.Options)) (*sshFixture, *atomic.Pointer[sftpScript]) {
	t.Helper()
	var script atomic.Pointer[sftpScript]
	f := newSSHFixtureOptions(t, func(o *sshtest.Options) {
		o.SFTP = func(n int, channel io.ReadWriter) bool {
			s := script.Load()
			if n == 1 || s == nil {
				return false
			}
			serveSFTPScript(channel, *s)
			return true
		}
		if change != nil {
			change(o)
		}
	})
	return f, &script
}

// waitSessions waits until the server has only the connection's own SFTP
// session open: a listing's is closed.
func waitSessions(t *testing.T, f *sshFixture) {
	t.Helper()
	testutil.WaitFor(t, "the listing's session to be closed", func() bool { return f.server.Sessions() == 1 })
}

// What the host sends while a directory is listed is held to bounds before
// anything is made of it: a packet too large or empty, a count of entries
// or of extended attributes larger than the packet holds, a name that is
// empty or has a / or a NUL, a string or attributes cut short, an answer of
// another type or to another request, a session ended within a packet, and
// a server of another version each fail the listing with a sentence saying
// what was wrong, without a panic, and the listing's session is closed.
func TestSFTPListerRefusesMalformedAnswers(t *testing.T) {
	f, script := scriptedSSHFixture(t, nil)
	entry := f.connect(t)
	for _, tc := range []struct {
		name   string
		answer func(id uint32) []byte
		at     byte
		want   string
	}{
		{"a packet past the bound", func(uint32) []byte { return sftpU32(sftpMaxPacket + 1) }, sftpPacketReadDir, "a packet of 262145 bytes, more than the 262144 an SFTP client takes"},
		{"an empty packet", func(uint32) []byte { return sftpU32(0) }, sftpPacketReadDir, "sent an empty packet"},
		{"a count past the packet", func(id uint32) []byte { return sftpNamePacket(id, 1000, sftpTestEntry("a", 1)) }, sftpPacketReadDir, "a count of 1000 directory entries, more than"},
		{"a count of four billion", func(id uint32) []byte { return sftpNamePacket(id, 1<<32-1) }, sftpPacketReadDir, "a count of 4294967295 directory entries"},
		{"a name with a slash", func(id uint32) []byte { return sftpNamePacket(id, 1, sftpTestEntry("a/b", 1)) }, sftpPacketReadDir, `the directory entry "a/b", which is not a name in a directory`},
		{"a name with a NUL", func(id uint32) []byte { return sftpNamePacket(id, 1, sftpTestEntry("a\x00b", 1)) }, sftpPacketReadDir, `"a\x00b", which is not a name`},
		{"an empty name", func(id uint32) []byte { return sftpNamePacket(id, 1, sftpTestEntry("", 1)) }, sftpPacketReadDir, `the directory entry "", which is not a name`},
		{"a name past the packet", func(id uint32) []byte { return sftpNamePacket(id, 1, sftpU32(100), []byte("abcdefgh")) }, sftpPacketReadDir, "directory entry 1 cut short in its name"},
		{"a long name past the packet", func(id uint32) []byte {
			return sftpNamePacket(id, 1, sshString("a"), sftpU32(100), []byte("abcd"))
		}, sftpPacketReadDir, "directory entry 1 cut short in its long name"},
		{"attributes cut short", func(id uint32) []byte {
			return sftpNamePacket(id, 2, sftpTestEntry("a", 1), sshString("b"), sshString("b"), sftpU32(sftpAttrSize), sftpU32(7))
		}, sftpPacketReadDir, "directory entry 2 cut short in its attributes: no size"},
		{"extended attributes past the packet", func(id uint32) []byte {
			return sftpNamePacket(id, 1, sshString("a"), sshString("a"), sftpU32(sftpAttrExtended), sftpU32(1000), sshString("x"), sshString("y"))
		}, sftpPacketReadDir, "more extended attributes than the packet holds"},
		{"an extended attribute cut short", func(id uint32) []byte {
			return sftpNamePacket(id, 1, sshString("a"), sshString("a"), sftpU32(sftpAttrExtended), sftpU32(1), sshString("x"), sftpU32(50), []byte("abcd"))
		}, sftpPacketReadDir, "an extended attribute cut short"},
		{"an answer of another type", func(id uint32) []byte { return sftpTestPacket(sftpPacketHandle, sftpU32(id), sshString("h")) }, sftpPacketReadDir, "answered a request for directory entries with a packet of type 102"},
		{"the answer to another request", func(id uint32) []byte { return sftpNamePacket(id+1, 1, sftpTestEntry("a", 1)) }, sftpPacketReadDir, "answered request 4 when request 3 was asked"},
		{"a packet without an id", func(uint32) []byte { return sftpTestPacket(sftpPacketName, []byte{1, 2}) }, sftpPacketReadDir, "too short to hold a request id"},
		{"a status without its code", func(id uint32) []byte { return sftpTestPacket(sftpPacketStatus, sftpU32(id)) }, sftpPacketReadDir, "a status too short to hold its code"},
		{"entries without their count", func(id uint32) []byte { return sftpTestPacket(sftpPacketName, sftpU32(id), []byte{0}) }, sftpPacketReadDir, "directory entries without their count"},
		{"a session ended within a packet", func(uint32) []byte { return slices.Concat(sftpU32(100), []byte("abc")) }, sftpPacketReadDir, "the host's SFTP server ended the session the directory was listed on before the listing was done"},
		{"a handle cut short", func(id uint32) []byte { return sftpTestPacket(sftpPacketHandle, sftpU32(id), sftpU32(9)) }, sftpPacketOpenDir, "a malformed handle for the directory"},
		{"an open answered by another type", func(id uint32) []byte { return sftpNamePacket(id, 0) }, sftpPacketOpenDir, "answered the opening of the directory with a packet of type 104"},
		{"another version", func(uint32) []byte { return sftpTestPacket(sftpPacketVersion, sftpU32(4)) }, sftpPacketInit, "does not speak SFTP version 3"},
		{"a start answered by another type", func(uint32) []byte { return sftpStatusPacket(0, 4, "") }, sftpPacketInit, "with a packet of type 101 rather than its version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readDirs := 0
			s := sftpScript(func(typ byte, id uint32, _ []byte) ([]byte, bool) {
				if typ == sftpPacketReadDir {
					// The first batch is good, so a malformed one comes
					// after entries were handed out.
					if readDirs++; readDirs == 1 {
						return sftpNamePacket(id, 3, sftpTestEntry(".", 0), sftpTestEntry("..", 0), sftpTestEntry("good", 1)), false
					}
				}
				if typ == tc.at {
					return tc.answer(id), true
				}
				return nil, false
			})
			script.Store(&s)
			lister := newSFTPLister(entry.client, "/listed")
			entries, err := listAll(lister)
			_ = lister.Close()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%v, want %q", err, tc.want)
			}
			if tc.at == sftpPacketReadDir && (len(entries) != 1 || entries[0].Name() != "good") {
				t.Errorf("entries before the malformed batch %v, want good", entries)
			}
			waitSessions(t, f)
		})
	}
}

// A status in answer to the opening of the directory or to a request for
// entries fails the listing as it failed through pkg/sftp: the end of the
// directory ends it, no such file and permission denied are those errors,
// and another status has pkg/sftp's text, so a directory read says what it
// said, "file ... does not exist", "cannot be read by the exporter:
// permission denied" or "reading ...: sftp: ...".
func TestSFTPListerFailsOnAStatusAsPkgSFTPDid(t *testing.T) {
	f, script := scriptedSSHFixture(t, nil)
	entry := f.connect(t)
	for _, at := range []byte{sftpPacketOpenDir, sftpPacketReadDir} {
		for _, status := range []struct {
			code uint32
			msg  string
		}{{sftpStatusEOF, ""}, {sftpStatusNoSuchFile, "no such file"}, {sftpStatusPermissionDenied, "denied"}, {4, "boom"}, {8, "unsupported"}, {5, ""}, {300, "odd"}, {0, "ok?"}} {
			// The first request for entries is answered with one, so a
			// status there comes after entries were handed out.
			scripted := func() *sftpScript {
				readDirs := 0
				s := sftpScript(func(typ byte, id uint32, _ []byte) ([]byte, bool) {
					if typ != at {
						return nil, false
					}
					if readDirs++; typ == sftpPacketReadDir && readDirs == 1 {
						return sftpNamePacket(id, 1, sftpTestEntry("first", 1)), false
					}
					return sftpStatusPacket(id, status.code, status.msg), false
				})
				return &s
			}
			script.Store(scripted())
			lister := newSFTPLister(entry.client, "/listed")
			got, err := listAll(lister)
			_ = lister.Close()
			script.Store(scripted())
			client, oerr := sftp.NewClient(entry.client)
			if oerr != nil {
				t.Fatal(oerr)
			}
			oracle := &oracleSFTPDir{client: client, path: "/listed"}
			want, werr := listAll(oracle)
			_ = client.Close()
			name := fmt.Sprintf("status %d at packet type %d", status.code, at)
			if (err == nil) != (werr == nil) || err != nil && localFileError("/listed", err).Error() != localFileError("/listed", werr).Error() {
				t.Errorf("%s: err=%v, want %v", name, err, werr)
			}
			if err == nil && len(got) != len(want) {
				t.Errorf("%s: %d entries, want %d", name, len(got), len(want))
			}
			for _, target := range []error{fs.ErrNotExist, fs.ErrPermission} {
				if errors.Is(err, target) != errors.Is(werr, target) {
					t.Errorf("%s: err=%v, want %v", name, err, werr)
				}
			}
			waitSessions(t, f)
		}
	}
}

// A directory read whose directory cannot be opened says what it said when
// pkg/sftp listed it: a directory gone between its stat and its opening
// does not exist, and one the account may not list cannot be read by the
// exporter; a root that is a file is not a directory, and a missing root
// does not exist.
func TestSSHFileDirectoryFailuresSayWhatTheySaid(t *testing.T) {
	f, script := scriptedSSHFixture(t, nil)
	f.connect(t)
	reader := func(root string) *model.Collector {
		return f.collector(t, "dir", "", func(r *model.RequestConfig) {
			r.Root = root
			r.Files = []string{"*.prom"}
		})
	}
	for _, tc := range []struct {
		name, root string
		code       uint32
		want       string
	}{
		{"gone", f.root, sftpStatusNoSuchFile, "file " + f.root + " does not exist (on " + f.server.Addr + ")"},
		{"denied", f.root, sftpStatusPermissionDenied, "file " + f.root + " cannot be read by the exporter: permission denied (on " + f.server.Addr + ")"},
		{"a file", filepath.Join(f.root, "connect.prom"), 0, filepath.Join(f.root, "connect.prom") + " is not a directory; a collector with request.files reads a directory (on " + f.server.Addr + ")"},
		{"missing", filepath.Join(f.root, "missing"), 0, "request.root " + filepath.Join(f.root, "missing") + " does not exist (on " + f.server.Addr + ")"},
	} {
		s := sftpScript(func(typ byte, id uint32, _ []byte) ([]byte, bool) {
			if typ == sftpPacketOpenDir {
				return sftpStatusPacket(id, tc.code, "refused"), false
			}
			return nil, false
		})
		script.Store(&s)
		_, err := f.fetch(reader(tc.root), RequestOverrides{})
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: err=%v, want %s", tc.name, err, tc.want)
		}
		waitSessions(t, f)
	}
}

// A host that stops answering a request for directory entries does not
// hold the read past the probe's deadline: the listing's session is closed
// at once, the read returns and releases its connection, which is kept,
// not dropped as stalled two seconds later as a read of a file's is.
func TestSSHFileEndsAStalledListingAtTheDeadline(t *testing.T) {
	f, script := scriptedSSHFixture(t, nil)
	f.connect(t)
	events := sshEvents(t, f.server.Addr)
	stalled := make(chan struct{}, 1)
	s := sftpScript(func(typ byte, _ uint32, _ []byte) ([]byte, bool) {
		if typ == sftpPacketReadDir {
			stalled <- struct{}{}
			return []byte{}, false
		}
		return nil, false
	})
	script.Store(&s)
	c := f.collector(t, "dir", "", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} })
	ctx, end := testutil.DeadlineEndedByHand()
	done := make(chan error, 1)
	go func() {
		_, err := FetchCollector(ctx, f.server.Addr, c, RequestOverrides{}, nil)
		done <- err
	}()
	select {
	case <-stalled:
	case err := <-done:
		t.Fatalf("the read ended before the host stalled the listing: %v", err)
	}
	end()
	if err := <-done; err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err=%v, want the deadline", err)
	}
	testutil.WaitFor(t, "the read to release its connection", func() bool { return heldReads(f.server.Addr) == 0 })
	for {
		select {
		case event := <-events:
			if strings.HasPrefix(event, "closed") {
				t.Fatalf("the connection was %s", event)
			}
			continue
		default:
		}
		break
	}
	waitSessions(t, f)
	if f.server.Accepted() != 1 || keptConn(f.server.Addr) == nil {
		t.Errorf("accepted %d connections, kept %v; want the one kept", f.server.Accepted(), keptConn(f.server.Addr))
	}
}

// A listing takes an SSH session of its own beside the connection's SFTP
// session, for as long as the directory is listed: a host that allows two
// sessions on a connection lists a directory at every scrape, each
// listing's session closed before the next, and one that allows one fails
// the read saying its MaxSessions must allow one more.
func TestSSHFileListsADirectoryOnASessionOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		maxSessions int
		want        string
	}{{2, ""}, {1, "the host's limit on sessions, MaxSessions in OpenSSH (10 by default), must allow one more"}} {
		f := newSSHFixtureOptions(t, func(o *sshtest.Options) { o.MaxSessions = tc.maxSessions })
		testutil.WriteIn(t, f.root, "a.prom", sshPromFile)
		c := f.collector(t, "dir", "", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} })
		for range 3 {
			resp, err := f.fetch(c, RequestOverrides{})
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("MaxSessions %d: err=%v, want %q", tc.maxSessions, err, tc.want)
				}
				break
			}
			if err != nil {
				t.Fatalf("MaxSessions %d: %v", tc.maxSessions, err)
			}
			if len(resp.Directory.Files) != 1 || resp.Directory.Files[0].Err != nil {
				t.Errorf("MaxSessions %d: files %+v", tc.maxSessions, resp.Directory.Files)
			}
		}
		if f.server.Accepted() != 1 {
			t.Errorf("MaxSessions %d: %d connections, want 1", tc.maxSessions, f.server.Accepted())
		}
		waitSessions(t, f)
	}
}

// A host frees a listing's session only when it closes its side, and a busy
// one may take a while to: a listing waits for that close before the scrape
// goes on, so the next scrape's listing is not refused a session the host
// has not freed yet, at a MaxSessions that allows the connection's SFTP
// session and one listing.
func TestSSHFileWaitsForTheHostToCloseAListingsSession(t *testing.T) {
	f := newSSHFixtureOptions(t, func(o *sshtest.Options) {
		o.MaxSessions = 2
		o.SessionCloseDelay = 200 * time.Millisecond
	})
	testutil.WriteIn(t, f.root, "a.prom", sshPromFile)
	c := f.collector(t, "dir", "", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} })
	for i := range 3 {
		resp, err := f.fetch(c, RequestOverrides{})
		if err != nil {
			t.Fatalf("scrape %d: %v", i+1, err)
		}
		if len(resp.Directory.Files) != 1 || resp.Directory.Files[0].Err != nil {
			t.Fatalf("scrape %d: files %+v", i+1, resp.Directory.Files)
		}
	}
	waitSessions(t, f)
}

// keepingSSHFixture is a fixture whose host serves every listing's session
// by serveSFTPScript's defaults and then, after the end of the session's
// input, does after: it never closes its side while after runs, so the
// host keeps counting the session. after ends when the test does, or when
// a write fails, the client having closed the session.
func keepingSSHFixture(t *testing.T, change func(*sshtest.Options), after func(channel io.ReadWriter, hold <-chan struct{})) *sshFixture {
	t.Helper()
	hold := make(chan struct{})
	f := newSSHFixtureOptions(t, func(o *sshtest.Options) {
		o.SFTP = func(n int, channel io.ReadWriter) bool {
			if n == 1 {
				return false
			}
			serveSFTPScript(channel, func(byte, uint32, []byte) ([]byte, bool) { return nil, false })
			after(channel, hold)
			return true
		}
		if change != nil {
			change(o)
		}
	})
	t.Cleanup(func() { close(hold) })
	return f
}

// holdSession keeps a listing's session open on the host until the test
// ends.
func holdSession(_ io.ReadWriter, hold <-chan struct{}) { <-hold }

// countHostCloseWaits counts the listings that wait for the host to close
// their session, until the test ends.
func countHostCloseWaits(t *testing.T) *atomic.Int64 {
	t.Helper()
	var waits atomic.Int64
	hook := func() { waits.Add(1) }
	sftpHostCloseWait.Store(&hook)
	t.Cleanup(func() { sftpHostCloseWait.Store(nil) })
	return &waits
}

// A host that never closes its side of a listing's session after the end of
// its input costs the scrapes of a connection the wait for that close
// once: the first listing waits, at most two seconds, and closes the
// session itself, its connection remembering the host by the time the
// scrape answers, and the listings after it on the connection close theirs
// without waiting. Before, every scrape waited two seconds, and never closed
// the session, its goroutine left reading it for the life of the
// connection; and the scrape could answer before the connection remembered
// the host, so that the next one waited too.
func TestSSHFileWaitsOnceForAHostThatKeepsListingSessionsOpen(t *testing.T) {
	f := keepingSSHFixture(t, nil, holdSession)
	waits := countHostCloseWaits(t)
	c := f.collector(t, "dir", "", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} })
	for i := range 3 {
		if _, err := f.fetch(c, RequestOverrides{}); err != nil {
			t.Fatalf("scrape %d: %v", i+1, err)
		}
		if entry := keptConn(f.server.Addr); entry == nil || !entry.listingSessionsKept.Load() {
			t.Fatalf("after scrape %d, the connection does not remember that its host keeps listing sessions open", i+1)
		}
		if n := waits.Load(); n != 1 {
			t.Errorf("after scrape %d, %d listings waited for the host, want the first alone", i+1, n)
		}
	}
	if n := f.server.Accepted(); n != 1 {
		t.Errorf("%d connections, want the scrapes on one", n)
	}
}

// A listing on a connection whose host is known to keep listing sessions
// open closes its session at once when it ends, and its conversation with
// the host ends with it; and a listing the host then refuses a session,
// still counting the earlier one, fails saying so rather than only that
// MaxSessions must be raised.
func TestSFTPListerClosesTheSessionOfAHostThatKeepsItOpen(t *testing.T) {
	f := keepingSSHFixture(t, func(o *sshtest.Options) { o.MaxSessions = 2 }, holdSession)
	entry := f.connect(t)
	waits := countHostCloseWaits(t)
	kept := &atomic.Bool{}
	kept.Store(true)
	first := newSFTPLister(entry.client, "/listed")
	first.hostKeepsSessions = kept
	if _, err := listAll(first); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	testutil.WaitFor(t, "the listing to close its session and end", func() bool { return isClosedChannel(first.done) })
	if n := waits.Load(); n != 0 {
		t.Errorf("%d listings waited for the host, want none", n)
	}
	second := newSFTPLister(entry.client, "/listed")
	second.hostKeepsSessions = kept
	_, err := listAll(second)
	_ = second.Close()
	if want := "the host refused an SSH session to list the directory in (ssh: rejected: administratively prohibited (\"open failed\")); it did not close the session of an earlier listing on this connection within 2s of the end of its input, and may still count it against its limit on sessions, MaxSessions in OpenSSH: the host's SFTP server must end when its input ends"; err == nil || err.Error() != want {
		t.Errorf("err=%v\nwant %s", err, want)
	}
}

// What the host still sends on a listing's session after the end of its
// input is read only up to two packets' worth before the session is closed:
// a host that sends without end after the end of the listing, or after a
// packet past the bound, does not hold the listing for the two seconds a
// host's close is waited for, nor is it read for as long as it sends.
func TestSFTPListerReadsABoundedAmountOfWhatTheHostSendsAtTheEnd(t *testing.T) {
	flood := func(channel io.ReadWriter, hold <-chan struct{}) {
		chunk := make([]byte, 32<<10)
		for {
			select {
			case <-hold:
				return
			default:
			}
			if _, err := channel.Write(chunk); err != nil {
				return
			}
		}
	}
	for _, tc := range []struct {
		name   string
		script sftpScript
	}{
		{"after the end of the directory", nil},
		{"after a packet past the bound", func(typ byte, _ uint32, _ []byte) ([]byte, bool) {
			if typ == sftpPacketReadDir {
				return sftpU32(1 << 30), false
			}
			return nil, false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold := make(chan struct{})
			t.Cleanup(func() { close(hold) })
			f := newSSHFixtureOptions(t, func(o *sshtest.Options) {
				o.SFTP = func(n int, channel io.ReadWriter) bool {
					if n == 1 {
						return false
					}
					if tc.script != nil {
						// The script's answer is the head of a packet the
						// flood is the rest of.
						serveSFTPScript(channel, func(typ byte, id uint32, body []byte) ([]byte, bool) {
							answer, _ := tc.script(typ, id, body)
							return answer, answer != nil
						})
					} else {
						serveSFTPScript(channel, func(byte, uint32, []byte) ([]byte, bool) { return nil, false })
					}
					flood(channel, hold)
					return true
				}
			})
			entry := f.connect(t)
			l := newSFTPLister(entry.client, "/listed")
			_, _ = listAll(l)
			// The wait ends having read two packets' worth, not at its
			// two seconds, which would mark the host.
			_ = l.Close()
			testutil.WaitFor(t, "the listing to close its session and end", func() bool { return isClosedChannel(l.done) })
			if l.hostKeepsSessions.Load() {
				t.Error("the host is taken for one that keeps listing sessions open")
			}
		})
	}
}

// A listing closed while the host has not answered the opening of its
// session is not waited for: Close returns before the host answers, and
// the session is closed when it does. Before, Close waited two seconds,
// holding the scrape past its deadline.
func TestSFTPListerIsNotWaitedForWhileTheHostOpensItsSession(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(sshtest.NewEd25519Key(t))
	opening := make(chan ssh.NewChannel, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_, channels, requests, err := ssh.NewServerConn(conn, config)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(requests)
		for channel := range channels {
			opening <- channel // answered only when the test says
		}
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{User: "x", HostKeyCallback: ssh.InsecureIgnoreHostKey()}) //nolint:gosec // G106: the test's own server
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	l := newSFTPLister(client, "/listed")
	read := make(chan error, 1)
	go func() {
		_, err := l.ReadDir(1)
		read <- err
	}()
	open := <-opening
	closed := make(chan struct{})
	go func() {
		_ = l.Close()
		close(closed)
	}()
	testutil.WaitFor(t, "Close to return while the host has not answered", func() bool { return isClosedChannel(closed) })
	if err := <-read; !errors.Is(err, errListingAbandoned) {
		t.Errorf("ReadDir: %v, want the listing abandoned", err)
	}
	channel, requests, err := open.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(requests)
	// The session the host opened late is closed by the listing.
	if _, err := io.Copy(io.Discard, channel); err != nil {
		t.Error(err)
	}
	testutil.WaitFor(t, "the listing to end", func() bool { return isClosedChannel(l.done) })
}

// A read abandoned at the probe's deadline while a listing waits for the
// host to close its session ends that wait, Close returning before the
// host closes anything, so the read releases its connection then. Before,
// the listing was forgotten by the read as its Close began, and waited its
// two seconds.
func TestSSHFileAbandonEndsTheWaitForAListingsSession(t *testing.T) {
	ended := make(chan struct{}, 1)
	f := keepingSSHFixture(t, nil, func(_ io.ReadWriter, hold <-chan struct{}) {
		ended <- struct{}{}
		<-hold
	})
	entry := f.connect(t)
	src := &sftpSource{client: entry.sftp, conn: entry.client, root: "/"}
	l := newSFTPLister(entry.client, "/listed")
	src.listing = map[*sftpLister]bool{l: true}
	d := &sftpDir{lister: l, src: src}
	if _, err := listAll(d); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		_ = d.Close()
		close(closed)
	}()
	<-ended // the listing has ended its input and waits for the host
	src.abandon()
	testutil.WaitFor(t, "Close to return after abandon", func() bool { return isClosedChannel(closed) })
	testutil.WaitFor(t, "the listing to close its session and end", func() bool { return isClosedChannel(l.done) })
	if l.hostKeepsSessions.Load() {
		t.Error("an abandoned listing takes its host for one that keeps listing sessions open")
	}
}

// Close and the first ReadDir of a listing may run at once, as the probe's
// deadline and the directory read do (go test -race).
func TestSFTPListerClosesWhileItStarts(t *testing.T) {
	f := newSSHFixture(t)
	entry := f.connect(t)
	for range 20 {
		l := newSFTPLister(entry.client, f.root)
		closed := make(chan struct{})
		go func() {
			_ = l.Close()
			close(closed)
		}()
		_, _ = l.ReadDir(1)
		<-closed
		testutil.WaitFor(t, "the listing to end", func() bool {
			l.mu.Lock()
			defer l.mu.Unlock()
			return !l.started || isClosedChannel(l.done)
		})
	}
}

// A host that answers request after request for directory entries with . and
// .. alone, or with no entry, is asked sixteen times in a row and no more:
// the listing fails saying so, and the directory is closed. Fewer in a row,
// or such answers between batches of entries, are asked past as before.
// Before, it was asked without end until the probe's deadline, none of
// those answers counting toward the listing's bound.
func TestSFTPListerStopsAHostAnsweringWithoutEntries(t *testing.T) {
	f, script := scriptedSSHFixture(t, nil)
	entry := f.connect(t)
	dots := func(id uint32) []byte { return sftpNamePacket(id, 2, sftpTestEntry(".", 0), sftpTestEntry("..", 0)) }
	none := func(id uint32) []byte { return sftpNamePacket(id, 0) }
	file := func(id uint32) []byte { return sftpNamePacket(id, 1, sftpTestEntry("f", 1)) }
	repeat := func(n int, answer func(uint32) []byte) []func(uint32) []byte {
		return slices.Repeat([]func(uint32) []byte{answer}, n)
	}
	for _, tc := range []struct {
		name    string
		answers []func(uint32) []byte // then the end of the directory
		entries int
		err     bool
	}{
		{". and .. without end", repeat(1000, dots), 0, true},
		{"no entry without end", repeat(1000, none), 0, true},
		{"fifteen in a row, then an entry", append(repeat(15, dots), file), 1, false},
		{"fifteen in a row between entries", slices.Concat(repeat(15, none), []func(uint32) []byte{file}, repeat(15, dots), []func(uint32) []byte{file}), 2, false},
		{"sixteen in a row after an entry", append([]func(uint32) []byte{file}, repeat(16, dots)...), 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked int
			var closedDir atomic.Bool
			s := sftpScript(func(typ byte, id uint32, _ []byte) ([]byte, bool) {
				switch typ {
				case sftpPacketReadDir:
					if asked++; asked <= len(tc.answers) {
						return tc.answers[asked-1](id), false
					}
				case sftpPacketClose:
					closedDir.Store(true)
				}
				return nil, false
			})
			script.Store(&s)
			l := newSFTPLister(entry.client, "/listed")
			entries, err := listAll(l)
			_ = l.Close()
			waitSessions(t, f)
			want := ""
			if tc.err {
				want = "the host's SFTP server answered 16 requests for directory entries in a row with no entry but . and .., rather than with entries or the end of the directory; the listing was stopped"
			}
			if got := fmt.Sprint(err); err != nil && got != want || err == nil && want != "" {
				t.Errorf("err=%v, want %q", err, want)
			}
			if len(entries) != tc.entries {
				t.Errorf("%d entries, want %d", len(entries), tc.entries)
			}
			if tc.err && asked != sftpMaxEmptyBatches+tc.entries {
				t.Errorf("the host was asked %d times", asked)
			}
			if !closedDir.Load() {
				t.Error("the directory was not closed")
			}
		})
	}
}

// FuzzParseSFTPNames holds the parser of a NAME packet to its bounds on any
// input: it never panics, never answers more entries than the packet's
// count, and every entry it answers has a name in a directory, not . or ..
// (go test -fuzz FuzzParseSFTPNames ./internal/fetch/).
func FuzzParseSFTPNames(f *testing.F) {
	f.Add(slices.Concat(sftpU32(2), sftpTestEntry("a.prom", 10), sftpTestEntry(".", 0)))
	f.Add(slices.Concat(sftpU32(1), sshString("x"), sshString("x"), sftpU32(sftpAttrSize|sftpAttrUIDGID|sftpAttrPermissions|sftpAttrACModTime|sftpAttrExtended), make([]byte, 24), sftpU32(1), sshString("k"), sshString("v")))
	f.Add(slices.Concat(sftpU32(3), sftpTestEntry("a", 1)))
	f.Add(slices.Concat(sftpU32(1), sftpTestEntry("a/b", 1)))
	f.Add([]byte{0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseSFTPNames(data)
		if err != nil {
			return
		}
		count, _, _ := sshUint32(data)
		if uint64(len(entries)) > uint64(count) {
			t.Fatalf("%d entries of a count of %d", len(entries), count)
		}
		for _, e := range entries {
			if name := e.Name(); name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
				t.Fatalf("entry %q", name)
			}
		}
	})
}

// A host whose SFTP server ends a listing's session, at its start or in the
// middle of the listing, while the SSH connection stays up, fails the
// directory read saying its SFTP server ended the session, and the
// connection is kept: no new one is made, at that scrape or the next. Before,
// the end of the session was taken for the connection lost, so the
// connection was dropped and made again, twice a scrape, and the read failed
// saying the connection was lost.
func TestSSHFileKeepsTheConnectionOfAListingWhoseSessionTheHostEnded(t *testing.T) {
	for _, at := range []byte{sftpPacketInit, sftpPacketReadDir} {
		f, script := scriptedSSHFixture(t, nil)
		entry := f.connect(t)
		events := sshEvents(t, f.server.Addr)
		s := sftpScript(func(typ byte, id uint32, _ []byte) ([]byte, bool) {
			if typ == at {
				return []byte{}, true
			}
			return nil, false
		})
		script.Store(&s)
		c := f.collector(t, "dir", "", func(r *model.RequestConfig) { r.Files = []string{"*.prom"} })
		for i := range 2 {
			_, err := f.fetch(c, RequestOverrides{})
			if err == nil || !strings.Contains(err.Error(), errListingSessionEnded.Error()) {
				t.Errorf("at packet %d, scrape %d: err=%v, want %q", at, i+1, err, errListingSessionEnded)
			}
		}
		if n := f.server.Accepted(); n != 1 || keptConn(f.server.Addr) != entry {
			t.Errorf("at packet %d: %d connections, the one kept %v rather than %v; want the first kept", at, n, keptConn(f.server.Addr), entry)
		}
		for len(events) > 0 {
			if event := <-events; strings.HasPrefix(event, "closed") {
				t.Errorf("at packet %d: the connection was %s", at, event)
			}
		}
		waitSessions(t, f)
	}
}

// A listing whose session ends because the connection ended under it still
// takes the connection for lost: the listing fails as on a lost connection,
// and a directory read is made again on a new connection, and lists the
// directory.
func TestSSHFileListsAgainOnANewConnectionWhenTheListingLosesIt(t *testing.T) {
	var server atomic.Pointer[sshtest.Server]
	// dropAt is the SFTP session, counted across connections, at whose
	// start the server drops its connections: a listing's, never a
	// connection's own.
	var dropAt atomic.Int64
	f := newSSHFixtureOptions(t, func(o *sshtest.Options) {
		o.SFTP = func(n int, _ io.ReadWriter) bool {
			if int64(n) == dropAt.Load() {
				server.Load().DropConnections()
				return true
			}
			return false
		}
	})
	server.Store(f.server)
	entry := f.connect(t)
	dropAt.Store(2)
	lister := newSFTPLister(entry.client, f.root)
	_, err := listAll(lister)
	_ = lister.Close()
	if !errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, errListingSessionEnded) {
		t.Errorf("err=%v, want the connection lost", err)
	}
	testutil.WriteIn(t, f.root, "a.prom", sshPromFile)
	c := f.collector(t, "dir", "", func(r *model.RequestConfig) { r.Files = []string{"a.prom"} })
	// The new connection's own session is the third, its listing's the
	// fourth, and the next connection's listing the sixth.
	dropAt.Store(4)
	resp, err := f.fetch(c, RequestOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Directory.Files) != 1 || resp.Directory.Files[0].Err != nil {
		t.Errorf("files %+v, want a.prom read", resp.Directory.Files)
	}
	if n := f.server.Accepted(); n != 3 {
		t.Errorf("%d connections, want a third for the read that lost the second", n)
	}
}

// The errors a listing ends with are those it ended with before, but for a
// session that ended on a connection still up: the end of the directory,
// the statuses, the refusals of a malformed answer, the abandon and no error
// at all are handed out as they were, and so is a session that ended on a
// connection that has ended, which stays a lost connection; a session that
// ended on a connection still up alone says its SFTP server ended it.
func TestSFTPListerChangesOnlyTheErrorOfASessionEndedWithItsConnectionUp(t *testing.T) {
	f := newSSHFixture(t)
	entry := f.connect(t)
	ended := sftpSessionEnded(io.ErrUnexpectedEOF)
	others := []error{nil, io.EOF, fs.ErrNotExist, fs.ErrPermission, errListingAbandoned, sftpStatusError(slices.Concat(sftpU32(4), sshString("failure"))), errors.New("the host's SFTP server sent an empty packet"), sftpSessionEnded(errors.New("write failed")), fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF)}
	up := newSFTPLister(entry.client, "/")
	for _, err := range others {
		// The old lister handed out err itself.
		if got := up.sessionOnly(err); got != err { //nolint:errorlint // the very error is what is compared
			t.Errorf("sessionOnly(%v) = %v, want it unchanged", err, got)
		}
	}
	if got := up.sessionOnly(ended); got != errListingSessionEnded { //nolint:errorlint // the very error is what is compared
		t.Errorf("on a connection up, sessionOnly(%v) = %v, want %v", ended, got, errListingSessionEnded)
	}
	sshConns.drop(entry, "test")
	down := newSFTPLister(entry.client, "/")
	for _, err := range append(others, ended) {
		if got := down.sessionOnly(err); got != err { //nolint:errorlint // the very error is what is compared
			t.Errorf("on a connection ended, sessionOnly(%v) = %v, want it unchanged", err, got)
		}
	}
}
