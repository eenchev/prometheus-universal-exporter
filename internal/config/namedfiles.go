package config

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// Loading a configuration opens more files than the configuration and its
// collector files: the files it names that validation reads, so the load
// refuses what would otherwise fail later. A reload can therefore be refused
// for a file the watch never looked at, such as a certificate missing for the
// moment in which a Secret is replaced. With the configuration itself
// unchanged, that reload would never be tried again. So the watch also
// stamps those files while a configuration is refused (Manager.retryFiles).
// It stamps the descriptor files among them, which the configuration is
// checked against, as long as the configuration is in force
// (Manager.descriptors): a collector reads them again at a call when they
// change, and the reload at the next tick says whether the configuration
// still fits them.

// namedFiles are the files named by the configurations that loading one
// opens, each once, in order:
//
//   - otlp.tls.ca_file, cert_file and key_file, when otlp is enabled: the
//     export needs them from the start (fetch.CheckClientTLS);
//   - web.basic_auth.username_file and password_file, when web.basic_auth is
//     enabled (validateWebAuth);
//   - the files a collector's request names that checking it reads
//     (fetch.NamedFiles): a grpc collector's request.protoset_file and
//     request.proto_files, which its method and message are checked against;
//   - the files the value_map_file keys of a collector's rules name
//     (valueMapFiles), whose maps are part of the rules.
//
// A collector's own credential and TLS files are not among them: they are
// read at a request, not at the load, so they refuse no configuration. The
// files a .proto file imports are not named by the configuration, and are
// known only once the load has read the importing files (importedFiles). A
// nil configuration names none. A load WithoutNamedFiles (the target
// allocator's) opens none of them.
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
			add(fetch.NamedFiles(&c.Collectors[i])...)
			add(valueMapFiles(&c.Collectors[i])...)
		}
	}
	return files
}

// readFiles is fetch.ReadFiles; a test counts its calls.
var readFiles = fetch.ReadFiles

// importedFiles are the paths that reading the descriptor files of the
// configurations' collectors looked at, other than those in named. These are
// the files that the .proto files of a grpc collector's request.proto_files
// import, directly or through other files, as its import paths resolved
// them, and the places where such a file was looked for and not found
// (fetch.ReadFiles). A load is refused for these just as for a file the
// configuration names (an imported file that does not compile, or is
// missing), so they are stamped with the named files while it is refused.
//
// They are known only after the load has read the files that import them,
// so unlike the named files they cannot be stamped before that reading.
// They are stamped here, after it. settled reports that the stamp is as good
// as one taken before: nothing the reading looked at changed between the
// reading and the stamp. When something did, the caller reloads at the next
// tick whatever it finds, as it would for an edit made while a named file
// was being read.
func importedFiles(named []string, configs ...*model.Config) (files []string, stamp string, settled bool) {
	look := func() (files, reads []string) {
		for _, c := range configs {
			if c == nil {
				continue
			}
			for i := range c.Collectors {
				paths, read := readFiles(&c.Collectors[i])
				reads = append(reads, read)
				for _, path := range paths {
					if !slices.Contains(named, path) && !slices.Contains(files, path) {
						files = append(files, path)
					}
				}
			}
		}
		return files, reads
	}
	files, reads := look()
	if len(files) == 0 {
		return nil, "", true
	}
	stamp = filesStamp(files)
	_, again := look()
	return files, stamp, slices.Equal(reads, again)
}

// stampedFiles are files and their state at one moment (filesStamp). A stamp
// that no files have, like the empty stamp of files that are there to stamp,
// says they must be read again whatever their state. imports reports that
// the files imported by the .proto files among them are included.
type stampedFiles struct {
	files   []string
	stamp   string
	imports bool
}

// changed reports whether one of the files is no longer as it was stamped.
func (s stampedFiles) changed() bool {
	return len(s.files) > 0 && filesStamp(s.files) != s.stamp
}

// namedDescriptorFiles are the descriptor files a configuration names: the
// request.protoset_file and request.proto_files of its collectors. Only a
// grpc collector may set them (fetch.ValidateRequest), so a build without
// the grpc request type has none in force. Of the files in namedFiles, these
// are the ones the configuration (its method and its messages) is checked
// against, so the watch keeps looking at them once the configuration is in
// force (Manager.descriptors). The certificates and credential files in
// namedFiles say nothing about the configuration, and are watched only while
// it is refused. A nil configuration names none.
func namedDescriptorFiles(c *model.Config) []string {
	if c == nil {
		return nil
	}
	return namedFiles(&model.Config{Collectors: c.Collectors})
}

// stampDescriptors stamps the descriptor files of a configuration that is
// about to be validated. It stamps the files the configuration names, as
// they are before anything reads them. It also stamps the files its .proto
// files import: these are read here to learn which they are, before the
// validation needs them, and stamped after the reading (importedFiles). The
// validation that follows checks the configuration against that reading
// while the files stay unchanged, and reads them again when one changed,
// which the stamp then shows. A file edited at any moment from here on,
// while the configuration is validated or put in force, no longer matches
// the stamp, and the next tick of the watch reloads. The compile is the one
// the validation would make, kept for it (fetch.ReadFiles), so stamping
// costs no second compile, except for files that do not compile, which
// whoever needs them reads again.
//
// The configuration is as its files write it, and the validation changes
// nothing that decides which the files are: the paths are used as written,
// and the request.type and request.descriptors that decide whether there are
// imported files are read here as the validation names them, in any case
// (fetch.ReadFiles). So the files stamped are the ones the validated
// configuration reads, and nothing is left to stamp once it is in force.
func stampDescriptors(c *model.Config) stampedFiles {
	named := namedDescriptorFiles(c)
	files, stamp, imports := retryImported(named, filesStamp(named), c)
	return stampedFiles{files: files, stamp: stamp, imports: imports}
}

// targetsNamedFiles are the files that checking a static target file against
// the configurations opens, each once: the descriptor files of the collectors
// its targets name that set a request.message. A grpc collector's message
// type is read from them to check the target's (fetch.CheckTargetRequest).
// So a reload of the target file can be refused for a file that neither the
// target file nor the watch of a configuration in force looks at. It is
// tried again when that file changes (Manager.targetsRetryFiles). The files
// a target itself names, its credential files, are read at a scrape, not at
// the load, and refuse no target file.
func targetsNamedFiles(f *model.StaticTargetFile, configs ...*model.Config) []string {
	return namedFiles(targetsChecked(f, configs...))
}

// targetsChecked are the collectors whose descriptor files are opened when a
// static target file is checked against the configurations, as a
// configuration of their own. The files they name are targetsNamedFiles, and
// the files those import are watched with them (importedFiles).
func targetsChecked(f *model.StaticTargetFile, configs ...*model.Config) *model.Config {
	var checked model.Config
	for _, c := range configs {
		if c == nil {
			continue
		}
		// Only a target that sets a message has its collector looked for,
		// so a target file that sets none, as every one without a grpc
		// collector, goes through no collectors at all.
		var collectors map[string]*model.Collector
		for i := range f.Targets {
			if f.Targets[i].Request.Message == "" {
				continue
			}
			if collectors == nil {
				collectors = collectorsByName(c)
			}
			if collector := collectors[f.Targets[i].Collector]; collector != nil {
				checked.Collectors = append(checked.Collectors, *collector)
			}
		}
	}
	return &checked
}

// filesStamp describes files as they are on disk now: for each path, the
// file it leads to with its modification time, size and permissions, or the
// fact that there is none. The path is followed as reading it would follow
// it, so a file replaced by pointing a symbolic link elsewhere is another
// file even when its time and size match the old one. This is how
// Kubernetes publishes a new version of a mounted Secret or ConfigMap, by
// swapping ..data. The file is also identified by the device and inode its
// stat holds, where the system gives them (fetch.FileIdentity), as the check
// of a grpc collector's descriptor files does at each call. A file renamed
// over by another with the same time, size and permissions at the same path
// leads to the same resolved path, and only its identity tells the call's
// new files from the ones the configuration was checked against. The
// identity costs nothing beyond the stat.
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
		if dev, ino, ok := fetch.FileIdentity(st); ok {
			b.WriteString(strconv.FormatUint(dev, 10))
			b.WriteByte('.')
			b.WriteString(strconv.FormatUint(ino, 10))
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
