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

	"github.com/eenchev/prometheus-universal-exporter/internal/grpctest"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Checking a static target file opens files too: the descriptor files of a
// grpc collector, which the request.message of a target is checked against.
// A reload of the target file in the moment such a file was being replaced
// was refused, and never tried again: the target file had not changed. The
// files the check of a refused target file opens are watched until it is in
// force (Manager.targetsRetryFiles). They are descriptor files of the
// configuration in force as well, which the watch reloads the configuration
// for (Manager.descriptors): a tick that finds one changed reads the
// configuration too, and logs its line before the target file's.

// messageTargets is a target file whose one target sends queue to the
// queue_stats collector of grpcConfig.
func messageTargets(queue string) string {
	return "interval: 1m\ntargets:\n  - name: q\n    collector: queue_stats\n    target: grpcs://queue.internal:9090\n    request:\n      message: '{\"queue\": \"" + queue + "\"}'\n"
}

// descriptorPair is a manager of grpcConfig, its queue_stats collector
// reading the descriptor set it returns the path and the content of, and of
// a target file sending orders, with the watch on; and what the manager
// logs.
func descriptorPair(t *testing.T) (p *pair, set, content string, logs *bytes.Buffer) {
	t.Helper()
	set = grpctest.WriteProtoset(t, filepath.Join(t.TempDir(), "queue.pb"), false)
	raw, err := os.ReadFile(set)
	if err != nil {
		t.Fatal(err)
	}
	p = newPair(t, strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset\n      protoset_file: "+set, 1), messageTargets("orders"))
	logs = &bytes.Buffer{}
	p.manager.logger = slog.New(slog.NewJSONHandler(logs, nil))
	p.manager.SetWatchInterval(time.Minute)
	return p, set, string(raw), logs
}

// logged is the one line logged since the last look.
func logged(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	line := testutil.AssertJSONLines(t, logs, 1)[0]
	logs.Reset()
	return line
}

// loggedOfBoth is the two lines logged since the last look by a tick that
// read both files: the configuration's and the static target file's.
func loggedOfBoth(t *testing.T, logs *bytes.Buffer) (config, targets map[string]any) {
	t.Helper()
	lines := testutil.AssertJSONLines(t, logs, 2)
	logs.Reset()
	return lines[0], lines[1]
}

// messageInForce is the message the target in force sends.
func messageInForce(p *pair) string { return p.manager.StaticTargets()[0].Request.Message }

// A reload of the target file refused because a file its check opens was not
// there is logged once and tried again at the tick after the file is back,
// the target file untouched, and then logged as any reload; the
// configuration, whose descriptor file it is, is read with it each time,
// refused while the file is gone and in force again when it is back. The
// ticks between find nothing changed: they read nothing and log nothing.
// Once the target file is in force, that file changing reloads both again.
func TestARefusedTargetFileIsTriedAgainWhenAFileItsCheckOpensIsBack(t *testing.T) {
	p, set, content, logs := descriptorPair(t)

	// The descriptor set is gone for the moment in which the reload reads it.
	if err := os.Remove(set); err != nil {
		t.Fatal(err)
	}
	p.write(t, p.targetsAt, messageTargets("invoices"))
	p.manager.reloadChanged()
	config, line := loggedOfBoth(t, logs)
	if config["msg"] != "configuration reload rejected" || !strings.Contains(config["error"].(string), "reading protoset_file: open "+set) {
		t.Fatalf("with the file gone: logged %v for the configuration", config)
	}
	if line["msg"] != "static target reload rejected" || !strings.Contains(line["error"].(string), "reading protoset_file: open "+set) || messageInForce(p) != `{"queue": "orders"}` {
		t.Fatalf("with the file gone: logged %v, in force %s", line, messageInForce(p))
	}

	for range 3 {
		p.manager.reloadChanged()
	}
	if _, failures := p.reloads(ReloadFileStaticTargets); logs.Len() != 0 || failures != 1 {
		t.Fatalf("ticks with nothing changed: %d reloads refused, logged %q", failures, logs)
	}

	p.write(t, set, content)
	p.manager.reloadChanged()
	config, line = loggedOfBoth(t, logs)
	if line["msg"] != "static targets reloaded" || line["trigger"] != reloadTriggerWatch || messageInForce(p) != `{"queue": "invoices"}` {
		t.Fatalf("with the file back: logged %v, in force %s", line, messageInForce(p))
	}
	if successes, failures := p.reloads(reloadFileConfig); config["msg"] != "configuration reloaded" || successes != 1 || failures != 1 {
		t.Fatalf("the configuration, whose descriptor file it is: logged %v, %d reloads, %d refused", config, successes, failures)
	}

	// In force, both are read again for the file, and nothing else is.
	p.write(t, set, content)
	p.manager.reloadChanged()
	config, line = loggedOfBoth(t, logs)
	if successes, _ := p.reloads(ReloadFileStaticTargets); config["msg"] != "configuration reloaded" || line["msg"] != "static targets reloaded" || successes != 2 {
		t.Fatalf("the file replaced with the target file in force: %d reloads, logged %v and %v", successes, config, line)
	}
	p.manager.reloadChanged()
	if logs.Len() != 0 {
		t.Fatalf("a tick with nothing changed logged %q", logs)
	}
}

// A file that changes and is still unreadable is one more reload, refused
// and logged once; and a reload asked for by SIGHUP while the file is gone is
// tried again by the watch just the same.
func TestARefusedTargetFileIsReadOnceForEachChange(t *testing.T) {
	p, set, content, logs := descriptorPair(t)
	if err := os.Remove(set); err != nil {
		t.Fatal(err)
	}
	p.write(t, p.targetsAt, messageTargets("invoices"))
	p.manager.reloadChanged()
	logs.Reset()

	p.write(t, set, "not a descriptor set")
	p.manager.reloadChanged()
	p.manager.reloadChanged()
	config, line := loggedOfBoth(t, logs)
	if _, failures := p.reloads(ReloadFileStaticTargets); failures != 2 || line["msg"] != "static target reload rejected" || !strings.Contains(line["error"].(string), "is not a FileDescriptorSet") {
		t.Fatalf("a file that is no descriptor set, and two ticks: %d reloads refused, logged %v", failures, line)
	}
	if _, failures := p.reloads(reloadFileConfig); failures != 2 || config["msg"] != "configuration reload rejected" || !strings.Contains(config["error"].(string), "is not a FileDescriptorSet") {
		t.Fatalf("a file that is no descriptor set, and two ticks: %d reloads of the configuration refused, logged %v", failures, config)
	}
	p.write(t, set, content)
	p.manager.reloadChanged()
	if config, line := loggedOfBoth(t, logs); config["msg"] != "configuration reloaded" || line["msg"] != "static targets reloaded" || messageInForce(p) != `{"queue": "invoices"}` {
		t.Fatalf("with the descriptor set back: logged %v and %v, in force %s", config, line, messageInForce(p))
	}

	// Asked for by a signal, with the file gone: both files are refused,
	// and the watch reads both again when it is back.
	if err := os.Remove(set); err != nil {
		t.Fatal(err)
	}
	p.write(t, p.targetsAt, messageTargets("refunds"))
	if err := p.manager.Reload(ReloadTriggerSignal); err == nil || !strings.Contains(err.Error(), "static target file "+p.targetsAt) {
		t.Fatalf("a reload with the file gone: %v", err)
	}
	logs.Reset()
	p.manager.reloadChanged()
	if logs.Len() != 0 {
		t.Fatalf("a tick with nothing changed logged %q", logs)
	}
	p.write(t, set, content)
	p.manager.reloadChanged()
	if lines := testutil.AssertJSONLines(t, logs, 2); lines[0]["msg"] != "configuration reloaded" || lines[1]["msg"] != "static targets reloaded" || messageInForce(p) != `{"queue": "refunds"}` {
		t.Fatalf("with the file back after the signal: logged %v, in force %s", lines, messageInForce(p))
	}
}

// A target file refused for what it says itself — it is not YAML — opens no
// other file, and is read again only when it changes: a descriptor set
// replaced meanwhile reloads the configuration, whose file it is, and not
// the target file, even one whose absence refused the reload before.
func TestATargetFileRefusedForItselfWaitsForItsOwnChange(t *testing.T) {
	p, set, content, logs := descriptorPair(t)
	if err := os.Remove(set); err != nil {
		t.Fatal(err)
	}
	p.write(t, p.targetsAt, messageTargets("invoices"))
	p.manager.reloadChanged()
	if _, line := loggedOfBoth(t, logs); !strings.Contains(line["error"].(string), "reading protoset_file") {
		t.Fatalf("with the file gone: logged %v", line)
	}

	p.write(t, p.targetsAt, "interval: 1m\ntargets: [\n")
	p.manager.reloadChanged()
	if line := logged(t, logs); line["msg"] != "static target reload rejected" || strings.Contains(line["error"].(string), "protoset_file") {
		t.Fatalf("a target file that is not YAML: logged %v", line)
	}
	for range 2 {
		p.write(t, set, content)
		p.manager.reloadChanged()
		if line := logged(t, logs); line["msg"] != "configuration reloaded" {
			t.Fatalf("the descriptor set back, the target file still not YAML: logged %v", line)
		}
	}
	if _, failures := p.reloads(ReloadFileStaticTargets); failures != 2 || messageInForce(p) != `{"queue": "orders"}` {
		t.Fatalf("the descriptor set back, the target file still not YAML: %d reloads of it refused, in force %s", failures, messageInForce(p))
	}

	p.write(t, p.targetsAt, messageTargets("invoices"))
	p.manager.reloadChanged()
	if line := logged(t, logs); line["msg"] != "static targets reloaded" || messageInForce(p) != `{"queue": "invoices"}` {
		t.Fatalf("the target file written anew: logged %v, in force %s", line, messageInForce(p))
	}
}

// With the watch on, the line of a target file refused while a file its
// check opens is gone names that file's change, with the target file's own
// and the configuration's, as what the watch reads it again for; refused
// next for what it says itself, with no file stamped for it, the line names
// the target file alone.
func TestARefusedTargetFileSaysItIsRetriedWhenAFileItsCheckOpensChanges(t *testing.T) {
	p, set, _, logs := descriptorPair(t)
	p.manager.SetWatchInterval(time.Minute)
	if err := os.Remove(set); err != nil {
		t.Fatal(err)
	}
	p.write(t, p.targetsAt, messageTargets("invoices"))
	p.manager.reloadChanged()
	config, line := loggedOfBoth(t, logs)
	if line["msg"] != "static target reload rejected" || line["retried_when"] != "the static target file, a file its check opens or the configuration changes" {
		t.Fatalf("with the file gone: logged %v", line)
	}
	if config["msg"] != "configuration reload rejected" || config["retried_when"] != "the configuration or a file it names changes" {
		t.Fatalf("with the file gone: logged %v for the configuration", config)
	}
	p.write(t, p.targetsAt, "interval: 1m\ntargets: [\n")
	p.manager.reloadChanged()
	if line := logged(t, logs); line["msg"] != "static target reload rejected" || line["retried_when"] != "the static target file changes" {
		t.Fatalf("a target file that is not YAML: logged %v", line)
	}
}

// The files watched for a refused target file are the ones its check opens:
// the descriptor files of the collectors that its targets with a
// request.message name, in each configuration it is checked against, each
// once. A target without a message opens none, and the credential files a
// target or a collector names are read at a scrape, so they refuse no target
// file.
func TestTheFilesTheCheckOfATargetFileOpens(t *testing.T) {
	inForce := &model.Config{
		OTLP: model.OTLPConfig{Enabled: true, TLS: model.TLSConfig{CAFile: "/tls/ca.pem"}},
		Collectors: []model.Collector{
			{Name: "set", Request: model.RequestConfig{ProtosetFile: "/proto/a.protoset", BearerTokenFile: "/run/token"}},
			{Name: "sources", Request: model.RequestConfig{ProtoFiles: []string{"/proto/b.proto", "/proto/common.proto"}}},
			{Name: "unused", Request: model.RequestConfig{ProtosetFile: "/proto/unused.protoset"}},
			{Name: "plain", Request: model.RequestConfig{ProtosetFile: "/proto/plain.protoset"}},
		},
	}
	candidate := &model.Config{Collectors: []model.Collector{{Name: "set", Request: model.RequestConfig{ProtosetFile: "/proto/next.protoset"}}}}
	message := model.TargetRequestConfig{Message: `{"queue": "orders"}`}
	file := &model.StaticTargetFile{Targets: []model.StaticTarget{
		{Name: "a", Collector: "set", Request: model.TargetRequestConfig{Message: "{}", BearerTokenFile: "/run/target-token"}},
		{Name: "b", Collector: "sources", Request: message},
		{Name: "c", Collector: "set", Request: message},
		{Name: "d", Collector: "plain", Request: model.TargetRequestConfig{BasicAuthFile: &model.BasicAuthFile{Username: "/run/u", Password: "/run/p"}}},
		{Name: "e", Collector: "gone", Request: message},
	}}
	if got, want := targetsNamedFiles(file, candidate, inForce, nil), []string{"/proto/next.protoset", "/proto/a.protoset", "/proto/b.proto", "/proto/common.proto"}; !slices.Equal(got, want) {
		t.Fatalf("the files of the check: %v, want %v", got, want)
	}
	if got := targetsNamedFiles(&model.StaticTargetFile{Targets: file.Targets[3:4]}, inForce); len(got) != 0 {
		t.Fatalf("the files of a target file without a message: %v", got)
	}
}
