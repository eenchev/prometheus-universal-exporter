//go:build !select_request_types || request_type_http

package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Loading a configuration opens files it only names — the OTLP export's
// certificates, the exporter's own credential files — and refuses it when one
// cannot be read. The watch looked at the configuration and its collector
// files alone, so a reload refused in the moment a Secret was being replaced
// was never tried again: the configuration had not changed. The files a
// refused configuration names are now watched until it is in force.

// selfSigned is a certificate and its key, as PEM.
func selfSigned(t *testing.T) (certificate, key string) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "collector.invalid"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// fileTimes hands out modification times a second apart, so a file written
// twice within the clock's resolution still has a time of its own.
var fileTimes = time.Now().Add(-time.Hour).Truncate(time.Second)

// rewrite writes a file anew, with a modification time no file had before.
func rewrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	fileTimes = fileTimes.Add(time.Second)
	if err := os.Chtimes(path, fileTimes, fileTimes); err != nil {
		t.Fatal(err)
	}
}

// namedFileManager is a manager of the configuration at path, logging to
// the buffer it returns.
func namedFileManager(t *testing.T, path string) (*Manager, *bytes.Buffer) {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	m := NewManager(cfg, path, slog.New(slog.NewJSONHandler(logs, nil)))
	m.SetPythonPath("python3")
	return m, logs
}

// oneLine is the one line logged since the last look.
func oneLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	line := testutil.AssertJSONLines(t, logs, 1)[0]
	logs.Reset()
	return line
}

// watched is the configuration these tests reload: a collector whose metric
// name tells which version is in force, after the block under test.
func watched(block, metric string) string {
	return block + strings.Replace(watchConfigTemplate, "%s", metric, 1)
}

// A reload refused because a file the configuration names was not there is
// tried again at the tick after the file is back, and then logged as any
// reload. The ticks between find nothing changed: they do not read the
// configuration and log nothing. The file is the one of the configuration
// that was refused, which the one in force may not name at all.
func TestARefusedReloadIsTriedAgainWhenAFileItNamesIsBack(t *testing.T) {
	certificate, _ := selfSigned(t)
	for name, test := range map[string]struct {
		// first and second are the block of the configuration in force and
		// of the one reloaded, given the file.
		first, second func(file string) string
		content       string
		want          string
	}{
		"an OTLP certificate the new configuration adds": {
			func(string) string { return "" },
			func(file string) string {
				return "otlp:\n  enabled: true\n  endpoint: https://collector.invalid:4318/v1/metrics\n  tls: {ca_file: " + file + "}\n"
			},
			certificate, "otlp.tls cannot be used: open ",
		},
		"the exporter's own password file": {
			func(file string) string {
				return "web:\n  basic_auth: {enabled: true, username: admin, password_file: " + file + "}\n"
			},
			func(file string) string {
				return "web:\n  basic_auth: {enabled: true, username: operator, password_file: " + file + "}\n"
			},
			"s3cret\n", "web.basic_auth.password_file ",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := testutil.WriteIn(t, dir, "secret/file", test.content)
			path := testutil.WriteIn(t, dir, "config.yaml", watched(test.first(file), "first_value"))
			m, logs := namedFileManager(t, path)

			// The file is gone for the moment in which the reload reads it.
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			rewrite(t, path, watched(test.second(file), "second_value"))
			m.reloadChanged()
			line := oneLine(t, logs)
			if line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), test.want) || activeMetric(m) != "first_value" {
				t.Fatalf("with the file gone: logged %v, in force %s", line, activeMetric(m))
			}

			for range 3 {
				m.reloadChanged()
			}
			if st := m.Reloads.files[reloadFileConfig]; logs.Len() != 0 || st.failures != 1 {
				t.Fatalf("ticks with nothing changed: %d reloads refused, logged %q", st.failures, logs)
			}

			rewrite(t, file, test.content)
			m.reloadChanged()
			line = oneLine(t, logs)
			if line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch || activeMetric(m) != "second_value" {
				t.Fatalf("with the file back: logged %v, in force %s", line, activeMetric(m))
			}

			// In force, the configuration is not read again for the file:
			// what uses it reads it again itself.
			rewrite(t, file, test.content+"\n")
			m.reloadChanged()
			if st := m.Reloads.files[reloadFileConfig]; logs.Len() != 0 || st.successes != 1 {
				t.Fatalf("the file replaced with the configuration in force: %d reloads, logged %q", st.successes, logs)
			}
		})
	}
}

// A file that changes several times between two ticks, and is still wrong,
// is one more reload, refused and logged once; right at last, it is the
// reload that goes through.
func TestAFileThatKeepsChangingIsOneReloadATick(t *testing.T) {
	certificate, _ := selfSigned(t)
	dir := t.TempDir()
	ca := testutil.WriteIn(t, dir, "ca.pem", certificate)
	block := "otlp:\n  enabled: true\n  endpoint: https://collector.invalid:4318/v1/metrics\n  tls: {ca_file: " + ca + "}\n"
	path := testutil.WriteIn(t, dir, "config.yaml", watched(block, "first_value"))
	m, logs := namedFileManager(t, path)

	rewrite(t, ca, "not a certificate\n")
	rewrite(t, path, watched(block, "second_value"))
	m.reloadChanged()
	if line := oneLine(t, logs); !strings.Contains(line["error"].(string), "otlp.tls cannot be used: no certificates found in "+ca) {
		t.Fatalf("a file that is no certificate: %v", line)
	}
	rewrite(t, ca, "still not one\n")
	rewrite(t, ca, "nor this\n")
	m.reloadChanged()
	m.reloadChanged()
	if st := m.Reloads.files[reloadFileConfig]; st.failures != 2 || oneLine(t, logs)["msg"] != "configuration reload rejected" {
		t.Fatalf("two changes and two ticks: %d reloads refused, want 2 in all", st.failures)
	}
	rewrite(t, ca, certificate)
	m.reloadChanged()
	if line := oneLine(t, logs); line["msg"] != "configuration reloaded" || activeMetric(m) != "second_value" {
		t.Fatalf("with a certificate: logged %v, in force %s", line, activeMetric(m))
	}
}

// Kubernetes replaces a mounted Secret by pointing its ..data link at a new
// directory. The file the path leads to is then another, and the watch sees
// that although the new file has the size and the time of the old.
func TestARefusedReloadIsTriedAgainWhenALinkIsSwapped(t *testing.T) {
	certificate, _ := selfSigned(t)
	dir := t.TempDir()
	mount := filepath.Join(dir, "tls")
	written := time.Now().Add(-time.Hour).Truncate(time.Second)
	for version, content := range map[string]string{"..v1": certificate, "..v2": strings.Repeat("x", len(certificate)), "..v3": certificate} {
		file := testutil.WriteIn(t, mount, version+"/ca.pem", content)
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
	ca := filepath.Join(mount, "ca.pem")
	if err := os.Symlink(filepath.Join("..data", "ca.pem"), ca); err != nil {
		t.Fatal(err)
	}
	block := "otlp:\n  enabled: true\n  endpoint: https://collector.invalid:4318/v1/metrics\n  tls: {ca_file: " + ca + "}\n"
	path := testutil.WriteIn(t, dir, "config.yaml", watched(block, "first_value"))
	m, logs := namedFileManager(t, path)

	publish("..v2")
	rewrite(t, path, watched(block, "second_value"))
	m.reloadChanged()
	if line := oneLine(t, logs); line["msg"] != "configuration reload rejected" || !strings.Contains(line["error"].(string), "no certificates found") {
		t.Fatalf("a Secret without a certificate: %v", line)
	}
	m.reloadChanged()
	if logs.Len() != 0 {
		t.Fatalf("a tick with nothing changed logged %q", logs)
	}
	publish("..v3")
	m.reloadChanged()
	if line := oneLine(t, logs); line["msg"] != "configuration reloaded" || activeMetric(m) != "second_value" {
		t.Fatalf("after the link was swapped: logged %v, in force %s", line, activeMetric(m))
	}
}

// A reload asked for while a client certificate is half replaced — the new
// certificate beside the old key — is refused, leaving the configuration in
// force as it was, and goes through at the tick after the key follows.
func TestAReloadDuringACertificateRotationRecovers(t *testing.T) {
	certificate, key := selfSigned(t)
	nextCertificate, nextKey := selfSigned(t)
	dir := t.TempDir()
	certFile := testutil.WriteIn(t, dir, "tls.crt", certificate)
	keyFile := testutil.WriteIn(t, dir, "tls.key", key)
	block := "otlp:\n  enabled: true\n  endpoint: https://collector.invalid:4318/v1/metrics\n  tls: {cert_file: " + certFile + ", key_file: " + keyFile + "}\n"
	path := testutil.WriteIn(t, dir, "config.yaml", watched(block, "first_value"))
	m, logs := namedFileManager(t, path)
	inForce := m.Get()

	rewrite(t, certFile, nextCertificate)
	err := m.Reload(ReloadTriggerSignal)
	if err == nil || !strings.Contains(err.Error(), "otlp.tls cannot be used: tls: private key does not match public key") || m.Get() != inForce {
		t.Fatalf("a certificate without its key: %v", err)
	}
	if line := oneLine(t, logs); line["msg"] != "configuration reload rejected" || line["trigger"] != ReloadTriggerSignal {
		t.Fatalf("the refusal: %v", line)
	}
	m.reloadChanged()
	if logs.Len() != 0 || m.Get() != inForce {
		t.Fatalf("a tick before the key: logged %q", logs)
	}
	rewrite(t, keyFile, nextKey)
	m.reloadChanged()
	if line := oneLine(t, logs); line["msg"] != "configuration reloaded" || line["trigger"] != reloadTriggerWatch || m.Get() == inForce {
		t.Fatalf("with both in place: %v", line)
	}
}

// The files watched for a refused configuration are the ones loading it
// opens: the TLS files of an enabled OTLP export, the credential files of
// the exporter's own enabled authentication, and a collector's descriptor
// files. A collector's credential and TLS files are read at a request, and
// refuse no configuration.
func TestTheFilesAConfigurationNamesAndTheLoadOpens(t *testing.T) {
	c := &model.Config{
		OTLP: model.OTLPConfig{Enabled: true, TLS: model.TLSConfig{CAFile: "/tls/ca.pem", CertFile: "/tls/tls.crt", KeyFile: "/tls/tls.key"}},
		Web:  model.WebConfig{BasicAuth: &model.ExporterBasicAuth{Enabled: true, UsernameFile: "/auth/username", PasswordFile: "/auth/password"}},
		Collectors: []model.Collector{
			{Name: "a", Request: model.RequestConfig{ProtosetFile: "/proto/a.protoset", BearerTokenFile: "/run/token", TLS: model.TLSConfig{CAFile: "/collector/ca.pem"}}},
			{Name: "b", Request: model.RequestConfig{ProtoFiles: []string{"/proto/b.proto", "/proto/common.proto"}, BasicAuthFile: &model.BasicAuthFile{Username: "/run/u", Password: "/run/p"}}},
		},
	}
	want := []string{"/tls/ca.pem", "/tls/tls.crt", "/tls/tls.key", "/auth/username", "/auth/password", "/proto/a.protoset", "/proto/b.proto", "/proto/common.proto"}
	if got := namedFiles(c); !slices.Equal(got, want) {
		t.Fatalf("named files %v, want %v", got, want)
	}
	// A file two configurations name is stamped once.
	other := &model.Config{OTLP: model.OTLPConfig{Enabled: true, TLS: model.TLSConfig{CAFile: "/tls/ca.pem", CertFile: "/tls/other.crt"}}}
	if got := namedFiles(nil, other, c); len(got) != len(want)+1 || got[0] != "/tls/ca.pem" || got[1] != "/tls/other.crt" {
		t.Fatalf("the files of two configurations: %v", got)
	}
	// Switched off, a block's files are not opened.
	c.OTLP.Enabled, c.Web.BasicAuth.Enabled, c.Collectors = false, false, nil
	if got := namedFiles(c, nil); len(got) != 0 {
		t.Fatalf("the files of blocks switched off: %v", got)
	}
}
