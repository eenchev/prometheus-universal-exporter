package fetch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// bodySource is a body as a connection or a file hands it over: in pieces of
// sizes of its own, some of them of nothing, ending with an error or with
// io.EOF, which may come with the last bytes or after them. It counts what it
// handed over.
type bodySource struct {
	data []byte
	// pieces are the sizes of the reads, gone through in a circle.
	pieces []int
	// failAfter, when not negative, is how many bytes are read before err.
	failAfter  int
	err        error
	eofAtOnce  bool
	read, call int
}

func (s *bodySource) Read(p []byte) (int, error) {
	end := len(s.data)
	if s.failAfter >= 0 {
		end = min(end, s.failAfter)
	}
	n := min(len(p), end-s.read)
	if len(s.pieces) > 0 {
		n = min(n, s.pieces[s.call%len(s.pieces)])
		s.call++
	}
	copy(p, s.data[s.read:s.read+n])
	s.read += n
	if s.read < end {
		return n, nil
	}
	if n > 0 && !s.eofAtOnce {
		return n, nil
	}
	if s.failAfter >= 0 {
		return n, s.err
	}
	return n, io.EOF
}

// bodyCase is one body and how it is read.
type bodyCase struct {
	size            int
	limit, declared int64
	// first is the largest first buffer, where it is not the exporter's
	// maxFirstBodyBuffer: outgrowing that one would take a body of 64 MiB.
	first     int64
	pieces    []int
	failAfter int
	eofAtOnce bool
}

func (c bodyCase) String() string {
	return fmt.Sprintf("size=%d limit=%d declared=%d first=%d pieces=%v failAfter=%d eofAtOnce=%v", c.size, c.limit, c.declared, c.first, c.pieces, c.failAfter, c.eofAtOnce)
}

var errBodyBroke = errors.New("the connection broke")

// bodyBytes is what the bodies of the tests below are cut from: 5 MiB that
// do not repeat.
var bodyBytes = sync.OnceValue(func() []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, 5<<20)
	for i := 0; i < len(b); i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.Uint64())
	}
	return b
})

// checkBodyRead holds readBody against io.ReadAll(io.LimitReader(r, limit+1)),
// which it replaced, for one body: the same bytes, the same error, never nil,
// and not a byte more taken from the source.
func checkBodyRead(t *testing.T, c bodyCase) {
	t.Helper()
	source := func() *bodySource {
		return &bodySource{data: bodyBytes()[:c.size], pieces: c.pieces, failAfter: c.failAfter, err: errBodyBroke, eofAtOnce: c.eofAtOnce}
	}
	was := source()
	want, wantErr := io.ReadAll(io.LimitReader(was, c.limit+1))
	is := source()
	var got []byte
	var err error
	if c.declared > 0 && c.first > 0 {
		got, err = readSizedBody(is, c.limit, c.declared, c.first)
	} else {
		got, err = readBody(is, c.limit, c.declared)
	}
	switch {
	case !errors.Is(err, wantErr):
		t.Errorf("%v: error %v, io.ReadAll's %v", c, err, wantErr)
	case !bytes.Equal(got, want):
		t.Errorf("%v: read %d bytes, io.ReadAll %d, or others", c, len(got), len(want))
	case got == nil:
		t.Errorf("%v: a nil body", c)
	case is.read != was.read:
		t.Errorf("%v: took %d bytes from the source, io.ReadAll %d", c, is.read, was.read)
	}
}

// A body is read as io.ReadAll read it through a reader stopping one byte
// past the limit: with no length declared, the right one, one too small or
// too large, or one beyond the limit; shorter than, at and over the limit;
// empty; at and around round sizes and the size at which the first buffer is
// full; handed over whole or in pieces,
// with the end coming with the last bytes or after them; and broken off by an
// error at the start, in the middle or at the end. Under the race detector,
// which follows every byte that is copied, the small bodies are every
// eleventh of their table, among which each way of any three things still
// meets each of the others, and the bodies of more than a megabyte and a
// byte are left to a run without it.
func TestABodyIsReadAsBefore(t *testing.T) {
	const unlimited = math.MaxInt64 - 1
	pooled := testBodyStep * 32
	cases := 0
	check := func(c bodyCase) {
		// As the exporter reads it, unless that would take 64 MiB.
		if c.first == 0 && min(c.declared, c.limit) > 8<<20 {
			return
		}
		checkBodyRead(t, c)
		cases++
	}
	// Small bodies, every way.
	at, every := 0, alloctest.UnlessRaced(1, 11)
	for _, size := range []int{0, 1, 2, 511, 512, 513, 2500} {
		for _, limit := range []int64{0, 1, int64(size) - 1, int64(size), int64(size) + 1, int64(size) / 2, 10 << 20, unlimited} {
			for _, declared := range []int64{-1, 0, 1, int64(size) - 1, int64(size), int64(size) + 1, int64(size) / 2, 2 * int64(size), 1 << 50} {
				for _, first := range []int64{0, 1, 1000} {
					for _, pieces := range [][]int{nil, {1 << 30}, {100}, {700, 0, 1, 330}} {
						for _, failAfter := range []int{-1, 0, size / 2, size} {
							for _, eofAtOnce := range []bool{false, true} {
								if at++; at%every != 0 {
									continue
								}
								if limit >= 0 && (declared > 0 || first == 0) {
									check(bodyCase{size, limit, declared, first, pieces, failAfter, eofAtOnce})
								}
							}
						}
					}
				}
			}
		}
	}
	// Bodies of a round size or a few, to the byte or all but.
	for _, size := range []int{testBodyStep - 1, testBodyStep, testBodyStep + 1, 3*testBodyStep + 7} {
		for _, limit := range []int64{int64(size) - 1, int64(size), int64(size) + 1, unlimited} {
			for _, declared := range []int64{-1, int64(size), int64(size) / 2, 1 << 50} {
				for _, pieces := range [][]int{nil, {70000, 0, 1, 33000}} {
					check(bodyCase{size, limit, declared, 0, pieces, -1, false})
					check(bodyCase{size, limit, declared, 1000, pieces, -1, true})
				}
			}
		}
		check(bodyCase{size, unlimited, -1, 0, []int{4096}, size / 2, false})
		check(bodyCase{size, unlimited, int64(size), 0, []int{4096}, size, true})
	}
	// Bodies of a megabyte and more: fewer of these.
	large := []int{pooled - 1, pooled, pooled + 1, pooled + 16*testBodyStep + 1, 4<<20 + 3}
	if alloctest.RaceDetector {
		large = large[:3]
	}
	for _, size := range large {
		check(bodyCase{size, unlimited, -1, 0, nil, -1, false})
		check(bodyCase{size, int64(size), -1, 0, []int{70000, 0, 1, 33000}, -1, true})
		check(bodyCase{size, int64(size) - 1, -1, 0, nil, -1, false})
		check(bodyCase{size, unlimited, -1, 0, nil, size - 1, false})
		check(bodyCase{size, int64(size), int64(size), 0, nil, -1, false})
		check(bodyCase{size, unlimited, int64(size) / 2, 1000, []int{70000, 0, 1, 33000}, -1, true})
	}
	t.Logf("%d bodies", cases)
}

// The same over bodies made at random from a fixed seed: 3,000 of them, of
// sizes up to 5 MiB but nearly all small or near a round size, with limits and declared lengths near the size and far from it.
// Under the race detector they are the first 600 of those.
func TestARandomBodyIsReadAsBefore(t *testing.T) {
	r := rand.New(rand.NewPCG(20261002, 2))
	near := func(n int64) int64 { return max(0, n+int64(r.IntN(5))-2) }
	for i := 0; i < alloctest.UnlessRaced(3000, 600) && !t.Failed(); i++ {
		var size int
		switch n := r.IntN(200); {
		case n == 0:
			size = r.IntN(len(bodyBytes()) + 1)
		case n < 3:
			size = int(near(32 * testBodyStep))
		case n < 40:
			size = int(near(int64(testBodyStep * (1 + r.IntN(3)))))
		case n < 60:
			size = r.IntN(300000)
		default:
			size = r.IntN(3000)
		}
		c := bodyCase{size: size, failAfter: -1, eofAtOnce: r.IntN(2) == 0}
		c.limit = []int64{near(int64(size)), near(int64(size)), int64(r.IntN(size + 1)), 10 << 20, math.MaxInt64 - 1}[r.IntN(5)]
		c.declared = []int64{-1, 0, int64(size), int64(size), near(int64(size)), int64(r.IntN(size + 1)), 2 * int64(size), 1 << 50}[r.IntN(8)]
		c.first = []int64{1, 100, 4096, 50000}[r.IntN(4)]
		if min(c.declared, c.limit) <= 8<<20 && r.IntN(2) == 0 {
			// As the exporter reads it, with the first buffer it allows.
			c.first = 0
		}
		for n := r.IntN(4); n > 0; n-- {
			c.pieces = append(c.pieces, []int{0, 1, 100, 4096, 32768, 100000, 1 << 30}[r.IntN(7)])
		}
		if slicesAllZero(c.pieces) || size > 100000 && slices.Contains(c.pieces, 1) {
			// Pieces of nothing never end, and a megabyte a byte at a
			// time takes too long.
			c.pieces = nil
		}
		if r.IntN(5) == 0 {
			c.failAfter = r.IntN(size + 1)
		}
		checkBodyRead(t, c)
	}
}

func slicesAllZero(pieces []int) bool {
	for _, p := range pieces {
		if p != 0 {
			return false
		}
	}
	return true
}

// A declared length is not taken at its word for more than the limit allows
// nor for more than the first buffer may be: a body said to be a petabyte
// long is read into a buffer of the limit and a byte, or of the largest first
// buffer and a byte, and one said to be longer than it is into a buffer of
// what was said.
func TestADeclaredLengthIsOnlyAHint(t *testing.T) {
	for _, test := range []struct {
		limit, declared, first int64
		want                   int
	}{
		{limit: 1000, declared: 1 << 50, first: 1 << 20, want: 1001},
		{limit: math.MaxInt64 - 1, declared: 1 << 50, first: 4096, want: 4097},
		{limit: 1 << 20, declared: 500, first: 1 << 20, want: 501},
		{limit: 1 << 20, declared: 3, first: 1 << 20, want: 4},
	} {
		body, err := readSizedBody(bytes.NewReader([]byte("abc")), test.limit, test.declared, test.first)
		if err != nil || string(body) != "abc" || cap(body) != test.want {
			t.Errorf("limit=%d declared=%d first=%d: %q in a buffer of %d, err=%v; want a buffer of %d", test.limit, test.declared, test.first, body, cap(body), err, test.want)
		}
	}
}

// allocatedPerRun is how many bytes and how many allocations one call of f
// takes, over several calls after a first one: the least of the measurements
// alloctest makes, since what the goroutines of the tests before this one
// allocate meanwhile is counted with it.
func allocatedPerRun(f func()) (bytesPerRun uint64, allocations float64) {
	allocations, bytesPerRun = alloctest.Allocations(8, f)
	return bytesPerRun, allocations
}

// testBodyStep is a size around which the bodies of the tests are taken, so
// that they end just before, at and just after a round number of bytes.
const testBodyStep = 32 << 10

// megabyteBody is the size of the body whose reading the tests below
// measure: a megabyte, all but a few bytes, as an answer is of no round size.
const megabyteBody = 1<<20 - 1234

// A megabyte with a declared length is read into one buffer: one allocation,
// of the megabyte. io.ReadAll took 2.1 MiB in 25 allocations for it, reading
// into buffers of growing size and copying them into one. The bounds are
// between the two.
func TestABodyOfDeclaredLengthIsAllocatedOnce(t *testing.T) {
	data := bodyBytes()[:megabyteBody]
	source := bytes.NewReader(data)
	size, allocations := allocatedPerRun(func() {
		source.Reset(data)
		if body, err := readBody(source, 10<<20, int64(len(data))); err != nil || len(body) != len(data) {
			t.Fatalf("%d bytes, err=%v", len(body), err)
		}
	})
	if size > 3<<19 || allocations > 4 {
		t.Errorf("reading %d bytes of declared length allocated %d bytes in %v allocations, want little more than the body, at once", len(data), size, allocations)
	}
}
