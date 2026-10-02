package fetch

import "io"

// A response's body is read whole into memory, on every probe, and how the
// memory for it is come by showed in a probe's allocations: io.ReadAll, which
// cannot know how long a body is, reads a body into buffers of growing size
// and copies them along, allocating about twice the body. So a body whose
// length is known beforehand, from the Content-Length of an answer or the
// size of a file, is read by readBody straight into a buffer of that length,
// with nothing copied. A body of unknown length, as a chunked or a
// compressed answer is, is read as before.
//
// Neither reads a byte more than io.ReadAll(io.LimitReader(r, limit+1)) did,
// and a length is only ever a hint for the size of the buffer: a body longer
// or shorter than it said is read as it comes.

// maxFirstBodyBuffer is the largest buffer made on the word of a declared
// length alone, before a byte of the body has been read. A longer body is
// read into buffers that double from there, each made once the one before it
// has been filled with what really came.
const maxFirstBodyBuffer = 64 << 20

// readBody reads r to its end, but no further than one byte past limit, and
// returns what it read, as io.ReadAll(io.LimitReader(r, limit+1)) does: a
// result longer than limit says that the body is too long, without saying by
// how much, and a failed read returns its error with what was read before it.
// The result is never nil.
//
// declared is the length the body is said to have, or 0 or less when nothing
// says.
func readBody(r io.Reader, limit, declared int64) ([]byte, error) {
	if declared > 0 {
		return readSizedBody(r, limit, declared, maxFirstBodyBuffer)
	}
	return readUnsizedBody(r, limit)
}

// readSizedBody reads a body said to be declared bytes long into one buffer
// of that length and a byte more, in which the read that finds the end, or
// finds the body to be over the limit, has room. The buffer is no longer than
// the limit allows nor than first, which is maxFirstBodyBuffer but in a test,
// whatever was declared, and grows when the body turns out longer than it.
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
