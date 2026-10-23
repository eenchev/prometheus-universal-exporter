//go:build request_type_redis

package fetch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The redis request type speaks RESP2, the protocol every Redis and Valkey
// server answers a client in until it asks for RESP3 with HELLO 3, which
// this client never sends. A command is an array of bulk strings; a reply
// is one of five kinds, told apart by its first byte:
//
//	+OK\r\n                    simple string
//	-ERR unknown command\r\n   error
//	:42\r\n                    integer
//	$5\r\nhello\r\n            bulk string, $-1\r\n for nil
//	*2\r\n:1\r\n:2\r\n         array of replies, *-1\r\n for nil
//
// Everything the server sends is read against one budget, the collector's
// response limit, counted in the bytes of the replies as they come off the
// wire. A length the server declares is checked against what is left of the
// budget before anything is read for it, and nothing is allocated for it
// beyond what the server has actually sent: a bulk string's length of a
// gigabyte, followed by nothing, costs the bytes that came and a failed
// scrape, never a gigabyte. An array's length is checked against the least
// its elements could take on the wire, three bytes each, and arrays nest
// no deeper than redisMaxDepth.

// redisMaxLine is the longest line of a reply other than a bulk string's
// data: a reply's kind and length, an integer, a simple string or an
// error's text, none of which a server makes long.
const redisMaxLine = 64 << 10

// redisMaxDepth is how deeply arrays of a reply may nest. XINFO STREAM's,
// the deepest of the commands a collector may send, nests four deep.
const redisMaxDepth = 16

// redisSimple is a simple string reply, such as OK.
type redisSimple string

// redisError is an error reply: the command failed, and the server said
// why.
type redisError struct{ text string }

func (e *redisError) Error() string { return "the server answered: " + e.text }

// errRedisProtocol marks a reply that is no RESP2 reply at all.
var errRedisProtocol = errors.New("not a RESP2 reply")

// redisConn is one connection to a server, with the budget of what its
// replies may still take.
type redisConn struct {
	conn  net.Conn
	r     *bufio.Reader
	w     *bufio.Writer
	limit int64
	read  int64
}

func newRedisConn(conn net.Conn, limit int64) *redisConn {
	return &redisConn{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn), limit: limit}
}

// do sends a command and reads its reply.
func (s *redisConn) do(args []string) (any, error) {
	if err := s.send(args); err != nil {
		return nil, err
	}
	return s.reply(0)
}

// send writes a command as an array of bulk strings.
func (s *redisConn) send(args []string) error {
	_, _ = fmt.Fprintf(s.w, "*%d\r\n", len(args))
	for _, arg := range args {
		_, _ = fmt.Fprintf(s.w, "$%d\r\n", len(arg))
		_, _ = s.w.WriteString(arg)
		_, _ = s.w.WriteString("\r\n")
	}
	return s.w.Flush()
}

// charge counts n bytes of a reply against the budget.
func (s *redisConn) charge(n int64) error {
	s.read += n
	if s.read > s.limit {
		return s.overLimit()
	}
	return nil
}

// left is what the budget has left.
func (s *redisConn) left() int64 { return s.limit - s.read }

func (s *redisConn) overLimit() error {
	return model.MarkError(model.Errorf("the replies are larger than the response limit of %d bytes; raise request.max_response_bytes or limits.max_response_bytes, or ask for less", model.Size(s.limit)), model.ErrLimitExceeded)
}

// line reads a line of a reply, without its CRLF.
func (s *redisConn) line() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := s.r.ReadSlice('\n')
		if chargeErr := s.charge(int64(len(chunk))); chargeErr != nil {
			return nil, chargeErr
		}
		if len(buf)+len(chunk) > redisMaxLine {
			return nil, fmt.Errorf("%w: a line of the reply is longer than %d bytes", errRedisProtocol, redisMaxLine)
		}
		buf = append(buf, chunk...)
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
	if len(buf) < 2 || buf[len(buf)-2] != '\r' {
		return nil, fmt.Errorf("%w: a line of the reply does not end in CRLF", errRedisProtocol)
	}
	return buf[:len(buf)-2], nil
}

// length reads the length of a bulk string or an array, -1 for nil.
func redisLength(text []byte) (int64, error) {
	n, err := strconv.ParseInt(string(text), 10, 64)
	if err != nil || n < -1 {
		return 0, fmt.Errorf("%w: the length %q", errRedisProtocol, redisShown(string(text)))
	}
	return n, nil
}

// reply reads one reply, at the depth of the arrays around it.
func (s *redisConn) reply(depth int) (any, error) {
	line, err := s.line()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, fmt.Errorf("%w: an empty line", errRedisProtocol)
	}
	kind, text := line[0], line[1:]
	switch kind {
	case '+':
		return redisSimple(text), nil
	case '-':
		return &redisError{text: redisShown(string(text))}, nil
	case ':':
		n, err := strconv.ParseInt(string(text), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: the integer %q", errRedisProtocol, redisShown(string(text)))
		}
		return n, nil
	case '$':
		n, err := redisLength(text)
		if err != nil || n == -1 {
			return nil, err
		}
		if n > s.left()-2 {
			return nil, s.overLimit()
		}
		// Read as it comes, so that a length the server does not go on to
		// send costs what it did send.
		var data bytes.Buffer
		data.Grow(int(min(n+2, 64<<10)))
		if _, err := io.CopyN(&data, s.r, n+2); err != nil {
			return nil, err
		}
		if err := s.charge(n + 2); err != nil {
			return nil, err
		}
		body := data.Bytes()
		if !bytes.HasSuffix(body, []byte("\r\n")) {
			return nil, fmt.Errorf("%w: a bulk string does not end in CRLF", errRedisProtocol)
		}
		return body[:n], nil
	case '*':
		n, err := redisLength(text)
		if err != nil || n == -1 {
			return nil, err
		}
		if depth >= redisMaxDepth {
			return nil, fmt.Errorf("%w: arrays nest more than %d deep", errRedisProtocol, redisMaxDepth)
		}
		// Each element takes three bytes on the wire at the least.
		if n > s.left()/3 {
			return nil, s.overLimit()
		}
		items := make([]any, 0, min(n, 1024))
		for range n {
			item, err := s.reply(depth + 1)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	}
	return nil, fmt.Errorf("%w: a reply begins with %q, which no RESP2 reply does; is the target a Redis or Valkey server?", errRedisProtocol, redisShown(string(kind)))
}

// redisShown is text the server sent, as an error or a debug report shows
// it: control characters written as ?, no longer than any failure is shown.
func redisShown(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, text)
	return model.CutTo(text, model.MaxFailureBytes, false)
}

// redisReplyShape is how a command's reply becomes JSON beyond the kinds'
// own mapping.
type redisReplyShape int

const (
	// redisPlain maps a reply kind by kind: integer to number, bulk and
	// simple strings to strings, nil to null, an array to a list.
	redisPlain redisReplyShape = iota
	// redisPairs reads a flat array of name, value, name, value as an
	// object, as HGETALL and XINFO STREAM answer.
	redisPairs
	// redisPairsList reads an array of such arrays as a list of objects,
	// as XINFO GROUPS and XINFO CONSUMERS answer.
	redisPairsList
	// redisInfoText reads a bulk string of name:value lines as an object,
	// as CLUSTER INFO answers.
	redisInfoText
)

// redisJSON is a reply as the document holds it, in its shape. An error
// reply inside an array fails it, as a command's own does.
func redisJSON(reply any, shape redisReplyShape) (any, error) {
	switch shape {
	case redisPairs:
		return redisObject(reply)
	case redisPairsList:
		items, ok := reply.([]any)
		if !ok {
			return nil, fmt.Errorf("the reply is %s, not a list of name and value pairs", redisKind(reply))
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			object, err := redisObject(item)
			if err != nil {
				return nil, err
			}
			out = append(out, object)
		}
		return out, nil
	case redisInfoText:
		text, ok := reply.([]byte)
		if !ok {
			return nil, fmt.Errorf("the reply is %s, not text of name:value lines", redisKind(reply))
		}
		values := map[string]any{}
		for line := range strings.SplitSeq(string(text), "\n") {
			if name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ":"); ok && name != "" {
				values[name] = redisInfoValue(value)
			}
		}
		return values, nil
	}
	return redisPlainJSON(reply)
}

// redisPlainJSON maps a reply kind by kind.
func redisPlainJSON(reply any) (any, error) {
	switch v := reply.(type) {
	case nil:
		return nil, nil
	case int64:
		return v, nil
	case redisSimple:
		return string(v), nil
	case []byte:
		return string(v), nil
	case *redisError:
		return nil, v
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			value, err := redisPlainJSON(item)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	}
	return nil, fmt.Errorf("a reply of the type %T", reply)
}

// redisObject reads a flat array of names and values as an object; a name
// given twice keeps its last value.
func redisObject(reply any) (map[string]any, error) {
	items, ok := reply.([]any)
	if !ok || len(items)%2 != 0 {
		return nil, fmt.Errorf("the reply is %s, not a list of name and value pairs", redisKind(reply))
	}
	out := make(map[string]any, len(items)/2)
	for i := 0; i < len(items); i += 2 {
		var name string
		switch key := items[i].(type) {
		case []byte:
			name = string(key)
		case redisSimple:
			name = string(key)
		case int64:
			name = strconv.FormatInt(key, 10)
		default:
			return nil, fmt.Errorf("a name of the reply's pairs is %s, not a string", redisKind(items[i]))
		}
		value, err := redisPlainJSON(items[i+1])
		if err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, nil
}

// redisKind names the kind of a reply, for a failure and a debug report.
func redisKind(reply any) string {
	switch v := reply.(type) {
	case nil:
		return "nil"
	case int64:
		return "the integer " + strconv.FormatInt(v, 10)
	case redisSimple:
		return "the simple string " + redisShown(string(v))
	case []byte:
		if len(v) == 1 {
			return "a bulk string of 1 byte"
		}
		return fmt.Sprintf("a bulk string of %d bytes", len(v))
	case []any:
		if len(v) == 1 {
			return "an array of 1 element"
		}
		return fmt.Sprintf("an array of %d elements", len(v))
	case *redisError:
		return "the error " + v.text
	}
	return fmt.Sprintf("a reply of the type %T", reply)
}

// redisNumber is a JSON number as JSON writes one, which is what INFO's
// numbers are made of: no sign but -, no leading zero, no bare point.
var redisNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?$`)

// redisInfoScalar is a value of INFO as the document holds it: a number,
// written exactly as the server wrote it, when it is one, its text
// otherwise.
func redisInfoScalar(value string) any {
	if redisNumber.MatchString(value) {
		return json.Number(value)
	}
	return value
}

// redisInfoValue is a value of an INFO line. One made of name=value pairs
// joined by commas, as the keyspace's db0:keys=1,expires=0,avg_ttl=0, a
// replica's slave0:ip=...,port=...,lag=0 and every commandstats line are,
// is an object of them; any other is a scalar (redisInfoScalar).
func redisInfoValue(value string) any {
	if !strings.Contains(value, "=") {
		return redisInfoScalar(value)
	}
	pairs := map[string]any{}
	for part := range strings.SplitSeq(value, ",") {
		name, v, ok := strings.Cut(part, "=")
		if !ok || name == "" {
			return redisInfoScalar(value)
		}
		pairs[name] = redisInfoScalar(v)
	}
	return pairs
}

// parseRedisInfo adds the sections of an INFO reply to info: each
// "# Section" line opens one, named in lower case with _ for a blank, and
// each name:value line under it is a value of it (redisInfoValue). It
// returns the sections the reply held, which a section asked for and not
// known to the server has none of.
func parseRedisInfo(text string, info map[string]map[string]any) int {
	sections := 0
	var section map[string]any
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if heading, ok := strings.CutPrefix(line, "#"); ok {
			name := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(heading)), " ", "_")
			if info[name] == nil {
				info[name] = map[string]any{}
			}
			section = info[name]
			sections++
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" {
			continue
		}
		// A value before any heading belongs to no section, which no
		// server writes.
		if section != nil {
			section[name] = redisInfoValue(value)
		}
	}
	return sections
}
