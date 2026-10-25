package repository

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// `make run` is how a shipped example is tried out. Running one by hand went
// wrong twice: an old ./prometheus-universal-exporter, left in the repository
// root by a `go build`, ran in place of the bin/ binary `make build` had just
// made, and later the bin/ binary was itself older than the tree. The target
// builds first, through `make build`, and is no part of `make ci`, which
// would never return from it.
func TestMakeRunBuildsThenRunsAndIsNoPartOfCI(t *testing.T) {
	prerequisites, recipes := makefileRules(t)
	recipe, ok := recipes["run"]
	if !ok {
		t.Fatal("the Makefile has no run target")
	}
	phony := false
	for _, match := range regexp.MustCompile(`(?m)^\.PHONY:(.*)$`).FindAllStringSubmatch(read(t, "Makefile"), -1) {
		phony = phony || slices.Contains(strings.Fields(match[1]), "run")
	}
	if !phony {
		t.Error("the run target is not .PHONY")
	}
	build := strings.Index(recipe, "$(MAKE) --no-print-directory build OPT_IN_REQUEST_TYPES=")
	run := strings.Index(recipe, "sh tools/run-example.sh run '$(EXAMPLE)' '$(HOST)' '$(PORT)' $(ARGS)")
	if build < 0 || run < 0 || run < build {
		t.Errorf("make run does not run make build and then tools/run-example.sh run:\n%s", recipe)
	}
	reached, queue := map[string]bool{}, []string{"ci"}
	for len(queue) > 0 {
		target := queue[0]
		queue = queue[1:]
		if reached[target] {
			continue
		}
		reached[target] = true
		queue = append(queue, prerequisites[target]...)
	}
	if reached["run"] {
		t.Error("make ci reaches run, which runs the exporter until it is stopped")
	}
	makefile := read(t, "Makefile")
	for _, want := range []string{"\nEXAMPLE =\n", "\nHOST = 127.0.0.1\n", "\nPORT = 8080\n", "\nARGS =\n"} {
		if !strings.Contains(makefile, want) {
			t.Errorf("the Makefile does not set %q: set with =, only the command line changes it, and HOST is this machine alone", strings.TrimSpace(want))
		}
	}
}

// exampleName is the name `make run` knows an example configuration by:
// its directory's under examples/, or <name> of examples/config.<name>.*.yaml.
func exampleName(t *testing.T, config string) string {
	t.Helper()
	if dir := filepath.Dir(config); dir != "examples" {
		return filepath.Base(dir)
	}
	name, ok := strings.CutPrefix(filepath.Base(config), "config.")
	if !ok {
		t.Fatalf("%s: a configuration directly in examples/ is named config.<name>.<anything>.yaml, or make run cannot name it", config)
	}
	name, _, _ = strings.Cut(name, ".")
	return name
}

func runExample(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"tools/run-example.sh"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stderr.String(), err
	}
	return stdout.String(), nil
}

// Every shipped example runs under its name, which comes from the tree, as
// the files the tree holds for it: its configuration and, beside it, its
// static target file. No two examples share a name, and a name no example
// has fails, naming the ones there are.
func TestEveryExampleRunsUnderItsName(t *testing.T) {
	tree := shippedExamples(t)
	names := map[string]string{}
	for _, config := range tree.configs {
		name := exampleName(t, config)
		if other, ok := names[name]; ok {
			t.Errorf("%s and %s are both named %s for make run", other, config, name)
		}
		names[name] = config
		want := []string{config}
		for _, targets := range tree.staticTargets {
			if filepath.Dir(targets) == filepath.Dir(config) && filepath.Dir(config) != "examples" {
				want = append(want, targets)
			}
		}
		out, err := runExample(t, "files", name)
		if err != nil {
			t.Errorf("tools/run-example.sh files %s: %v: %s", name, err, out)
			continue
		}
		if got := strings.Fields(out); !slices.Equal(got, want) {
			t.Errorf("tools/run-example.sh files %s gives %v, want %v", name, got, want)
		}
	}
	for _, want := range []string{"open-meteo", "metar", "tcp", "ecb"} {
		if _, ok := names[want]; !ok {
			t.Errorf("no example is named %s: %v", want, names)
		}
	}
	out, err := runExample(t, "files", "no-such-example")
	if err == nil || !strings.Contains(out, `no example "no-such-example"`) || !strings.Contains(out, "  open-meteo\n") {
		t.Errorf("an unknown example gave %v: %s", err, out)
	}
}

// `make run` alone lists the examples, every one the tree has, with a usage
// line, and fails; it builds nothing.
func TestMakeRunWithoutAnExampleListsThem(t *testing.T) {
	out, err := runMake(t, t.TempDir(), "run")
	if err == nil {
		t.Errorf("make run without EXAMPLE succeeded:\n%s", out)
	}
	if !strings.Contains(out, "usage: make run EXAMPLE=<name>") {
		t.Errorf("make run without EXAMPLE gives no usage line:\n%s", out)
	}
	if strings.Contains(out, "go build") {
		t.Errorf("make run without EXAMPLE built the exporter:\n%s", out)
	}
	for _, config := range shippedExamples(t).configs {
		if want := "  " + exampleName(t, config) + " "; !strings.Contains(out, want) || !strings.Contains(out, config) {
			t.Errorf("make run without EXAMPLE does not list %s as %q:\n%s", config, strings.TrimSpace(want), out)
		}
	}
}

// An example of an opt-in request type runs without the user knowing the
// type: the types its collectors name are added to OPT_IN_REQUEST_TYPES,
// beside any the command line gives, for every example; examples/tcp is
// built with tcp, and an unknown example fails before anything is built.
func TestMakeRunAddsTheOptInTypesAnExampleNeeds(t *testing.T) {
	optIn := optInRequestTypes(t)
	tree := shippedExamples(t)
	for _, config := range tree.configs {
		var want []string
		for _, name := range exampleCollectorTypes(t, tree, config) {
			if slices.Contains(optIn, name) {
				want = append(want, name)
			}
		}
		slices.Sort(want)
		name := exampleName(t, config)
		out, err := runExample(t, "opt-in-types", name)
		if err != nil {
			t.Fatalf("tools/run-example.sh opt-in-types %s: %v: %s", name, err, out)
		}
		got := strings.Split(strings.TrimSpace(out), ",")
		if strings.TrimSpace(out) == "" {
			got = nil
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: the opt-in types added are %v, want %v", name, got, want)
		}
	}
	if out, err := runExample(t, "opt-in-types", "tcp", "redis,tcp"); err != nil || strings.TrimSpace(out) != "redis,tcp" {
		t.Errorf("tcp with OPT_IN_REQUEST_TYPES=redis,tcp gives %q (%v), want redis,tcp", out, err)
	}
	out, err := runMake(t, t.TempDir(), "-n", "run", "EXAMPLE=tcp")
	if err != nil || !strings.Contains(out, "sh tools/request-type-tags.sh '' '' 'tcp'") {
		t.Errorf("make -n run EXAMPLE=tcp does not build with tcp (%v):\n%s", err, out)
	}
	out, err = runMake(t, t.TempDir(), "-n", "run", "EXAMPLE=no-such-example")
	if err == nil || strings.Contains(out, "go build") {
		t.Errorf("make -n run EXAMPLE=no-such-example built or succeeded (%v):\n%s", err, out)
	}
}

// The run itself: the URLs to open first, the landing page, the status page,
// for the static targets' last scrapes when the example has them and the
// recent probes always, and the first probe the configuration's opening
// comment gives, when its collector is the example's, then bin/'s binary
// with the example's files, the listen address and the flags of ARGS. It
// runs in a copy of the tree's shape whose bin/ binary is a script that
// prints what it was given.
func TestRunExampleRunsTheBinOfMakeBuild(t *testing.T) {
	script, err := filepath.Abs("tools/run-example.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, file := range []string{"examples/open-meteo/config.yaml", "examples/open-meteo/static-targets.yaml", "examples/config.ecb.xml-test.yaml", "examples/config.filebeat.json-test.yaml", "examples/metar/config.yaml", "examples/metar/static-targets.yaml"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(read(t, file)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A comment that names a collector the configuration no longer has gives
	// no probe URL.
	stale := strings.Replace(read(t, "examples/config.ecb.xml-test.yaml"), "collector=ecb_reference_rates", "collector=renamed_rates", 1)
	if !strings.Contains(stale, "collector=renamed_rates") {
		t.Fatal("examples/config.ecb.xml-test.yaml no longer gives a probe of ecb_reference_rates")
	}
	if err := os.WriteFile(filepath.Join(dir, "examples", "config.stale.xml-test.yaml"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "prometheus-universal-exporter"), []byte("#!/bin/sh\nfor a in \"$@\"; do echo \"arg: $a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"open-meteo": {
			"Open http://127.0.0.1:9115/ for the landing page",
			"Open http://127.0.0.1:9115/static-targets/status for the static targets' last scrapes and the recent probes.",
			"arg: --config.file=examples/open-meteo/config.yaml",
			"arg: --web.listen-address=127.0.0.1:9115",
			"arg: --static-targets-file=examples/open-meteo/static-targets.yaml",
			"arg: --log.level=debug",
		},
		"ecb": {
			"Open http://127.0.0.1:9115/ for the landing page",
			"Open http://127.0.0.1:9115/static-targets/status for the recent probes.",
			"Open http://127.0.0.1:9115/probe?collector=ecb_reference_rates&target=https://www.ecb.europa.eu for a probe.",
			"arg: --config.file=examples/config.ecb.xml-test.yaml",
			"arg: --web.listen-address=127.0.0.1:9115",
			"arg: --log.level=debug",
		},
		// filebeat's comment probes a placeholder host, filebeat-host, which
		// a local run cannot reach: a hint, with the reader's target.
		"filebeat": {
			"Open http://127.0.0.1:9115/ for the landing page",
			"Open http://127.0.0.1:9115/static-targets/status for the recent probes.",
			"Probe it with http://127.0.0.1:9115/probe?collector=filebeat&target=<your target>: the example's target, http://filebeat-host:5066, is a placeholder for yours.",
			"arg: --config.file=examples/config.filebeat.json-test.yaml",
			"arg: --web.listen-address=127.0.0.1:9115",
			"arg: --log.level=debug",
		},
		"metar": {
			"Open http://127.0.0.1:9115/ for the landing page",
			"Open http://127.0.0.1:9115/static-targets/status for the static targets' last scrapes and the recent probes.",
			"Open http://127.0.0.1:9115/probe?collector=metar&target=https://tgftp.nws.noaa.gov&param_station=EGLL for a probe.",
			"arg: --config.file=examples/metar/config.yaml",
			"arg: --web.listen-address=127.0.0.1:9115",
			"arg: --static-targets-file=examples/metar/static-targets.yaml",
			"arg: --log.level=debug",
		},
		"stale": {
			"Open http://127.0.0.1:9115/ for the landing page",
			"Open http://127.0.0.1:9115/static-targets/status for the recent probes.",
			"arg: --config.file=examples/config.stale.xml-test.yaml",
			"arg: --web.listen-address=127.0.0.1:9115",
			"arg: --log.level=debug",
		},
	} {
		cmd := exec.Command("sh", script, "run", name, "127.0.0.1", "9115", "--log.level=debug")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		var lines []string
		for line := range strings.Lines(string(out)) {
			if strings.HasPrefix(line, "Open ") || strings.HasPrefix(line, "Probe ") || strings.HasPrefix(line, "arg: ") {
				lines = append(lines, strings.TrimSuffix(line, "\n"))
			}
		}
		if len(lines) != len(want) {
			t.Errorf("%s gives %q, want %q", name, lines, want)
			continue
		}
		for i := range want {
			if !strings.HasPrefix(lines[i], want[i]) {
				t.Errorf("%s gives %q, want %q", name, lines, want)
				break
			}
		}
	}
}

// The probe URL a run prints is one a local run can reach: its target is
// localhost, 127.0.0.1 or [::1], or a DNS name with a dot that is no
// placeholder; for any other the run prints a hint with the reader's target
// in its place, as for filebeat's filebeat-host.
func TestRunExamplePrintsAProbeURLOnlyForAReachableTarget(t *testing.T) {
	script := read(t, "tools/run-example.sh")
	start := strings.Index(script, "probe_target_is_local() {")
	end := strings.Index(script[max(start, 0):], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("tools/run-example.sh no longer has probe_target_is_local")
	}
	function := script[start : start+end+3]
	for query, want := range map[string]bool{
		"collector=a&target=http://localhost:5066":                    true,
		"collector=a&target=localhost:11211":                          true,
		"collector=a&target=http://127.0.0.1:9100/x":                  true,
		"collector=a&target=http://[::1]:9100":                        true,
		"collector=a&target=https://www.ecb.europa.eu":                true,
		"collector=a&target=https://tgftp.nws.noaa.gov&param_x=1":     true,
		"collector=a&target=https://API.Frankfurter.dev.":             true,
		"collector=a&target=http://filebeat-host:5066":                false,
		"collector=a&target=http://db-host.corp.acme.io":              false,
		"collector=a&target=http://filebeat:5066":                     false,
		"collector=a&target=https://otel.example.com":                 false,
		"collector=a&target=https://api.example":                      false,
		"collector=a&target=http://u:p@kibana.svc.cluster.local:5601": false,
		"collector=a&target=http://es.logging.svc":                    false,
		"collector=a&target=http://nas.local":                         false,
		"collector=a&target=http://vault.internal:8200":               false,
		"collector=a&target=http://10.0.0.7:9100":                     false,
		"collector=a&target=http://[2001:db8::1]:9100":                false,
		"collector=a": false,
	} {
		out, err := exec.Command("sh", "-c", function+"\nprobe_target_is_local \"$1\"", "sh", query).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", query, err, out)
		}
		if got := strings.TrimSpace(string(out)) == "yes"; got != want {
			t.Errorf("%s: a probe URL printed is %v, want %v", query, got, want)
		}
	}
}

// HOST is where the exporter listens and the host of the URLs printed: a
// bare IPv6 address is bracketed for both, as Go's listen address and a URL
// need it, an already bracketed one kept; 0.0.0.0 and :: listen on every
// interface, which the run says, and the URLs open localhost and [::1], what
// a browser on the same machine can open; an empty HOST fails before make run
// builds anything.
func TestRunExampleTakesEveryHost(t *testing.T) {
	script, err := filepath.Abs("tools/run-example.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "examples"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "examples", "config.ecb.xml-test.yaml"), []byte(read(t, "examples/config.ecb.xml-test.yaml")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "prometheus-universal-exporter"), []byte("#!/bin/sh\nfor a in \"$@\"; do echo \"arg: $a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]struct{ listen, url, everyInterface string }{
		"127.0.0.1": {"127.0.0.1:9115", "http://127.0.0.1:9115/", ""},
		"localhost": {"localhost:9115", "http://localhost:9115/", ""},
		"::1":       {"[::1]:9115", "http://[::1]:9115/", ""},
		"[::1]":     {"[::1]:9115", "http://[::1]:9115/", ""},
		"fe80::1":   {"[fe80::1]:9115", "http://[fe80::1]:9115/", ""},
		"0.0.0.0":   {"0.0.0.0:9115", "http://localhost:9115/", "The exporter listens on every interface, 0.0.0.0:9115; on this machine it is http://localhost:9115/."},
		"::":        {"[::]:9115", "http://[::1]:9115/", "The exporter listens on every interface, [::]:9115; on this machine it is http://[::1]:9115/."},
		"[::]":      {"[::]:9115", "http://[::1]:9115/", "The exporter listens on every interface, [::]:9115; on this machine it is http://[::1]:9115/."},
	} {
		cmd := exec.Command("sh", script, "run", "ecb", host, "9115")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("HOST=%s: %v: %s", host, err, out)
			continue
		}
		text := string(out)
		for _, line := range []string{
			"arg: --web.listen-address=" + want.listen + "\n",
			"Open " + want.url + " for the landing page",
			"Open " + want.url + "static-targets/status ",
			"Open " + want.url + "probe?collector=ecb_reference_rates",
		} {
			if !strings.Contains(text, line) {
				t.Errorf("HOST=%s gives no %q:\n%s", host, line, text)
			}
		}
		if got := strings.Contains(text, "every interface"); got != (want.everyInterface != "") || want.everyInterface != "" && !strings.Contains(text, want.everyInterface+"\n") {
			t.Errorf("HOST=%s: want the note %q:\n%s", host, want.everyInterface, text)
		}
	}
	cmd := exec.Command("sh", script, "run", "ecb", "", "9115")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "HOST is empty") || strings.Contains(string(out), "arg: ") || strings.Contains(string(out), "http://") {
		t.Errorf("an empty HOST gave %v:\n%s", err, out)
	}
	// make run checks HOST before it builds: under -n, make runs the line
	// that calls $(MAKE), so the check runs and nothing is built.
	out, err := runMake(t, t.TempDir(), "-n", "run", "EXAMPLE=ecb", "HOST=")
	if err == nil || !strings.Contains(out, "HOST is empty") || strings.Contains(out, "go build") {
		t.Errorf("make -n run EXAMPLE=ecb HOST= built or succeeded (%v):\n%s", err, out)
	}
	if out, err := runMake(t, t.TempDir(), "-n", "run", "EXAMPLE=ecb", "HOST=::1"); err != nil || !strings.Contains(out, "go build") {
		t.Errorf("make -n run EXAMPLE=ecb HOST=::1 does not build (%v):\n%s", err, out)
	}
}

// A binary that `go build` left in the repository root is ignored by git and
// soon older than bin/'s, so `make build` warns while one is there, naming
// bin/'s as the one to run.
func TestMakeBuildWarnsOfABinaryInTheRoot(t *testing.T) {
	_, recipes := makefileRules(t)
	if !strings.Contains(recipes["build"], "sh tools/run-example.sh check-root-binaries $(APP) $(ALLOCATOR)") {
		t.Errorf("make build does not check the repository root for old binaries:\n%s", recipes["build"])
	}
	gitignore := strings.Split(read(t, ".gitignore"), "\n")
	for _, want := range []string{"/prometheus-universal-exporter", "/pue-allocator"} {
		if !slices.Contains(gitignore, want) {
			t.Errorf(".gitignore does not ignore %s", want)
		}
	}
	script, err := filepath.Abs("tools/run-example.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prometheus-universal-exporter"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script, "check-root-binaries", "prometheus-universal-exporter", "pue-allocator")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "warning: ./prometheus-universal-exporter is an old binary") || !strings.Contains(string(out), "run that one") {
		t.Errorf("no warning of ./prometheus-universal-exporter: %s", out)
	}
	if strings.Contains(string(out), "pue-allocator") {
		t.Errorf("a warning of ./pue-allocator, which is not there: %s", out)
	}
}

// The README's first steps and the development page give make run, and the
// README says the binary is in bin/.
func TestDocsGiveMakeRun(t *testing.T) {
	readme := read(t, "README.md")
	for _, want := range []string{"## Try it", "make run EXAMPLE=open-meteo PORT=9115", "http://127.0.0.1:9115/static-targets/status", "`bin/prometheus-universal-exporter`"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not give %q", want)
		}
	}
	if !strings.Contains(read(t, "docs/DEVELOPMENT.md"), "make run EXAMPLE=open-meteo") {
		t.Error("docs/DEVELOPMENT.md does not list make run")
	}
}

// writeTree writes files, each path from dir, into dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runExampleIn runs tools/run-example.sh with args in the tree dir.
func runExampleIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("tools/run-example.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// exampleCollector is a collector named name of the request type kind, as a
// configuration or a collector file lists it.
func exampleCollector(name, kind string) string {
	return "  - name: " + name + "\n    request:\n      type: " + kind + "\n    transform:\n      type: jq\n    metrics:\n      - name: " + name + "_up\n        expression: .up\n"
}

// A directory example may keep collectors in collector files beside its
// config.yaml, as docs/CONFIGURATION.md shows collector_files: the
// configuration is config.yaml, the static target file the one with
// targets, and a collector file is neither listed as an example nor run on
// its own, but read with its configuration — for the opt-in types it needs
// among the rest. The example tree's tests read the directory the same way,
// and refuse a file of collectors that the configuration does not name, and
// a directory of YAML without a config.yaml, which the script refuses too.
func TestAnExampleDirectoryMayHaveCollectorFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"examples/team/config.yaml":           "# curl 'http://localhost:9115/probe?collector=extra&target=http://127.0.0.1:8080'\ncollector_files: ['collectors-*.yaml']\ncollectors:\n" + exampleCollector("main", "http"),
		"examples/team/collectors-extra.yaml": "collectors:\n" + exampleCollector("extra", "tcp"),
		"examples/team/static-targets.yaml":   "interval: 1m\ntargets:\n  - name: one\n    collector: main\n    target: http://127.0.0.1:8080\n",
		"examples/config.solo.json-test.yaml": "collectors:\n" + exampleCollector("solo", "http"),
		"bin/prometheus-universal-exporter":   "#!/bin/sh\nfor a in \"$@\"; do echo \"arg: $a\"; done\n",
		// opt-in-types reads the opt-in types from the tree.
		"internal/fetch/requesttype_tcp.go": read(t, "internal/fetch/requesttype_tcp.go"),
	})
	if err := os.Chmod(filepath.Join(dir, "bin", "prometheus-universal-exporter"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree, problems := exampleTreeAt(dir)
	if len(problems) != 0 || !slices.Equal(tree.configs, []string{"examples/config.solo.json-test.yaml", "examples/team/config.yaml"}) ||
		!slices.Equal(tree.staticTargets, []string{"examples/team/static-targets.yaml"}) ||
		!slices.Equal(tree.collectorFiles["examples/team/config.yaml"], []string{"examples/team/collectors-extra.yaml"}) {
		t.Errorf("the tree reads as %+v with the problems %v", tree, problems)
	}
	out, err := runExampleIn(t, dir, "list")
	if err != nil || !strings.Contains(out, "  team             examples/team/config.yaml examples/team/static-targets.yaml\n") || !strings.Contains(out, "  solo ") || strings.Contains(out, "collectors-extra") {
		t.Errorf("list gives (%v):\n%s", err, out)
	}
	if out, err := runExampleIn(t, dir, "files", "team"); err != nil || out != "examples/team/config.yaml\nexamples/team/static-targets.yaml\n" {
		t.Errorf("files team gives (%v):\n%s", err, out)
	}
	if out, err := runExampleIn(t, dir, "opt-in-types", "team"); err != nil || strings.TrimSpace(out) != "tcp" {
		t.Errorf("opt-in-types team gives %q (%v), want tcp from the collector file", out, err)
	}
	out, err = runExampleIn(t, dir, "run", "team", "127.0.0.1", "9115")
	if err != nil || !strings.Contains(out, "arg: --config.file=examples/team/config.yaml\n") || !strings.Contains(out, "arg: --static-targets-file=examples/team/static-targets.yaml\n") ||
		!strings.Contains(out, "Open http://127.0.0.1:9115/probe?collector=extra&target=http://127.0.0.1:8080 for a probe.") {
		t.Errorf("run team gives (%v):\n%s", err, out)
	}

	for name, files := range map[string]map[string]string{
		"a file of collectors the configuration does not name": {"examples/team/stray.yaml": "collectors:\n" + exampleCollector("stray", "http")},
		"a directory of YAML without a config.yaml":            {"examples/loose/main.yaml": "collectors:\n" + exampleCollector("loose", "http")},
	} {
		broken := t.TempDir()
		writeTree(t, broken, map[string]string{
			"examples/team/config.yaml":           "collector_files: ['collectors-*.yaml']\n",
			"examples/team/collectors-extra.yaml": "collectors:\n" + exampleCollector("extra", "http"),
		})
		writeTree(t, broken, files)
		if _, problems := exampleTreeAt(broken); len(problems) == 0 {
			t.Errorf("the example tree's tests accept %s", name)
		}
	}
}

// examples() refusing the tree, for a directory of YAML without a
// config.yaml or with two static target files, makes list fail, as it makes
// files and run fail: the list is not printed in part and passed off as
// whole.
func TestRunExampleListFailsWhereTheTreeIsRefused(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no config.yaml": {
			"examples/loose/main.yaml":    "collectors:\n" + exampleCollector("loose", "http"),
			"examples/loose/targets.yaml": "targets: []\n",
		},
		"two static target files": {
			"examples/team/config.yaml": "collectors:\n" + exampleCollector("main", "http"),
			"examples/team/a.yaml":      "targets: []\n",
			"examples/team/b.yaml":      "targets: []\n",
		},
	} {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"examples/config.solo.json-test.yaml": "collectors:\n" + exampleCollector("solo", "http")})
		writeTree(t, dir, files)
		for _, args := range [][]string{{"list"}, {"files", "solo"}} {
			if out, err := runExampleIn(t, dir, args...); err == nil {
				t.Errorf("%s: %v succeeded:\n%s", name, args, out)
			}
		}
	}
}
