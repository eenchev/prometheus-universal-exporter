package fetch

import (
	"hash/maphash"
	"io"
	"slices"
	"sync/atomic"
)

// A response's body is read whole into memory on every probe, and how that
// memory was obtained showed in a probe's allocations. io.ReadAll cannot
// know how long a body is. It reads into buffers of growing size and copies
// them along, allocating about twice the body. So:
//   - A body whose length is known beforehand, from an answer's
//     Content-Length or a file's size, is read by readBody straight into a
//     buffer of that length, with nothing copied.
//   - A body of unknown length, such as a chunked or a compressed answer,
//     is read by readHintedBody into a buffer the size of the last answer
//     of the same collector and target. The next answer is most often that
//     size, or within a few percent of it.
//
// Neither reads a byte more than io.ReadAll(io.LimitReader(r, limit+1))
// did. A length is only ever a hint for the size of the buffer: a body
// longer or shorter than it said is read as it comes.

// maxFirstBodyBuffer is the largest buffer made on a declared length alone,
// before any byte of the body has been read. A longer body is read into
// buffers that double from there. Each is made once the one before it has
// been filled with what really came.
const maxFirstBodyBuffer = 64 << 20

// readBody reads r to its end, but no further than one byte past limit, and
// returns what it read, as io.ReadAll(io.LimitReader(r, limit+1)) does. A
// result longer than limit says that the body is too long, without saying
// by how much. A failed read returns its error with what was read before
// it. The result is never nil.
//
// declared is the length the body is said to have, or 0 or less when
// nothing says.
func readBody(r io.Reader, limit, declared int64) ([]byte, error) {
	if declared > 0 {
		return readSizedBody(r, limit, declared, maxFirstBodyBuffer)
	}
	return readUnsizedBody(r, limit)
}

// readSizedBody reads a body said to be declared bytes long into one buffer
// of that length plus one byte. That byte gives room to the read that finds
// the end, or finds the body over the limit. Whatever was declared, the
// buffer is no longer than the limit allows, nor than first (which is
// maxFirstBodyBuffer except in a test). It grows when the body turns out
// longer.
func readSizedBody(r io.Reader, limit, declared, first int64) ([]byte, error) {
	sized := min(declared, limit) + 1
	body := make([]byte, 0, min(sized, first+1))
	for {
		if len(body) == cap(body) {
			// Twice the size, up to the declared length while the body
			// is still within it, and up to the limit beyond.
			most := limit + 1
			if int64(cap(body)) < sized {
				most = sized
			}
			size := most
			if int64(cap(body)) <= most/2 {
				size = 2 * int64(cap(body))
			}
			grown := make([]byte, len(body), size)
			copy(grown, body)
			body = grown
		}
		// The buffer is never longer than limit+1, so neither is what
		// is read.
		n, err := r.Read(body[len(body):cap(body)])
		body = body[:len(body)+n]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return body, err
		}
		if int64(len(body)) > limit {
			return body, nil
		}
	}
}

// readUnsizedBody reads a body of unknown length as io.ReadAll does, into a
// buffer that grows as the body comes.
func readUnsizedBody(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit+1))
}

// The size of the last answer without a length is kept for each collector
// and target in one of a fixed number of slots, found by a hash of the two.
// So the memory it takes is bounded whatever targets are probed. Two that
// share a slot only give each other's first buffer the wrong size, which is
// the worst a hint can do. A slot is read and written atomically, because
// probes of one collector and target run at once.
const bodySizeHintSlots = 1024

// maxBodySizeHint is the largest first buffer made on the word of a hint: one
// answer of a hundred megabytes must not have every later one, perhaps of a
// kilobyte, start in a buffer of that size. A longer body grows from there.
const maxBodySizeHint = 4 << 20

var (
	bodySizeHints    [bodySizeHintSlots]atomic.Int64
	bodySizeHintSeed = maphash.MakeSeed()
)

// bodySizeHintOf is the slot of the hint of the collector named collector
// probing target.
func bodySizeHintOf(collector, target string) *atomic.Int64 {
	var h maphash.Hash
	h.SetSeed(bodySizeHintSeed)
	_, _ = h.WriteString(collector)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(target)
	return &bodySizeHints[h.Sum64()%bodySizeHintSlots]
}

// nextBodySizeHint is the hint an answer of size bytes leaves: its size with
// an eighth more, so that an answer a little longer still fits, up to
// maxBodySizeHint and the limit.
func nextBodySizeHint(size int, limit int64) int64 {
	return min(int64(size)+int64(size)/8+512, maxBodySizeHint, limit)
}

// readHintedBody reads a body of unknown length as readBody does, into a
// buffer of the size hint holds (the last answer's), and stores in hint the
// size of this one. With no hint yet it reads as io.ReadAll does.
//
// The body belongs to the response and is kept as long as the response is,
// by the cache too, so it cannot be pooled. A buffer more than twice the
// body, from an answer much shorter than the last, is replaced by one of
// the body's size. So the memory the response holds is never much more
// than its body.
func readHintedBody(r io.Reader, limit int64, hint *atomic.Int64) ([]byte, error) {
	var body []byte
	var err error
	if size := hint.Load(); size > 0 {
		body, err = readSizedBody(r, limit, size, maxFirstBodyBuffer)
	} else {
		body, err = readUnsizedBody(r, limit)
	}
	if err != nil || int64(len(body)) > limit {
		return body, err
	}
	// An empty body says nothing of the size of the next.
	if len(body) > 0 {
		hint.Store(nextBodySizeHint(len(body), limit))
	}
	if cap(body) > 2*len(body)+4096 {
		body = slices.Clone(body)
	}
	return body, nil
}
