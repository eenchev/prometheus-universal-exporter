package decode

import (
	"strconv"
	"strings"
)

// The YAML library (gopkg.in/yaml.v3 v3.0.1) puts a part of the document in
// full into some of its errors. So does the exporter where it words an error
// as the library does:
//
//   - `yaml: invalid map key: %#v` (decode.go, mapping; yamlWalk.key): the
//     Go syntax of a key that is a sequence or a mapping, with all it holds.
//   - `yaml: cannot decode !!str `%s` as a !!int` (resolve.go, resolve): the
//     scalar that does not fit its tag.
//   - `yaml: unknown anchor '%s' referenced` (decode.go, alias) and `yaml:
//     anchor '%s' value contains itself` (decode.go, alias; yamlHoldsItself):
//     the anchor's name, which may be of any length.
//   - `line %d: mapping key %#v already defined at line %d` (decode.go,
//     mapping; yamlWrittenTwice): the key, or the name of the anchor an alias
//     key stands for, quoted as Go quotes a text.
//
// No other error of the library holds more of the document than it bounds
// itself. `line %d: cannot unmarshal %s%s into %s` (decode.go, terror) holds
// the first seven bytes of a scalar and a tag, which for a document decoded
// into plain values is !!str. The scanner's and the parser's problems,
// `yaml: line N: ...`, are fixed texts that quote nothing. The rest
// (excessive aliasing, a merge of something that is not a mapping, invalid
// base64) are fixed texts too.
//
// So those four forms are parsed here, and only here, as the library writes
// them. The part is cut to its start, with how long it was, as
// model.QuoteValue cuts a value. What the error says after the part is kept, such as the tag
// a value was to fit or the line a key was first written on. An error in
// none of the forms is left as it is, and is bounded with every other decode
// error (boundedFailure).
//
// A cut key or part is recognised by its start alone. The length is a size
// measured from the response and not part of what identifies the failure,
// so two keys that start with the same yamlKeyBytes bytes, of any lengths,
// count as one failure in the log. That is accepted: the error tells such
// keys apart by the lines they are on.

const (
	// yamlKeyBytes is how many bytes of a key written twice its problem
	// shows, which is how much of a value model.QuoteValue shows. An error
	// lists up to yamlProblemsShown such problems, and ten keys cut to this
	// length are under maxFailureBytes together.
	yamlKeyBytes = 64
	// yamlPartBytes is how many bytes an error of another form shows of the
	// one part of the document it holds. It is more than a key's: the Go
	// syntax of a small collection, such as a template left unrendered for
	// a key makes (`{{ name }}: x`), is some hundred bytes and is shown whole.
	yamlPartBytes = 256
)

// yamlKeyText is a key as the problem of one written twice names it: quoted
// as the library quotes it, and cut as model.QuoteValue cuts a value, to its
// first yamlKeyBytes bytes with its length after the quotes. A key no longer
// than that reads as the library wrote it.
func yamlKeyText(key string, recognised bool) string {
	head := headOf(key, yamlKeyBytes)
	if head == len(key) {
		return strconv.Quote(key)
	}
	return strconv.Quote(key[:head]) + cutMark(len(key), recognised)
}

// yamlKeyCut is the library's problem with a key written twice, `line N:
// mapping key "..." already defined at line N`, with the key cut as
// yamlKeyText cuts one, and whether it was cut. Any other problem, and one
// whose key is yamlKeyBytes bytes or fewer, is not cut. The key is what
// stands between the words before it and the last ` already defined at line `,
// which only the line follows. It is unquoted for its length and its
// characters, and text there that is not a quoted text is left alone.
func yamlKeyCut(problem string, recognised bool) (string, bool) {
	const starts, names, ends = "line ", ": mapping key ", " already defined at line "
	// A key that is cut is over yamlKeyBytes bytes within its quotes.
	if len(problem) <= yamlKeyBytes || !strings.HasPrefix(problem, starts) {
		return "", false
	}
	from := len(starts)
	for from < len(problem) && problem[from] >= '0' && problem[from] <= '9' {
		from++
	}
	if from == len(starts) || !strings.HasPrefix(problem[from:], names) {
		return "", false
	}
	from += len(names)
	to := strings.LastIndex(problem, ends)
	if to-from <= yamlKeyBytes+len(`""`) {
		return "", false
	}
	key, err := strconv.Unquote(problem[from:to])
	if err != nil || len(key) <= yamlKeyBytes {
		return "", false
	}
	return problem[:from] + yamlKeyText(key, recognised) + problem[to:], true
}

// yamlQuotes are the errors, other than the key written twice, that hold a
// part of the document in full. Each gives the words the error starts with,
// what opens the part after them, and what closes the part and follows it.
// The part starts after the first opening and ends at the last closing, as
// the library writes nothing of the document after the part: a tag after
// ` as a `, and nothing after the others. The key of an invalid map key runs
// to the end.
var yamlQuotes = [...]struct{ starts, opens, closes string }{
	{"yaml: invalid map key: ", "", ""},
	{"yaml: cannot decode ", "`", "` as a "},
	{"yaml: unknown anchor ", "'", "' referenced"},
	{"yaml: anchor ", "'", "' value contains itself"},
}

// yamlPartCut returns an error's text with the part of the document it holds
// cut to its first yamlPartBytes bytes, at a character boundary, followed
// after what closes it by the part's length, as in
// `yaml: cannot decode !!str `aaa`... (1000000 bytes) as a !!int`. It also
// returns the same with the mark for the length, which the failure is
// recognised by, and whether anything was cut. Nothing is cut for a text in
// none of the forms, or one whose part is no longer than that.
func yamlPartCut(text string) (short, same string, cut bool) {
	if len(text) <= yamlPartBytes {
		return "", "", false
	}
	for _, quote := range yamlQuotes {
		rest, ok := strings.CutPrefix(text, quote.starts)
		if !ok {
			continue
		}
		opens := strings.Index(rest, quote.opens)
		from, to := len(quote.starts)+opens+len(quote.opens), len(text)
		if quote.closes != "" {
			to = strings.LastIndex(text, quote.closes)
		}
		if opens < 0 || to < from {
			return "", "", false
		}
		head := headOf(text[from:to], yamlPartBytes)
		if head == to-from {
			return "", "", false
		}
		// What closes the part is as long as what opens it.
		shut := to + len(quote.opens)
		start, end := text[:from+head]+text[to:shut], text[shut:]
		return start + cutMark(to-from, false) + end, start + cutMark(to-from, true) + end, true
	}
	return "", "", false
}
