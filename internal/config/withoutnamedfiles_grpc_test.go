//go:build !select_request_types || request_type_grpc

package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A configuration naming files that are not there — a descriptor set, .proto
// files, otlp.tls files, web.basic_auth files — is refused by the loader as
// the exporter uses it, and loaded with WithoutNamedFiles, as the target
// allocator loads it, which opens none of them: its grpc collectors whose
// descriptors come from files are marked as not checked against them, and
// so are the messages of the static targets using them. Everything that is
// not read from those files is checked as before: a key without its file, a
// certificate without its key, two credentials of one kind, a message of the
// built-in health service that does not fit it.
func TestALoadWithoutNamedFilesOpensNoneOfThemAndChecksTheRest(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	protoset := strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset\n      protoset_file: "+missing+".pb", 1)
	proto := strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "proto\n      proto_files: ["+missing+".proto]", 1)
	const otlp = "otlp:\n  enabled: true\n  endpoint: https://collector.invalid/v1/metrics\n  tls:\n"
	const web = "web:\n  basic_auth:\n    enabled: true\n"
	for name, configuration := range map[string]string{
		"a missing protoset_file":    protoset,
		"missing proto_files":        proto,
		"missing otlp.tls files":     otlp + "    ca_file: " + missing + ".ca\n    cert_file: " + missing + ".crt\n    key_file: " + missing + ".key\n" + protoset,
		"missing basic_auth files":   web + "    username_file: " + missing + ".user\n    password_file: " + missing + ".pass\n" + protoset,
		"a missing basic_auth file":  web + "    username: admin\n    password_file: " + missing + ".pass\n" + strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "reflection", 1),
		"a missing otlp.tls ca_file": otlp + "    ca_file: " + missing + ".ca\n" + strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "reflection", 1),
	} {
		path := testutil.WriteIn(t, dir, "config.yaml", configuration)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("%s: the exporter's load gave %v, want it refused naming the file", name, err)
		}
		cfg, err := Load(path, WithoutNamedFiles())
		if err != nil {
			t.Errorf("%s: refused without named files: %v", name, err)
			continue
		}
		queue, health := &cfg.Collectors[0], &cfg.Collectors[1]
		if !queue.DescriptorFilesUnread || !health.DescriptorFilesUnread {
			t.Errorf("%s: the collectors are not marked as unchecked against their descriptor files", name)
		}
		if queue.Request.Retry.Codes[0] != "UNAVAILABLE" || cfg.OTLP.Enabled && cfg.OTLP.Timeout == 0 {
			t.Errorf("%s: the defaults were not filled in: %v, %v", name, queue.Request.Retry.Codes, cfg.OTLP.Timeout)
		}
		// The static targets of the queue collector bring a message that
		// is not checked against the descriptor set; the health service's
		// is still checked against its built-in types.
		f, err := LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", "interval: 1m\ntargets:\n  - name: q\n    collector: queue_stats\n    target: q:9090\n    request: {message: '{\"queue\": \"a\", \"no_such_field\": 1}'}\n"))
		if err == nil {
			err = ValidateStaticTargetsAgainst(f, cfg)
		}
		if err != nil {
			t.Errorf("%s: a target's message was checked against a descriptor file: %v", name, err)
		}
		f, err = LoadStaticTargets(testutil.WriteIn(t, dir, "targets.yaml", "interval: 1m\ntargets:\n  - name: h\n    collector: health\n    target: q:9090\n    request: {message: '{\"no_such_field\": 1}'}\n"))
		if err == nil {
			err = ValidateStaticTargetsAgainst(f, cfg)
		}
		if err == nil || !strings.Contains(err.Error(), "does not fit grpc.health.v1.HealthCheckRequest") {
			t.Errorf("%s: a health target's wrong message gave %v", name, err)
		}
	}
	for name, test := range map[string]struct{ configuration, want string }{
		"protoset without its file": {strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "protoset", 1), "has descriptors: protoset but no request.protoset_file"},
		"proto_files with protoset": {strings.Replace(protoset, "protoset_file:", "proto_files: [a.proto]\n      protoset_file:", 1), "sets request.proto_files or proto_import_paths, which apply only with descriptors: proto"},
		"a certificate without key": {otlp + "    cert_file: " + missing + ".crt\n" + protoset, "otlp.tls sets only one of cert_file and key_file"},
		"two passwords":             {web + "    username: admin\n    password: a\n    password_file: " + missing + ".pass\n" + protoset, "web.basic_auth sets both password and password_file"},
		"a wrong health message":    {strings.Replace(protoset, "rpc: grpc.health.v1.Health/Check", "rpc: grpc.health.v1.Health/Check\n      message: '{\"no_such_field\": 1}'", 1), `collector "health" request.message does not fit`},
		"an unknown method":         {strings.Replace(protoset, "rpc: grpc.health.v1.Health/Check", "rpc: grpc.health.v1.Health/Nope", 1), `collector "health"`},
	} {
		_, err := Load(testutil.WriteIn(t, dir, "config.yaml", test.configuration), WithoutNamedFiles())
		checkRefusal(t, name, err, test.want)
	}
	// The exporter's own load marks no collector.
	cfg, err := Load(testutil.WriteIn(t, dir, "config.yaml", strings.Replace(grpcConfig, "PROTOSET_OR_REFLECTION", "reflection", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collectors[0].DescriptorFilesUnread || cfg.Collectors[1].DescriptorFilesUnread {
		t.Error("the exporter's load marked a collector as unchecked against its descriptor files")
	}
}
