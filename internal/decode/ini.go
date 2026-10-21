package decode

import (
	"bytes"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The ini decoder reads an INI file, the sections and keys of a Windows
// .ini, a php.ini or a Python configparser file, as a small parser of its
// own: no INI module is in the build, and the dialects agree on little more
// than what follows. Its document is a mapping of each section's name to the
// mapping of its keys to their values, as text, and of each key written
// before the first section to its value, so that a jq rule reads .server.port
// and .timeout.
//
//   - A line is what lies between two line feeds, and is read without the
//     spaces, tabs and carriage returns around it. An empty line, and one
//     that starts with ; or #, is passed over: a comment is a whole line, so
//     a value may hold ; and #, as a URL or a colour does.
//   - [name] starts a section, and may be followed by a comment. The name is
//     read without the blanks around it, must not be empty, and is a name and
//     no path: [a.b] is the section "a.b", which jq reads as .["a.b"]. A
//     section written twice is one section, its keys gathered from both.
//   - Any other line is a key and its value, split at the first = or :,
//     each read without the blanks around it. The key must not be empty; the
//     value may be, and is taken as written, quotes included: dialects
//     differ on what quotes mean. A value does not go on to the next line.
//   - Of a key written twice in one section, the later is kept: an INI file
//     is often a default file with lines appended to override it. Keys and
//     names are compared as written, in their case.
//   - A key written before the first section and a section of the same name
//     would be one member of the mapping; the body is refused, naming both
//     lines, rather than one hiding the other.
//   - An error names the line, as model.Position, and quotes no more than
//     the start of a name (model.Quoted).

// decodeINI decodes body as an INI file.
func decodeINI(body []byte) (map[string]any, error) {
	document := map[string]any{}
	// Where each key before the first section was first written, for the
	// error of a section of its name, which can only follow it.
	topLevel := map[string]model.Position{}
	var section map[string]any
	for start, line := 0, model.Position(1); start <= len(body); line++ {
		end := bytes.IndexByte(body[start:], '\n')
		if end < 0 {
			end = len(body)
		} else {
			end += start
		}
		text := bytes.Trim(body[start:end], " \t\r")
		start = end + 1
		switch {
		case len(text) == 0 || text[0] == ';' || text[0] == '#':
			continue
		case text[0] == '[':
			closing := bytes.IndexByte(text, ']')
			if closing < 0 {
				return nil, model.Errorf("line %d: the section name has no closing ]", line)
			}
			if rest := bytes.TrimLeft(text[closing+1:], " \t"); len(rest) > 0 && rest[0] != ';' && rest[0] != '#' {
				return nil, model.Errorf("line %d: %s follows the section name; only a comment, starting with ; or #, may", line, model.Quoted(rest))
			}
			name := string(bytes.Trim(text[1:closing], " \t"))
			if name == "" {
				return nil, model.Errorf("line %d: the section has no name", line)
			}
			if at, clash := topLevel[name]; clash {
				return nil, model.Errorf("line %d: section %s has the name of the key at line %d, written before the first section; rename one", line, model.Quoted(name), at)
			}
			if existing, ok := document[name].(map[string]any); ok {
				section = existing
				continue
			}
			section = map[string]any{}
			document[name] = section
		default:
			split := bytes.IndexAny(text, "=:")
			if split < 0 {
				return nil, model.Errorf("line %d: neither a section, a key = value nor a comment", line)
			}
			key := string(bytes.TrimRight(text[:split], " \t"))
			if key == "" {
				return nil, model.Errorf("line %d: the line has no key before its %s", line, quoteJSONByte(text[split]))
			}
			value := string(bytes.TrimLeft(text[split+1:], " \t"))
			if section != nil {
				section[key] = value
				continue
			}
			if _, seen := topLevel[key]; !seen {
				topLevel[key] = line
			}
			document[key] = value
		}
	}
	return document, nil
}
