//go:build !select_request_types || request_type_grpc

package config

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// A grpc collector reads its descriptor files again at a call when they
// change, and nothing checked the configuration against them: a descriptor
// set replaced by one without the collector's method, or a .proto file that
// no longer compiled, was found by the probes that then failed, while the
// reload metrics still read a successful load. With the watch on, the
// descriptor files of the configuration in force are now watched as long as
// it is in force (Manager.descriptors): one changing reloads at the next
// tick, as a change to the configuration file would, so what no longer fits
// is a rejected reload, and what fits is logged as a reload like any other.

// lastReloadSuccessful is http_exporter_config_last_reload_successful of a
// file.
func lastReloadSuccessful(t *testing.T, m *Manager, file string) float64 {
	t.Helper()
	for _, metric := range m.ReloadMetrics() {
		if metric.Name == "http_exporter_config_last_reload_successful" && metric.Labels["file"] == file {
			return metric.Value
		}
	}
	t.Fatalf("no http_exporter_config_last_reload_successful of %s", file)
	return 0
}

// withoutMethod is a descriptor set with a method of its services renamed,
// so that the service no longer has it.
func withoutMethod(t *testing.T, set, method string) string {
	t.Helper()
	files := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal([]byte(set), files); err != nil {
		t.Fatal(err)
	}
	renamed := false
	for _, file := range files.GetFile() {
		for _, service := range file.GetService() {
			for _, m := range service.GetMethod() {
				if m.GetName() == method {
					m.Name, renamed = proto.String(method+"Renamed"), true
				}
			}
		}
	}
	raw, err := proto.Marshal(files)
	if err != nil || !renamed {
		t.Fatalf("the set has no method %s to rename: %v", method, err)
	}
	return string(raw)
}

// A descriptor set replaced, with the configuration in force, by one that no
// longer has the collector's method is a rejected reload at the next tick:
// logged with the reason and the trigger watch, counted as a failed reload,
// the reload metric at 0 and the configuration in force the one it was; the
// static target file, whose target's message is checked against the same
// set, is read with it and rejected on its own line. It is logged once: the
// ticks after it read nothing. The old set back, the next tick accepts both.
func TestADescriptorSetWithoutTheMethodIsRejectedAtTheNextTick(t *testing.T) {
	p, set, content, logs := descriptorPair(t)
	p.manager.SetWatchInterval(time.Minute)
	inForce, targetsInForce := p.manager.InForce()

	p.write(t, set, withoutMethod(t, content, "GetStats"))
	p.manager.reloadChanged()
	config, targets := loggedOfBoth(t, logs)
	if config["msg"] != "configuration reload rejected" || config["level"] != "ERROR" || config["trigger"] != reloadTriggerWatch || config["file"] != p.configPath ||
		!strings.Contains(config["error"].(string), "has no method GetStats; it has GetStatsRenamed") {
		t.Fatalf("with a set without the method: logged %v for the configuration", config)
	}
	if want := "the configuration or a file it names changes"; config["retried_when"] != want {
		t.Errorf("the configuration's retried_when is %q, want %q", config["retried_when"], want)
	}
	if targets["msg"] != "static target reload rejected" || targets["trigger"] != reloadTriggerWatch || targets["file"] != p.targetsAt || !strings.Contains(targets["error"].(string), "has no method GetStats") {
		t.Fatalf("with a set without the method: logged %v for the static target file", targets)
	}
	if want := "the static target file, a file its check opens or the configuration changes"; targets["retried_when"] != want {
		t.Errorf("the static target file's retried_when is %q, want %q", targets["retried_when"], want)
	}
	for _, file := range []string{reloadFileConfig, ReloadFileStaticTargets} {
		if successes, failures := p.reloads(file); successes != 0 || failures != 1 || lastReloadSuccessful(t, p.manager, file) != 0 {
			t.Errorf("%s: %d reloads, %d refused, last reload successful %v; want one refused and 0", file, successes, failures, lastReloadSuccessful(t, p.manager, file))
		}
	}
	if cfg, file := p.manager.InForce(); cfg != inForce || file != targetsInForce {
		t.Fatal("a rejected reload changed what is in force")
	}
	for range 3 {
		p.manager.reloadChanged()
	}
	if _, failures := p.reloads(reloadFileConfig); logs.Len() != 0 || failures != 1 {
		t.Fatalf("ticks with nothing changed: %d reloads refused, logged %q", failures, logs)
	}

	p.write(t, set, content)
	p.manager.reloadChanged()
	config, targets = loggedOfBoth(t, logs)
	if config["msg"] != "configuration reloaded" || config["trigger"] != reloadTriggerWatch || targets["msg"] != "static targets reloaded" || targets["trigger"] != reloadTriggerWatch {
		t.Fatalf("with the old set back: logged %v and %v", config, targets)
	}
	for _, file := range []string{reloadFileConfig, ReloadFileStaticTargets} {
		if successes, failures := p.reloads(file); successes != 1 || failures != 1 || lastReloadSuccessful(t, p.manager, file) != 1 {
			t.Errorf("%s with the old set back: %d reloads, %d refused, last reload successful %v", file, successes, failures, lastReloadSuccessful(t, p.manager, file))
		}
	}
	if cfg, _ := p.manager.InForce(); cfg == inForce {
		t.Fatal("the accepted reload put no configuration in force")
	}
}

// A .proto file of the configuration in force that changes and still fits
// is one accepted reload at the next tick, logged as any other with the
// trigger watch, whichever file it is: the one the configuration names, one
// it imports, one two imports away, or several of them at once. The static
// target file, whose target sets no message and so is checked against no
// descriptor, is not read. The ticks before and after find nothing changed.
func TestAProtoFileChangedInForceIsOneAcceptedReload(t *testing.T) {
	for name, edit := range map[string]func(t *testing.T, dir string){
		"the file the configuration names": func(t *testing.T, dir string) {
			writeProto(t, dir, importingService, serviceSource+"// a comment\n")
		},
		"a file it imports": func(t *testing.T, dir string) {
			writeProto(t, dir, importedTypes, typesSource+"message Other { string name = 1; }\n")
		},
		"a file two imports away": func(t *testing.T, dir string) { writeProto(t, dir, importedBase, baseWithRegion) },
		"three files at once": func(t *testing.T, dir string) {
			writeProto(t, dir, importingService, serviceSource+"// a comment\n")
			writeProto(t, dir, importedTypes, typesSource+"// another\n")
			writeProto(t, dir, importedBase, baseWithRegion)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			service, types, base := writeService(t, dir)
			p, logs, looks := importingPair(t, dir, dir)
			if want := []string{service, base, types}; !slices.Equal(p.manager.descriptors.files, want) {
				t.Fatalf("the descriptor files watched in force are %v, want %v", p.manager.descriptors.files, want)
			}
			inForce := p.manager.Get()
			quietTicks(t, p, logs, looks, reloadFileConfig)

			edit(t, dir)
			p.manager.reloadChanged()
			line := theLine(t, logs)
			if line["msg"] != "configuration reloaded" || line["level"] != "INFO" || line["trigger"] != reloadTriggerWatch || line["collectors"] != 1.0 {
				t.Fatalf("with the file changed: logged %v", line)
			}
			if successes, failures := p.reloads(reloadFileConfig); successes != 1 || failures != 0 || lastReloadSuccessful(t, p.manager, reloadFileConfig) != 1 || p.manager.Get() == inForce {
				t.Fatalf("with the file changed: %d reloads, %d refused", successes, failures)
			}
			if successes, failures := p.reloads(ReloadFileStaticTargets); successes != 0 || failures != 0 {
				t.Fatalf("the static target file, checked against no descriptor, was read: %d reloads, %d refused", successes, failures)
			}
			quietTicks(t, p, logs, looks, reloadFileConfig)
		})
	}
}

// A .proto file of the configuration in force that no longer compiles is a
// rejected reload at the next tick, naming the file and the line, logged
// once, with the files of the refused reload named as what it is tried
// again for; mended, the next tick accepts.
func TestAProtoFileThatNoLongerCompilesIsRejectedAtTheNextTick(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	p, logs, looks := importingPair(t, dir, dir)
	inForce := p.manager.Get()
	writeProto(t, dir, importedBase, baseBroken)
	p.manager.reloadChanged()
	line := theLine(t, logs)
	if line["msg"] != "configuration reload rejected" || line["trigger"] != reloadTriggerWatch || !strings.Contains(line["error"].(string), "base.proto:3:") || p.manager.Get() != inForce {
		t.Fatalf("with an imported file that does not compile: logged %v", line)
	}
	if want := "the configuration, a file it names or a file one of those imports changes"; line["retried_when"] != want {
		t.Errorf("retried_when %q, want %q", line["retried_when"], want)
	}
	if lastReloadSuccessful(t, p.manager, reloadFileConfig) != 0 {
		t.Error("the reload metric reads a successful reload")
	}
	quietTicks(t, p, logs, looks, reloadFileConfig)
	writeProto(t, dir, importedBase, baseSource)
	p.manager.reloadChanged()
	if line = theLine(t, logs); line["msg"] != "configuration reloaded" || lastReloadSuccessful(t, p.manager, reloadFileConfig) != 1 {
		t.Fatalf("with the file mended: logged %v", line)
	}
	quietTicks(t, p, logs, looks, reloadFileConfig)
}

// The files watched in force are the ones the compile looked at, as for a
// refused reload: a file appearing in an earlier import path than the one
// it was compiled from reloads at the next tick, and is watched from then
// on; a file of the same name in a later import path is not looked at, and
// changing it reloads nothing; and no place of a well-known file is watched.
func TestAnImportAppearingEarlierInTheImportPathsReloads(t *testing.T) {
	first, second, third := t.TempDir(), t.TempDir(), t.TempDir()
	writeService(t, second)
	writeProto(t, third, importedBase, baseSource)
	p, logs, looks := importingPair(t, second, first, second, third)
	earlier := filepath.Join(first, filepath.FromSlash(importedBase))
	if !slices.Contains(p.manager.descriptors.files, earlier) {
		t.Fatalf("the place of the import in the first import path is not watched: %v", p.manager.descriptors.files)
	}
	for _, path := range p.manager.descriptors.files {
		if strings.HasPrefix(path, third) || strings.Contains(path, "google") {
			t.Errorf("%s is watched: a later import path, or a well-known file", path)
		}
	}
	writeProto(t, third, importedBase, baseBroken)
	quietTicks(t, p, logs, looks, reloadFileConfig)

	writeProto(t, first, importedBase, baseWithRegion)
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch {
		t.Fatalf("with the import in an earlier import path: logged %v", line)
	}
	quietTicks(t, p, logs, looks, reloadFileConfig)
	// The file compiled from now is the one watched.
	writeProto(t, second, importedBase, baseBroken)
	quietTicks(t, p, logs, looks, reloadFileConfig)
	writeProto(t, first, importedBase, baseSource)
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" {
		t.Fatalf("with the file of the earlier import path changed: logged %v", line)
	}
}

// A descriptor set behind a link that is pointed at another directory, as
// Kubernetes publishes a new version of a mounted ConfigMap, is another
// file, and reloads at the next tick although the new file has the size and
// the time of the old; a link swapped back and forth between two ticks is
// the file it was, and reloads nothing.
func TestADescriptorSetBehindASwappedLinkReloads(t *testing.T) {
	dir := t.TempDir()
	mount := filepath.Join(dir, "protos")
	written := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, version := range []string{"..v1", "..v2"} {
		if err := os.MkdirAll(filepath.Join(mount, version), 0o750); err != nil {
			t.Fatal(err)
		}
		file := grpctest.WriteProtoset(t, filepath.Join(mount, version, "queue.pb"), false)
		if err := os.Chtimes(file, written, written); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(version string) {
		t.Helper()
		next := filepath.Join(mount, "..data_tmp")
		if err := os.Symlink(version, next); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(mount, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	publish("..v1")
	set := filepath.Join(mount, "queue.pb")
	if err := os.Symlink(filepath.Join("..data", "queue.pb"), set); err != nil {
		t.Fatal(err)
	}
	p := newPair(t, strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset\n      protoset_file: "+set, 1), messageTargets("orders"))
	logs := &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	p.manager.reloadChanged()
	publish("..v2")
	publish("..v1")
	p.manager.reloadChanged()
	if logs.Len() != 0 {
		t.Fatalf("ticks with the link as it was logged %q", logs)
	}

	publish("..v2")
	p.manager.reloadChanged()
	if config, targets := loggedOfBoth(t, logs); config["msg"] != "configuration reloaded" || config["trigger"] != reloadTriggerWatch || targets["msg"] != "static targets reloaded" {
		t.Fatalf("with the link swapped: logged %v and %v", config, targets)
	}
	p.manager.reloadChanged()
	if successes, _ := p.reloads(reloadFileConfig); logs.Len() != 0 || successes != 1 {
		t.Fatalf("a tick after the reload: %d reloads, logged %q", successes, logs)
	}
}

// A static target's request.message that the changed descriptors refuse is
// reported for the target file, on its own line and in its own reload
// metric, at the tick that finds the descriptor file changed, although the
// target file is as it was; the configuration, which the target file in
// force no longer agrees with, is rejected with it, and both stay as they
// were. The field back in the file, the next tick accepts both.
func TestATargetMessageTheChangedDescriptorsRefuseRejectsTheTargetFile(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	writeProto(t, dir, importedBase, baseWithRegion)
	p := newPair(t, importingDocument("v1", plainMessage, dir, dir), importingTargets(regionMessage))
	logs := &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	inForce, targetsInForce := p.manager.InForce()

	writeProto(t, dir, importedBase, baseSource)
	p.manager.reloadChanged()
	config, targets := loggedOfBoth(t, logs)
	if targets["msg"] != "static target reload rejected" || targets["trigger"] != reloadTriggerWatch || targets["file"] != p.targetsAt ||
		!strings.Contains(targets["error"].(string), `target "q": request.message does not fit w.common.Request`) || !strings.Contains(targets["error"].(string), "region") {
		t.Fatalf("with the field gone from the imported file: logged %v for the static target file", targets)
	}
	if want := "the static target file, a file its check opens or the configuration changes"; targets["retried_when"] != want {
		t.Errorf("the static target file's retried_when is %q, want %q", targets["retried_when"], want)
	}
	if config["msg"] != "configuration reload rejected" || !strings.Contains(config["error"].(string), `target "q"`) {
		t.Fatalf("with the field gone from the imported file: logged %v for the configuration", config)
	}
	if want := "the configuration, a file it names, a file one of those imports or the static target file changes"; config["retried_when"] != want {
		t.Errorf("the configuration's retried_when is %q, want %q", config["retried_when"], want)
	}
	if lastReloadSuccessful(t, p.manager, ReloadFileStaticTargets) != 0 || lastReloadSuccessful(t, p.manager, reloadFileConfig) != 0 {
		t.Error("a reload metric reads a successful reload")
	}
	if cfg, file := p.manager.InForce(); cfg != inForce || file != targetsInForce {
		t.Fatal("a rejected reload changed what is in force")
	}
	for range 3 {
		p.manager.reloadChanged()
	}
	if logs.Len() != 0 {
		t.Fatalf("ticks with nothing changed logged %q", logs)
	}

	writeProto(t, dir, importedBase, baseWithRegion)
	p.manager.reloadChanged()
	if config, targets = loggedOfBoth(t, logs); config["msg"] != "configuration reloaded" || targets["msg"] != "static targets reloaded" {
		t.Fatalf("with the field back: logged %v and %v", config, targets)
	}
	if lastReloadSuccessful(t, p.manager, ReloadFileStaticTargets) != 1 || lastReloadSuccessful(t, p.manager, reloadFileConfig) != 1 {
		t.Error("a reload metric reads a failed reload after both were accepted")
	}
}

// A target file whose last reload was refused is not the one in force, and
// a descriptor file changing does not read it again when it was refused for
// what it says itself: the configuration alone is reloaded, and checked with
// the target file in force.
func TestARefusedTargetFileIsNotReadForAChangedDescriptor(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	writeProto(t, dir, importedBase, baseWithRegion)
	p := newPair(t, importingDocument("v1", plainMessage, dir, dir), importingTargets(regionMessage))
	logs := &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	p.write(t, p.targetsAt, "interval: 1m\ntargets: [\n")
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "static target reload rejected" {
		t.Fatalf("a target file that is not YAML: logged %v", line)
	}
	writeProto(t, dir, importedBase, baseWithRegion+"// a comment\n")
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" {
		t.Fatalf("with the descriptor file changed: logged %v", line)
	}
	if _, failures := p.reloads(ReloadFileStaticTargets); failures != 1 {
		t.Fatalf("the refused target file was read again for a descriptor file: %d reloads of it refused", failures)
	}
}

// A file edited while the reload that reads it runs is a change the next
// tick sees, whenever in the reload it was edited: an imported file changed
// between its reading and its stamp, before the configuration is validated,
// or the named file, as the reload begins to read. Each is one more reload,
// and the ticks after it are quiet. The reload looks at the files twice, to
// stamp them before it validates, and not again: a file edited after that,
// as the configuration is put in force, is the next test's.
func TestADescriptorFileEditedDuringTheReloadIsReadAtTheNextTick(t *testing.T) {
	for name, test := range map[string]struct {
		// look is the look at the descriptor files, counted from the reload's
		// first, at which the file is edited.
		look int
		file string
	}{
		"an imported file, between its reading and its stamp":            {2, importedBase},
		"the named file, before the first look at the files it leads to": {1, importingService},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeService(t, dir)
			p, logs, looks := importingPair(t, dir, dir)
			p.write(t, p.configPath, importingDocument("v2", plainMessage, dir, dir))
			counting := readFiles
			calls := 0
			readFiles = func(c *model.Collector) ([]string, string) {
				if calls++; calls == test.look {
					writeProto(t, dir, test.file, map[string]string{importedBase: baseWithRegion, importingService: serviceSource + "// a comment\n"}[test.file])
				}
				return counting(c)
			}
			p.manager.reloadChanged()
			readFiles = counting
			if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v2" || calls != 2 {
				t.Fatalf("the reload of the configuration: logged %v after %d looks, want the two that stamp the files", line, calls)
			}
			p.manager.reloadChanged()
			if line := theLine(t, logs); line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch {
				t.Fatalf("at the tick after the file was edited during the reload: logged %v", line)
			}
			if successes, _ := p.reloads(reloadFileConfig); successes != 2 {
				t.Fatalf("%d reloads, want the one that read the file and the one after it changed", successes)
			}
			quietTicks(t, p, logs, looks, reloadFileConfig)
		})
	}
}

// A descriptor file edited while what a reload is about to put in force is
// being prepared, long after the reload read it, is read at the next tick
// too: the files are watched as they were before the reload read them, not
// as they are when the configuration is in force.
func TestADescriptorFileEditedWhileTheReloadIsPreparedIsReadAtTheNextTick(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	p, logs, looks := importingPair(t, dir, dir)
	edits := 0
	p.manager.OnPrepare(func(*model.Config, *model.StaticTargetFile) {
		if edits++; edits == 1 {
			writeProto(t, dir, importedBase, baseWithRegion)
		}
	})
	p.write(t, p.configPath, importingDocument("v2", plainMessage, dir, dir))
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v2" {
		t.Fatalf("the reload of the configuration: logged %v", line)
	}
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" {
		t.Fatalf("at the tick after the file was edited while the reload was prepared: logged %v", line)
	}
	quietTicks(t, p, logs, looks, reloadFileConfig)
}

// startedPair is a manager started as the exporter with --config.watch
// starts one (main.go): the files stamped, the configuration read with its
// stamp, between is called, and the manager made of them, with the watch on.
// Not stamped, the configuration is read by Load, as a manager that is not
// the exporter's may be. It returns the manager and what it logs.
func startedPair(t *testing.T, document string, stamped bool, between func()) (*Manager, *bytes.Buffer) {
	t.Helper()
	path := testutil.WriteIn(t, t.TempDir(), "config.yaml", document)
	stamp := TakeStamp(path, true)
	load := func() (*model.Config, error) { return LoadStamped(path, &stamp) }
	if !stamped {
		load = func() (*model.Config, error) { return Load(path) }
	}
	cfg, err := load()
	if err != nil {
		t.Fatal(err)
	}
	between()
	logs := &bytes.Buffer{}
	m := NewManager(cfg, path, slog.New(slog.NewJSONHandler(logs, nil)))
	m.UseStamp(stamp)
	m.SetWatchInterval(time.Minute)
	return m, logs
}

// Starting the watch reloads no configuration that has not changed since it
// was loaded: the descriptor files are stamped at startup, the ones the
// configuration names before it is validated and the ones those import as
// they are read, and the first ticks find them as stamped. A descriptor
// file edited after the startup read it, before the manager took over, is a
// change the first tick reloads for, a named file and one two imports away
// alike; a configuration read without its stamp takes the edit as read.
func TestStartingTheWatchStampsTheDescriptorFilesBeforeTheyAreRead(t *testing.T) {
	for name, file := range map[string]string{"the named file": importingService, "a file two imports away": importedBase} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			service, types, base := writeService(t, dir)
			document := importingDocument("v1", plainMessage, dir, dir)
			edit := func() {
				writeProto(t, dir, file, map[string]string{importedBase: baseWithRegion, importingService: serviceSource + "// a comment\n"}[file])
			}

			m, logs := startedPair(t, document, true, func() {})
			if want := []string{service, base, types}; !slices.Equal(m.descriptors.files, want) {
				t.Fatalf("the descriptor files watched from the start are %v, want %v", m.descriptors.files, want)
			}
			for range 3 {
				m.reloadChanged()
			}
			if logs.Len() != 0 || len(m.Reloads.Rejected()) != 0 || m.Reloads.files[reloadFileConfig].successes != 0 {
				t.Fatalf("the first ticks after the start reloaded: logged %q", logs)
			}

			m, logs = startedPair(t, document, true, edit)
			m.reloadChanged()
			if line := theLine(t, logs); line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch {
				t.Fatalf("with the file edited while the exporter started: logged %v", line)
			}
			m.reloadChanged()
			if logs.Len() != 0 {
				t.Fatalf("the tick after that reload logged %q", logs)
			}

			writeProto(t, dir, file, map[string]string{importedBase: baseSource, importingService: serviceSource}[file])
			m, logs = startedPair(t, document, false, edit)
			m.reloadChanged()
			if logs.Len() != 0 {
				t.Fatalf("read without its stamp, the configuration was reloaded for an edit made before the manager was: %q", logs)
			}
		})
	}
}

// A request.type and a request.descriptors written in capitals, with blanks
// around them, are read by the stamp taken before the validation as the
// validation names them: the files the .proto files import are watched from
// the start and after a reload, and the ticks after each are quiet.
func TestTheImportsOfATypeWrittenInCapitalsAreWatched(t *testing.T) {
	dir := t.TempDir()
	service, types, base := writeService(t, dir)
	document := strings.Replace(importingDocument("v1", plainMessage, dir, dir), "type: grpc", "type: ' GRPC '", 1)
	document = strings.Replace(document, "descriptors: proto", "descriptors: ' Proto '", 1)
	m, logs := startedPair(t, document, true, func() {})
	for step := range 2 {
		if want := []string{service, base, types}; !slices.Equal(m.descriptors.files, want) {
			t.Fatalf("step %d: the descriptor files watched are %v, want %v", step, m.descriptors.files, want)
		}
		for range 3 {
			m.reloadChanged()
		}
		if logs.Len() != 0 {
			t.Fatalf("step %d: ticks with nothing changed logged %q", step, logs)
		}
		writeProto(t, dir, importedBase, baseWithRegion+strings.Repeat("// again\n", step))
		m.reloadChanged()
		if line := theLine(t, logs); line["msg"] != "configuration reloaded" {
			t.Fatalf("step %d: with the imported file changed: logged %v", step, line)
		}
	}
}

// The files watched are those of the configuration in force: a reload that
// points the collector at other .proto files watches those from then on and
// the old ones no longer, and one that takes the descriptor files from the
// collector watches none.
func TestAReloadChangesTheDescriptorFilesWatched(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeService(t, first)
	service, types, base := writeService(t, second)
	p, logs, looks := importingPair(t, first, first)

	p.write(t, p.configPath, importingDocument("v2", plainMessage, second, second))
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v2" {
		t.Fatalf("the reload to other files: logged %v", line)
	}
	if want := []string{service, base, types}; !slices.Equal(p.manager.descriptors.files, want) {
		t.Fatalf("the descriptor files watched after the reload are %v, want %v", p.manager.descriptors.files, want)
	}
	writeProto(t, first, importedBase, baseBroken)
	quietTicks(t, p, logs, looks, reloadFileConfig)
	writeProto(t, second, importedBase, baseWithRegion)
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" {
		t.Fatalf("with a file of the new ones changed: logged %v", line)
	}

	// The collector calls the health service, whose types are built in.
	health := "collectors:\n  - name: imports\n    request:\n      type: grpc\n      rpc: grpc.health.v1.Health/Check\n    transform:\n      type: jq\n    metrics:\n      - name: v3\n        expression: '1'\n"
	p.write(t, p.configPath, health)
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v3" {
		t.Fatalf("the reload to a collector without descriptor files: logged %v", line)
	}
	if len(p.manager.descriptors.files) != 0 {
		t.Fatalf("descriptor files are watched for a configuration that names none: %v", p.manager.descriptors.files)
	}
	writeProto(t, second, importedBase, baseBroken)
	writeProto(t, second, importingService, baseBroken)
	quietTicks(t, p, logs, looks, reloadFileConfig)
}

// The exporter's own credential files are not descriptor files: with the
// configuration in force, one replaced reloads nothing, as before, while the
// descriptor set of the same configuration replaced reloads at the next
// tick. Only the descriptor set is watched in force.
func TestACredentialFileChangedInForceReloadsNothing(t *testing.T) {
	dir := t.TempDir()
	username, password := testutil.WriteIn(t, dir, "username", "admin\n"), testutil.WriteIn(t, dir, "password", "s3cret\n")
	set := grpctest.WriteProtoset(t, filepath.Join(dir, "queue.pb"), false)
	raw, err := os.ReadFile(set)
	if err != nil {
		t.Fatal(err)
	}
	document := "web:\n  basic_auth: {enabled: true, username_file: " + username + ", password_file: " + password + "}\n" +
		strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset\n      protoset_file: "+set, 1)
	p := newPair(t, document, messageTargets("orders"))
	logs := &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	if want := []string{set}; !slices.Equal(p.manager.descriptors.files, want) {
		t.Fatalf("the files watched in force are %v, want %v", p.manager.descriptors.files, want)
	}
	for _, file := range []string{username, password} {
		p.write(t, file, "another\n")
		p.manager.reloadChanged()
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		p.manager.reloadChanged()
		p.write(t, file, "s3cret\n")
		p.manager.reloadChanged()
	}
	if successes, failures := p.reloads(reloadFileConfig); logs.Len() != 0 || successes != 0 || failures != 0 {
		t.Fatalf("a credential file replaced in force: %d reloads, %d refused, logged %q", successes, failures, logs)
	}
	p.write(t, set, string(raw))
	p.manager.reloadChanged()
	if config, _ := loggedOfBoth(t, logs); config["msg"] != "configuration reloaded" {
		t.Fatalf("with the descriptor set replaced: logged %v", config)
	}
}

// Without the watch there is no tick: the loop returns at once, a
// descriptor file changed reloads nothing, and no descriptor file is looked
// at.
func TestWithoutTheWatchADescriptorFileReloadsNothing(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	p, logs, looks := importingPair(t, dir, dir)
	p.manager.SetWatchInterval(0)
	writeProto(t, dir, importedBase, baseBroken)
	before := *looks
	p.manager.ReloadLoop(context.Background())
	if successes, failures := p.reloads(reloadFileConfig); logs.Len() != 0 || successes != 0 || failures != 0 || *looks != before {
		t.Fatalf("without the watch: %d reloads, %d refused, %d looks at the descriptor files, logged %q", successes, failures, *looks-before, logs)
	}
}

// writtenTypes are ways a configuration may write request.type: grpc for a
// collector, as the types are named and not.
var writtenTypes = []string{"grpc", "GRPC", "' Grpc '"}

// An imported file edited while a reload is put in force, after the
// validation read it, reloads at the next tick however the collector's
// request.type is written: the files are stamped before the configuration
// is validated, when the type is still as the file writes it, and the
// imports of a collector written `type: GRPC` are found then as those of one
// written `type: grpc` are. The file no longer compiles, so the tick after
// the reload is a rejected one, and the reload metric does not go on reading
// a successful load while the collector's calls fail.
func TestAnImportEditedWhileTheReloadIsPreparedReloadsHoweverTheTypeIsWritten(t *testing.T) {
	for _, written := range writtenTypes {
		t.Run(written, func(t *testing.T) {
			dir := t.TempDir()
			service, types, base := writeService(t, dir)
			document := func(metric string) string {
				return strings.Replace(importingDocument(metric, plainMessage, dir, dir), "type: grpc", "type: "+written, 1)
			}
			p := newPair(t, document("v1"), importingTargets(""))
			logs := &bytes.Buffer{}
			p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
			p.manager.SetWatchInterval(time.Minute)
			edits := 0
			p.manager.OnPrepare(func(*model.Config, *model.StaticTargetFile) {
				if edits++; edits == 1 {
					writeProto(t, dir, importedBase, baseBroken)
				}
			})
			p.write(t, p.configPath, document("v2"))
			p.manager.reloadChanged()
			if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v2" {
				t.Fatalf("the reload of the configuration: logged %v", line)
			}
			if want := []string{service, base, types}; !slices.Equal(p.manager.descriptors.files, want) {
				t.Fatalf("the descriptor files watched after the reload are %v, want %v", p.manager.descriptors.files, want)
			}
			p.manager.reloadChanged()
			if line := theLine(t, logs); line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), "base.proto:3:") || lastReloadSuccessful(t, p.manager, reloadFileConfig) != 0 {
				t.Fatalf("the tick after an imported file was broken while the reload was put in force: logged %v, and the reload metric reads %v", line, lastReloadSuccessful(t, p.manager, reloadFileConfig))
			}
			for range 3 {
				p.manager.reloadChanged()
			}
			if logs.Len() != 0 {
				t.Fatalf("the ticks after the rejected reload logged %q", logs)
			}
		})
	}
}

// The same at startup: an imported file broken after the startup read the
// configuration, before the manager took over, is one rejected reload at
// the first tick, however the collector's request.type is written.
func TestAnImportEditedDuringStartupIsSeenByTheFirstTickHoweverTheTypeIsWritten(t *testing.T) {
	for _, written := range writtenTypes {
		t.Run(written, func(t *testing.T) {
			dir := t.TempDir()
			writeService(t, dir)
			document := strings.Replace(importingDocument("v1", plainMessage, dir, dir), "type: grpc", "type: "+written, 1)
			m, logs := startedPair(t, document, true, func() { writeProto(t, dir, importedBase, baseBroken) })
			for range 3 {
				m.reloadChanged()
			}
			if line := theLine(t, logs); line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), "base.proto:3:") {
				t.Fatalf("the first ticks after an imported file was broken during the startup logged %v, want one rejected reload", line)
			}
		})
	}
}

// The descriptor files a configuration is stamped with before it is
// validated are the ones it has once it is: over the ways a collector may
// write its request.type and its request.descriptors, the files stamped for
// the configuration as its file writes it are those stamped for the
// validated one, the named file and the two it leads to, and they are
// learnt by one compile.
func TestTheFilesStampedBeforeTheValidationAreTheValidatedConfigurations(t *testing.T) {
	dir := t.TempDir()
	service, types, base := writeService(t, dir)
	for _, writtenType := range writtenTypes {
		for _, descriptors := range []string{"proto", "PROTO", "' Proto '"} {
			document := strings.Replace(importingDocument("v1", plainMessage, dir, dir), "type: grpc", "type: "+writtenType, 1)
			document = strings.Replace(document, "descriptors: proto", "descriptors: "+descriptors, 1)
			path := testutil.WriteIn(t, t.TempDir(), "config.yaml", document)
			var written stampedFiles
			validated, err := load(path, nil, func(c *model.Config) { written = stampDescriptors(c) })
			if err != nil {
				t.Fatalf("type %s, descriptors %s: %v", writtenType, descriptors, err)
			}
			if want := []string{service, base, types}; !slices.Equal(written.files, want) || !written.imports {
				t.Errorf("type %s, descriptors %s: the files stamped before the validation are %v, want %v", writtenType, descriptors, written.files, want)
			}
			if after := stampDescriptors(validated); !slices.Equal(after.files, written.files) || after.stamp != written.stamp || after.imports != written.imports {
				t.Errorf("type %s, descriptors %s: the validated configuration is stamped %v, and was %v before it was validated", writtenType, descriptors, after, written)
			}
		}
	}
}

// A target file refused because the configuration in force disagrees with
// it — it names a collector the configuration does not have — waits for the
// configuration, and says so. It is not read again for a descriptor file of
// the configuration in force: the tick that finds one changed reloads the
// configuration alone, each time, and the target file's one refusal stays
// the one. The configuration file changing reads it again, as before.
func TestATargetFileThatWaitsIsNotReadForADescriptorFile(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	p, logs, _ := importingPair(t, dir, dir)
	p.write(t, p.targetsAt, strings.Replace(importingTargets(""), "collector: imports", "collector: nope", 1))
	p.manager.reloadChanged()
	line := theLine(t, logs)
	if line["msg"] != "static target reload rejected" || line["retried_when"] != "the static target file or the configuration changes" {
		t.Fatalf("a target file naming a collector the configuration has not: logged %v", line)
	}
	for step := range 2 {
		writeProto(t, dir, importedBase, baseWithRegion+strings.Repeat("// again\n", step))
		p.manager.reloadChanged()
		if line := theLine(t, logs); line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch {
			t.Fatalf("step %d: a descriptor file changed, the configuration and the refused target file untouched: logged %v", step, line)
		}
		if _, failures := p.reloads(ReloadFileStaticTargets); failures != 1 {
			t.Fatalf("step %d: the target file that waits was read for a descriptor file: %d reloads of it refused, want the one", step, failures)
		}
	}
	p.write(t, p.configPath, importingDocument("v2", plainMessage, dir, dir))
	p.manager.reloadChanged()
	config, targets := loggedOfBoth(t, logs)
	if _, failures := p.reloads(ReloadFileStaticTargets); config["msg"] != "configuration reloaded" || targets["msg"] != "static target reload rejected" || failures != 2 {
		t.Fatalf("with the configuration file changed: logged %v and %v, %d reloads of the target file refused; want it read again with the configuration", config, targets, failures)
	}
}

// A target file refused because the descriptors refuse its message waits
// for the configuration too, and the descriptor files its own check opened
// are among what it is tried again for: the tick after the imported file
// has the field reads the target file again, and accepts it, with the
// configuration whose descriptor file it is. The ticks between read nothing.
func TestATargetFileRefusedByTheDescriptorsIsTriedAgainWhenTheyChange(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	p, logs, looks := importingPair(t, dir, dir)
	p.write(t, p.targetsAt, importingTargets(regionMessage))
	p.manager.reloadChanged()
	line := theLine(t, logs)
	if line["msg"] != "static target reload rejected" || !strings.Contains(line["error"].(string), "region") {
		t.Fatalf("a target whose message sets a field the imported file has not: logged %v", line)
	}
	if want := "the static target file, a file its check opens or the configuration changes"; line["retried_when"] != want {
		t.Errorf("retried_when %q, want %q", line["retried_when"], want)
	}
	quietTicks(t, p, logs, looks, ReloadFileStaticTargets)

	writeProto(t, dir, importedBase, baseWithRegion)
	p.manager.reloadChanged()
	config, targets := loggedOfBoth(t, logs)
	if config["msg"] != "configuration reloaded" || targets["msg"] != "static targets reloaded" || messageInForce(p) != regionMessage {
		t.Fatalf("with the field in the imported file: logged %v and %v, in force %s", config, targets, messageInForce(p))
	}
	quietTicks(t, p, logs, looks, ReloadFileStaticTargets)
}

// A target file that waits for the configuration is read again when a file
// of the refused configuration changes, a descriptor file among them: the
// configuration adds a collector whose imported file does not compile, and
// is refused; the target file names that collector, and waits; the file
// mended, the next tick reads both, and both go in force.
func TestATargetFileThatWaitsIsReadWhenAFileOfTheRefusedConfigurationIsMended(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	writeProto(t, dir, importedBase, baseBroken)
	p := newPair(t, healthDocument("v0"), importingTargets(""))
	logs := &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	added := strings.Replace(importingDocument("v1", plainMessage, dir, dir), "name: imports", "name: added", 1)
	p.write(t, p.configPath, healthDocument("v0")+strings.TrimPrefix(added, "collectors:\n"))
	p.write(t, p.targetsAt, strings.Replace(importingTargets(""), "collector: imports", "collector: added", 1))
	p.manager.reloadChanged()
	config, targets := loggedOfBoth(t, logs)
	if config["msg"] != "configuration reload rejected" || !strings.Contains(config["error"].(string), "base.proto:3:") {
		t.Fatalf("the configuration adding a collector whose import does not compile: logged %v", config)
	}
	if targets["msg"] != "static target reload rejected" || targets["retried_when"] != "the static target file or the configuration changes" {
		t.Fatalf("the target file naming the collector that is not in force: logged %v", targets)
	}
	for range 3 {
		p.manager.reloadChanged()
	}
	if logs.Len() != 0 {
		t.Fatalf("ticks with nothing changed logged %q", logs)
	}
	writeProto(t, dir, importedBase, baseSource)
	p.manager.reloadChanged()
	config, targets = loggedOfBoth(t, logs)
	if config["msg"] != "configuration reloaded" || targets["msg"] != "static targets reloaded" || p.manager.StaticTargets()[0].Collector != "added" {
		t.Fatalf("with the imported file mended: logged %v and %v", config, targets)
	}
}

// healthDocument is a configuration of one grpc collector that calls the
// health service, whose types are built in, so it reads no descriptor file.
// Its metric name tells which version is in force.
func healthDocument(metric string) string {
	return "collectors:\n  - name: imports\n    request:\n      type: grpc\n      rpc: grpc.health.v1.Health/Check\n    transform:\n      type: jq\n    metrics:\n      - name: " + metric + "\n        expression: '1'\n"
}

// An imported file mended after the validation read it, and before the
// reload is refused, is not taken as read: the descriptor files of the
// configuration being read are stamped before it is validated, and stay
// watched as they were then while it is refused, so the next tick reads the
// configuration again and puts it in force. The reload adds the collector
// that reads the files, so none of them is a file of the configuration in
// force.
func TestAnImportMendedBetweenTheValidationAndTheRefusalIsTriedAgain(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	writeProto(t, dir, importedBase, baseBroken)
	p := newPair(t, healthDocument("v0"), importingTargets(""))
	logs := &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	p.write(t, p.configPath, importingDocument("v1", plainMessage, dir, dir))
	// The first two looks stamp the files of the configuration being read,
	// before it is validated; the validation compiles the files and
	// refuses; the third look is the refusal's first, at the collectors in
	// force.
	inner := readFiles
	looks := 0
	readFiles = func(c *model.Collector) ([]string, string) {
		if looks++; looks == 3 {
			writeProto(t, dir, importedBase, baseSource)
		}
		return inner(c)
	}
	t.Cleanup(func() { readFiles = inner })
	p.manager.reloadChanged()
	line := theLine(t, logs)
	if line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), "base.proto:3:") || looks != 3 {
		t.Fatalf("the reload with the imported file broken: logged %v after %d looks, want three", line, looks)
	}
	if want := "the configuration, a file it names or a file one of those imports changes"; line["retried_when"] != want {
		t.Errorf("retried_when %q, want %q", line["retried_when"], want)
	}
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v1" {
		t.Fatalf("the tick after the imported file was mended while the reload was being refused: logged %v, in force %s", line, versionInForce(p))
	}
	for range 3 {
		p.manager.reloadChanged()
	}
	if logs.Len() != 0 {
		t.Fatalf("the ticks after it logged %q", logs)
	}
}

// The static target file is held to the same: the files its check opens,
// those its collector's .proto file imports among them, are stamped before
// the check reads them. An imported file that gains the field the target's
// message sets while the target file's reload runs, at its first look at
// those files, is not lost to it: the target file is in force by the next
// tick, and is not left refused for a file that fits.
func TestAnImportMendedWhileTheTargetFileIsReloadedIsNotLost(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	p, logs, _ := importingPair(t, dir, dir)
	p.write(t, p.targetsAt, importingTargets(regionMessage))
	inner := readFiles
	looks := 0
	readFiles = func(c *model.Collector) ([]string, string) {
		if looks++; looks == 1 {
			writeProto(t, dir, importedBase, baseWithRegion)
		}
		return inner(c)
	}
	t.Cleanup(func() { readFiles = inner })
	for range 2 {
		p.manager.reloadChanged()
	}
	if looks == 0 || messageInForce(p) != regionMessage {
		t.Fatalf("the imported file gained the field while the target file was reloaded, and two ticks later the target file in force sends %s after %d looks; logged %q", messageInForce(p), looks, logs)
	}
}

// Which descriptor file changed is not asked: a target file in force with a
// target that sets a message for one collector is read again, with the
// configuration, when a descriptor file of another collector changes, and
// is found as it was. A target file whose targets set no message is not.
func TestATargetFileWithAMessageIsReadForADescriptorFileOfAnyCollector(t *testing.T) {
	for message, read := range map[string]bool{plainMessage: true, "": false} {
		used, other := t.TempDir(), t.TempDir()
		writeService(t, used)
		writeService(t, other)
		second := strings.Replace(importingDocument("v1", plainMessage, other, other), "name: imports", "name: others", 1)
		p := newPair(t, importingDocument("v1", plainMessage, used, used)+strings.TrimPrefix(second, "collectors:\n"), importingTargets(message))
		logs := &bytes.Buffer{}
		p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
		p.manager.SetWatchInterval(time.Minute)
		writeProto(t, other, importedBase, baseWithRegion)
		p.manager.reloadChanged()
		lines := testutil.AssertJSONLines(t, logs, map[bool]int{true: 2, false: 1}[read])
		if lines[0]["msg"] != "configuration reloaded" || read && lines[1]["msg"] != "static targets reloaded" {
			t.Fatalf("a target with the message %q, and a descriptor file of a collector it does not use changed: logged %v", message, lines)
		}
		logs.Reset()
		p.manager.reloadChanged()
		if logs.Len() != 0 {
			t.Fatalf("the tick after it logged %q", logs)
		}
	}
}

// lookCounts are the times the files the compile looked at were asked for
// by a startup, by a reload that is accepted, by a tick that finds nothing
// changed and by a reload that is refused, of a configuration of one
// collector that compiles .proto files, started as the exporter starts it
// with the watch on or off (main.go).
func lookCounts(t *testing.T, watch bool) (startup, accepted, quiet, refused int, m *Manager) {
	t.Helper()
	dir := t.TempDir()
	writeService(t, dir)
	p := &pair{configPath: testutil.WriteIn(t, t.TempDir(), "config.yaml", importingDocument("v1", plainMessage, dir, dir))}
	inner := readFiles
	looks := 0
	readFiles = func(c *model.Collector) ([]string, string) { looks++; return inner(c) }
	t.Cleanup(func() { readFiles = inner })
	since := func() int {
		n := looks
		looks = 0
		return n
	}

	stamp := TakeStamp(p.configPath, true)
	load := func() (*model.Config, error) { return Load(p.configPath) }
	if watch {
		load = func() (*model.Config, error) { return LoadStamped(p.configPath, &stamp) }
	}
	cfg, err := load()
	if err != nil {
		t.Fatal(err)
	}
	m = NewManager(cfg, p.configPath, testutil.QuietLogger(t))
	m.UseStamp(stamp)
	if watch {
		m.SetWatchInterval(time.Minute)
	}
	p.manager = m
	startup = since()
	reload := func() error {
		if watch {
			m.reloadChanged()
			return nil
		}
		return m.Reload(ReloadTriggerSignal)
	}

	p.write(t, p.configPath, importingDocument("v2", plainMessage, dir, dir))
	if err := reload(); err != nil || versionInForce(p) != "v2" {
		t.Fatalf("the reload was not accepted: %v", err)
	}
	accepted = since()
	if watch {
		m.reloadChanged()
	}
	quiet = since()
	p.write(t, p.configPath, strings.Replace(importingDocument("v3", plainMessage, dir, dir), "type: jq", "type: nope", 1))
	if err := reload(); versionInForce(p) != "v2" || !watch && err == nil {
		t.Fatal("the reload of a configuration with an unknown transform was accepted")
	}
	return startup, accepted, quiet, since(), m
}

// Without the watch no descriptor file is stamped, and none is read to learn
// what it imports: a startup and a reload that is accepted ask for the files
// the compile looked at no more than they did before the descriptor files in
// force were watched, which is never, and a reload that is refused as often
// as it did, twice for the configuration in force and twice for the one
// read; no file is watched in force.
func TestWithoutTheWatchNoDescriptorFileIsStamped(t *testing.T) {
	startup, accepted, _, refused, m := lookCounts(t, false)
	if m.WatchEnabled() {
		t.Fatal("the watch is on")
	}
	if startup != 0 || accepted != 0 || refused != 4 {
		t.Fatalf("without the watch the startup looked %d times at the files the compile read, an accepted reload %d times and a refused one %d times; want 0, 0 and 4, as before", startup, accepted, refused)
	}
	if len(m.descriptors.files) != 0 || len(m.loading.files) != 0 {
		t.Fatalf("without the watch the descriptor files %v and %v are stamped", m.descriptors.files, m.loading.files)
	}
}

// With the watch on, stamping the descriptor files asks for the files the
// compile looked at twice — once to learn which they are, and once, the
// files stamped, to see that reading is still the one — at a startup and at
// a reload, before the configuration is validated; a reload that is refused
// asks twice more, for the files of the configuration in force; and a tick
// that finds nothing changed asks for none.
func TestWithTheWatchTheDescriptorFilesAreLookedAtTwiceALoad(t *testing.T) {
	startup, accepted, quiet, refused, m := lookCounts(t, true)
	if startup != 2 || accepted != 2 || quiet != 0 || refused != 4 {
		t.Fatalf("with the watch the startup looked %d times at the files the compile read, an accepted reload %d times, a quiet tick %d times and a refused reload %d times; want 2, 2, 0 and 4", startup, accepted, quiet, refused)
	}
	if len(m.descriptors.files) != 3 {
		t.Fatalf("the descriptor files watched in force are %v, want the named file and the two it leads to", m.descriptors.files)
	}
}
