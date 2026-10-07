//go:build !select_request_types || request_type_grpc

package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A .proto file a grpc collector names imports others, which the
// configuration does not name and loading it reads all the same. A reload
// refused because one of those was missing, or did not compile, was never
// tried again: the watch looked at the named files alone. The files the
// compile looked at are now watched with them while the reload is refused,
// for the configuration and for the static target file; those of the
// configuration in force are watched as long as it is
// (watchdescriptors_grpc_test.go).

// The .proto files of these tests: a service whose file imports a
// well-known file and the file of its request, which imports another, so the
// request's Base message is two imports away from the file the collector
// names.
const (
	importingService = "w/api/svc.proto"
	importedTypes    = "w/common/types.proto"
	importedBase     = "w/common/base.proto"
	serviceSource    = "syntax = \"proto3\";\npackage w.api;\nimport \"google/protobuf/timestamp.proto\";\nimport \"w/common/types.proto\";\n" +
		"service S { rpc Get(w.common.Request) returns (Reply); }\nmessage Reply { int64 total = 1; google.protobuf.Timestamp at = 2; }\n"
	typesSource = "syntax = \"proto3\";\npackage w.common;\nimport \"w/common/base.proto\";\nmessage Request { string queue = 1; Base base = 2; }\n"
	baseSource  = "syntax = \"proto3\";\npackage w.common;\nmessage Base { string tenant = 1; }\n"
	// baseWithRegion is base.proto with one more field, which a message can
	// only set when this is the file compiled; baseBroken does not compile.
	baseWithRegion = "syntax = \"proto3\";\npackage w.common;\nmessage Base { string tenant = 1; string region = 2; }\n"
	baseBroken     = "syntax = \"proto3\";\npackage w.common;\nmessage Base { strin tenant = 1; }\n"
	plainMessage   = `{"queue": "orders"}`
	regionMessage  = `{"queue": "orders", "base": {"region": "eu"}}`
)

// protoTimes gives each file written a modification time no file had before.
var protoTimes = time.Now().Add(-time.Hour).Truncate(time.Second)

// writeProto writes a file under dir, by its path from it, with a
// modification time of its own, and returns its path.
func writeProto(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	protoTimes = protoTimes.Add(time.Second)
	if err := os.Chtimes(path, protoTimes, protoTimes); err != nil {
		t.Fatal(err)
	}
	return path
}

// importingDocument is a configuration of one grpc collector that names the
// service's file under root and resolves imports in importPaths. Its metric
// name tells which version is in force.
func importingDocument(metric, message, root string, importPaths ...string) string {
	return "collectors:\n  - name: imports\n    request:\n      type: grpc\n      rpc: w.api.S/Get\n      message: '" + message + "'\n" +
		"      descriptors: proto\n      proto_files: [" + filepath.Join(root, filepath.FromSlash(importingService)) + "]\n" +
		"      proto_import_paths: [" + strings.Join(importPaths, ", ") + "]\n" +
		"    transform:\n      type: jq\n    metrics:\n      - name: " + metric + "\n        expression: .total\n"
}

// importingTargets is a target file whose one target calls the collector,
// with its own message when one is given.
func importingTargets(message string) string {
	targets := "interval: 1m\ntargets:\n  - name: q\n    collector: imports\n    target: grpcs://queue.internal:9090\n"
	if message != "" {
		targets += "    request:\n      message: '" + message + "'\n"
	}
	return targets
}

// importingPair is a manager of a configuration in force whose collector's
// files are under dir, with the watch on, and of a target file without a
// message; what the manager logs; and a count of the times the files the
// compile looked at were asked for.
func importingPair(t *testing.T, dir string, importPaths ...string) (p *pair, logs *bytes.Buffer, looks *int) {
	t.Helper()
	p = newPair(t, importingDocument("v1", plainMessage, dir, importPaths...), importingTargets(""))
	logs = &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	looks = new(int)
	inner := readFiles
	readFiles = func(c *model.Collector) ([]string, string) {
		*looks++
		return inner(c)
	}
	t.Cleanup(func() { readFiles = inner })
	return p, logs, looks
}

// versionInForce is the metric name of the configuration in force.
func versionInForce(p *pair) string { return p.manager.Get().Collectors[0].Metrics[0].Name }

// theLine is the one line logged since the last look.
func theLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	line := testutil.AssertJSONLines(t, logs, 1)[0]
	logs.Reset()
	return line
}

// quietTicks fails the test unless ticks of the watch with nothing changed
// read nothing, ask for no file the compile looked at, and log nothing.
func quietTicks(t *testing.T, p *pair, logs *bytes.Buffer, looks *int, file string) {
	t.Helper()
	successes, failures := p.reloads(file)
	before := *looks
	for range 3 {
		p.manager.reloadChanged()
	}
	if s, f := p.reloads(file); logs.Len() != 0 || s != successes || f != failures || *looks != before {
		t.Fatalf("ticks with nothing changed: %d reloads and %d refused more, %d looks at the descriptor files, logged %q", s-successes, f-failures, *looks-before, logs)
	}
}

// The three files in dir, as they compile.
func writeService(t *testing.T, dir string) (service, types, base string) {
	t.Helper()
	return writeProto(t, dir, importingService, serviceSource), writeProto(t, dir, importedTypes, typesSource), writeProto(t, dir, importedBase, baseSource)
}

// A reload refused because a file two imports away from the one the
// configuration names does not compile is logged once, saying it is tried
// again when an imported file changes, and is tried again at the tick after
// that file is mended, the configuration untouched. The ticks between read
// nothing, compile nothing and log nothing. The files watched meanwhile are
// the named file and those the compile read, and no place of a well-known
// file. Once the configuration is in force, the imported file changing
// reloads it once more, and no file is watched as one of a refused reload.
func TestAReloadRefusedForAnImportedFileIsTriedAgainWhenThatFileIsMended(t *testing.T) {
	dir := t.TempDir()
	service, types, base := writeService(t, dir)
	p, logs, looks := importingPair(t, dir, dir)

	writeProto(t, dir, importedBase, baseBroken)
	p.write(t, p.configPath, importingDocument("v2", plainMessage, dir, dir))
	p.manager.reloadChanged()
	line := theLine(t, logs)
	if line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), "base.proto:3:") || versionInForce(p) != "v1" {
		t.Fatalf("with the imported file broken: logged %v, in force %s", line, versionInForce(p))
	}
	if want := "the configuration, a file it names or a file one of those imports changes"; line["retried_when"] != want {
		t.Errorf("retried_when %q, want %q", line["retried_when"], want)
	}
	if want := []string{service, base, types}; !slices.Equal(p.manager.retryFiles, want) {
		t.Errorf("the files watched while it is refused are %v, want %v", p.manager.retryFiles, want)
	}
	quietTicks(t, p, logs, looks, reloadFileConfig)

	writeProto(t, dir, importedBase, baseSource)
	p.manager.reloadChanged()
	line = theLine(t, logs)
	if line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch || versionInForce(p) != "v2" {
		t.Fatalf("with the imported file mended: logged %v, in force %s", line, versionInForce(p))
	}

	// In force, the configuration is read again for the file, once.
	writeProto(t, dir, importedBase, baseWithRegion)
	p.manager.reloadChanged()
	if line = theLine(t, logs); line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch {
		t.Fatalf("with the imported file changed in force: logged %v", line)
	}
	quietTicks(t, p, logs, looks, reloadFileConfig)
	if len(p.manager.retryFiles) != 0 {
		t.Fatalf("files are watched for a refused reload of a configuration in force: %v", p.manager.retryFiles)
	}
}

// An imported file edited is what the next reload checks the configuration
// against: a configuration whose message sets a field the imported file does
// not have is refused, and is in force at the tick after the file has the
// field, the configuration untouched.
func TestAReloadRefusedForAFieldIsTriedAgainWhenTheImportedFileHasIt(t *testing.T) {
	dir := t.TempDir()
	writeService(t, dir)
	p, logs, looks := importingPair(t, dir, dir)
	p.write(t, p.configPath, importingDocument("v2", regionMessage, dir, dir))
	p.manager.reloadChanged()
	line := theLine(t, logs)
	if line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), "region") || versionInForce(p) != "v1" {
		t.Fatalf("with a field the imported file does not have: logged %v, in force %s", line, versionInForce(p))
	}
	quietTicks(t, p, logs, looks, reloadFileConfig)
	writeProto(t, dir, importedBase, baseWithRegion)
	p.manager.reloadChanged()
	if line = theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v2" {
		t.Fatalf("with the field in the imported file: logged %v, in force %s", line, versionInForce(p))
	}
}

// A reload refused because an imported file is not there is tried again when
// the file appears, in whichever import path: every place it was looked for
// is watched. A reload asked for by SIGHUP while it is missing is tried
// again by the watch just the same.
func TestAReloadRefusedForAMissingImportIsTriedAgainWhenItAppears(t *testing.T) {
	for _, appearsIn := range []string{"the import path the other files are in", "an earlier import path"} {
		first, second := t.TempDir(), t.TempDir()
		_, _, base := writeService(t, second)
		p, logs, looks := importingPair(t, second, first, second)
		if err := os.Remove(base); err != nil {
			t.Fatal(err)
		}
		p.write(t, p.configPath, importingDocument("v2", plainMessage, second, first, second))
		if err := p.manager.Reload(ReloadTriggerSignal); err == nil {
			t.Fatal("the reload was accepted without the imported file")
		}
		lines := testutil.AssertJSONLines(t, logs, 2)
		logs.Reset()
		line := lines[0]
		if line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), importedBase) || versionInForce(p) != "v1" {
			t.Fatalf("%s: with the imported file missing: logged %v, in force %s", appearsIn, line, versionInForce(p))
		}
		if want := "the configuration, a file it names or a file one of those imports changes"; line["retried_when"] != want {
			t.Errorf("%s: retried_when %q, want %q", appearsIn, line["retried_when"], want)
		}
		for _, dir := range []string{first, second} {
			if candidate := filepath.Join(dir, filepath.FromSlash(importedBase)); !slices.Contains(p.manager.retryFiles, candidate) {
				t.Errorf("%s: %s is not watched: %v", appearsIn, candidate, p.manager.retryFiles)
			}
		}
		for _, path := range p.manager.retryFiles {
			if strings.Contains(path, "google") {
				t.Errorf("%s: a place of a well-known file is watched: %s", appearsIn, path)
			}
		}
		quietTicks(t, p, logs, looks, reloadFileConfig)
		if appearsIn == "an earlier import path" {
			writeProto(t, first, importedBase, baseSource)
		} else {
			writeProto(t, second, importedBase, baseSource)
		}
		p.manager.reloadChanged()
		if line = theLine(t, logs); line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch || versionInForce(p) != "v2" {
			t.Fatalf("%s: with the imported file there: logged %v, in force %s", appearsIn, line, versionInForce(p))
		}
	}
}

// The files watched are the ones the compile looked at, as the import paths
// resolved them: a file of the same name in a later import path than the one
// it was found in is not, and changing it reloads nothing; a file appearing
// in an earlier import path, which is the one compiled from then on, is, and
// reloads at the next tick.
func TestTheImportedFilesWatchedAreTheOnesTheImportPathsResolved(t *testing.T) {
	first, second, third := t.TempDir(), t.TempDir(), t.TempDir()
	writeService(t, second)
	writeProto(t, third, importedTypes, typesSource)
	p, logs, looks := importingPair(t, second, first, second, third)
	// Refused for the file two imports away, found in the second path.
	writeProto(t, second, importedBase, baseBroken)
	p.write(t, p.configPath, importingDocument("v2", plainMessage, second, first, second, third))
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reload rejected" || versionInForce(p) != "v1" {
		t.Fatalf("with the imported file broken: logged %v", line)
	}
	for _, path := range p.manager.retryFiles {
		if strings.HasPrefix(path, third) {
			t.Errorf("a file of a later import path is watched: %s", path)
		}
	}
	// The later import path is never looked at for a file found before it.
	writeProto(t, third, importedTypes, baseBroken)
	writeProto(t, third, importedBase, baseSource)
	quietTicks(t, p, logs, looks, reloadFileConfig)
	// The earlier one is: the file there takes the broken one's place.
	writeProto(t, first, importedBase, baseSource)
	p.manager.reloadChanged()
	if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v2" {
		t.Fatalf("with the file in an earlier import path: logged %v, in force %s", line, versionInForce(p))
	}
}

// An imported file that changes between a reading of it and the stamp taken
// of it afterwards is a change the next tick sees, as an edit made while a
// named file was being read is: the refused configuration is read once
// more, and the ticks after that are quiet again. It is so for each of the
// two readings a refused reload stamps: that of the configuration being
// read, before it is validated, and that of the configuration in force,
// when the reload is refused.
func TestAnImportedFileChangedWhileItWasReadIsReadAtTheNextTick(t *testing.T) {
	for name, look := range map[string]int{
		// The reload looks at the files four times: twice at the
		// configuration being read, to read what it imports and then, the
		// files stamped, to see that reading is still the one; and twice at
		// the configuration in force, when the reload is refused. The file
		// is written again, as broken as it was, before the second look of a
		// pair, when its stamp has been taken.
		"of the configuration being read": 2,
		"of the configuration in force":   4,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeService(t, dir)
			p, logs, looks := importingPair(t, dir, dir)
			writeProto(t, dir, importedBase, baseBroken)
			p.write(t, p.configPath, importingDocument("v2", plainMessage, dir, dir))
			counting := readFiles
			calls := 0
			readFiles = func(c *model.Collector) ([]string, string) {
				if calls++; calls == look {
					writeProto(t, dir, importedBase, baseBroken+"// again\n")
				}
				return counting(c)
			}
			p.manager.reloadChanged()
			readFiles = counting
			if line := theLine(t, logs); line["msg"] != "configuration reload rejected" || calls != 4 {
				t.Fatalf("with the imported file broken: logged %v after %d looks, want four", line, calls)
			}
			p.manager.reloadChanged()
			if line := theLine(t, logs); line["msg"] != "configuration reload rejected" {
				t.Fatalf("at the tick after the file changed under the stamp: logged %v", line)
			}
			if _, failures := p.reloads(reloadFileConfig); failures != 2 {
				t.Fatalf("%d reloads refused, want the one that read the file and the one after it changed", failures)
			}
			quietTicks(t, p, logs, looks, reloadFileConfig)
			writeProto(t, dir, importedBase, baseSource)
			p.manager.reloadChanged()
			if line := theLine(t, logs); line["msg"] != "configuration reloaded" || versionInForce(p) != "v2" {
				t.Fatalf("with the imported file mended: logged %v, in force %s", line, versionInForce(p))
			}
		})
	}
}

// The static target file is held to the same: a reload of it refused because
// a file its collector's .proto file imports did not compile, or lacked a
// field the target's message sets, is tried again at the tick after that
// file changes, the target file untouched; the configuration, whose
// descriptor file it is, is read with it each time, and its line comes
// first. The ticks between read nothing and log nothing.
func TestARefusedTargetFileIsTriedAgainWhenAnImportedFileChanges(t *testing.T) {
	dir := t.TempDir()
	service, types, base := writeService(t, dir)
	p, logs, looks := importingPair(t, dir, dir)
	inForce := func() string { return p.manager.StaticTargets()[0].Request.Message }

	writeProto(t, dir, importedBase, baseBroken)
	p.write(t, p.targetsAt, importingTargets(regionMessage))
	// both is the two lines of a tick that read both files, the
	// configuration's with the message it must have.
	both := func(configLine string) map[string]any {
		t.Helper()
		lines := testutil.AssertJSONLines(t, logs, 2)
		logs.Reset()
		if lines[0]["msg"] != configLine {
			t.Fatalf("the configuration's line is %v, want %q", lines[0], configLine)
		}
		return lines[1]
	}
	p.manager.reloadChanged()
	line := both("configuration reload rejected")
	if line["msg"] != "static target reload rejected" || !strings.Contains(line["error"].(string), "base.proto:3:") || inForce() != "" {
		t.Fatalf("with the imported file broken: logged %v, in force %q", line, inForce())
	}
	if want := "the static target file, a file its check opens or the configuration changes"; line["retried_when"] != want {
		t.Errorf("retried_when %q, want %q", line["retried_when"], want)
	}
	if want := []string{service, base, types}; !slices.Equal(p.manager.targetsRetryFiles, want) {
		t.Errorf("the files watched while it is refused are %v, want %v", p.manager.targetsRetryFiles, want)
	}
	quietTicks(t, p, logs, looks, ReloadFileStaticTargets)

	// Mended, the file still lacks the field the message sets: one more
	// reload, refused and logged once.
	writeProto(t, dir, importedBase, baseSource)
	p.manager.reloadChanged()
	line = both("configuration reloaded")
	if line["msg"] != "static target reload rejected" || !strings.Contains(line["error"].(string), "region") || inForce() != "" {
		t.Fatalf("with the imported file without the field: logged %v, in force %q", line, inForce())
	}
	quietTicks(t, p, logs, looks, ReloadFileStaticTargets)

	writeProto(t, dir, importedBase, baseWithRegion)
	p.manager.reloadChanged()
	line = both("configuration reloaded")
	if line["msg"] != "static targets reloaded" || line["trigger"] != reloadTriggerWatch || inForce() != regionMessage {
		t.Fatalf("with the field in the imported file: logged %v, in force %q", line, inForce())
	}
	if successes, failures := p.reloads(reloadFileConfig); successes != 2 || failures != 1 {
		t.Fatalf("the configuration was read at each change of its descriptor file: %d reloads, %d refused, want 2 and 1", successes, failures)
	}
	quietTicks(t, p, logs, looks, ReloadFileStaticTargets)
}
