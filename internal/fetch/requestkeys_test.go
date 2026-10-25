package fetch

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// ownKeyValue is a value of a key read into a field of Go type goType, as
// the catalogue names it, that is set, and one that is the key left out.
func ownKeyValue(goType string) (set, unset string) {
	switch goType {
	case "string":
		return "x", `""`
	case "bool":
		return "true", "false"
	case "int", "model.ByteSize":
		return "3", "0"
	case "model.Duration", "*model.Duration":
		return "5s", "~"
	case "*int":
		return "3", "~"
	case "map[string]string":
		return "{a: b}", "~"
	case "[]string":
		return "[x]", "~"
	case "*model.SQLConnections":
		return "{}", "~"
	}
	// A list of the type's own entries, such as redis's commands.
	return "[{}]", "~"
}

// A request block that sets a key of another request type's own is
// refused as setting a key of another type, whether the build has that
// type, whose struct the key is read into, or left it out, the key being
// known from the catalogue (requestkeys.json): the message is the one of a
// key of the common part the type does not take. The key written as left
// out is not refused.
func TestAKeyOfAnotherTypesOwnIsRefusedWhetherTheBuildHasTheTypeOrNot(t *testing.T) {
	built := BuiltRequestTypes()
	if len(built) == 0 {
		t.Skip("the build has no request type")
	}
	checked := 0
	for _, entry := range RequestKeyCatalogue() {
		owner := built[0]
		if owner == entry.Type {
			if len(built) == 1 {
				continue
			}
			owner = built[1]
		}
		set, unset := ownKeyValue(entry.GoType)
		for value, refused := range map[string]bool{set: true, unset: false} {
			var c model.Collector
			if err := yaml.Unmarshal(fmt.Appendf(nil, "name: c\nrequest:\n  type: %s\n  %s: %s\n", owner, entry.Key, value), &c); err != nil {
				t.Fatalf("%s: %s: %v", entry.Key, value, err)
			}
			err := ValidateRequest(&c)
			want := fmt.Sprintf(`collector "c" sets request.%s, which does not apply to request.type %q`, entry.Key, owner)
			if got := err != nil && err.Error() == want; got != refused {
				t.Errorf("%s of %s written %s under %s: %v, want refused %t", entry.Key, entry.Type, value, owner, err, refused)
			}
		}
		checked++
	}
	if checked == 0 && len(UnbuiltRequestKeys()) > 0 {
		t.Fatal("no key was checked")
	}
}

// A key of no request type's own, and of no common part, is refused as
// the decoder refuses an unknown key, and a value of the wrong kind for a
// key of a type the build left out as for one it has, a type of the
// request type's own named as that build names it.
func TestARequestBlockRefusesAnUnknownKeyAndAWrongKind(t *testing.T) {
	var c model.Collector
	err := yaml.Unmarshal([]byte("name: c\nrequest:\n  type: http\n  sned: x\n"), &c)
	if err == nil || !strings.Contains(err.Error(), "line 4: field sned not found in type model.RequestConfig") {
		t.Errorf("an unknown key: %v", err)
	}
	for _, entry := range RequestKeyCatalogue() {
		var document, want string
		switch {
		case entry.GoType == "string":
			document, want = "[x]", "line 4: cannot unmarshal !!seq into string"
		case strings.HasPrefix(entry.GoType, "[]fetch."):
			// An entry of a list of the type's own, such as redis's
			// commands, named as a build with the type names it.
			document, want = "[x]", "line 4: cannot unmarshal !!str `x` into "+strings.TrimPrefix(entry.GoType, "[]")
		case strings.HasPrefix(entry.GoType, "[]"):
			document, want = "[[x]]", "line 4: cannot unmarshal !!seq into "
		default:
			continue
		}
		err := yaml.Unmarshal(fmt.Appendf(nil, "name: c\nrequest:\n  type: %s\n  %s: %s\n", entry.Type, entry.Key, document), &c)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s of %s written %s: %v, want %q", entry.Key, entry.Type, document, err, want)
		}
	}
}

// Every type's own keys are its own: no key is the common part's, and no
// two types share one, since a key several types take belongs to the
// common part; and the catalogue names the keys of every built type's
// struct as the struct has them.
func TestEveryOwnKeyIsOneTypesAlone(t *testing.T) {
	owners := map[string]string{}
	for _, field := range yamlKeysOf(reflect.TypeFor[model.RequestConfig]()) {
		owners[field.key] = "the common part"
	}
	for _, entry := range RequestKeyCatalogue() {
		if was, ok := owners[entry.Key]; ok {
			t.Errorf("request.%s is %s's and %s's", entry.Key, was, entry.Type)
		}
		owners[entry.Key] = entry.Type
	}
	for _, rt := range OwnKeyTypes() {
		for _, key := range rt.OwnKeys() {
			if owners[key] != rt.Name {
				t.Errorf("request.%s of %s is %q's in the catalogue", key, rt.Name, owners[key])
			}
		}
	}
}
