package exporter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
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

// debugCollectorAnswer is what debugCollectorPath answers: the collector's
// name and YAML, and the keys it took from its file's defaults; or Error.
type debugCollectorAnswer struct {
	Name         string   `json:"name,omitempty"`
	YAML         string   `json:"yaml,omitempty"`
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
	var n *yaml.Node
	if x.Written != nil {
		n = copyNode(x.Written)
	} else {
		var err error
		if n, err = heldCollector(x); err != nil {
			return "", err
		}
	}
	withhold(n, "")
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
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
		default:
			withhold(value, at)
		}
	}
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
	form := debugFormOf(x)
	a := debugCollectorAnswer{Name: x.Name, YAML: shown, FromDefaults: x.FromDefaults, Form: &form}
	if s.debug == DebugFull {
		// The page offers them when this collector is chosen or the
		// playground is a copy of it, read at each change of collector, so
		// they follow a reload of the static target file.
		targets := debugStaticTargets(followed, x.Name)
		a.StaticTargets = &targets
	}
	answer(http.StatusOK, a)
}
