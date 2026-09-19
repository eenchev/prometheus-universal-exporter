package main

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Environment expansion is opt-in, through --config.export-env, because a
// configuration file is full of characters that look like references and are
// not: a regex metric rule, a jq expression and a Python pre-script can all
// contain a dollar sign, and expanding by default would rewrite them behind the
// operator's back. Off by default, nothing in a configuration file means
// anything but itself.
//
// Only the braced form is a reference. `$VAR` is left exactly as written, which
// is what keeps `expression: '\$([0-9]+)'` and shell-style text in a pre-script
// working with expansion switched on. `$$` escapes a literal dollar, so
// `$${NOT_A_REFERENCE}` survives as `${NOT_A_REFERENCE}`.
var envReference = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvironment substitutes ${NAME} references in a configuration document.
//
// A reference to a variable that is not set is an error rather than an empty
// string. An empty substitution produces a document that parses and is wrong —
// a collector with no target, or credentials that silently become blank — and
// the exporter would serve it. Every missing name is reported at once, because
// finding them one restart at a time is miserable.
func expandEnvironment(path string, raw []byte) ([]byte, error) {
	var missing []string
	var invalid []string
	seen := map[string]bool{}

	out := envReference.ReplaceAllFunc(raw, func(match []byte) []byte {
		if string(match) == "$$" {
			return []byte("$")
		}
		name := string(match[2 : len(match)-1])
		value, ok := os.LookupEnv(name)
		if !ok {
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
			return match
		}
		// A value is substituted into the document before it is parsed, so a
		// newline in one does not set a long string: it ends the line and the
		// rest becomes YAML. That is a mistake or an injection, never a working
		// configuration, so it is refused rather than parsed.
		if strings.ContainsAny(value, "\n\r") {
			if !seen[name] {
				seen[name] = true
				invalid = append(invalid, name)
			}
			return match
		}
		return []byte(value)
	})

	sort.Strings(missing)
	sort.Strings(invalid)
	switch {
	case len(missing) > 0 && len(invalid) > 0:
		return nil, fmt.Errorf("%s: %s, and %s", path, missingMessage(missing), invalidMessage(invalid))
	case len(missing) > 0:
		return nil, fmt.Errorf("%s: %s", path, missingMessage(missing))
	case len(invalid) > 0:
		return nil, fmt.Errorf("%s: %s", path, invalidMessage(invalid))
	}
	return out, nil
}

func missingMessage(names []string) string {
	return fmt.Sprintf("%s %s not set; --config.export-env requires every ${NAME} it finds to be defined",
		plural(len(names), "environment variable", "environment variables"), quoteAll(names))
}

func invalidMessage(names []string) string {
	return fmt.Sprintf("%s %s contain a line break, which would change the structure of the document rather than the value",
		plural(len(names), "environment variable", "environment variables"), quoteAll(names))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func quoteAll(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return strings.Join(quoted, ", ")
}
