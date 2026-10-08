package model

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	"gopkg.in/yaml.v3"
)

// A mapping key may be an alias of an anchored scalar: with x: [&p body],
// request: {*p : text} sets the body, which is how yaml.v3 decodes it. In the
// node tree such a key is an alias node whose Value is the anchor's name, p,
// and the code that reads a mapping's keys from the tree itself — the unknown
// keys of otlp, web.basic_auth and a static target's request, the switch of
// the first two, the request's PathSet and BodySet, the keys of a cache —
// matched that name: a valid alias key was refused as an unknown field, and
// an anchor named path that holds body set PathSet. Each now reads a key as
// the decoder does (KeyName), and is held here to yaml.v3's own decoding of
// the same document into the same structs without their UnmarshalYAML.

type (
	plainOTLP    OTLPConfig
	plainAuth    ExporterBasicAuth
	plainRequest TargetRequestConfig
	plainCache   CacheConfig
)

// aliasKeyBlocks holds every block that reads its own keys, and anchors for
// the documents below to use as keys and to merge.
type aliasKeyBlocks struct {
	X       []string             `yaml:"x"`
	Base    map[string]any       `yaml:"base"`
	OTLP    *OTLPConfig          `yaml:"otlp"`
	Auth    *ExporterBasicAuth   `yaml:"auth"`
	Request *TargetRequestConfig `yaml:"request"`
	Cache   *CacheConfig         `yaml:"cache"`
}

// aliasKeyOracle is aliasKeyBlocks as yaml.v3 decodes it on its own: the
// same blocks without the code that checks their keys.
type aliasKeyOracle struct {
	X       []string       `yaml:"x"`
	Base    map[string]any `yaml:"base"`
	OTLP    *plainOTLP     `yaml:"otlp"`
	Auth    *plainAuth     `yaml:"auth"`
	Request *plainRequest  `yaml:"request"`
	Cache   *plainCache    `yaml:"cache"`
}

// decodeKnown decodes a document as the configuration's loader does, refusing
// unknown keys.
func decodeKnown(document string, out any) error {
	decoder := yaml.NewDecoder(strings.NewReader(document))
	decoder.KnownFields(true)
	return decoder.Decode(out)
}

// A key that is an alias is the key its anchor holds in every block that
// checks its own keys, alone, in a mapping merged in and in a list of them:
// accepted where that key is, with the value yaml.v3 decodes, and refused
// where it is unknown, in the message naming the key it stands for. An alias
// of <<, which yaml.v3 reads as the text "<<" and not as a merge, is an
// unknown key as yaml.v3 says. A request's path and body are set as the keys
// the document means, whatever their anchors are called.
func TestAKeyThatIsAnAliasIsTheKeyItsAnchorHolds(t *testing.T) {
	const names = "x: [&on enabled, &ep endpoint, &u username, &pw password, &path body, &body path, &t ttl, &s stale_if_error, &a attempts, &m <<, &bad endpiont, &user user, &pth pth, &ttll ttll]\n"
	for _, test := range []struct {
		name, document, want string
		// unswitched is a block that sets keys without enabled, which
		// yaml.v3 alone has no rule against.
		unswitched bool
		pathSet    bool
		bodySet    bool
	}{
		{name: "otlp", document: "otlp: {*on : true, *ep : 'http://c.invalid:4318'}\n"},
		{name: "otlp merging alias keys", document: "base: {o: &o {*ep : 'http://c.invalid:4318'}}\notlp: {<<: *o, *on : true}\n"},
		{name: "otlp merging a list", document: "base: {o: &o {*ep : 'http://c.invalid:4318'}, p: &p {*on : true, *ep : other}}\notlp: {<<: [*o, *p]}\n"},
		{name: "otlp's unknown key", document: "otlp: {*on : true, *bad : x}\n", want: "line 2: field endpiont not found in type model.OTLPConfig"},
		{name: "otlp's alias of <<", document: "base: {o: &o {endpoint: e}}\notlp: {*on : true, *m : *o}\n", want: "line 3: field << not found in type model.OTLPConfig"},
		{name: "otlp without enabled", document: "otlp: {*ep : e, *u : x}\n", unswitched: true, want: "line 2: otlp sets endpoint and username but not enabled"},
		{name: "basic_auth", document: "auth: {*on : true, *u : admin, *pw : secret}\n"},
		{name: "basic_auth's unknown key", document: "auth: {*on : true, *user : admin}\n", want: "line 2: field user not found in type model.ExporterBasicAuth"},
		{name: "request's path and body swapped", document: "request: {*path : from the target, *body : /target}\n", pathSet: true, bodySet: true},
		{name: "request's body under an anchor named path", document: "request: {*path : ''}\n", bodySet: true},
		{name: "request's path under an anchor named body", document: "request: {method: POST, *body : /p}\n", pathSet: true},
		{name: "request merging an alias key", document: "base: {r: &r {*path : merged body}}\nrequest: {<<: *r}\n", bodySet: true},
		{name: "request's nested alias key", document: "request: {retry: {*a : 2}}\n"},
		{name: "request's unknown key", document: "request: {*pth : /x}\n", want: "line 2: field pth not found in type model.TargetRequestConfig"},
		{name: "cache", document: "cache: {*t : 1m, *s : 1h}\n"},
		{name: "cache merging alias keys", document: "base: {c: &c {*t : 1m}, d: &d {*s : 1h, *t : 5m}}\ncache: {<<: [*c, *d]}\n"},
		{name: "cache's unknown key", document: "cache: {*ttll : 1m}\n", want: `line 2: cache has the unknown key "ttll"; it takes ttl and stale_if_error`},
		{name: "cache's alias of <<", document: "base: {c: &c {ttl: 1m}}\ncache: {*m : *c}\n", want: `line 3: cache has the unknown key "<<"; it takes ttl and stale_if_error`},
	} {
		document := names + test.document
		var got aliasKeyBlocks
		err := decodeKnown(document, &got)
		switch {
		case test.want == "" && err != nil:
			t.Errorf("%s: %v\n%s", test.name, err, document)
			continue
		case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
			t.Errorf("%s: error %v, want %q\n%s", test.name, err, test.want, document)
		}
		var want aliasKeyOracle
		wantErr := decodeKnown(document, &want)
		if test.unswitched {
			continue
		}
		// yaml.v3's verdict is the authority, and so are the values it
		// decodes.
		if (err == nil) != (wantErr == nil) {
			t.Errorf("%s: error %v, and yaml.v3 alone says %v\n%s", test.name, err, wantErr, document)
			continue
		}
		if err != nil {
			continue
		}
		if !sameBlock(got.OTLP, want.OTLP) || !sameBlock(got.Auth, want.Auth) || !sameBlock(got.Cache, want.Cache) {
			t.Errorf("%s: decoded %+v %+v %+v, and yaml.v3 alone %+v %+v %+v", test.name, got.OTLP, got.Auth, got.Cache, want.OTLP, want.Auth, want.Cache)
		}
		if !strings.HasPrefix(test.name, "request") {
			continue
		}
		request := *got.Request
		if request.PathSet != test.pathSet || request.BodySet != test.bodySet {
			t.Errorf("%s: path set %v and body set %v, want %v and %v", test.name, request.PathSet, request.BodySet, test.pathSet, test.bodySet)
		}
		// Which keys yaml.v3 decodes, merged in or not, is what is set.
		var keys struct {
			Request map[string]any `yaml:"request"`
		}
		if err := yaml.Unmarshal([]byte(document), &keys); err != nil {
			t.Fatal(err)
		}
		_, path := keys.Request["path"]
		_, body := keys.Request["body"]
		request.PathSet, request.BodySet = false, false
		if path != test.pathSet || body != test.bodySet || !reflect.DeepEqual(plainRequest(request), *want.Request) {
			t.Errorf("%s: request %+v, and yaml.v3 alone %+v with path %v and body %v", test.name, request, *want.Request, path, body)
		}
	}
}

// sameBlock reports whether a block decoded with its own UnmarshalYAML holds
// what yaml.v3 alone decodes into its plain twin.
func sameBlock[T, P any](got *T, want *P) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	converted, _ := reflect.ValueOf(want).Convert(reflect.TypeOf(got)).Interface().(*T)
	return converted != nil && reflect.DeepEqual(*got, *converted)
}

// checkKnownKeysAsItWas is checkKnownKeys before a key that is an alias was
// read as the key it names, kept as the oracle of the mappings without one.
func checkKnownKeysAsItWas(n *yaml.Node, t reflect.Type, name string) error {
	var problems []string
	visiting := map[*yaml.Node]bool{}
	var walk func(n *yaml.Node, t reflect.Type, top bool)
	walk = func(n *yaml.Node, t reflect.Type, top bool) {
		n = resolveAlias(n)
		if n == nil || visiting[n] {
			return
		}
		visiting[n] = true
		defer delete(visiting, n)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if !top && reflect.PointerTo(t).Implements(unmarshalerType) {
			return
		}
		switch t.Kind() {
		case reflect.Struct:
			if n.Kind != yaml.MappingNode {
				return
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
				if key != "" && key != "-" {
					fields[key] = t.Field(i).Type
				}
			}
			for _, entry := range MappingEntries(n) {
				key := entry.Key
				field, ok := fields[key.Value]
				if !ok {
					typeName := t.String()
					if top {
						typeName = name
					}
					problems = append(problems, fmt.Sprintf("line %d: field %s not found in type %s", key.Line, key.Value, typeName))
					continue
				}
				walk(entry.Value, field, false)
			}
		case reflect.Slice:
			if n.Kind == yaml.SequenceNode {
				for _, item := range n.Content {
					walk(item, t.Elem(), false)
				}
			}
		case reflect.Map:
			if n.Kind == yaml.MappingNode {
				for _, entry := range MappingEntries(n) {
					walk(entry.Value, t.Elem(), false)
				}
			}
		}
	}
	walk(n, t, true)
	if len(problems) > 0 {
		return &yaml.TypeError{Errors: problems}
	}
	return nil
}

// decodeSwitchedBlockAsItWas is decodeSwitchedBlock before a key that is an
// alias was read as the key it names.
func decodeSwitchedBlockAsItWas(n *yaml.Node, out any, name, block string) error {
	if n.Kind != yaml.MappingNode {
		return notAMapping(n, name)
	}
	var problems []string
	collect := func(err error) error {
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			problems = append(problems, typeErr.Errors...)
			return nil
		}
		return err
	}
	if err := collect(checkKnownKeysAsItWas(n, reflect.TypeOf(out).Elem(), name)); err != nil {
		return err
	}
	if err := collect(n.Decode(out)); err != nil {
		return err
	}
	var others []*yaml.Node
	switched := false
	for _, entry := range DecodedEntries(n) {
		switch {
		case entry.Key.Value != "enabled":
			others = append(others, entry.Key)
		case entry.Value != nil && entry.Value.ShortTag() != nullTag:
			switched = true
		}
	}
	if !switched && len(others) > 0 {
		keys := make([]string, 0, len(others))
		for _, key := range others {
			keys = append(keys, key.Value)
		}
		problems = append(problems, fmt.Sprintf("line %d: %s sets %s but not enabled; say enabled: true to turn it on, or enabled: false to keep the settings without using them", others[0].Line, block, joinWithAnd(keys)))
	}
	if len(problems) > 0 {
		return &yaml.TypeError{Errors: problems}
	}
	return nil
}

// requestAsItWas is TargetRequestConfig.UnmarshalYAML before a key that is
// an alias was read as the key it names.
func requestAsItWas(n *yaml.Node) (TargetRequestConfig, error) {
	var t TargetRequestConfig
	if err := checkKnownKeysAsItWas(n, reflect.TypeOf(plainRequest{}), "model.TargetRequestConfig"); err != nil {
		return t, err
	}
	var out plainRequest
	if err := n.Decode(&out); err != nil {
		return t, err
	}
	t = TargetRequestConfig(out)
	for _, entry := range MappingEntries(n) {
		switch entry.Key.Value {
		case "path":
			t.PathSet = true
		case "body":
			t.BodySet = true
		}
	}
	return t, nil
}

// cacheAsItWas is CacheConfig.UnmarshalYAML before a key that is an alias was
// read as the key it names.
func cacheAsItWas(n *yaml.Node) (CacheConfig, error) {
	var c CacheConfig
	if n.Kind == yaml.ScalarNode {
		return c, fmt.Errorf("line %d: cache is a mapping: write cache: {ttl: %s} to answer repeats of a probe for %s, and add stale_if_error to answer with the last good result when the target fails", n.Line, n.Value, n.Value)
	}
	if n.Kind != yaml.MappingNode {
		return c, fmt.Errorf("line %d: cache must be a mapping with ttl and stale_if_error", n.Line)
	}
	for _, entry := range MappingEntries(n) {
		if key := entry.Key.Value; !slices.Contains(cacheConfigKeys, key) {
			return c, fmt.Errorf("line %d: cache has the unknown key %q; it takes %s", entry.Key.Line, key, strings.Join(cacheConfigKeys, " and "))
		}
	}
	err := n.Decode((*plainCache)(&c))
	return c, err
}

// errorText is an error's text, and "" for none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// hasAliasKey reports whether any mapping in n has a key that is an alias.
func hasAliasKey(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	if n == nil || seen[n] {
		return false
	}
	seen[n] = true
	for i, child := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && child.Kind == yaml.AliasNode {
			return true
		}
		if hasAliasKey(child, seen) {
			return true
		}
	}
	return false
}

// keyTypes are the types whose keys checkKnownKeys is asked of below.
var keyTypes = []reflect.Type{
	reflect.TypeOf(Config{}), reflect.TypeOf(Collector{}), reflect.TypeOf(StaticTargetFile{}), reflect.TypeOf(Limits{}),
	reflect.TypeOf(plainOTLP{}), reflect.TypeOf(plainAuth{}), reflect.TypeOf(plainRequest{}), reflect.TypeOf(plainCache{}),
}

// sameAsItWas fails unless every mapping of n, at any depth, gives what it
// gave before to the checks of its keys, the switch of otlp and basic_auth,
// a request's PathSet and BodySet and a cache, and every key that is no
// alias is its own text to KeyName.
func sameAsItWas(t *testing.T, where string, n *yaml.Node, seen map[*yaml.Node]bool) int {
	t.Helper()
	if n == nil || seen[n] {
		return 0
	}
	seen[n] = true
	compared := 0
	for i, child := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && KeyName(child) != child.Value {
			t.Fatalf("%s: the key at line %d is %q to KeyName", where, child.Line, KeyName(child))
		}
		compared += sameAsItWas(t, where, child, seen)
	}
	if n.Kind != yaml.MappingNode {
		return compared
	}
	for _, kind := range keyTypes {
		if now, was := errorText(checkKnownKeys(n, kind, "the type")), errorText(checkKnownKeysAsItWas(n, kind, "the type")); now != was {
			t.Fatalf("%s: line %d: checkKnownKeys of %v says %q, and said %q", where, n.Line, kind, now, was)
		}
	}
	var otlpNow, otlpWas plainOTLP
	if now, was := errorText(decodeSwitchedBlock(n, &otlpNow, "model.OTLPConfig", "otlp")), errorText(decodeSwitchedBlockAsItWas(n, &otlpWas, "model.OTLPConfig", "otlp")); now != was || !reflect.DeepEqual(otlpNow, otlpWas) {
		t.Fatalf("%s: line %d: otlp is %q %+v, and was %q %+v", where, n.Line, now, otlpNow, was, otlpWas)
	}
	var authNow, authWas plainAuth
	if now, was := errorText(decodeSwitchedBlock(n, &authNow, "model.ExporterBasicAuth", "web.basic_auth")), errorText(decodeSwitchedBlockAsItWas(n, &authWas, "model.ExporterBasicAuth", "web.basic_auth")); now != was || !reflect.DeepEqual(authNow, authWas) {
		t.Fatalf("%s: line %d: basic_auth is %q %+v, and was %q %+v", where, n.Line, now, authNow, was, authWas)
	}
	var request TargetRequestConfig
	requestErr := request.UnmarshalYAML(n)
	if was, wasErr := requestAsItWas(n); errorText(requestErr) != errorText(wasErr) || (requestErr == nil && !reflect.DeepEqual(request, was)) {
		t.Fatalf("%s: line %d: request is %v %+v, and was %v %+v", where, n.Line, requestErr, request, wasErr, was)
	}
	var cache CacheConfig
	cacheErr := cache.UnmarshalYAML(n)
	if was, wasErr := cacheAsItWas(n); errorText(cacheErr) != errorText(wasErr) || !reflect.DeepEqual(cache, was) {
		t.Fatalf("%s: line %d: cache is %v %+v, and was %v %+v", where, n.Line, cacheErr, cache, wasErr, was)
	}
	return compared + 1
}

// The repository's YAML files — the example configurations, the published
// example files, the chart's values and the test data — have no key that is
// an alias, and every mapping in them, at any depth, gives each check of its
// keys exactly what it gave before keys were read through KeyName.
func TestTheRepositorysYAMLFilesGiveTheKeyChecksWhatTheyGave(t *testing.T) {
	root := filepath.Join("..", "..")
	files, mappings := 0, 0
	for _, dir := range []string{"examples", "configs", "testdata", "charts"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// The chart's templates are no YAML until rendered.
			if doc := new(yaml.Node); yaml.Unmarshal(b, doc) == nil {
				if hasAliasKey(doc, map[*yaml.Node]bool{}) {
					t.Errorf("%s has a key that is an alias, which this test assumes it has not", path)
				}
				files++
				mappings += sameAsItWas(t, path, doc, map[*yaml.Node]bool{})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 15 || mappings < 500 {
		t.Fatalf("only %d files and %d mappings were compared", files, mappings)
	}
}

// Over generated documents without a key that is an alias — the keys of
// every block and unknown ones, quoted, not text and null keys, values of
// every kind and aliases, nested retries, merges of one mapping and of a
// list, quoted "<<" — every check of a mapping's keys gives what it gave.
func TestGeneratedDocumentsWithoutAnAliasKeyGiveTheKeyChecksWhatTheyGave(t *testing.T) {
	random := rand.New(rand.NewPCG(46, 119))
	keys := []string{"enabled", "endpoint", "username", "password", "path", "body", "'path'", "method", "ttl", "stale_if_error", "retry", "headers", "zz", "1", "true", "~", `"<<"`, "x-a"}
	values := []string{"1", "x", "~", "*v", "true", "false", "1m", "''", "{attempts: 2}", "{atempts: 2}", "[a]", "{X-A: b}"}
	mapping := func(merges []string) string {
		var parts []string
		for range random.IntN(5) {
			parts = append(parts, keys[random.IntN(len(keys))]+": "+values[random.IntN(len(values))])
		}
		if len(merges) > 0 && random.IntN(3) > 0 {
			merge := "<<: " + merges[random.IntN(len(merges))]
			if random.IntN(2) == 0 {
				merge = "<<: [" + strings.Join(merges, ", ") + "]"
			}
			parts = slices.Insert(parts, random.IntN(len(parts)+1), merge)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	rounds := alloctest.UnlessRaced(800, 200)
	compared := 0
	for range rounds {
		var document strings.Builder
		document.WriteString("v: &v 7\n")
		var anchors []string
		for i := range 3 {
			fmt.Fprintf(&document, "a%d: &a%d %s\n", i, i, mapping(anchors))
			anchors = append(anchors, fmt.Sprintf("*a%d", i))
		}
		fmt.Fprintf(&document, "m: %s\n", mapping(anchors))
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(document.String()), &doc); err != nil {
			continue
		}
		sameAsItWas(t, document.String(), &doc, map[*yaml.Node]bool{})
		compared++
	}
	if compared < rounds*3/4 {
		t.Fatalf("only %d of %d generated documents were YAML", compared, rounds)
	}
}
