//go:build request_type_smtp || request_type_ftp || request_type_websocket

package fetch

import (
	"bufio"
	"errors"
)

// errNetLineTooLong is a line past its limit, which each type words.
var errNetLineTooLong = errors.New("the server sent a line longer than a reply may be")

// netLine reads one line, its line feed included, of at most limit bytes:
// a server that sends no line feed is not read into memory until the
// scrape's deadline, as bufio's ReadString would read it.
func netLine(r *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > limit {
			return "", errNetLineTooLong
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return string(line), err
		}
	}
}
