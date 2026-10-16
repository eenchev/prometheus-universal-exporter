package repository

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sshAuth mounts what an sshfile collector reads — a Secret's private key,
// passphrase and password, and known_hosts lines given inline or by the
// Secret — as one read-only projected directory. The tests below hold the
// chart to rendering nothing of it by default, to the volume, the mount and
// the ConfigMap it renders enabled, to refusing each incomplete or
// contradictory block by the values schema and by the templates alike, and
// to rolling the pods when the inline lines change.

// sshValues is an enabled sshAuth block the checks take, for tests about
// something else.
const sshValues = "sshAuth: {enabled: true, secretName: s, passwordKey: pw, knownHostsSecretKey: kh}\n"

// knownHostsLine is a known_hosts entry as ssh-keyscan prints it.
const knownHostsLine = "sftp.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMbWeyhTNTo7zGLaBVnPBDqg4tqfCW7DfdUAsdthK+Ew\n"

// renderedObjects renders the chart with args and returns every manifest
// as a map, failing the test when rendering or parsing fails.
func renderedObjects(t *testing.T, helm string, args ...string) []map[string]any {
	t.Helper()
	out, ok := helmTemplate(t, helm, chartDir, args...)
	if !ok {
		t.Fatalf("rendering failed:\n%s", out)
	}
	var objects []map[string]any
	decoder := yaml.NewDecoder(strings.NewReader(out))
	for {
		var object map[string]any
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			return objects
		}
		if err != nil {
			t.Fatalf("the rendered chart does not parse: %v\n%s", err, out)
		}
		if object != nil {
			objects = append(objects, object)
		}
	}
}

// podSpecOf returns the Deployment's pod spec among the objects.
func podSpecOf(t *testing.T, objects []map[string]any) map[string]any {
	t.Helper()
	for _, object := range objects {
		if object["kind"] == "Deployment" {
			return object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		}
	}
	t.Fatal("no Deployment was rendered")
	return nil
}

// namedEntry returns the entry of a list of objects whose name is name, or
// nil.
func namedEntry(list any, name string) map[string]any {
	entries, _ := list.([]any)
	for _, entry := range entries {
		if m, ok := entry.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	return nil
}

// Disabled, the default, sshAuth renders nothing: the chart renders what it
// rendered before the block existed, byte for byte, its checksum included,
// and so it does with the block's keys filled in but not enabled, which the
// schema does not hold to the enabled block's rules.
func TestChartWithoutSSHAuthRendersNothingOfIt(t *testing.T) {
	helm := requireHelm(t)
	plain, ok := helmTemplate(t, helm, chartDir)
	if !ok {
		t.Fatalf("rendering failed:\n%s", plain)
	}
	for _, word := range []string{"ssh-auth", "ssh-known-hosts", "known_hosts", "private_key"} {
		if strings.Contains(plain, word) {
			t.Errorf("the default values render %q", word)
		}
	}
	disabled, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, "sshAuth: {enabled: false, passphraseKey: p, knownHosts: \"no entry\", knownHostsSecretKey: kh, mountPath: /etc/prometheus-universal-exporter/ssh}\n"))
	if !ok {
		t.Fatalf("a disabled sshAuth with its keys filled in does not render:\n%s", disabled)
	}
	if disabled != plain {
		t.Errorf("a disabled sshAuth renders differently from the default values:\n%s", disabled)
	}
}

// Enabled, sshAuth renders one projected volume, mounted read-only at
// mountPath, of files mode 0400 under fixed names: each Secret key that is
// set, and known_hosts from the Secret or from a ConfigMap the chart renders
// with the inline lines as given. The files are readable by the exporter
// through the pod's fsGroup, which the default security context sets to the
// exporter's group.
func TestChartMountsTheSSHCredentialsAndKnownHosts(t *testing.T) {
	helm := requireHelm(t)
	configMapName := "test-prometheus-universal-exporter-ssh-known-hosts"
	for name, tc := range map[string]struct {
		values    string
		secret    string
		mountPath string
		items     []any
		configMap string
	}{
		"a key with its passphrase and inline known hosts": {
			values:    "sshAuth:\n  enabled: true\n  secretName: sftp-credentials\n  privateKeyKey: id_ed25519\n  passphraseKey: passphrase.txt\n  knownHosts: |\n    # sftp.example.com:22 SSH-2.0-OpenSSH_9.6\n    " + knownHostsLine,
			secret:    "sftp-credentials",
			mountPath: "/var/run/prometheus-universal-exporter/ssh",
			items: []any{
				map[string]any{"key": "id_ed25519", "path": "private_key"},
				map[string]any{"key": "passphrase.txt", "path": "passphrase"},
			},
			configMap: "# sftp.example.com:22 SSH-2.0-OpenSSH_9.6\n" + knownHostsLine,
		},
		"a password and known hosts from the Secret": {
			values:    "sshAuth: {enabled: true, secretName: sftp-credentials, passwordKey: password, knownHostsSecretKey: known_hosts, mountPath: /ssh/}\n",
			secret:    "sftp-credentials",
			mountPath: "/ssh/",
			items: []any{
				map[string]any{"key": "password", "path": "password"},
				map[string]any{"key": "known_hosts", "path": "known_hosts"},
			},
		},
		"a key and a password with inline lines without a final newline": {
			values:    "sshAuth: {enabled: true, secretName: s, privateKeyKey: k, passwordKey: p, knownHosts: \"h t k\"}\n",
			secret:    "s",
			mountPath: "/var/run/prometheus-universal-exporter/ssh",
			items: []any{
				map[string]any{"key": "k", "path": "private_key"},
				map[string]any{"key": "p", "path": "password"},
			},
			configMap: "h t k",
		},
	} {
		t.Run(name, func(t *testing.T) {
			objects := renderedObjects(t, helm, "-f", valuesFile(t, tc.values))
			pod := podSpecOf(t, objects)
			if group := pod["securityContext"].(map[string]any)["fsGroup"]; group != 65532 {
				t.Errorf("the pod's fsGroup is %v, want 65532, the exporter's group, which reads the 0400 files", group)
			}
			container := pod["containers"].([]any)[0].(map[string]any)
			mount := namedEntry(container["volumeMounts"], "ssh-auth")
			if want := map[string]any{"name": "ssh-auth", "mountPath": tc.mountPath, "readOnly": true}; !reflect.DeepEqual(mount, want) {
				t.Errorf("the mount is %v, want %v", mount, want)
			}
			sources := []any{map[string]any{"secret": map[string]any{"name": tc.secret, "items": tc.items}}}
			if tc.configMap != "" {
				sources = append(sources, map[string]any{"configMap": map[string]any{"name": configMapName, "items": []any{map[string]any{"key": "known_hosts", "path": "known_hosts"}}}})
			}
			want := map[string]any{"name": "ssh-auth", "projected": map[string]any{"defaultMode": 0o400, "sources": sources}}
			if volume := namedEntry(pod["volumes"], "ssh-auth"); !reflect.DeepEqual(volume, want) {
				t.Errorf("the volume is\n%v\nwant\n%v", volume, want)
			}
			var configMap map[string]any
			for _, object := range objects {
				if object["kind"] == "ConfigMap" && object["metadata"].(map[string]any)["name"] == configMapName {
					configMap = object
				}
			}
			switch {
			case tc.configMap == "" && configMap != nil:
				t.Errorf("a known hosts ConfigMap is rendered without inline lines: %v", configMap)
			case tc.configMap != "" && configMap == nil:
				t.Error("no known hosts ConfigMap is rendered")
			case tc.configMap != "":
				if got := configMap["data"].(map[string]any)["known_hosts"]; got != tc.configMap {
					t.Errorf("the ConfigMap holds %q, want %q", got, tc.configMap)
				}
			}
		})
	}
}

// Each incomplete or contradictory sshAuth block fails rendering: by the
// values schema, at the value it names, and with the schema skipped by the
// templates' message saying what to set. A Secret source with no name, a
// credential or known hosts missing, a passphrase with no key to decrypt,
// known hosts from two places that one file cannot hold, and inline lines
// with no entry would each render a pod Kubernetes refuses or whose every
// SSH scrape fails.
func TestChartRefusesAnIncompleteSSHAuth(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	for name, tc := range map[string]struct {
		values, schema, word, message string
	}{
		"no Secret": {
			values: "sshAuth: {enabled: true, privateKeyKey: k, knownHostsSecretKey: kh}\n",
			schema: "sshAuth.secretName", word: "minLength", message: "sshAuth.secretName is required when sshAuth.enabled is true",
		},
		"no credential": {
			values: "sshAuth: {enabled: true, secretName: s, knownHostsSecretKey: kh}\n",
			schema: "sshAuth.passwordKey", word: "minLength", message: "neither sshAuth.privateKeyKey nor sshAuth.passwordKey names a key of the Secret; set one of them, or both",
		},
		"a passphrase without a key": {
			values: "sshAuth: {enabled: true, secretName: s, passwordKey: p, passphraseKey: x, knownHostsSecretKey: kh}\n",
			schema: "sshAuth.privateKeyKey", word: "minLength", message: "sshAuth.passphraseKey is set without sshAuth.privateKeyKey",
		},
		"no known hosts": {
			values: "sshAuth: {enabled: true, secretName: s, privateKeyKey: k}\n",
			schema: "sshAuth.knownHostsSecretKey", word: "minLength", message: "no known hosts are given",
		},
		"known hosts from both": {
			values: "sshAuth: {enabled: true, secretName: s, privateKeyKey: k, knownHosts: \"h t k\", knownHostsSecretKey: kh}\n",
			schema: "sshAuth", word: "not", message: "sshAuth.knownHosts and sshAuth.knownHostsSecretKey are both set",
		},
		"inline lines of comments and short lines": {
			values: "sshAuth: {enabled: true, secretName: s, privateKeyKey: k, knownHosts: \"# h t k\\nh t\\n  #a b c\\n\"}\n",
			schema: "sshAuth.knownHosts", word: "pattern", message: "sshAuth.knownHosts holds no known_hosts entry",
		},
		"inline blanks": {
			values: "sshAuth: {enabled: true, secretName: s, privateKeyKey: k, knownHosts: \"  \\n\"}\n",
			schema: "sshAuth.knownHosts", word: "pattern", message: "sshAuth.knownHosts holds no known_hosts entry",
		},
	} {
		t.Run(name, func(t *testing.T) {
			values := valuesFile(t, tc.values)
			out, ok := helmTemplate(t, helm, chartDir, "-f", values)
			if ok || !schemaRefusal(out, tc.schema, tc.word) {
				t.Errorf("ok=%v, want the values schema to refuse %s by %s:\n%s", ok, tc.schema, tc.word, out)
			}
			skipped, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", values)
			if ok || !strings.Contains(skipped, tc.message) {
				t.Errorf("with the schema skipped: ok=%v, want an error with %q:\n%s", ok, tc.message, skipped)
			}
		})
	}
}

// sshFail is a fail of validateSSHAuth, a whole line of its own.
var sshFail = regexp.MustCompile(`(?m)^\{\{- fail (".*") -\}\}$`)

// The values schema and the templates give one verdict on every sshAuth
// block: a chart of the chart's own validateSSHAuth helper, its fails turned
// into records, runs it over every combination of the block's keys — each
// empty, set or blanks, and inline lines with an entry, without one, of
// comments, short lines, blanks, tabs and carriage returns — and the
// repository's schema validator takes exactly the blocks the helper takes.
// Helm's own validator gives the same verdict on one block of each outcome.
// The schema's pattern for an entry is the helper's.
func TestTheSchemaAndTheTemplatesRefuseTheSameSSHAuth(t *testing.T) {
	helm := requireHelm(t)
	helpers := readChartFile(t, "templates/_helpers.tpl")
	start := strings.Index(helpers, `{{- define "prometheus-universal-exporter.validateSSHAuth" -}}`)
	end := strings.Index(helpers, `{{- /* The ConfigMap the inline known_hosts lines are rendered into. */ -}}`)
	if start < 0 || end < start {
		t.Fatal("_helpers.tpl no longer defines validateSSHAuth followed by sshKnownHostsEntry")
	}
	helper := helpers[start:end]
	if !sshFail.MatchString(helper) {
		t.Fatal("validateSSHAuth has no fail to record")
	}
	helper = sshFail.ReplaceAllString(helper, `{{- if not (hasKey $$.result "error") -}}{{- $$_ := set $$.result "error" $1 -}}{{- end -}}`)
	entry := regexp.MustCompile(`(?s)define "prometheus-universal-exporter.sshKnownHostsEntry" -\}\}\n(.*?)\n\{\{- end \}\}`).FindStringSubmatch(helper)
	if entry == nil {
		t.Fatal("sshKnownHostsEntry is not a pattern of one line")
	}
	schema := schemaAt(t, "sshAuth")
	then := schema["then"].(map[string]any)
	patternOf := then["properties"].(map[string]any)["knownHosts"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)["pattern"]
	if patternOf != entry[1] {
		t.Errorf("the schema's pattern for an entry is %q and the templates' %q; they must be the same", patternOf, entry[1])
	}
	dir := filepath.Join(t.TempDir(), "ssh")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	const table = `kind: Table
rows:
{{- range .Values.cases }}
{{- $context := dict "Values" (dict "sshAuth" .) "result" (dict) }}
{{- $_ := include "prometheus-universal-exporter.validateSSHAuth" $context }}
  - {{ $context.result.error | default "" | quote }}
{{- end }}
`
	for name, content := range map[string]string{
		"Chart.yaml":           "apiVersion: v2\nname: ssh\nversion: 0.1.0\n",
		"templates/_ssh.tpl":   helper,
		"templates/table.yaml": table,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var cases []map[string]any
	for _, enabled := range []bool{true, false} {
		for _, secret := range []string{"", "s", " "} {
			for _, key := range []string{"", "id_ed25519"} {
				for _, passphrase := range []string{"", "p"} {
					for _, password := range []string{"", " "} {
						for _, inline := range []string{"", knownHostsLine, "# c a b\n", "a b\n", "  \n", "#x\n h t k", "h\tt\tk\r\n", "\n\n", "a  b \n"} {
							for _, secretKey := range []string{"", "known_hosts"} {
								cases = append(cases, map[string]any{
									"enabled": enabled, "secretName": secret, "privateKeyKey": key, "passphraseKey": passphrase,
									"passwordKey": password, "knownHosts": inline, "knownHostsSecretKey": secretKey,
									"mountPath": "/var/run/prometheus-universal-exporter/ssh",
								})
							}
						}
					}
				}
			}
		}
	}
	values, err := json.Marshal(map[string]any{"cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	out, ok := helmTemplate(t, helm, dir, "-f", valuesFile(t, string(values)))
	if !ok {
		t.Fatalf("rendering failed:\n%.2000s", out)
	}
	var rendered struct {
		Rows []string `yaml:"rows"`
	}
	if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("the table does not parse: %v", err)
	}
	if len(rendered.Rows) != len(cases) {
		t.Fatalf("the table has %d rows, want %d", len(rendered.Rows), len(cases))
	}
	// One block of each outcome, for helm's own validator.
	first := map[string]map[string]any{}
	for i, refusal := range rendered.Rows {
		var value any
		raw, _ := json.Marshal(cases[i])
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		errs := validateAgainstSchema(schema, value)
		if (len(errs) == 0) != (refusal == "") {
			t.Errorf("%s: the templates say %q and the schema %v; they must agree", raw, refusal, errs)
		}
		if _, seen := first[refusal]; !seen {
			first[refusal] = cases[i]
		}
	}
	// The table has to reach every outcome, or it proves nothing about one.
	if len(first) != 7 {
		t.Errorf("the table reaches %d outcomes of the templates, want 7, the six refusals and none", len(first))
	}
	for refusal, block := range first {
		raw, _ := json.Marshal(map[string]any{"sshAuth": block})
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, string(raw)))
		if ok != (refusal == "") {
			t.Errorf("%s: helm's schema validator renders it %v, and the templates refuse it with %q:\n%.600s", raw, ok, refusal, out)
		}
	}
}

// The pods roll when the inline known_hosts lines change, as they do when
// the configuration does: checksum/config follows the lines, is the same
// for the same lines, and is the configuration's alone when the lines come
// from the Secret, whose rotation the kubelet brings into the mounted file.
func TestChartChecksumFollowsTheSSHKnownHosts(t *testing.T) {
	helm := requireHelm(t)
	checksum := func(values string) string {
		args := []string{"--show-only", "templates/deployment.yaml"}
		if values != "" {
			args = append(args, "-f", valuesFile(t, values))
		}
		out, ok := helmTemplate(t, helm, chartDir, args...)
		if !ok {
			t.Fatalf("rendering failed:\n%s", out)
		}
		match := regexp.MustCompile(`checksum/config: ([0-9a-f]{64})\n`).FindStringSubmatch(out)
		if match == nil {
			t.Fatalf("the pod template has no checksum/config annotation:\n%s", out)
		}
		return match[1]
	}
	inline := func(lines string) string {
		raw, _ := json.Marshal(lines)
		return "sshAuth: {enabled: true, secretName: s, privateKeyKey: k, knownHosts: " + string(raw) + "}\n"
	}
	plain := checksum("")
	if got := checksum(sshValues); got != plain {
		t.Errorf("known hosts from the Secret change the checksum from %s to %s", plain, got)
	}
	first := checksum(inline(knownHostsLine))
	if again := checksum(inline(knownHostsLine)); again != first {
		t.Errorf("the same lines render two checksums, %s and %s", first, again)
	}
	seen := map[string]string{plain: "the configuration alone", first: "the first lines"}
	for name, lines := range map[string]string{
		"another key":                      strings.Replace(knownHostsLine, "AAAAC3", "AAAAC4", 1),
		"a line added":                     knownHostsLine + "other.example.com ssh-ed25519 AAAA\n",
		"the final newline removed":        strings.TrimSuffix(knownHostsLine, "\n"),
		"a carriage return, in binaryData": strings.TrimSuffix(knownHostsLine, "\n") + "\r\n",
	} {
		sum := checksum(inline(lines))
		if other, taken := seen[sum]; taken {
			t.Errorf("%s: the checksum is that of %s, so the pods would not roll", name, other)
		}
		seen[sum] = name
	}
}

// sshAuth.mountPath is a path of its own, as the other credentials' are: at
// the configuration directory, at another credential's path or at an
// extraVolumeMounts entry's, rendering fails naming both.
func TestChartRefusesAnSSHAuthMountPathTakenByAnotherMount(t *testing.T) {
	helm := requireHelm(t)
	for name, tc := range map[string]struct{ values, message string }{
		"the configuration directory": {
			values:  "sshAuth: {enabled: true, secretName: s, passwordKey: pw, knownHostsSecretKey: kh, mountPath: /etc/prometheus-universal-exporter/}\n",
			message: `sshAuth.mountPath "/etc/prometheus-universal-exporter/" is also the configuration directory, and a pod cannot mount two volumes at one path; choose another path`,
		},
		"webAuth's": {
			values:  "webAuth: {enabled: true, secretName: w, mountPath: /secrets}\nsshAuth: {enabled: true, secretName: s, passwordKey: pw, knownHostsSecretKey: kh, mountPath: /secrets/}\n",
			message: `sshAuth.mountPath "/secrets/" is also webAuth.mountPath`,
		},
		"an extraVolumeMounts entry's": {
			values:  sshValues + "extraVolumes: [{name: x, emptyDir: {}}]\nextraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/ssh}]\n",
			message: `extraVolumeMounts uses mountPath "/var/run/prometheus-universal-exporter/ssh", which the chart already mounts as sshAuth.mountPath; choose another path`,
		},
	} {
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, tc.values))
		if ok || !strings.Contains(out, tc.message) {
			t.Errorf("%s: ok=%v, want an error with %q:\n%s", name, ok, tc.message, out)
		}
	}
}

// The chart README's SSH file collectors section renders as it is shown:
// each sshAuth example, the collector configuration beside each, mounting
// the files at the paths the configuration names.
func TestChartReadmeSSHExamplesRender(t *testing.T) {
	helm := requireHelm(t)
	_, section, found := strings.Cut(readChartFile(t, "README.md"), "## SSH file collectors\n")
	if !found {
		t.Fatal("the chart README has no SSH file collectors section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	var values []string
	collector := ""
	for _, block := range yamlBlocksOf(section) {
		switch {
		case strings.HasPrefix(block, "sshAuth:\n"):
			values = append(values, block)
		case strings.HasPrefix(block, "config:\n"):
			collector = block
		}
	}
	if len(values) != 2 || collector == "" {
		t.Fatalf("the section shows %d sshAuth examples and a collector %v, want 2 and one", len(values), collector != "")
	}
	for _, path := range []string{"/var/run/prometheus-universal-exporter/ssh/private_key", "/var/run/prometheus-universal-exporter/ssh/known_hosts"} {
		if !strings.Contains(collector, path) {
			t.Errorf("the collector does not name %s", path)
		}
	}
	for _, example := range values {
		objects := renderedObjects(t, helm, "-f", valuesFile(t, example), "-f", valuesFile(t, collector))
		pod := podSpecOf(t, objects)
		volume := namedEntry(pod["volumes"], "ssh-auth")
		raw, _ := json.Marshal(volume)
		for _, want := range []string{`"path":"private_key"`, `"path":"known_hosts"`} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("the volume of\n%s\nhas no %s: %s", example, want, raw)
			}
		}
	}
}
