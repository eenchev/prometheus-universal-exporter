package repository

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The configuration directory is a ConfigMap volume and the credential
// directories Secret volumes, which the kubelet mounts read-only. The chart
// refused a mount at one of their paths but not below one: an
// extraVolumeMounts entry such as /etc/prometheus-universal-exporter/collectors,
// a subPath file mount among the configuration files, or a credential's
// mountPath inside the configuration directory or inside the other
// credential's rendered, and the pod never started, since its mount point
// cannot be created inside a read-only volume (CreateContainerError). The
// tests below hold the chart to refusing each while rendering, naming the
// value and where to mount instead, and to rendering all it rendered before.

// configurationDirectory is where the chart mounts its ConfigMap.
const configurationDirectory = "/etc/prometheus-universal-exporter"

// Each mount below a read-only volume the chart mounts is refused: by the
// values schema where a pattern can say it, naming the value, and by the
// templates' own check with the schema skipped, naming the value, the
// directory it is inside and where to mount instead. A path written with
// doubled or dotted segments is compared cleaned.
func TestChartRefusesAMountInsideAReadOnlyVolume(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	for name, tc := range map[string]struct {
		values string
		// schema is the value the values schema refuses, empty where only
		// the templates can tell.
		schema, message string
	}{
		"an extraVolumeMounts directory in the configuration directory": {
			values: "extraVolumes: [{name: x, configMap: {name: x}}]\nextraVolumeMounts: [{name: x, mountPath: /etc/prometheus-universal-exporter/collectors}]\n",
			schema: "extraVolumeMounts.0.mountPath", message: `extraVolumeMounts uses mountPath "/etc/prometheus-universal-exporter/collectors", which is inside the configuration directory /etc/prometheus-universal-exporter, a read-only ConfigMap volume in which the mount point cannot be created, so the container would never start; mount it outside /etc/prometheus-universal-exporter; to add a file to it, add the file to config.data instead`,
		},
		"a subPath file in the configuration directory": {
			values: "extraVolumes: [{name: x, configMap: {name: x}}]\nextraVolumeMounts: [{name: x, mountPath: /etc/prometheus-universal-exporter/extra.yaml, subPath: extra.yaml}]\n",
			schema: "extraVolumeMounts.0.mountPath", message: `extraVolumeMounts uses mountPath "/etc/prometheus-universal-exporter/extra.yaml", which is inside the configuration directory`,
		},
		"a doubled and a dotted segment": {
			values:  "extraVolumeMounts: [{name: x, mountPath: /data}, {name: z, mountPath: //etc/./prometheus-universal-exporter//a/}]\n",
			message: `extraVolumeMounts uses mountPath "//etc/./prometheus-universal-exporter//a/", which is inside the configuration directory`,
		},
		"a dotted segment that stays inside": {
			values:  "extraVolumeMounts: [{name: x, mountPath: /etc/prometheus-universal-exporter/a/../b}]\n",
			message: `extraVolumeMounts uses mountPath "/etc/prometheus-universal-exporter/a/../b", which is inside the configuration directory`,
		},
		"webAuth.mountPath in the configuration directory": {
			values: "webAuth: {enabled: true, secretName: s, mountPath: /etc/prometheus-universal-exporter/auth}\n",
			schema: "webAuth.mountPath", message: `webAuth.mountPath "/etc/prometheus-universal-exporter/auth" is inside the configuration directory /etc/prometheus-universal-exporter, a read-only ConfigMap volume in which the mount point cannot be created, so the container would never start; choose a path outside /etc/prometheus-universal-exporter`,
		},
		"targetAuth.mountPath in the configuration directory": {
			values: "targetAuth: {enabled: true, secretName: s, mountPath: /etc/prometheus-universal-exporter/auth/}\n",
			schema: "targetAuth.mountPath", message: `targetAuth.mountPath "/etc/prometheus-universal-exporter/auth/" is inside the configuration directory`,
		},
		"targetAuth.mountPath in webAuth's": {
			values:  "webAuth: {enabled: true, secretName: s, mountPath: /secrets}\ntargetAuth: {enabled: true, secretName: s, mountPath: /secrets/target}\n",
			message: `targetAuth.mountPath "/secrets/target" is inside webAuth.mountPath "/secrets", a read-only Secret volume in which the mount point cannot be created, so the container would never start; choose a path outside /secrets`,
		},
		"webAuth.mountPath in targetAuth's": {
			values:  "webAuth: {enabled: true, secretName: s, mountPath: /secrets/web}\ntargetAuth: {enabled: true, secretName: s, mountPath: /secrets/}\n",
			message: `webAuth.mountPath "/secrets/web" is inside targetAuth.mountPath "/secrets/", a read-only Secret volume`,
		},
		"the configuration directory in webAuth's": {
			values:  "webAuth: {enabled: true, secretName: s, mountPath: /etc}\n",
			message: `webAuth.mountPath "/etc" holds the configuration directory /etc/prometheus-universal-exporter, which cannot be mounted inside that read-only Secret volume, so the container would never start; choose a path that is not above /etc/prometheus-universal-exporter`,
		},
		"an extraVolumeMounts entry in webAuth's": {
			values:  "webAuth: {enabled: true, secretName: s}\nextraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/web-auth/ca.crt, subPath: ca.crt}]\n",
			message: `extraVolumeMounts uses mountPath "/var/run/prometheus-universal-exporter/web-auth/ca.crt", which is inside webAuth.mountPath "/var/run/prometheus-universal-exporter/web-auth", a read-only Secret volume in which the mount point cannot be created, so the container would never start; mount it outside /var/run/prometheus-universal-exporter/web-auth`,
		},
		"an extraVolumeMounts entry in targetAuth's": {
			values:  "targetAuth: {enabled: true, secretName: s}\nextraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/target-auth/x}]\n",
			message: `which is inside targetAuth.mountPath "/var/run/prometheus-universal-exporter/target-auth"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			values := valuesFile(t, tc.values)
			out, ok := helmTemplate(t, helm, chartDir, "-f", values)
			if ok {
				t.Fatalf("rendered:\n%.400s", out)
			}
			if tc.schema != "" && !schemaRefusal(out, tc.schema, "not") {
				t.Errorf("the values schema does not refuse %s:\n%s", tc.schema, out)
			}
			if tc.schema == "" && !strings.Contains(out, tc.message) {
				t.Errorf("no %q in helm's error:\n%s", tc.message, out)
			}
			skipped, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", values)
			if ok || !strings.Contains(skipped, tc.message) {
				t.Errorf("with the schema skipped: ok=%v, want an error with %q:\n%s", ok, tc.message, skipped)
			}
		})
	}
}

// What is not below a read-only volume the chart mounts still renders, with
// the schema and without: a path beside the configuration directory, one
// that climbs out of it again, the credentials beside each other and below a
// volume of the values' own, and a mountPath below the configuration
// directory on a credential that is not enabled, which the chart does not
// mount.
func TestChartRendersAMountBesideTheReadOnlyVolumes(t *testing.T) {
	helm := requireHelm(t)
	requireSchemaSkipping(t, helm)
	for name, values := range map[string]string{
		"beside the configuration directory":     "extraVolumeMounts: [{name: x, mountPath: /etc/prometheus-universal-exporter-extra}, {name: z, mountPath: /etc/collectors}]\n",
		"out of it again":                        "extraVolumeMounts: [{name: x, mountPath: /etc/prometheus-universal-exporter/a/../../collectors}]\n",
		"the credentials beside each other":      "webAuth: {enabled: true, secretName: s, mountPath: /secrets/web/}\ntargetAuth: {enabled: true, secretName: s, mountPath: /secrets/webs}\nextraVolumes: [{name: x, emptyDir: {}}]\nextraVolumeMounts: [{name: x, mountPath: /secrets}]\n",
		"credentials that are not enabled":       "webAuth: {enabled: false, mountPath: /etc/prometheus-universal-exporter/web}\ntargetAuth: {enabled: false, mountPath: /etc/prometheus-universal-exporter/target}\nextraVolumeMounts: [{name: x, mountPath: /etc/prometheus-universal-exporter-web/x}]\n",
		"below a credential that is not enabled": "webAuth: {enabled: false}\nextraVolumeMounts: [{name: x, mountPath: /var/run/prometheus-universal-exporter/web-auth/x}]\n",
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

// oldValidateMounts is the validateMounts helper as it was before a mount
// below a read-only volume was refused, the oracle of the test below.
const oldValidateMounts = `{{- define "old.validateMounts" -}}
{{- $taken := dict "/etc/prometheus-universal-exporter" "the configuration directory" -}}
{{- range $name := list "targetAuth" "webAuth" -}}
{{- $auth := index $.Values $name -}}
{{- if and $auth $auth.enabled -}}
{{- $path := $auth.mountPath | toString | clean -}}
{{- if hasKey $taken $path -}}
{{- fail (printf "%s.mountPath %q is also %s, and a pod cannot mount two volumes at one path; choose another path" $name (toString $auth.mountPath) (get $taken $path)) -}}
{{- end -}}
{{- $_ := set $taken $path (printf "%s.mountPath" $name) -}}
{{- end -}}
{{- end -}}
{{- $extra := dict -}}
{{- range $mount := .Values.extraVolumeMounts -}}
{{- $path := $mount.mountPath | toString | clean -}}
{{- if hasKey $taken $path -}}
{{- fail (printf "extraVolumeMounts uses mountPath %q, which the chart already mounts as %s; choose another path" (toString $mount.mountPath) (get $taken $path)) -}}
{{- end -}}
{{- if hasKey $extra $path -}}
{{- fail (printf "extraVolumeMounts uses mountPath %q twice, and a pod cannot mount two volumes at one path; give each entry a path of its own" (toString $mount.mountPath)) -}}
{{- end -}}
{{- $_ := set $extra $path true -}}
{{- end -}}
{{- end }}
`

// failCall is a fail of the mount helpers, a whole line of its own.
var failCall = regexp.MustCompile(`(?m)^\{\{- fail (\(.*\)) -\}\}$`)

// recordingHelper turns a mount helper into one that records its first
// failure in the context's result instead of failing, so that one render
// can run it over a whole table.
func recordingHelper(t *testing.T, helper, name string) string {
	t.Helper()
	if !failCall.MatchString(helper) {
		t.Fatalf("%s has no fail to record", name)
	}
	return failCall.ReplaceAllString(helper, `{{- if not (hasKey $$.result "error") -}}{{- $$_ := set $$.result "error" $1 -}}{{- end -}}`)
}

// belowReadOnly reports whether the case puts a mount below a read-only
// volume the chart mounts, as Kubernetes would find it: the paths cleaned,
// one below another when it starts with the other and a slash.
func belowReadOnly(webAuth, targetAuth map[string]any, extra []any) bool {
	readOnly := []string{configurationDirectory}
	mounts := []string{configurationDirectory}
	for _, auth := range []map[string]any{targetAuth, webAuth} {
		if auth["enabled"] == true {
			p := pathpkg.Clean(auth["mountPath"].(string))
			readOnly, mounts = append(readOnly, p), append(mounts, p)
		}
	}
	for _, mount := range extra {
		mounts = append(mounts, pathpkg.Clean(mount.(map[string]any)["mountPath"].(string)))
	}
	for _, m := range mounts {
		for _, r := range readOnly {
			if m != r && strings.HasPrefix(m, strings.TrimSuffix(r, "/")+"/") {
				return true
			}
		}
	}
	return false
}

// What the fix changes, and only that, shown against the helper as it was:
// a chart of the two helpers runs both over a generated table of credential
// paths, enabled or not, and extra mounts, among them every path at, below,
// above and beside the configuration directory and the credentials, written
// with trailing slashes, doubled and dotted segments. Where the old helper
// refused, the new one refuses with the same message; where it rendered, the
// new one refuses exactly the cases with a mount below a read-only volume,
// as Go's path.Clean finds them, and renders every other.
func TestTheMountCheckRefusesAsBeforeAndBelowAReadOnlyVolume(t *testing.T) {
	helm := requireHelm(t)
	helpers := readChartFile(t, "templates/_helpers.tpl")
	start := strings.Index(helpers, `{{- define "prometheus-universal-exporter.validateMounts" -}}`)
	if start < 0 {
		t.Fatal("_helpers.tpl no longer defines validateMounts")
	}
	end := strings.Index(helpers[start+1:], "{{- define ")
	if end < 0 {
		t.Fatal("validateMounts is not followed by another helper")
	}
	current := strings.Replace(helpers[start:start+1+end], `"prometheus-universal-exporter.validateMounts"`, `"new.validateMounts"`, 1)
	dir := filepath.Join(t.TempDir(), "mounts")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o750); err != nil {
		t.Fatal(err)
	}
	const table = `kind: Table
rows:
{{- range .Values.cases }}
{{- $old := dict "Values" . "result" (dict) }}
{{- $new := dict "Values" . "result" (dict) }}
{{- $_ := include "old.validateMounts" $old }}
{{- $_ := include "new.validateMounts" $new }}
  - [{{ $old.result.error | default "" | quote }}, {{ $new.result.error | default "" | quote }}]
{{- end }}
`
	for name, content := range map[string]string{
		"Chart.yaml":           "apiVersion: v2\nname: mounts\nversion: 0.1.0\n",
		"templates/_old.tpl":   recordingHelper(t, oldValidateMounts, "the old helper"),
		"templates/_new.tpl":   recordingHelper(t, current, "validateMounts"),
		"templates/table.yaml": table,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const web, target = "/var/run/prometheus-universal-exporter/web-auth", "/var/run/prometheus-universal-exporter/target-auth"
	var pool []string
	for _, base := range []string{configurationDirectory, web, target, "/secrets", "/etc"} {
		pool = append(pool, base, base+"/", base+"//", base+"/.", base+"/a", base+"/a/", base+"/a/b", base+"/x.yaml",
			base+"/a/../b", base+"/a/../../x", base+"-x", base+"x/a", "/"+base+"/a", base+"/./a")
	}
	pool = append(pool, "/", "/etc/./prometheus-universal-exporter/a", "/var/run", "/var/run/prometheus-universal-exporter", "/data", "/data/a")
	random := rand.New(rand.NewPCG(52, 16))
	auth := func() map[string]any {
		return map[string]any{"enabled": random.IntN(3) > 0, "mountPath": pool[random.IntN(len(pool))]}
	}
	cases := []any{
		map[string]any{"webAuth": map[string]any{"enabled": false, "mountPath": web}, "targetAuth": map[string]any{"enabled": false, "mountPath": target}, "extraVolumeMounts": []any{}},
	}
	for range 4000 {
		var extra []any
		for range random.IntN(4) {
			extra = append(extra, map[string]any{"name": "x", "mountPath": pool[random.IntN(len(pool))]})
		}
		cases = append(cases, map[string]any{"webAuth": auth(), "targetAuth": auth(), "extraVolumeMounts": extra})
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
		Rows [][]string `yaml:"rows"`
	}
	if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("the table does not parse: %v", err)
	}
	if len(rendered.Rows) != len(cases) {
		t.Fatalf("the table has %d rows, want %d", len(rendered.Rows), len(cases))
	}
	counts := map[string]int{}
	for i, row := range rendered.Rows {
		c := cases[i].(map[string]any)
		extra, _ := c["extraVolumeMounts"].([]any)
		below := belowReadOnly(c["webAuth"].(map[string]any), c["targetAuth"].(map[string]any), extra)
		old, current := row[0], row[1]
		describe := func() string { raw, _ := json.Marshal(c); return string(raw) }
		switch {
		case old != "":
			counts["refused before"]++
			if current != old {
				t.Errorf("%s: the old helper refuses with %q and the new one with %q; they must be the same", describe(), old, current)
			}
		case below:
			counts["below a read-only volume"]++
			if !strings.Contains(current, "read-only") || !strings.Contains(current, "would never start") {
				t.Errorf("%s: a mount below a read-only volume gives %q, want it refused", describe(), current)
			}
		default:
			counts["rendered"]++
			if current != "" {
				t.Errorf("%s: the old helper renders it, but the new one refuses with %q", describe(), current)
			}
		}
	}
	// The table has to reach every branch, or it proves nothing about one.
	for _, kind := range []string{"refused before", "below a read-only volume", "rendered"} {
		if counts[kind] < 200 {
			t.Errorf("only %d cases of the table are %s; the table has to hold more", counts[kind], kind)
		}
	}
}

// insideConfigurationDirectory is the pattern the values schema refuses a
// mount path by.
func insideConfigurationDirectory(t *testing.T) *regexp.Regexp {
	t.Helper()
	mount, _ := schemaAt(t, "extraVolumeMounts")["items"].(map[string]any)["properties"].(map[string]any)["mountPath"].(map[string]any)
	not, _ := mount["not"].(map[string]any)
	pattern, _ := not["pattern"].(string)
	if pattern == "" {
		t.Fatal("the values schema refuses no extraVolumeMounts mountPath by a pattern")
	}
	for _, name := range []string{"webAuth", "targetAuth"} {
		then, _ := schemaAt(t, name)["then"].(map[string]any)
		got := fmt.Sprint(then["properties"].(map[string]any)["mountPath"].(map[string]any)["not"].(map[string]any)["pattern"])
		if got != pattern {
			t.Errorf("the values schema refuses %s.mountPath by %q, and an extraVolumeMounts mountPath by %q; they must be the same", name, got, pattern)
		}
	}
	return regexp.MustCompile(pattern)
}

// The values schema refuses a mount path by a pattern only where the
// templates refuse it too, so that both give one verdict wherever the schema
// gives one: over generated paths of plain, empty, dotted and doubled
// segments, every path the pattern matches is, cleaned, below the
// configuration directory, and every plain path below it is matched. The
// credentials are refused by it only while enabled, as the templates mount
// them, which the repository's own schema validator finds too.
func TestTheSchemaRefusesAMountPathInsideTheConfigurationDirectoryAsTheTemplatesDo(t *testing.T) {
	inside := insideConfigurationDirectory(t)
	segments := []string{"a", "x.yaml", ".", "..", "", ".a", "..a", "...", "a.", "prometheus-universal-exporter", "etc"}
	random := rand.New(rand.NewPCG(52, 36))
	for range 20000 {
		parts := []string{configurationDirectory}
		if random.IntN(8) == 0 {
			parts[0] = "/etc/" + segments[random.IntN(len(segments))]
		}
		for range random.IntN(5) {
			parts = append(parts, segments[random.IntN(len(segments))])
		}
		p := strings.Join(parts, "/")
		cleaned := pathpkg.Clean(p)
		below := strings.HasPrefix(cleaned, configurationDirectory+"/")
		if inside.MatchString(p) && !below {
			t.Errorf("the schema refuses %q, which is %q and not below the configuration directory", p, cleaned)
		}
	}
	for _, p := range []string{"/a", "/collectors", "/extra.yaml", "/a/b/", "//a", "/a//b"} {
		if !inside.MatchString(configurationDirectory + p) {
			t.Errorf("the schema does not refuse %q", configurationDirectory+p)
		}
	}
	for _, p := range []string{configurationDirectory, configurationDirectory + "/", configurationDirectory + "//", configurationDirectory + "-x/a", "/etc/collectors"} {
		if inside.MatchString(p) {
			t.Errorf("the schema refuses %q, which is not below the configuration directory", p)
		}
	}
	for _, name := range []string{"webAuth", "targetAuth"} {
		schema := schemaAt(t, name)
		for _, enabled := range []bool{true, false} {
			value := map[string]any{"enabled": enabled, "mountPath": configurationDirectory + "/auth"}
			if errs := validateAgainstSchema(schema, value); (len(errs) != 0) != enabled {
				t.Errorf("%s %v: the repository's validator says %v, want refused %v", name, value, errs, enabled)
			}
		}
	}
}
