package config

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// The files a refused reload is tried again for now include the files a
// .proto file imports (importedFiles). These tests hold what that left as it
// was: the files a configuration names, the files watched for a
// configuration with no .proto files to compile, and the words of
// retried_when.

// oldNamedFiles is namedFiles as it was before imported files were watched.
func oldNamedFiles(configs ...*model.Config) []string {
	var files []string
	add := func(paths ...string) {
		for _, path := range paths {
			if path != "" && !slices.Contains(files, path) {
				files = append(files, path)
			}
		}
	}
	for _, c := range configs {
		if c == nil {
			continue
		}
		if c.OTLP.Enabled {
			add(c.OTLP.TLS.CAFile, c.OTLP.TLS.CertFile, c.OTLP.TLS.KeyFile)
		}
		if auth := c.Web.BasicAuth; auth != nil && auth.Enabled {
			add(auth.UsernameFile, auth.PasswordFile)
		}
		for i := range c.Collectors {
			request := &c.Collectors[i].Request
			add(request.ProtosetFile)
			add(request.ProtoFiles...)
		}
	}
	return files
}

// generatedConfig is a configuration whose blocks that name files are on or
// off, with files that are there or not, and whose collectors are of the
// types that compile no .proto file: http, localfile and graphite ones, grpc
// ones that read a descriptor set, ask the reflection service or call the
// health service, and one of no type that names .proto files.
func generatedConfig(r *rand.Rand, dir string) *model.Config {
	file := func() string {
		switch r.IntN(4) {
		case 0:
			return ""
		case 1:
			return dir + "/absent-" + strconv.Itoa(r.IntN(3))
		}
		return dir + "/present-" + strconv.Itoa(r.IntN(3))
	}
	c := &model.Config{}
	c.OTLP.Enabled = r.IntN(2) == 0
	c.OTLP.TLS = model.TLSConfig{CAFile: file(), CertFile: file(), KeyFile: file()}
	if r.IntN(3) > 0 {
		c.Web.BasicAuth = &model.ExporterBasicAuth{Enabled: r.IntN(2) == 0, UsernameFile: file(), PasswordFile: file()}
	}
	for i := range r.IntN(4) {
		collector := model.Collector{Name: fmt.Sprint("c", i)}
		switch r.IntN(7) {
		case 0:
			collector.Request = model.RequestConfig{Type: "http", BearerTokenFile: file(), TLS: model.TLSConfig{CAFile: file()}}
		case 1:
			collector.Request = model.RequestConfig{Type: "localfile"}
		case 2:
			collector.Request = model.RequestConfig{Type: "graphite"}
		case 3:
			collector.Request = model.RequestConfig{Type: "grpc", Descriptors: "protoset", ProtosetFile: file()}
		case 4:
			collector.Request = model.RequestConfig{Type: "grpc", Descriptors: "reflection"}
		case 5:
			collector.Request = model.RequestConfig{Type: "grpc"}
		case 6:
			collector.Request = model.RequestConfig{ProtoFiles: []string{file(), file()}, ProtoImportPaths: []string{dir}}
		}
		c.Collectors = append(c.Collectors, collector)
	}
	return c
}

// A configuration with no .proto files to compile is watched for exactly the
// files it was: over generated configurations, alone and in pairs as a
// refused one is with the one in force, the files named are those the old
// list had, in its order; no imported file joins them, so the files stamped
// for a refused reload, and their stamp, are the named ones'; and the files
// of a target file's check are the same too.
func TestAConfigurationThatCompilesNoProtoFilesIsWatchedForTheFilesItWas(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		testutil.WriteIn(t, dir, fmt.Sprint("present-", i), "x")
	}
	r := rand.New(rand.NewPCG(1, 2))
	for round := range 400 {
		configs := []*model.Config{generatedConfig(r, dir), nil}
		if r.IntN(2) == 0 {
			configs[1] = generatedConfig(r, dir)
		}
		want := oldNamedFiles(configs...)
		named := namedFiles(configs...)
		if !slices.Equal(named, want) {
			t.Fatalf("round %d: the files named are %v, and were %v", round, named, want)
		}
		stamp := filesStamp(named)
		files, stamped, imports := retryImported(named, stamp, configs...)
		if !slices.Equal(files, want) || stamped != stamp || imports {
			t.Fatalf("round %d: the files stamped for a refused reload are %v (imported files among them: %v), and were %v", round, files, imports, want)
		}
		if imported, _, settled := importedFiles(nil, configs...); len(imported) != 0 || !settled {
			t.Fatalf("round %d: files %v are imported by a configuration that compiles no .proto file", round, imported)
		}
		// Every collector with a target that sets a message.
		file := &model.StaticTargetFile{}
		for _, c := range configs {
			if c == nil {
				continue
			}
			for i := range c.Collectors {
				file.Targets = append(file.Targets, model.StaticTarget{Name: fmt.Sprint("t", i), Collector: c.Collectors[i].Name, Request: model.TargetRequestConfig{Message: "{}"}})
			}
		}
		var checked model.Config
		for _, c := range configs {
			if c == nil {
				continue
			}
			for i := range file.Targets {
				if collector := model.CollectorByName(c, file.Targets[i].Collector); collector != nil {
					checked.Collectors = append(checked.Collectors, *collector)
				}
			}
		}
		if got, want := targetsNamedFiles(file, configs...), oldNamedFiles(&checked); !slices.Equal(got, want) {
			t.Fatalf("round %d: the files of the target file's check are %v, and were %v", round, got, want)
		}
	}
}

// oldRetriedWhen is what retried_when said before imported files were
// watched, for a file with or without stamped files, waiting for the other
// file or not.
func oldRetriedWhen(file, stamped string, hasStamped bool, other string, waits bool) string {
	when := file
	switch {
	case hasStamped && waits:
		when += ", " + stamped + " or " + other
	case hasStamped:
		when += " or " + stamped
	case waits:
		when += " or " + other
	}
	return when + " changes"
}

// retried_when says what it said for a file refused with no imported file
// among those stamped for it, and names the imported files as one more
// thing whose change reads the file again when there are such; without the
// watch it says nothing either way.
func TestRetriedWhenSaysWhatItSaidAndNamesImportedFiles(t *testing.T) {
	for _, watch := range []bool{true, false} {
		m := &Manager{}
		if watch {
			m.SetWatchInterval(time.Minute)
		}
		for _, file := range []struct{ file, stamped, other string }{
			{"the configuration", "a file it names", "the static target file"},
			{"the static target file", "a file its check opens", "the configuration"},
		} {
			for _, hasStamped := range []bool{false, true} {
				for _, waits := range []bool{false, true} {
					var stamped []string
					if hasStamped {
						stamped = []string{file.stamped}
					}
					attrs := m.retriedWhen([]any{"trigger", "watch"}, file.file, stamped, file.other, waits)
					if !watch {
						if len(attrs) != 2 {
							t.Fatalf("without the watch the line carries %v", attrs)
						}
						continue
					}
					if want := oldRetriedWhen(file.file, file.stamped, hasStamped, file.other, waits); len(attrs) != 4 || attrs[2] != "retried_when" || attrs[3] != want {
						t.Errorf("stamped %v, waits %v: the line carries %v, and said %q", hasStamped, waits, attrs, want)
					}
				}
			}
		}
		if !watch {
			continue
		}
		both := []string{"a file it names", "a file one of those imports"}
		for waits, want := range map[bool]string{
			false: "the configuration, a file it names or a file one of those imports changes",
			true:  "the configuration, a file it names, a file one of those imports or the static target file changes",
		} {
			if attrs := m.retriedWhen(nil, "the configuration", both, "the static target file", waits); attrs[1] != want {
				t.Errorf("with imported files, waits %v: retried_when %q, want %q", waits, attrs[1], want)
			}
		}
	}
}
