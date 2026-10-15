package repository

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sqlAuth mounts what an sql collector reads — a Secret's database
// password, a Snowflake private key, and the TLS CA
// bundle and client certificate and key — as one read-only Secret
// directory. The tests below hold the chart to rendering nothing of it by
// default, to the volume and the mount it renders enabled, to refusing each
// incomplete or contradictory block by the values schema and by the
// templates alike, to giving its mountPath a path of its own, and to the
// chart README's examples rendering as shown.

// sqlValues is an enabled sqlAuth block the checks take, for tests about
// something else.
const sqlValues = "sqlAuth: {enabled: true, secretName: s, passwordKey: pw}\n"

// Disabled, the default, sqlAuth renders nothing: the chart renders what it
// rendered before the block existed, byte for byte, and so it does with the
// block's keys filled in but not enabled, which the schema does not hold to
// the enabled block's rules.
func TestChartWithoutSQLAuthRendersNothingOfIt(t *testing.T) {
	helm := requireHelm(t)
	plain, ok := helmTemplate(t, helm, chartDir)
	if !ok {
		t.Fatalf("rendering failed:\n%s", plain)
	}
	for _, word := range []string{"sql-auth", "client.key", "ca.crt"} {
		if strings.Contains(plain, word) {
			t.Errorf("the default values render %q", word)
		}
	}
	disabled, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, "sqlAuth: {enabled: false, caKey: ca, certKey: c, mountPath: /etc/prometheus-universal-exporter/sql}\n"))
	if !ok {
		t.Fatalf("a disabled sqlAuth with its keys filled in does not render:\n%s", disabled)
	}
	if disabled != plain {
		t.Errorf("a disabled sqlAuth renders differently from the default values:\n%s", disabled)
	}
}

// Enabled, sqlAuth renders one Secret volume, mounted read-only at
// mountPath, of files mode 0400 under fixed names, one for each key that is
// set and none for a key left empty. The files are readable by the exporter
// through the pod's fsGroup, which the default security context sets to the
// exporter's group.
func TestChartMountsTheSQLCredentials(t *testing.T) {
	helm := requireHelm(t)
	for name, tc := range map[string]struct {
		values    string
		mountPath string
		items     []any
	}{
		"a password": {
			values:    "sqlAuth: {enabled: true, secretName: db, passwordKey: password}\n",
			mountPath: "/var/run/prometheus-universal-exporter/sql",
			items:     []any{map[string]any{"key": "password", "path": "password"}},
		},
		"a private key": {
			values:    "sqlAuth: {enabled: true, secretName: db, privateKeyKey: rsa_key.p8}\n",
			mountPath: "/var/run/prometheus-universal-exporter/sql",
			items:     []any{map[string]any{"key": "rsa_key.p8", "path": "private_key"}},
		},
		"a CA bundle at a mountPath with a trailing slash": {
			values:    "sqlAuth: {enabled: true, secretName: db, caKey: ca.pem, mountPath: /sql/}\n",
			mountPath: "/sql/",
			items:     []any{map[string]any{"key": "ca.pem", "path": "ca.crt"}},
		},
		"a client certificate and key": {
			values:    "sqlAuth: {enabled: true, secretName: db, certKey: tls.crt, keyKey: tls.key}\n",
			mountPath: "/var/run/prometheus-universal-exporter/sql",
			items: []any{
				map[string]any{"key": "tls.crt", "path": "client.crt"},
				map[string]any{"key": "tls.key", "path": "client.key"},
			},
		},
		"every key": {
			values:    "sqlAuth: {enabled: true, secretName: db, passwordKey: a, privateKeyKey: b, caKey: d, certKey: e, keyKey: f}\n",
			mountPath: "/var/run/prometheus-universal-exporter/sql",
			items: []any{
				map[string]any{"key": "a", "path": "password"},
				map[string]any{"key": "b", "path": "private_key"},
				map[string]any{"key": "d", "path": "ca.crt"},
				map[string]any{"key": "e", "path": "client.crt"},
				map[string]any{"key": "f", "path": "client.key"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := podSpecOf(t, renderedObjects(t, helm, "-f", valuesFile(t, tc.values)))
			if group := pod["securityContext"].(map[string]any)["fsGroup"]; group != 65532 {
				t.Errorf("the pod's fsGroup is %v, want 65532, the exporter's group, which reads the 0400 files", group)
			}
			container := pod["containers"].([]any)[0].(map[string]any)
			mount := namedEntry(container["volumeMounts"], "sql-auth")
			if want := map[string]any{"name": "sql-auth", "mountPath": tc.mountPath, "readOnly": true}; !reflect.DeepEqual(mount, want) {
				t.Errorf("the mount is %v, want %v", mount, want)
			}
			want := map[string]any{"name": "sql-auth", "secret": map[string]any{"secretName": "db", "defaultMode": 0o400, "items": tc.items}}
			if volume := namedEntry(pod["volumes"], "sql-auth"); !reflect.DeepEqual(volume, want) {
				t.Errorf("the volume is\n%v\nwant\n%v", volume, want)
			}
		})
	}
}

// Each incomplete or contradictory sqlAuth block fails rendering: by the
// values schema, at the value it names, and with the schema skipped by the
// templates' message saying what to set. A Secret volume with no name, one
// of no file, and a client certificate without its key or a key without its
// certificate would each render a pod Kubernetes refuses or whose every SQL
// scrape fails.
func TestChartRefusesAnIncompleteSQLAuth(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	for name, tc := range map[string]struct {
		values, schema, message string
	}{
		"no Secret": {
			values: "sqlAuth: {enabled: true, passwordKey: pw}\n",
			schema: "sqlAuth.secretName", message: "sqlAuth.secretName is required when sqlAuth.enabled is true",
		},
		"no key": {
			values: "sqlAuth: {enabled: true, secretName: s}\n",
			schema: "sqlAuth.keyKey", message: "sqlAuth.enabled is true but no key of the Secret is named; set at least one of sqlAuth.passwordKey, sqlAuth.privateKeyKey, sqlAuth.caKey, sqlAuth.certKey and sqlAuth.keyKey",
		},
		"a client certificate without its key": {
			values: "sqlAuth: {enabled: true, secretName: s, caKey: ca, certKey: c}\n",
			schema: "sqlAuth.keyKey", message: "sqlAuth.certKey is set without sqlAuth.keyKey",
		},
		"a client key without its certificate": {
			values: "sqlAuth: {enabled: true, secretName: s, keyKey: k}\n",
			schema: "sqlAuth.certKey", message: "sqlAuth.keyKey is set without sqlAuth.certKey",
		},
	} {
		t.Run(name, func(t *testing.T) {
			values := valuesFile(t, tc.values)
			out, ok := helmTemplate(t, helm, chartDir, "-f", values)
			if ok || !schemaRefusal(out, tc.schema, "minLength") {
				t.Errorf("ok=%v, want the values schema to refuse %s by minLength:\n%s", ok, tc.schema, out)
			}
			skipped, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", values)
			if ok || !strings.Contains(skipped, tc.message) {
				t.Errorf("with the schema skipped: ok=%v, want an error with %q:\n%s", ok, tc.message, skipped)
			}
		})
	}
}

// sqlAuth has no passphraseKey: the exporter reads only an unencrypted
// Snowflake key and refuses passphrase_file on an sql request, so the values
// schema refuses the key as one the block does not have, enabled or not, and
// the chart mounts no passphrase file.
func TestChartSQLAuthHasNoPassphrase(t *testing.T) {
	helm := requireHelm(t)
	for _, values := range []string{
		"sqlAuth: {enabled: true, secretName: s, privateKeyKey: k, passphraseKey: p}\n",
		"sqlAuth: {enabled: false, passphraseKey: p}\n",
	} {
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, values))
		if ok || !strings.Contains(out, "additional properties 'passphraseKey' not allowed") {
			t.Errorf("%s: ok=%v, want the values schema to refuse passphraseKey:\n%s", values, ok, out)
		}
		// With the schema skipped the templates refuse it too, rather than
		// leave a passphrase the exporter would never read in the values.
		out, ok = helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, values))
		if ok || !strings.Contains(out, "sqlAuth.passphraseKey is not a key of sqlAuth") {
			t.Errorf("%s with the schema skipped: ok=%v, want the templates to refuse passphraseKey:\n%s", values, ok, out)
		}
	}
	var values struct {
		SQLAuth map[string]any `yaml:"sqlAuth"`
	}
	if err := yaml.Unmarshal([]byte(readChartFile(t, "values.yaml")), &values); err != nil {
		t.Fatal(err)
	}
	if _, found := values.SQLAuth["passphraseKey"]; found || len(values.SQLAuth) == 0 {
		t.Errorf("values.yaml's sqlAuth is %v, want it without passphraseKey", values.SQLAuth)
	}
	if _, found := schemaAt(t, "sqlAuth")["properties"].(map[string]any)["passphraseKey"]; found {
		t.Error("the values schema gives sqlAuth a passphraseKey")
	}
}

// The values schema and the templates give one verdict on every sqlAuth
// block: a chart of the chart's own validateSQLAuth helper, its fails turned
// into records, runs it over every combination of the block's keys — each
// empty, set or blanks — and the repository's schema validator takes
// exactly the blocks the helper takes. Helm's own validator gives the same
// verdict on one block of each outcome.
func TestTheSchemaAndTheTemplatesRefuseTheSameSQLAuth(t *testing.T) {
	helm := requireHelm(t)
	helpers := readChartFile(t, "templates/_helpers.tpl")
	start := strings.Index(helpers, `{{- define "prometheus-universal-exporter.validateSQLAuth" -}}`)
	if start < 0 {
		t.Fatal("_helpers.tpl no longer defines validateSQLAuth")
	}
	end := strings.Index(helpers[start:], "\n{{- end }}\n")
	if end < 0 {
		t.Fatal("validateSQLAuth has no end")
	}
	helper := helpers[start : start+end+len("\n{{- end }}\n")]
	if !sshFail.MatchString(helper) {
		t.Fatal("validateSQLAuth has no fail to record")
	}
	helper = sshFail.ReplaceAllString(helper, `{{- if not (hasKey $$.result "error") -}}{{- $$_ := set $$.result "error" $1 -}}{{- end -}}`)
	dir := filepath.Join(t.TempDir(), "sql")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	const table = `kind: Table
rows:
{{- range .Values.cases }}
{{- $context := dict "Values" (dict "sqlAuth" .) "result" (dict) }}
{{- $_ := include "prometheus-universal-exporter.validateSQLAuth" $context }}
  - {{ $context.result.error | default "" | quote }}
{{- end }}
`
	for name, content := range map[string]string{
		"Chart.yaml":           "apiVersion: v2\nname: sql\nversion: 0.1.0\n",
		"templates/_sql.tpl":   helper,
		"templates/table.yaml": table,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var cases []map[string]any
	for _, enabled := range []bool{true, false} {
		for _, secret := range []string{"", "s", " "} {
			for _, password := range []string{"", "pw"} {
				for _, key := range []string{"", " "} {
					for _, ca := range []string{"", "ca"} {
						for _, cert := range []string{"", "c", " "} {
							for _, clientKey := range []string{"", "k"} {
								cases = append(cases, map[string]any{
									"enabled": enabled, "secretName": secret, "passwordKey": password, "privateKeyKey": key,
									"caKey": ca, "certKey": cert, "keyKey": clientKey,
									"mountPath": "/var/run/prometheus-universal-exporter/sql",
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
	schema := schemaAt(t, "sqlAuth")
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
	if len(first) != 5 {
		t.Errorf("the table reaches %d outcomes of the templates, want 5, the four refusals and none", len(first))
	}
	for refusal, block := range first {
		raw, _ := json.Marshal(map[string]any{"sqlAuth": block})
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, string(raw)))
		if ok != (refusal == "") {
			t.Errorf("%s: helm's schema validator renders it %v, and the templates refuse it with %q:\n%.600s", raw, ok, refusal, out)
		}
	}
}

// sqlAuth.mountPath is a path of its own, as the other credentials' are: at
// the configuration directory, at the static target directory, at another
// credential's path or at an extraVolumeMounts entry's, rendering fails
// naming both; inside or above another read-only volume, or with an
// extraVolumeMounts entry inside it, rendering fails naming the read-only
// Secret volume. Beside the others, and not enabled, it renders.
func TestChartRefusesAnSQLAuthMountPathTakenByAnotherMount(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	const static = "staticTargets: {enabled: true, data: \"targets: []\\n\"}\n"
	for name, tc := range map[string]struct{ values, message string }{
		"the configuration directory": {
			values:  "sqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /etc/prometheus-universal-exporter/}\n",
			message: `sqlAuth.mountPath "/etc/prometheus-universal-exporter/" is also the configuration directory, and a pod cannot mount two volumes at one path; choose another path`,
		},
		"the static target directory": {
			values:  static + "sqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /etc/prometheus-universal-exporter-static-targets}\n",
			message: `sqlAuth.mountPath "/etc/prometheus-universal-exporter-static-targets" is also the static target directory`,
		},
		"targetAuth's": {
			values:  "targetAuth: {enabled: true, secretName: t, mountPath: /secrets}\nsqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /secrets/}\n",
			message: `sqlAuth.mountPath "/secrets/" is also targetAuth.mountPath`,
		},
		"webAuth's": {
			values:  "webAuth: {enabled: true, secretName: w, mountPath: /secrets}\nsqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /secrets}\n",
			message: `sqlAuth.mountPath "/secrets" is also webAuth.mountPath`,
		},
		"sshAuth's": {
			values:  sshValues + "sqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /var/run/prometheus-universal-exporter/ssh}\n",
			message: `sqlAuth.mountPath "/var/run/prometheus-universal-exporter/ssh" is also sshAuth.mountPath`,
		},
		"an extraVolumeMounts entry's": {
			values:  sqlValues + "extraVolumes: [{name: x, emptyDir: {}}]\nextraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/sql/}]\n",
			message: `extraVolumeMounts uses mountPath "/var/run/prometheus-universal-exporter/sql/", which the chart already mounts as sqlAuth.mountPath; choose another path`,
		},
		"inside the configuration directory": {
			values:  "sqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /etc/prometheus-universal-exporter/sql}\n",
			message: `sqlAuth.mountPath "/etc/prometheus-universal-exporter/sql" is inside the configuration directory /etc/prometheus-universal-exporter, a read-only ConfigMap volume`,
		},
		"inside sshAuth's": {
			values:  "sshAuth: {enabled: true, secretName: s, passwordKey: pw, knownHostsSecretKey: kh, mountPath: /secrets}\nsqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /secrets/sql}\n",
			message: `sqlAuth.mountPath "/secrets/sql" is inside sshAuth.mountPath "/secrets", a read-only projected volume`,
		},
		"webAuth's inside it": {
			values:  "webAuth: {enabled: true, secretName: w, mountPath: /secrets/web}\nsqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /secrets}\n",
			message: `webAuth.mountPath "/secrets/web" is inside sqlAuth.mountPath "/secrets", a read-only Secret volume in which the mount point cannot be created, so the container would never start; choose a path outside /secrets`,
		},
		"above the configuration directory": {
			values:  "sqlAuth: {enabled: true, secretName: s, passwordKey: pw, mountPath: /etc/}\n",
			message: `sqlAuth.mountPath "/etc/" holds the configuration directory /etc/prometheus-universal-exporter, which cannot be mounted inside that read-only Secret volume, so the container would never start`,
		},
		"an extraVolumeMounts subPath file inside it": {
			values:  sqlValues + "extraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/sql/ca.crt, subPath: ca.crt}]\n",
			message: `extraVolumeMounts uses mountPath "/var/run/prometheus-universal-exporter/sql/ca.crt", which is inside sqlAuth.mountPath "/var/run/prometheus-universal-exporter/sql", a read-only Secret volume in which the mount point cannot be created, so the container would never start; mount it outside /var/run/prometheus-universal-exporter/sql`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, tc.values))
			if ok || !strings.Contains(out, tc.message) {
				t.Errorf("ok=%v, want an error with %q:\n%s", ok, tc.message, out)
			}
			if out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, tc.values)); ok {
				t.Errorf("rendered with the schema:\n%.400s", out)
			}
		})
	}
	for name, values := range map[string]string{
		"beside the others": sqlValues + sshValues + "webAuth: {enabled: true, secretName: s}\ntargetAuth: {enabled: true, secretName: s}\n" + static +
			"extraVolumes: [{name: x, emptyDir: {}}]\nextraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/sql-reporting}]\n",
		"not enabled": "sqlAuth: {enabled: false, mountPath: /etc/prometheus-universal-exporter/sql}\nextraVolumes: [{name: x, emptyDir: {}}]\nextraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/sql/x}]\n",
	} {
		for _, skip := range []bool{false, true} {
			args := []string{"-f", valuesFile(t, values)}
			if skip {
				args = append(args, "--skip-schema-validation")
			}
			if out, ok := helmTemplate(t, helm, chartDir, args...); !ok {
				t.Errorf("%s (schema skipped: %v): rendering failed:\n%s", name, skip, out)
			}
		}
	}
}

// The chart README's SQL collectors section renders as it is shown: the
// password and CA example with its collector, the collector naming the
// files the volume holds, by the sql request's own keys; the Snowflake
// example, a volume holding private_key alone; the further
// database's Secret mounted through extraVolumes beside sqlAuth; and the
// egress rule for the database port.
func TestChartReadmeSQLExamplesRender(t *testing.T) {
	helm := requireHelm(t)
	_, section, found := strings.Cut(readChartFile(t, "README.md"), "## SQL collectors\n")
	if !found {
		t.Fatal("the chart README has no SQL collectors section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	var auth, extra []string
	collector, policy := "", ""
	for _, block := range yamlBlocksOf(section) {
		switch {
		case strings.HasPrefix(block, "sqlAuth:\n"):
			auth = append(auth, block)
		case strings.HasPrefix(block, "config:\n"):
			collector = block
		case strings.HasPrefix(block, "networkPolicy:\n"):
			policy = block
		case strings.HasPrefix(block, "extraVolumes:\n"):
			extra = append(extra, block)
		}
	}
	if len(auth) != 2 || collector == "" || policy == "" || len(extra) != 1 {
		t.Fatalf("the section shows %d sqlAuth examples, a collector %v, a network policy %v and %d extra volumes, want 2, one, one and 1",
			len(auth), collector != "", policy != "", len(extra))
	}
	var config struct {
		Config struct {
			Data map[string]string `yaml:"data"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal([]byte(collector), &config); err != nil {
		t.Fatal(err)
	}
	var collectors struct {
		Collectors []struct {
			Request map[string]any `yaml:"request"`
		} `yaml:"collectors"`
	}
	if err := yaml.Unmarshal([]byte(config.Config.Data["config.yaml"]), &collectors); err != nil || len(collectors.Collectors) != 1 {
		t.Fatalf("the collector example does not parse as one collector: %v", err)
	}
	request := collectors.Collectors[0].Request
	if request["type"] != "sql" || request["password_file"] != "/var/run/prometheus-universal-exporter/sql/password" ||
		!reflect.DeepEqual(request["tls"], map[string]any{"ca_file": "/var/run/prometheus-universal-exporter/sql/ca.crt"}) {
		t.Errorf("the collector does not name the mounted password and CA files: %v", request)
	}
	for i, tc := range []struct {
		files  []string
		values []string
	}{
		{files: []string{"password", "ca.crt"}, values: []string{auth[0], collector}},
		{files: []string{"private_key"}, values: []string{auth[1]}},
		{files: []string{"password", "ca.crt"}, values: []string{auth[0], collector, extra[0], policy}},
	} {
		var args []string
		for _, v := range tc.values {
			args = append(args, "-f", valuesFile(t, v))
		}
		objects := renderedObjects(t, helm, args...)
		pod := podSpecOf(t, objects)
		volume := namedEntry(pod["volumes"], "sql-auth")
		var files []string
		for _, item := range volume["secret"].(map[string]any)["items"].([]any) {
			files = append(files, item.(map[string]any)["path"].(string))
		}
		if !reflect.DeepEqual(files, tc.files) {
			t.Errorf("example %d: the volume holds %v, want %v", i, files, tc.files)
		}
		if i != 2 {
			continue
		}
		if volume := namedEntry(pod["volumes"], "reporting-db"); volume == nil || volume["secret"].(map[string]any)["defaultMode"] != 0o400 {
			t.Errorf("the further database's Secret is not mounted with mode 0400: %v", volume)
		}
		var egress []any
		for _, object := range objects {
			if object["kind"] == "NetworkPolicy" {
				egress, _ = object["spec"].(map[string]any)["egress"].([]any)
			}
		}
		if raw, _ := json.Marshal(egress); !strings.Contains(string(raw), `{"port":5432}`) {
			t.Errorf("the network policy allows no egress to port 5432: %s", raw)
		}
	}
}
