package config

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Loading a configuration opens more files than the configuration and its
// collector files: the ones it names and validation reads, to refuse at the
// load what would otherwise fail later. A reload can therefore be refused
// for a file the watch never looked at — a certificate missing for the moment
// in which a Secret is replaced — and, with the configuration itself
// unchanged, would never be tried again. So the watch stamps those files
// too while a configuration is refused (Manager.retryFiles).

// namedFiles are the files the configurations name that loading one opens,
// each once, in order:
//
//   - otlp.tls.ca_file, cert_file and key_file, when otlp is enabled: the
//     export needs them from the start (fetch.CheckClientTLS);
//   - web.basic_auth.username_file and password_file, when web.basic_auth is
//     enabled (validateWebAuth);
//   - a collector's request.protoset_file and request.proto_files, which a
//     grpc collector's method and message are checked against.
//
// A collector's own credential and TLS files are not among them: they are
// read at a request, not at the load, so they refuse no configuration. Nor
// are the files a .proto file imports, which the configuration does not
// name. A nil configuration names none.
func namedFiles(configs ...*model.Config) []string {
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

// targetsNamedFiles are the files that checking a static target file
// against the configurations opens, each once: the descriptor files of the
// collectors named by its targets that set a request.message, which a grpc
// collector's message type is read from to check the target's
// (fetch.CheckTargetRequest). A reload of the target file can thus be
// refused for a file that neither it nor the watch of a configuration in
// force looks at, and is tried again when that file changes
// (Manager.targetsRetryFiles). The files a target itself names, its
// credential files, are read at a scrape, not at the load, and refuse no
// target file.
func targetsNamedFiles(f *model.StaticTargetFile, configs ...*model.Config) []string {
	var checked model.Config
	for _, c := range configs {
		if c == nil {
			continue
		}
		for i := range f.Targets {
			if collector := model.CollectorByName(c, f.Targets[i].Collector); collector != nil && f.Targets[i].Request.Message != "" {
				checked.Collectors = append(checked.Collectors, *collector)
			}
		}
	}
	return namedFiles(&checked)
}

// filesStamp describes files as they are on disk now: for each, the file its
// path leads to, with its modification time, size and permissions, or that
// there is none. The path is followed as reading it would be, so a file
// replaced by pointing a symbolic link elsewhere — how Kubernetes publishes
// a new version of a mounted Secret or ConfigMap, swapping ..data — is
// another file even when its time and size are those of the old one.
func filesStamp(paths []string) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		b.WriteByte(0)
		st, err := os.Stat(path)
		if err != nil {
			b.WriteString("missing\n")
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			b.WriteString(resolved)
		}
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(st.ModTime().UnixNano(), 10))
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(st.Size(), 10))
		b.WriteByte(0)
		b.WriteString(st.Mode().String())
		b.WriteByte('\n')
	}
	return b.String()
}
