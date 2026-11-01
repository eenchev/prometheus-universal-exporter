package repository

import (
	"strings"
	"testing"
)

// server.pythonMaxIdleWorkers and server.pythonIdleTimeout render
// --python.max-idle-workers and --python.idle-timeout when set, and nothing
// by default, so the exporter's own defaults, one worker and two minutes,
// apply. 0 is a value of its own, keeping no worker idle, and is rendered.
func TestChartRendersThePythonIdleWorkerFlags(t *testing.T) {
	helm := requireHelm(t)
	out, ok := helmTemplate(t, helm, chartDir)
	if !ok {
		t.Fatalf("the defaults fail rendering:\n%s", out)
	}
	for _, flag := range []string{"--python.max-idle-workers", "--python.idle-timeout"} {
		if strings.Contains(out, flag) {
			t.Errorf("the defaults render %s, want the exporter's default", flag)
		}
	}
	for values, want := range map[string][]string{
		"server:\n  pythonMaxIdleWorkers: 0\n  pythonIdleTimeout: 5m\n":     {`- "--python.max-idle-workers=0"`, `- "--python.idle-timeout=5m"`},
		"server:\n  pythonMaxIdleWorkers: \"3\"\n  pythonIdleTimeout: 1s\n": {`- "--python.max-idle-workers=3"`, `- "--python.idle-timeout=1s"`},
		"server:\n  pythonIdleTimeout: 1h30m\n":                             {`- "--python.idle-timeout=1h30m"`},
	} {
		out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, values))
		if !ok {
			t.Errorf("rendering failed with\n%s:\n%s", values, out)
			continue
		}
		for _, line := range want {
			if strings.Count(out, line) != 1 {
				t.Errorf("with\n%s the render has %q %d times, want once", values, line, strings.Count(out, line))
			}
		}
	}
}

// An idle timeout the exporter would refuse at startup, under a second or
// no whole hours, minutes and seconds, fails rendering, by the schema and
// with it skipped by the template; so does a count of idle workers that is
// no whole number from 0, the template's message saying what 0 does; and
// so does an extraArgs entry of either flag, which the chart manages.
func TestChartRefusesPythonIdleWorkerValuesOutOfRange(t *testing.T) {
	helm := requireHelm(t)
	for name, tc := range map[string]struct {
		values, want string
	}{
		"a timeout of zero":             {"server:\n  pythonIdleTimeout: 0s\n", "server.pythonIdleTimeout"},
		"a timeout under a second":      {"server:\n  pythonIdleTimeout: 500ms\n", "pythonIdleTimeout"},
		"a timeout that is no duration": {"server:\n  pythonIdleTimeout: ten\n", "pythonIdleTimeout"},
		"a negative count":              {"server:\n  pythonMaxIdleWorkers: -1\n", "pythonMaxIdleWorkers"},
		"a fractional count":            {"server:\n  pythonMaxIdleWorkers: 1.5\n", "pythonMaxIdleWorkers"},
		"a count read as octal":         {"server:\n  pythonMaxIdleWorkers: \"010\"\n", "pythonMaxIdleWorkers"},
		"the count flag in extraArgs":   {"extraArgs: [\"--python.max-idle-workers=2\"]\n", "set server.pythonMaxIdleWorkers instead"},
		"the timeout flag in extraArgs": {"extraArgs: [\"--python.idle-timeout=5m\"]\n", "set server.pythonIdleTimeout instead"},
	} {
		t.Run(name, func(t *testing.T) {
			out, ok := helmTemplate(t, helm, chartDir, "-f", valuesFile(t, tc.values))
			if ok || !namesInHelmError(out, tc.want) {
				t.Fatalf("ok=%v, want an error naming %q:\n%s", ok, tc.want, out)
			}
		})
	}
	requireSchemaSkipping(t, helm)
	for values, want := range map[string]string{
		"server:\n  pythonIdleTimeout: 0s\n":         `server.pythonIdleTimeout "0s" must be whole hours, minutes and seconds of at least 1s`,
		"server:\n  pythonIdleTimeout: 500ms\n":      `server.pythonIdleTimeout "500ms" must be whole hours, minutes and seconds of at least 1s`,
		"server:\n  pythonMaxIdleWorkers: \"010\"\n": `server.pythonMaxIdleWorkers "010" must be a whole number from 0 to 2147483647, 0 to keep no worker idle, or empty to keep the exporter's default`,
		"server:\n  pythonMaxIdleWorkers: -1\n":      `server.pythonMaxIdleWorkers "-1" must be a whole number from 0 to 2147483647, 0 to keep no worker idle`,
	} {
		out, ok := helmTemplate(t, helm, chartDir, "--skip-schema-validation", "-f", valuesFile(t, values))
		if ok || !strings.Contains(out, want) {
			t.Errorf("with the schema skipped and\n%s ok=%v, want an error saying %q:\n%s", values, ok, want, out)
		}
	}
}
