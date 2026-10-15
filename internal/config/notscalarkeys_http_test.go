//go:build !select_request_types || request_type_http

package config

import (
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
)

// A document without a mapping key that is a sequence or a mapping loads as
// it did before such a key was refused: every document the repository ships
// (shippedDocuments) and generated ones whose keys come near such a key
// without being one — a key written after ?, quoted text that looks like a
// list or a mapping, null, ~, numbers, booleans, an empty key, an alias of
// text, merges of a mapping — in the configuration, read as a collector file
// and in the static target file, with or without the expansion of the
// environment in turn, give the same configuration, or the same error word for word,
// as the load with the refusal taken out (refuseNotScalarKeys refusing
// nothing), which is the load as it was.
func TestADocumentWithoutAKeyThatIsNoScalarLoadsAsBefore(t *testing.T) {
	documents := shippedDocuments(t)
	random := rand.New(rand.NewPCG(69, 3))
	keys := []string{"A: b", "? A : b", "'[k]': v", "\"{k: 1}\": v", "~: v", "null: v", "1: v", "true: v", "'': v", "*k : v", "<<: *h", "? 'x y' : z", "B-c: '[d]'"}
	mapping := func() string {
		var parts []string
		for range 1 + random.IntN(3) {
			parts = append(parts, keys[random.IntN(len(keys))])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	anchors := "x-k: &k K\nx-h: &h {H: i}\nx-t: {? a : b, '[c]': d}\n"
	for range alloctest.UnlessRaced(60, 6) {
		documents[anchors+"collectors:\n  - name: a\n    request: {type: http, headers: "+mapping()+"}\n    transform: {type: regex}\n    metrics: [{name: x, expression: 'v=(\\d+)'}]\n"] = reflect.TypeOf(model.Config{})
		documents[anchors+"interval: 1m\ntargets:\n  - name: a\n    collector: c\n    target: http://x\n    labels: "+mapping()+"\n"] = reflect.TypeOf(model.StaticTargetFile{})
	}
	documents[anchors+"collectors:\n  - name: a\n    request: {type: http, headers: {A: b, <<: *h, *k : v}}\n    transform: {type: regex}\n    metrics: [{name: x, expression: 'v=(\\d+)'}]\n"] = reflect.TypeOf(model.Config{})
	t.Cleanup(func() { refuseNotScalarKeys = notScalarKeys })
	loads := map[string]func(path string, opts []LoadOption) (any, error){
		"configuration":  func(path string, opts []LoadOption) (any, error) { return Load(path, opts...) },
		"collector file": func(path string, opts []LoadOption) (any, error) { return loadCollectorFile(path, opts) },
		"static target file": func(path string, opts []LoadOption) (any, error) {
			return LoadStaticTargets(path, opts...)
		},
	}
	// Under the race detector a load is some ten times as slow: every third
	// document is compared there, each with and without the expansion in
	// turn, as it is here.
	texts := slices.Sorted(maps.Keys(documents))
	every := alloctest.UnlessRaced(1, 3)
	compared, loaded := 0, 0
	for i := 0; i < len(texts); i += every {
		document, into := texts[i], documents[texts[i]]
		path := testutil.WriteFile(t, "document.yaml", document)
		names := []string{"configuration", "collector file"}
		if into == reflect.TypeOf(model.StaticTargetFile{}) {
			names = []string{"static target file"}
		}
		for j, name := range names {
			var opts []LoadOption
			if (i/every+j)%2 == 1 {
				opts = []LoadOption{WithEnvExpansion(), WithStaticTargetsEnvExpansion()}
			}
			refuseNotScalarKeys = func([]byte) error { return nil }
			was, wasErr := loads[name](path, opts)
			refuseNotScalarKeys = notScalarKeys
			now, err := loads[name](path, opts)
			if (err == nil) != (wasErr == nil) || err != nil && err.Error() != wasErr.Error() || !reflect.DeepEqual(now, was) {
				t.Fatalf("%s as %s: loaded %+v, %v; was %+v, %v", document, name, now, err, was, wasErr)
			}
			compared++
			if err == nil {
				loaded++
			}
		}
	}
	// Some are refused for what the check of a configuration says of them,
	// which is as it was too; enough load to compare what they load into.
	if loaded < alloctest.UnlessRaced(100, 15) {
		t.Fatalf("only %d of %d loads succeeded", loaded, compared)
	}
	t.Logf("%d loads compared, %d of them loaded", compared, loaded)
}
