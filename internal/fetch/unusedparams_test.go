package fetch

import (
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The refusal of a probe parameter nothing uses names the places a
// placeholder of the collector can stand in (CheckPathParams).

// unusedRefusals are the refusal of param_tenat by a collector named
// "places" of each request type, whose request.path, where its type has
// one, is /p/{{param_path:x}}.
var unusedRefusals = map[string]string{
	RequestTypeHTTP:       `probe parameters param_tenat are not used by collector "places": no placeholder in its request.path ("/p/{{param_path:x}}"), body, header or query values or its label values names them`,
	RequestTypeGraphite:   `probe parameters param_tenat are not used by collector "places": no placeholder in its request.targets, its request.path ("/p/{{param_path:x}}"), its header or query values or its label values names them`,
	RequestTypeGRPC:       `probe parameters param_tenat are not used by collector "places": no placeholder in its request.message, its metadata values or its label values names them`,
	RequestTypeLocalFile:  `probe parameters param_tenat are not used by collector "places": no placeholder in its request.path ("/p/{{param_path:x}}") or its label values names them`,
	RequestTypeSSHFile:    `probe parameters param_tenat are not used by collector "places": no placeholder in its request.path ("/p/{{param_path:x}}") or its label values names them`,
	RequestTypeSQL:        `probe parameters param_tenat are not used by collector "places": no placeholder in its request.database, its args or its label values names them`,
	RequestTypeDNS:        `probe parameters param_tenat are not used by collector "places": no placeholder in its label values names them`,
	RequestTypeTLSCert:    `probe parameters param_tenat are not used by collector "places": no placeholder in its label values names them`,
	RequestTypeKubernetes: `probe parameters param_tenat are not used by collector "places": no placeholder in its label values names them`,
	RequestTypeS3File:     `probe parameters param_tenat are not used by collector "places": no placeholder in its request.path ("/p/{{param_path:x}}") or its label values names them`,
	RequestTypeRedis:      `probe parameters param_tenat are not used by collector "places": no placeholder in the arguments of its request.commands or its label values names them`,
	RequestTypeTCP:        `probe parameters param_tenat are not used by collector "places": no placeholder in its request.send or its label values names them`,
	RequestTypeLDAP:       `probe parameters param_tenat are not used by collector "places": no placeholder in the base or filter of its request.searches or its label values names them`,
	RequestTypeSshexec:    `probe parameters param_tenat are not used by collector "places": no placeholder in its label values names them`,
}

// ownPlaceholders put, by request type, a placeholder in each of the
// type's own keys that is filled; each type's test file adds its own.
var ownPlaceholders = map[string]func(*model.Collector){}

// placeWords are the words the refusal has for each place a placeholder is
// filled in, by the start of the place's name (TemplatedFields).
var placeWords = []struct{ field, word string }{
	{"request.path", "request.path"},
	{"request.body", "body"},
	{"request.headers.", "header"},
	{"request.query.", "query values"},
	{"request.message", "request.message"},
	{"request.metadata.", "metadata values"},
	{"request.targets[", "request.targets"},
	{"request.database", "request.database"},
	{"request.args[", "args"},
	{"request.commands[", "request.commands"},
	{"request.send", "request.send"},
	{"request.searches[", "request.searches"},
	{"transform.labels.", "label values"},
}

// For every request type of the build, the refusal of a parameter nothing
// uses names every place a placeholder of such a collector is filled in,
// and no place its type does not have: a graphite collector's expressions,
// and no body; a grpc collector's message and metadata values; a localfile,
// sshfile or s3file collector's path and nothing else of the request; a sql
// collector's database and args; a redis collector's command arguments; a
// tcp collector's send; an ldap collector's searches' base and filter. The
// collector has a placeholder in each request key its type takes of those
// that are filled, so a type that starts filling another place fails here
// until the refusal names it.
func TestTheRefusalOfAnUnusedParameterNamesThePlacesOfTheRequestType(t *testing.T) {
	types := BuiltRequestTypes()
	if len(types) == 0 {
		t.Skip("the build has no request type")
	}
	for _, name := range types {
		want, ok := unusedRefusals[name]
		if !ok {
			t.Errorf("request type %s has no refusal to compare with", name)
			continue
		}
		fields := RequestTypes[name].Keys()
		c := labelCollector(map[string]string{"tenant": "{{param_tenant:t}}"})
		c.Name = "places"
		c.Request.Type = name
		if slices.Contains(fields, "path") {
			c.Request.Path = "/p/{{param_path:x}}"
		}
		if slices.Contains(fields, "headers") {
			c.Request.Headers = map[string]string{"X-Set": "{{param_header:x}}"}
		}
		if slices.Contains(fields, "query") {
			c.Request.Query = map[string]string{"set": "{{param_query:x}}"}
		}
		if fill := ownPlaceholders[name]; fill != nil {
			fill(&c)
		}
		if err := readLabels(&c); err != nil {
			t.Fatal(err)
		}
		err := CheckPathParams(&c, RequestOverrides{Params: map[string]string{"param_tenat": "acme"}})
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v\nwant %s", name, err, want)
			continue
		}
		filled := TemplatedFields(&c)
		// A collector of a type with neither a path nor a placeholder in a
		// key of its own, as a dns, tlscert or kubernetes collector, has no
		// placeholder in its request, and fills its label values alone.
		least := 2
		if !slices.Contains(fields, "path") && ownPlaceholders[name] == nil {
			least = 1
		}
		if len(filled) < least {
			t.Errorf("%s: the places filled are %v", name, filled)
		}
		for _, place := range placeWords {
			has := slices.ContainsFunc(filled, func(field string) bool { return strings.HasPrefix(field, place.field) })
			// The words after the collector's name are the places.
			_, places, _ := strings.Cut(err.Error(), ": no placeholder in ")
			if named := strings.Contains(places, place.word); named != has {
				t.Errorf("%s: a placeholder in %s is filled: %t, and the refusal names %q: %t\n%s", name, place.field, has, place.word, named, err)
			}
		}
		// Each of the collector's own parameters is used.
		for _, param := range RequestParams(&c) {
			if err := CheckPathParams(&c, RequestOverrides{Params: map[string]string{param.Name: "v"}}); err != nil {
				t.Errorf("%s, %s: %v", name, param.Name, err)
			}
		}
	}
}
