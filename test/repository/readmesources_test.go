package repository

import (
	"path/filepath"
	"strings"
	"testing"
)

// The README's opening is where a reader decides whether this reads their
// source at all. It started as an HTTP exporter and kept saying so after it
// learned gRPC, Graphite, files, SSH, SQL and S3; this keeps the tagline and the
// paragraph after it naming every request type in the tree, and makes a new
// request type say how the README should name it.

// readmeSourceNames is, per request type, the words the tagline and the
// sources paragraph each use for it.
var readmeSourceNames = map[string]struct{ tagline, sources string }{
	"http":      {"HTTP", "HTTP services"},
	"grpc":      {"gRPC", "gRPC services"},
	"graphite":  {"Graphite", "Graphite servers"},
	"localfile": {"a file", "local files"},
	"sshfile":   {"another host", "over SSH"},
	"sql":       {"SQL database", "SQL databases"},
	"dns":       {"DNS name", "DNS records"},
	"tlscert":   {"TLS certificate", "TLS certificates"},
	"s3file":    {"S3 bucket", "objects in Amazon S3"},
}

// readmeOpening returns the README's tagline and its sources paragraph: the
// first two paragraphs of prose after the heading and the badges.
func readmeOpening(t *testing.T) (tagline, sources string) {
	t.Helper()
	intro, _, ok := strings.Cut(read(t, "README.md"), "\n## How it works")
	if !ok {
		t.Fatal("README.md has no \"## How it works\" section; the opening has moved")
	}
	var prose []string
	for _, para := range strings.Split(intro, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" || strings.HasPrefix(para, "<") || strings.HasPrefix(para, "#") || strings.HasPrefix(para, "[![") {
			continue
		}
		prose = append(prose, strings.Join(strings.Fields(para), " "))
	}
	if len(prose) < 2 {
		t.Fatalf("README.md's opening has %d paragraphs of prose, want a tagline and a sources paragraph", len(prose))
	}
	return prose[0], prose[1]
}

func TestReadmeOpeningNamesEveryRequestType(t *testing.T) {
	files, err := filepath.Glob("internal/fetch/requesttype_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "requesttype_"), ".go")
		if name == "none" {
			continue
		}
		types = append(types, name)
	}
	if len(types) < 6 {
		t.Fatalf("found %d request types under internal/fetch; the files have moved", len(types))
	}
	tagline, sources := readmeOpening(t)
	for _, typ := range types {
		names, ok := readmeSourceNames[typ]
		if !ok {
			t.Errorf("request type %q has no entry in readmeSourceNames: name it in the README's tagline and sources paragraph, then add it", typ)
			continue
		}
		if !strings.Contains(tagline, names.tagline) {
			t.Errorf("the README's tagline does not name %q (%s): %q", names.tagline, typ, tagline)
		}
		if !strings.Contains(sources, names.sources) {
			t.Errorf("the README's sources paragraph does not name %q (%s): %q", names.sources, typ, sources)
		}
	}
}
