package repository

import (
	"strings"
	"testing"
)

// The lines the edits below are made to, as each file writes them: the
// garbage collector's first check, its absence check, and two refusals.
const (
	recipeGCGrep   = "echo \"$$out\" | grep -q 'name: GOGC'"
	workflowGCGrep = "echo \"$out\" | grep -q 'name: GOGC'"

	recipeGCAbsence = "\tif echo \"$$out\" | grep -q GOGC; then \\\n" +
		"\t\techo \"the default values rendered GOGC\" >&2; \\\n" +
		"\t\texit 1; \\\n" +
		"\tfi\n"
	workflowGCAbsence = "          if echo \"$out\" | grep -q GOGC; then\n" +
		"            echo \"the default values rendered GOGC\" >&2\n" +
		"            exit 1\n" +
		"          fi\n"

	recipeGCRefusal = "\t@if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then \\\n" +
		"\t\techo \"helm template accepted a goGC.percent of 0\" >&2; \\\n" +
		"\t\texit 1; \\\n" +
		"\tfi\n"
	recipeGCOffRefusal = "\t@if helm template test charts/prometheus-universal-exporter --set-string goGC.percent=off --set goMemLimit.enabled=false >/dev/null 2>&1; then \\\n" +
		"\t\techo \"helm template accepted goGC.percent off with no Go memory limit in force\" >&2; \\\n" +
		"\t\texit 1; \\\n" +
		"\tfi\n"
	workflowGCOffRefusal = "          if helm template test charts/prometheus-universal-exporter --set-string goGC.percent=off --set goMemLimit.enabled=false >/dev/null 2>&1; then\n" +
		"            echo \"helm template accepted goGC.percent off with no Go memory limit in force\" >&2\n" +
		"            exit 1\n" +
		"          fi\n"

	recipeLint = "\thelm lint charts/prometheus-universal-exporter\n"
)

// the first two lines of the garbage collector's checks: the render kept and
// the first line looked for in it.
var (
	recipeGCFirst   = strings.Join(strings.SplitAfter(recipeGCChecks, "\n")[:2], "")
	workflowGCFirst = strings.Join(strings.SplitAfter(workflowGCChecks, "\n")[:2], "")
)

// helmCheckEdits are changes to the two lists that take no teeth from a
// check: other ways a maintainer would write the same thing, each made to a
// copy of one file or of both. refused is empty for an edit the readers take
// as it is, and otherwise what the finding says that refuses it — the way of
// writing it named, and what to write instead. The first twenty-eight are the
// ones a review made by hand; it found that `|| { ...; exit 1; }` was called a
// check that can never fail, which it is not, and that `printf`, a
// here-string and `[[ ]]` were only said to be a check the list "does not
// make".
var helmCheckEdits = []struct {
	name               string
	makefile, workflow [2]string
	refused            string
}{
	{
		name:     "a comment line in a block of the recipe",
		makefile: [2]string{recipeGCFirst, "\t# keep the render first\\\n" + recipeGCFirst},
	},
	{
		name:     "a comment line of make's before the lint",
		makefile: [2]string{recipeLint, "\t@# lint first\n" + recipeLint},
	},
	{
		name:     "a comment in a step",
		workflow: [2]string{workflowGCFirst, "          # keep the render first\n" + workflowGCFirst},
	},
	{
		name:     "a line of progress in a step",
		workflow: [2]string{workflowGCFirst, "          echo \"rendering goGC\"\n" + workflowGCFirst},
	},
	{
		name:     "a line of progress as a recipe line",
		makefile: [2]string{recipeLint, "\t@echo \"linting the chart\"\n" + recipeLint},
	},
	{
		name:     "a line of progress in a block of the recipe",
		makefile: [2]string{recipeGCFirst, "\techo \"rendering goGC\"; \\\n" + recipeGCFirst},
	},
	{
		name:     "two refusals in the other order, in both files",
		makefile: [2]string{recipeGCOffRefusal + recipeGCRefusal, recipeGCRefusal + recipeGCOffRefusal},
		workflow: [2]string{workflowGCOffRefusal + workflowGCRefusal, workflowGCRefusal + workflowGCOffRefusal},
	},
	{
		name:     "grep -c for grep -q, in both files",
		makefile: [2]string{recipeGCGrep, "echo \"$$out\" | grep -c 'name: GOGC' >/dev/null"},
		workflow: [2]string{workflowGCGrep, "echo \"$out\" | grep -c 'name: GOGC' >/dev/null"},
		refused:  "a render is read by `grep -c name: GOGC`, which is no check the reader of the helm checks knows: `grep -q` with a pattern, `python3 -c` or tools/check-manifests.py; write it as: echo \"$out\" | grep -q -- 'name: GOGC'",
	},
	{
		name:     "a helm command continued onto a second line",
		workflow: [2]string{"out=\"$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\"", "out=\"$(helm template test charts/prometheus-universal-exporter \\\n            --set goGC.percent=400)\""},
	},
	{
		name:     "an absence looked for with [[ ]], in both files",
		makefile: [2]string{recipeGCAbsence, strings.Replace(recipeGCAbsence, "if echo \"$$out\" | grep -q GOGC; then", "if [[ \"$$out\" == *GOGC* ]]; then", 1)},
		workflow: [2]string{workflowGCAbsence, strings.Replace(workflowGCAbsence, "if echo \"$out\" | grep -q GOGC; then", "if [[ \"$out\" == *GOGC* ]]; then", 1)},
		refused:  "a kept render is compared with `[[`, which holds the whole render against a pattern of the shell's rather than looking for a line, and is no check the reader of the helm checks knows; write it as: echo \"$out\" | grep -q -- 'GOGC'",
	},
	{
		name: "a refusal made in a loop over two values, in both files",
		makefile: [2]string{recipeGCRefusal, "\t@for percent in 0 00; do \\\n" +
			"\t\tif helm template test charts/prometheus-universal-exporter --set goGC.percent=$$percent >/dev/null 2>&1; then \\\n" +
			"\t\t\techo \"helm template accepted a goGC.percent of $$percent\" >&2; \\\n" +
			"\t\t\texit 1; \\\n" +
			"\t\tfi; \\\n" +
			"\tdone\n"},
		workflow: [2]string{workflowGCRefusal, "          for percent in 0 00; do\n" +
			"            if helm template test charts/prometheus-universal-exporter --set goGC.percent=$percent >/dev/null 2>&1; then\n" +
			"              echo \"helm template accepted a goGC.percent of $percent\" >&2\n" +
			"              exit 1\n" +
			"            fi\n" +
			"          done\n"},
	},
	{
		name:     "grep -qF for grep -q, in both files",
		makefile: [2]string{recipeGCGrep, "echo \"$$out\" | grep -qF 'name: GOGC'"},
		workflow: [2]string{workflowGCGrep, "echo \"$out\" | grep -qF 'name: GOGC'"},
		refused:  "a render is read by `grep -qF name: GOGC`, which is no check the reader of the helm checks knows: `grep -q` with a pattern, `python3 -c` or tools/check-manifests.py; write it as: echo \"$out\" | grep -q -- 'name: GOGC'",
	},
	{
		name:     "grep -q -e for grep -q, in both files",
		makefile: [2]string{recipeGCGrep, "echo \"$$out\" | grep -q -e 'name: GOGC'"},
		workflow: [2]string{workflowGCGrep, "echo \"$out\" | grep -q -e 'name: GOGC'"},
		refused:  "a render is read by `grep -q -e name: GOGC`, which is no check the reader of the helm checks knows: `grep -q` with a pattern, `python3 -c` or tools/check-manifests.py; write it as: echo \"$out\" | grep -q -- 'name: GOGC'",
	},
	{
		name:     "set -euo pipefail in a step",
		workflow: [2]string{"          set -eu\n          # goGC.percent renders", "          set -euo pipefail\n          # goGC.percent renders"},
	},
	{
		name:     "printf for echo, in both files",
		makefile: [2]string{recipeGCGrep, "printf '%s\\n' \"$$out\" | grep -q 'name: GOGC'"},
		workflow: [2]string{workflowGCGrep, "printf '%s\\n' \"$out\" | grep -q 'name: GOGC'"},
	},
	{
		name:     "exit 2 for exit 1",
		workflow: [2]string{"echo \"the default values rendered GOGC\" >&2\n            exit 1\n", "echo \"the default values rendered GOGC\" >&2\n            exit 2\n"},
	},
	{
		name:     "a message and an exit 1 after the || of a grep, in both files",
		makefile: [2]string{recipeGCGrep + "; \\\n", recipeGCGrep + " || { echo \"goGC.percent did not render GOGC\" >&2; exit 1; }; \\\n"},
		workflow: [2]string{workflowGCGrep + "\n", workflowGCGrep + " || { echo \"goGC.percent did not render GOGC\" >&2; exit 1; }\n"},
	},
	{
		name:     "a here-string for the echo, in both files",
		makefile: [2]string{recipeGCGrep, "grep -q 'name: GOGC' <<<\"$$out\""},
		workflow: [2]string{workflowGCGrep, "grep -q 'name: GOGC' <<<\"$out\""},
		refused:  "a here-string (`<<<`) is not a way to read a render that the reader of the helm checks knows, and the sh that make runs a recipe in does not have it; write it as: echo \"$out\" | grep -q -- 'name: GOGC'",
	},
	{
		name:     "a blank line between two checks of a step",
		workflow: [2]string{workflowGCFirst, strings.Replace(workflowGCFirst, "\n", "\n\n", 1)},
	},
	{
		name:     "a message worded otherwise, in both files",
		makefile: [2]string{"echo \"the default values rendered GOGC\" >&2", "echo \"GOGC was rendered by the default values\" >&2"},
		workflow: [2]string{"echo \"the default values rendered GOGC\" >&2", "echo \"GOGC was rendered by the default values\" >&2"},
	},
	{
		name: "a render written to a file and the file read",
		workflow: [2]string{workflowGCFirst, "          helm template test charts/prometheus-universal-exporter --set goGC.percent=400 > \"$RUNNER_TEMP/gc.yaml\"\n" +
			"          grep -q 'name: GOGC' \"$RUNNER_TEMP/gc.yaml\"\n"},
		refused: "this `grep` reads a file or what the step is given, not a kept render, so the reader of the helm checks cannot tell which render it looks at and takes it for no check; keep the render (out=\"$(helm ...)\") and write it as: echo \"$out\" | grep -q -- 'name: GOGC'",
	},
	{
		name:     "a render kept without the quotes",
		workflow: [2]string{"out=\"$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\"", "out=$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)"},
	},
	{
		// The edit as the review made it: the third line still reads $out.
		name:     "a kept render's variable renamed in two lines of three",
		workflow: [2]string{workflowGCFirst, strings.ReplaceAll(strings.ReplaceAll(workflowGCFirst, "out=", "gc="), "$out", "$gc")},
		refused:  "`$out` is read, and no render was kept in it by a name=\"$(helm ...)\" before:\n\techo \"$out\" | grep -q 'value: \"400\"'",
	},
	{
		name:     "the chart's path in a variable",
		workflow: [2]string{workflowGCFirst, "          chart=charts/prometheus-universal-exporter\n" + strings.Replace(workflowGCFirst, "test charts/prometheus-universal-exporter", "test \"$chart\"", 1)},
	},
	{
		name: "a refusal written behind ! with its exit in the else",
		workflow: [2]string{workflowGCRefusal, "          if ! helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then\n" +
			"            :\n" +
			"          else\n" +
			"            echo \"helm template accepted a goGC.percent of 0\" >&2\n" +
			"            exit 1\n" +
			"          fi\n"},
		refused: "the reader of the helm checks does not understand `!`; write a render that must succeed as a command of its own, and one that must be refused as `if helm ...; then ...; exit 1; fi`",
	},
	{
		name:     "a test that the kept render is not empty",
		workflow: [2]string{workflowGCFirst, workflowGCFirst + "          test -n \"$out\"\n"},
	},
	{
		name:     "--debug on a refusal the Go tests make too, in both files",
		makefile: [2]string{"--set goGC.percent=0 >/dev/null 2>&1; then", "--set goGC.percent=0 --debug >/dev/null 2>&1; then"},
		workflow: [2]string{"--set goGC.percent=0 >/dev/null 2>&1; then", "--set goGC.percent=0 --debug >/dev/null 2>&1; then"},
		refused: "both lists have to make it too, with this command line and this pattern (chartCasesMissing in test/repository/helmtest_test.go lists them; a case changed on purpose is changed there and in both lists):\n" +
			"fails: helm template test charts/prometheus-universal-exporter --set goGC.percent=0\n" +
			"the list makes this one, which is another check to the comparison:\n" +
			"fails: helm template test charts/prometheus-universal-exporter --set goGC.percent=0 --debug",
	},
	{
		name:     "&>/dev/null for >/dev/null 2>&1 in a step",
		workflow: [2]string{"--set goGC.percent=0 >/dev/null 2>&1; then", "--set goGC.percent=0 &>/dev/null; then"},
	},

	// The ways the readers take, in one file only: what one list writes with
	// printf, or hands to an exit, is the check the other writes plainly.
	{
		name:     "printf for echo in the recipe alone",
		makefile: [2]string{recipeGCGrep, "printf '%s\\n' \"$$out\" | grep -q 'name: GOGC'"},
	},
	{
		name:     "printf for echo in the workflow alone",
		workflow: [2]string{workflowGCAbsence, strings.Replace(workflowGCAbsence, "if echo \"$out\" |", "if printf '%s\\n' \"$out\" |", 1)},
	},
	{
		name:     "a message and an exit 1 after the || of a grep in the recipe alone",
		makefile: [2]string{recipeGCGrep + "; \\\n", recipeGCGrep + " || { echo \"goGC.percent did not render GOGC\" >&2; exit 1; }; \\\n"},
	},
	{
		name: "a message and an exit 3 on lines of their own after the || of a kept render",
		workflow: [2]string{"          out=\"$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\"\n",
			"          out=\"$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\" || {\n" +
				"            echo \"goGC.percent=400 did not render\" >&2\n" +
				"            exit 3\n" +
				"          }\n"},
	},
	{
		name:     "an exit 1 after the || of a lint that is a recipe line of its own",
		makefile: [2]string{recipeLint, "\thelm lint charts/prometheus-universal-exporter || exit 1\n"},
	},
	{
		name:     "a message and an exit 1 after the || of a render piped into the manifest check",
		makefile: [2]string{" | python3 tools/check-manifests.py\n", " | python3 tools/check-manifests.py || { echo \"a manifest is not its own document\" >&2; exit 1; }\n"},
	},
	{
		name:     "the kept render read as ${out}, in both files",
		makefile: [2]string{recipeGCGrep, "echo \"$${out}\" | grep -q 'name: GOGC'"},
		workflow: [2]string{workflowGCGrep, "echo \"${out}\" | grep -q 'name: GOGC'"},
	},
	{
		name:     "a kept render's variable renamed in every line that reads it",
		workflow: [2]string{workflowGCChecks, strings.ReplaceAll(strings.ReplaceAll(workflowGCChecks, "out=", "gc="), "$out", "$gc")},
	},
	{
		name:     "an if of the step's own, with no check and no exit in it",
		workflow: [2]string{"          helm package charts/prometheus-universal-exporter --destination dist\n", "          if [ -d dist ]; then\n            rm -rf dist\n          fi\n          helm package charts/prometheus-universal-exporter --destination dist\n"},
	},
	{
		name:     "a kept render printed as printf's format",
		workflow: [2]string{workflowGCGrep, "printf \"$out\" | grep -q 'name: GOGC'"},
		refused:  "a kept render is printed with `printf $out`, which the reader of the helm checks does not know to hand the reader the render as it is; write it as: echo \"$out\" | grep -q -- 'name: GOGC'",
	},
}

// An edit that leaves every check its teeth is either taken — nothing is
// reported, in the file it is made to or in the other — or refused by a
// finding that names the way it is written and says what to write instead.
// The reader called `grep -q X || { echo "..." >&2; exit 1; }` a check that
// can never fail, though the failure ends the shell right there, and of a
// render read through `printf`, a here-string or `[[ ]]` it said only that
// the list "does not make this check", which names nothing a maintainer
// could change. Each edit is made to a copy of the files' text, in memory.
func TestAHarmlessEditOfAHelmCheckIsTakenOrToldHowToBeWritten(t *testing.T) {
	makefile, workflow := readHelmLists(t)
	for _, edit := range helmCheckEdits {
		t.Run(edit.name, func(t *testing.T) {
			changedMakefile, changedWorkflow := makefile, workflow
			for file, change := range map[*string][2]string{&changedMakefile: edit.makefile, &changedWorkflow: edit.workflow} {
				if change[0] == "" {
					continue
				}
				if !strings.Contains(*file, change[0]) {
					t.Fatalf("a file no longer holds the text this edit is made to, so the test changes nothing:\n%s", change[0])
				}
				*file = strings.Replace(*file, change[0], change[1], 1)
			}
			findings := helmCheckFindings(t, changedMakefile, changedWorkflow)
			all := strings.Join(findings, "\n")
			if strings.Contains(all, "can never fail") {
				t.Errorf("a check that can fail is said not to:\n%s", all)
			}
			switch {
			case edit.refused == "" && len(findings) > 0:
				t.Errorf("the edit takes nothing from a check and is refused:\n%s", all)
			case edit.refused != "" && !strings.Contains(all, edit.refused):
				t.Errorf("the edit is not refused with a finding that says\n%s\n\n%d findings:\n%s", edit.refused, len(findings), all)
			}
		})
	}
}

// The two readers of the helm checks have to agree on what a check is: one
// lists what a script checks, for the comparison of the two files, and the
// other says whether each check can fail. A way of writing a check that only
// one of them took would be a check the comparison misses or one nothing
// vouches for. So each way below is read by both: the same list as the plain
// script's, with as many checks weighed and nothing reported. A render kept
// in `${out}` was a check to one reader and none to the other.
func TestBothReadersOfTheHelmChecksTakeTheSameWaysOfWritingOne(t *testing.T) {
	const plain = `set -eu
out="$(helm template test charts/x --set a=1)"
echo "$out" | grep -q 'kind: Deployment'
if echo "$out" | grep -q gone; then
  echo "helm template rendered what it should not" >&2
  exit 1
fi
helm lint charts/x
`
	want := sortedKeys(helmChecks(plain, nil))
	if len(want) != 4 {
		t.Fatalf("the plain script was read as %d checks, want 4:\n%s", len(want), strings.Join(want, "\n"))
	}
	replace := strings.NewReplacer
	for name, script := range map[string]string{
		"as it is":                  plain,
		"printf":                    replace(`echo "$out" |`, `printf '%s\n' "$out" |`).Replace(plain),
		"the variable in braces":    replace(`"$out"`, `"${out}"`).Replace(plain),
		"an exit after ||":          replace("Deployment'\n", "Deployment' || exit 1\n", "lint charts/x\n", "lint charts/x || exit 2\n").Replace(plain),
		"a message and an exit":     replace("Deployment'\n", "Deployment' || { echo \"no Deployment\" >&2; exit 1; }\n").Replace(plain),
		"the exit on its own line":  replace("--set a=1)\"\n", "--set a=1)\" || {\n  echo \"no render\" >&2\n  exit 1\n}\n").Replace(plain),
		"the chart in a variable":   "chart=charts/x\n" + replace("charts/x", `"$chart"`).Replace(plain),
		"no set -e, an exit each":   replace("set -eu\n", "", "--set a=1)\"\n", "--set a=1)\" || exit 1\n", "Deployment'\n", "Deployment' || exit 1\n").Replace(plain),
		"a loop over one value":     replace("helm lint charts/x\n", "for chart in charts/x; do\n  helm lint \"$chart\"\ndone\n").Replace(plain),
		"a clean-up trap":           replace("set -eu\n", "set -eu\ndir=$(mktemp -d)\ntrap 'rm -rf \"$dir\"' EXIT\n").Replace(plain),
		"an if with no check in it": replace("helm lint", "if [ -d dist ]; then\n  rm -rf dist\nfi\nhelm lint").Replace(plain),
	} {
		if got := sortedKeys(helmChecks(script, nil)); !equalStrings(got, want) {
			t.Errorf("%s: the script was read as checking\n%s\n\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		findings, weighed := ineffectiveHelmChecks(helmBlock{file: "ci.yml", where: "the step", line: 1, script: script})
		if len(findings) > 0 || weighed != len(want) {
			t.Errorf("%s: %d checks were weighed and these reported, want %d and none:\n%s", name, weighed, len(want), strings.Join(findings, "\n"))
		}
	}

	// A way neither takes is no check to the comparison, and the other
	// reader says so by name with what to write, at the line it is on.
	for name, tc := range map[string]struct{ script, finding string }{
		"a here-string": {
			replace(`echo "$out" | grep -q 'kind: Deployment'`, `grep -q 'kind: Deployment' <<<"$out"`).Replace(plain),
			"ci.yml:3: the step: a here-string (`<<<`) is not a way to read a render that the reader of the helm checks knows, and the sh that make runs a recipe in does not have it; write it as: echo \"$out\" | grep -q -- 'kind: Deployment':\n\tgrep -q 'kind: Deployment' <<<\"$out\"",
		},
		"a comparison": {
			replace(`if echo "$out" | grep -q gone; then`, `if [[ "$out" == *gone* ]]; then`).Replace(plain),
			"ci.yml:4: the step: a kept render is compared with `[[`, which holds the whole render against a pattern of the shell's rather than looking for a line, and is no check the reader of the helm checks knows; write it as: echo \"$out\" | grep -q -- 'gone':\n\tif [[ \"$out\" == *gone* ]]; then",
		},
		"a comparison by test": {
			replace(`if echo "$out" | grep -q gone; then`, `if test "$out" = gone; then`).Replace(plain),
			"ci.yml:4: the step: a kept render is compared with `test`, which holds the whole render against a pattern of the shell's rather than looking for a line, and is no check the reader of the helm checks knows; write it as: echo \"$out\" | grep -q -- 'gone':\n\tif test \"$out\" = gone; then",
		},
		"a file read": {
			replace(`echo "$out" | grep -q 'kind: Deployment'`, `echo "$out" > render.yaml; grep -q 'kind: Deployment' render.yaml`).Replace(plain),
			"ci.yml:3: the step: this `grep` reads a file or what the step is given, not a kept render, so the reader of the helm checks cannot tell which render it looks at and takes it for no check; keep the render (out=\"$(helm ...)\") and write it as: echo \"$out\" | grep -q -- 'kind: Deployment':\n\techo \"$out\" > render.yaml; grep -q 'kind: Deployment' render.yaml",
		},
		"echo -n": {
			replace(`echo "$out" | grep -q 'kind: Deployment'`, `echo -n "$out" | grep -q 'kind: Deployment'`).Replace(plain),
			"ci.yml:3: the step: a kept render is printed with `echo -n $out`, which the reader of the helm checks does not know to hand the reader the render as it is; write it as: echo \"$out\" | grep -q -- 'kind: Deployment':\n\techo -n \"$out\" | grep -q 'kind: Deployment'",
		},
	} {
		if got := helmChecks(tc.script, nil); len(got) != len(want)-1 {
			t.Errorf("%s: the script was read as making %d checks, want one fewer than the %d of the plain script:\n%s", name, len(got), len(want), strings.Join(sortedKeys(got), "\n"))
		}
		findings, _ := ineffectiveHelmChecks(helmBlock{file: "ci.yml", where: "the step", line: 1, script: tc.script})
		if len(findings) != 1 || findings[0] != tc.finding {
			t.Errorf("%s: the findings are\n%s\n\nwant the one\n%s", name, strings.Join(findings, "\n"), tc.finding)
		}
	}
}
