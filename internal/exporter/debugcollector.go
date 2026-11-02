package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"gopkg.in/yaml.v3"
)

// The debug page shows a configured collector it runs as YAML, in place of
// the collector written on the page, so what is shown is what runs. It is
// the collector as its file writes it (model.Collector.Written): the keys as
// the operator wrote them, the ${NAME} references unexpanded, the keys it
// takes from its file's defaults added with a comment that says so. A
// collector the exporter holds without a file is shown as it holds it, its
// empty keys left out.
//
// The page is for whoever may change the configuration, but what it shows
// is shown on a page, at edit as at full, so no credential is: a value whose
// key reads as a credential's (fetch.CredentialName) — bearer_token,
// basic_auth's password, a header or a query parameter named like one, a
// login's fields — is withheld, and so is every path a *_file key names,
// which the pages never show; a header's value and a URL are withheld as a
// debug report withholds them; and a Python script, which is shown as
// written, has what reads as a credential in it masked (fetch.RedactText).
// The YAML pasted back as a collector written on the page reads as it does
// in its file, but for what was withheld.

// debugCollectorPath answers a configured collector as the page shows it,
// ?name=<name>, as JSON.
const debugCollectorPath = "/debug/collector"

// notCredentials are keys whose names read as a credential's that hold none:
// where a login's answer holds the token and how the login authenticates,
// a redis key, a shard key.
var notCredentials = map[string]bool{
	"request.login.token": true, "request.login.token_header": true, "request.login.token_cookie": true,
	"request.login.auth_style": true, "request.key": true, "shard_key": true,
}

// urlValue is a value written as an absolute URL.
var urlValue = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://\S+$`)

// commandCredential is a command of a text protocol that logs in, which a
// tcp collector's send may write: AUTH of Redis, Memcached's SASL and SMTP,
// PASS and APOP of POP3 and FTP, at the start of a line or after a ; that
// puts several commands on one; IMAP's tagged LOGIN with its user and
// password; Redis's HELLO with AUTH; and AUTH framed as RESP, an array's
// bulk string AUTH followed by the next one's length, as a HELLO's AUTH is
// framed too. What follows the command is a credential whatever it looks
// like, which fetch.RedactText, reading names and values, cannot tell. A
// word that only begins with one, such as author, is no command.
var commandCredential = regexp.MustCompile(`(?im)` +
	`(?:^|;)[ \t]*(?:auth|pass|apop)[ \t]+\S` +
	`|^[ \t]*[A-Za-z0-9.]+[ \t]+login[ \t]+\S+[ \t]+\S` +
	`|^[ \t]*hello\b[^\r\n]*[ \t]auth[ \t]+\S` +
	`|^[ \t]*auth[ \t]*\r?\n[ \t]*\$[0-9]+[ \t]*\r?\n`)

// debugCollectorAnswer is what debugCollectorPath answers: the collector's
// name and YAML, and the keys it took from its file's defaults; or Error.
type debugCollectorAnswer struct {
	Name string `json:"name,omitempty"`
	YAML string `json:"yaml,omitempty"`
	// Copy is the YAML Edit a copy puts in the playground, and CopyNote
	// what the page says above it of what the copy left out
	// (copiedCollectorYAML).
	Copy         string   `json:"copy,omitempty"`
	CopyNote     string   `json:"copy_note,omitempty"`
	FromDefaults []string `json:"from_defaults,omitempty"`
	// StaticTargets are the static targets scraped through the collector,
	// at full alone, where the page scrapes them: empty, not left out, when
	// none is.
	StaticTargets *[]debugStaticTarget `json:"static_targets,omitempty"`
	// Form is what the page asks a run of the collector to fill
	// (debugForm), read again with the collector so it follows a reload.
	Form  *debugForm `json:"form,omitempty"`
	Error string     `json:"error,omitempty"`
}

// debugStaticTarget is a static target as the debug page offers it: its
// name, which a run sends, and its target as the status page shows it, with
// any credential in it withheld (fetch.DisplayTarget).
type debugStaticTarget struct {
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
}

// debugStaticTargets are the static targets in force that are scraped
// through the collector named name, in the order of their file; none for
// no name. The page offers these alone: the collector chosen, or the one the
// playground was copied from, is the one a response comes through, as a
// fetched target's does, so a static target is scraped through it.
func debugStaticTargets(followed *followedConfig, name string) []debugStaticTarget {
	out := []debugStaticTarget{}
	if name == "" {
		return out
	}
	c := followed.collectorOf(followed.config, name)
	if c == nil {
		return out
	}
	for _, target := range staticTargetsOf(followed.targets) {
		if target.Collector == name {
			out = append(out, debugStaticTarget{Name: target.Name, Target: fetch.DisplayTarget(c, target.Target)})
		}
	}
	return out
}

// shownCollectorYAML is the configured collector x as the page shows it.
func shownCollectorYAML(x *model.Collector) (string, error) {
	return collectorYAML(x, nil)
}

// collectorYAML is x as the page shows it, and, with drops set, as Edit a
// copy copies it, the keys the playground refuses dropped (copyDrops).
func collectorYAML(x *model.Collector, drops *copyDrops) (string, error) {
	var n *yaml.Node
	if x.Written != nil {
		n = copyNode(x.Written)
	} else {
		var err error
		if n, err = heldCollector(x); err != nil {
			return "", err
		}
	}
	// Told before withhold, which may change a scalar's style.
	folded := foldedScalars(n, nil)
	withhold(n, "")
	blocks := writtenBlocks(x, folded)
	if drops != nil {
		drops.dropFrom(n)
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	text := blocks.Replace(b.String())
	if drops != nil {
		text = drops.Replace(text)
	}
	return text, nil
}

// shownBlock stands in, as a plain scalar, for a folded block scalar of the
// collector's while it is encoded, and is then replaced with the block as
// its file writes it.
const shownBlock = "pue-written-block-"

// foldedScalars appends to out the folded block scalars of n, depth first,
// the order model.Collector.WrittenBlocks has them in.
func foldedScalars(n *yaml.Node, out []*yaml.Node) []*yaml.Node {
	if n.Kind == yaml.ScalarNode && n.Style&yaml.FoldedStyle != 0 {
		return append(out, n)
	}
	for _, child := range n.Content {
		out = foldedScalars(child, out)
	}
	return out
}

// writtenBlocks puts shownBlock<i> in place of each folded block scalar of
// the collector, folded, that its file writes over several lines
// (model.Collector.WrittenBlocks), and gives what writes the blocks in
// their place once the YAML is encoded. The encoder writes a folded scalar
// on one line, however long, as its value has it; a SQL statement of four
// lines would run off the editor. Only a block whose lines still read as
// the value shown, which the withholding has not changed, is written as
// its file writes it.
func writtenBlocks(x *model.Collector, folded []*yaml.Node) blockLines {
	var out blockLines
	if x.Written == nil || len(folded) != len(x.WrittenBlocks) {
		return nil
	}
	for i, block := range x.WrittenBlocks {
		n := folded[i]
		if n.Style&yaml.FoldedStyle == 0 || len(block.Lines) == 0 || !blockReads(block, n.Value) {
			continue
		}
		n.Value, n.Style, n.Tag = shownBlock+strconv.Itoa(len(out)), 0, "!!str"
		out = append(out, block)
	}
	return out
}

// blockReads says whether block, written under a key, reads as value.
func blockReads(block model.WrittenBlock, value string) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "v: %s\n", block.Header)
	for _, line := range block.Lines {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	var read struct{ V *string }
	return yaml.Unmarshal([]byte(b.String()), &read) == nil && read.V != nil && *read.V == value
}

// blockLines are the folded block scalars writtenBlocks stood shownBlock<i>
// in for, by i.
type blockLines []model.WrittenBlock

// Replace writes each block in place of its shownBlock<i> in text, the YAML
// encoded, its lines indented two spaces beyond its key's, or beyond its
// dash's for an entry of a list.
func (blocks blockLines) Replace(text string) string {
	if len(blocks) == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		// The value of a key, or an entry of a list, then perhaps a comment.
		at := strings.Index(line, shownBlock)
		if at < 0 || !strings.HasSuffix(line[:at], ": ") && !strings.HasSuffix(line[:at], "- ") {
			out = append(out, line)
			continue
		}
		rest := line[at+len(shownBlock):]
		digits := len(rest) - len(strings.TrimLeft(rest, "0123456789"))
		i, err := strconv.Atoi(rest[:digits])
		if err != nil || i >= len(blocks) || rest[digits:] != "" && !strings.HasPrefix(rest[digits:], " #") {
			out = append(out, line)
			continue
		}
		// Where the key starts, past the indentation and any dashes.
		start := len(line) - len(strings.TrimLeft(line, " "))
		for strings.HasPrefix(line[start:], "- ") {
			start = len(line) - len(strings.TrimLeft(line[start+1:], " "))
		}
		out = append(out, line[:at]+blocks[i].Header+rest[digits:])
		for _, written := range blocks[i].Lines {
			if written == "" {
				out = append(out, "")
				continue
			}
			out = append(out, strings.Repeat(" ", start+2)+written)
		}
	}
	return strings.Join(out, "\n")
}

// heldCollector is x as the exporter holds it, with the keys that hold
// nothing left out.
func heldCollector(x *model.Collector) (*yaml.Node, error) {
	encoded, err := yaml.Marshal(x)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(encoded, &doc); err != nil {
		return nil, err
	}
	n := doc.Content[0]
	leaveOutEmpty(n)
	return n, nil
}

// leaveOutEmpty drops from a mapping, at any depth, the keys whose values
// hold nothing: null, empty text, false, zero, and a block or a list left
// empty once its own are dropped.
func leaveOutEmpty(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.MappingNode:
		kept := n.Content[:0]
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !leaveOutEmpty(n.Content[i+1]) {
				kept = append(kept, n.Content[i], n.Content[i+1])
			}
		}
		n.Content = kept
		return len(kept) == 0
	case yaml.SequenceNode:
		for _, item := range n.Content {
			leaveOutEmpty(item)
		}
		return len(n.Content) == 0
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!null":
			return true
		case "!!str":
			return n.Value == ""
		case "!!bool":
			return n.Value == "false"
		case "!!int", "!!float":
			return n.Value == "0"
		}
	}
	return false
}

// copyNode is a deep copy of n, which withhold may change.
func copyNode(n *yaml.Node) *yaml.Node {
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		out.Content[i] = copyNode(child)
	}
	return &out
}

// withhold withholds the credentials of the mapping n, at path in the
// collector ("" at its top).
func withhold(n *yaml.Node, path string) {
	if n.Kind == yaml.SequenceNode {
		for _, item := range n.Content {
			if inRequest(path) && freeTextCredential(item) {
				withheldValue(item)
				continue
			}
			withhold(item, path)
		}
		return
	}
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i].Value, n.Content[i+1]
		at := key
		if path != "" {
			at = path + "." + key
		}
		switch {
		case at == "transform.script" || at == "transform.pre_script":
			if value.Kind == yaml.ScalarNode {
				value.Value = fetch.RedactText(value.Value)
			}
		case strings.HasSuffix(key, "_file") || strings.HasSuffix(key, "_files"):
			withholdAll(value)
		case fetch.CredentialName(key) && !notCredentials[at] && value.Kind == yaml.ScalarNode:
			if value.ShortTag() != "!!bool" && value.ShortTag() != "!!null" {
				withheldValue(value)
			}
		case path == "request.headers" && value.Kind == yaml.ScalarNode:
			if shown := fetch.RedactText(value.Value); shown != value.Value {
				withheldValue(value)
			}
		case value.Kind == yaml.ScalarNode && value.ShortTag() == "!!str" && urlValue.MatchString(value.Value):
			value.Value = shownURL(value.Value)
		case inRequest(at) && freeTextCredential(value):
			withheldValue(value)
		default:
			withhold(value, at)
		}
	}
}

// inRequest says whether the key at path is one of the collector's request
// block.
func inRequest(path string) bool {
	return path == "request" || strings.HasPrefix(path, "request.")
}

// freeTextCredential says whether n, a string of the request block whose
// key does not read as a credential's, such as an http body, a grpc
// message, a tcp send, a redis command's argument or a SQL statement, holds
// what reads as one: what fetch.RedactText masks, as it masks a Python
// script's, or a protocol's login command (commandCredential). Such a value
// is withheld whole, not masked where it reads as one, since a credential
// in free text may not look like one to the rule: a body that writes
// password=hunter2 may write the same password elsewhere in it.
func freeTextCredential(n *yaml.Node) bool {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return false
	}
	return fetch.RedactText(n.Value) != n.Value || commandCredential.MatchString(n.Value)
}

// withholdAll withholds every value n holds.
func withholdAll(n *yaml.Node) {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.ShortTag() != "!!null" {
			withheldValue(n)
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			withholdAll(n.Content[i])
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			withholdAll(item)
		}
	}
}

// withheldValue puts fetch.Redacted in place of a scalar's value.
func withheldValue(n *yaml.Node) {
	n.Value, n.Tag, n.Style = fetch.Redacted, "!!str", 0
}

// shownURL is a URL written in the collector with its userinfo and the
// values of query parameters named like credentials withheld, as a target
// is shown; one that holds neither is shown as written.
func shownURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return fetch.Redacted
	}
	shown := fetch.RedactURL(u, fetch.MaskCredentialQueryValues)
	if u.User == nil && !strings.Contains(shown, fetch.Redacted) {
		return raw
	}
	return shown
}

// debugCollectorHandler answers a configured collector as the page shows it.
func (s *Server) debugCollectorHandler(w http.ResponseWriter, r *http.Request) {
	answer := func(code int, a debugCollectorAnswer) {
		body, err := json.Marshal(a)
		if err != nil {
			http.Error(w, "the answer could not be written", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(code)
		_, _ = w.Write(append(body, '\n'))
	}
	if !readOnly(w, r, "use GET to read a configured collector") {
		return
	}
	name := r.URL.Query().Get("name")
	// The collector a run of the name runs (prepareDebugRun).
	followed := s.reconcile()
	x := followed.collectorOf(followed.config, name)
	if x == nil {
		answer(http.StatusNotFound, debugCollectorAnswer{Error: fmt.Sprintf("unknown collector %q; name a collector of the configuration in force", name)})
		return
	}
	shown, err := shownCollectorYAML(x)
	if err != nil {
		s.logger.Error("showing a collector on the debug page failed", "collector", name, "error", err)
		answer(http.StatusInternalServerError, debugCollectorAnswer{Error: "the collector could not be shown"})
		return
	}
	copied, note, err := copiedCollectorYAML(x, s.playgroundUnbound())
	if err != nil {
		s.logger.Error("copying a collector on the debug page failed", "collector", name, "error", err)
		answer(http.StatusInternalServerError, debugCollectorAnswer{Error: "the collector could not be shown"})
		return
	}
	form := debugFormOf(x)
	a := debugCollectorAnswer{Name: x.Name, YAML: shown, Copy: copied, CopyNote: note, FromDefaults: x.FromDefaults, Form: &form}
	if s.debug == DebugFull {
		// The page offers them when this collector is chosen or the
		// playground is a copy of it, read at each change of collector, so
		// they follow a reload of the static target file.
		targets := debugStaticTargets(followed, x.Name)
		a.StaticTargets = &targets
	}
	answer(http.StatusOK, a)
}
