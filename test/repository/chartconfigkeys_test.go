package repository

import (
	"regexp"
	"strings"
	"testing"
)

// staticTargetsDirectory is where the chart mounts the static target
// document: a directory of its own, beside the configuration directory.
const staticTargetsDirectory = "/etc/prometheus-universal-exporter-static-targets"

// configMapKey is what Kubernetes takes as a ConfigMap key, the oracle of the
// test below.
var configMapKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// A key of config.data is a ConfigMap key, which Kubernetes refuses unless it
// holds only letters, digits, -, _ and ., is at most 253 characters and is
// not . or ..; such a key rendered, and the release failed only when it was
// applied. The values schema and the templates now refuse exactly the keys
// Kubernetes does, with config.enabled on or off: the schema naming the key,
// the templates naming it, what a key may hold and that the directory is
// flat. Every other key renders, with the schema and without.
func TestChartRefusesAConfigDataKeyKubernetesRefuses(t *testing.T) {
	helm := requireHelm(t)
	type key struct{ name, enabled string }
	var keys []key
	for _, name := range []string{
		"collectors/payments.yaml", "my collectors.yaml", "a:b", "café.yaml", ".", "..", strings.Repeat("a", 254),
		"collectors-payments.yaml", "_x", "-x", "...", "123", strings.Repeat("a", 253),
	} {
		keys = append(keys, key{name, "true"})
	}
	keys = append(keys, key{"collectors/payments.yaml", "false"}, key{"collectors-payments.yaml", "false"})
	for _, k := range keys {
		key, enabled := k.name, k.enabled
		valid := configMapKey.MatchString(key) && len(key) <= 253 && key != "." && key != ".."
		values := valuesFile(t, "config:\n  enabled: "+enabled+"\n  data:\n    config.yaml: |\n"+indentLines(exampleCollectors, "      ")+"    \""+key+"\": |\n      collectors: []\n")
		name := key
		if len(name) > 40 {
			name = name[:20] + "..."
		}
		out, ok := helmTemplate(t, helm, chartDir, "-f", values)
		skipped, skippedOK := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", values)
		if valid {
			if !ok || !skippedOK {
				t.Errorf("key %q (config.enabled %s): refused, want it rendered:\n%s\n%s", name, enabled, out, skipped)
			}
			continue
		}
		if ok || !strings.Contains(out, "invalid propertyName '"+key+"'") {
			t.Errorf("key %q (config.enabled %s): the values schema does not refuse it:\n%.600s", name, enabled, out)
		}
		for _, want := range []string{`config.data key "` + key + `" is not a ConfigMap key`, "letters, digits, -, _ and ., at most 253 characters", "the configuration directory is flat"} {
			if skippedOK || !strings.Contains(skipped, want) {
				t.Errorf("key %q (config.enabled %s), schema skipped: ok=%v, want an error with %q:\n%.600s", name, enabled, skippedOK, want, skipped)
			}
		}
	}
}

// indentLines puts prefix before every line of text.
func indentLines(text, prefix string) string {
	var b strings.Builder
	for line := range strings.SplitAfterSeq(text, "\n") {
		if line != "" {
			b.WriteString(prefix)
			b.WriteString(line)
		}
	}
	return b.String()
}

// The static target document has a ConfigMap of its own, mounted at a
// directory of its own while static targets are enabled, so that directory
// is taken as the configuration directory is: an extraVolumeMounts entry or
// a credential's mountPath at it, a mount inside it, and a credential's
// mountPath above it fail rendering, naming the static target directory,
// while static targets are on, and render while they are off. With
// config.enabled: false the static target ConfigMap still renders, and the
// document is still required.
func TestChartMountsTheStaticTargetDocumentInADirectoryOfItsOwn(t *testing.T) {
	helm := requireHelm(t)
	const targets = "staticTargets:\n  enabled: true\n  monitor: {enabled: false}\n  data: |\n    targets:\n      - {collector: example, target: http://x}\n"
	for name, tc := range map[string]struct{ values, message string }{
		"an extraVolumeMounts entry at it": {
			"extraVolumes: [{name: x, emptyDir: {}}]\nextraVolumeMounts: [{name: x, mountPath: " + staticTargetsDirectory + "/}]\n",
			`extraVolumeMounts uses mountPath "` + staticTargetsDirectory + `/", which the chart already mounts as the static target directory`,
		},
		"webAuth.mountPath at it": {
			"webAuth: {enabled: true, secretName: s, mountPath: " + staticTargetsDirectory + "}\n",
			`webAuth.mountPath "` + staticTargetsDirectory + `" is also the static target directory`,
		},
		"an extraVolumeMounts subPath file inside it": {
			"extraVolumes: [{name: x, emptyDir: {}}]\nextraVolumeMounts: [{name: x, mountPath: " + staticTargetsDirectory + "/x.yaml, subPath: x.yaml}]\n",
			"which is inside the static target directory " + staticTargetsDirectory + ", a read-only ConfigMap volume",
		},
		"targetAuth.mountPath inside it": {
			"targetAuth: {enabled: true, secretName: s, mountPath: " + staticTargetsDirectory + "/auth}\n",
			`targetAuth.mountPath "` + staticTargetsDirectory + `/auth" is inside the static target directory`,
		},
	} {
		on := valuesFile(t, targets+tc.values)
		if out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", on); ok || !strings.Contains(out, tc.message) {
			t.Errorf("%s: ok=%v, want an error with %q:\n%.600s", name, ok, tc.message, out)
		}
		if out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, tc.values)); !ok {
			t.Errorf("%s, static targets off: refused:\n%.600s", name, out)
		}
	}

	external := valuesFile(t, targets+"config: {enabled: false}\n")
	files, _ := configMapFiles(t, helm, "-f", external)
	if _, rendered := files["static-targets.yaml"]; !rendered || len(files) != 1 {
		t.Errorf("with config.enabled: false the ConfigMaps hold %v, want the static target document alone", files)
	}
	if out, ok := helmTemplate(t, helm, chartDir, "--set", "config.enabled=false", "--set", "staticTargets.enabled=true", "--set", "staticTargets.monitor.enabled=false"); ok ||
		!strings.Contains(out, "staticTargets.enabled requires staticTargets.data") {
		t.Errorf("with config.enabled: false and no document: ok=%v:\n%.600s", ok, out)
	}
}
