package repository

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The two lists of helm checks are compared check by check (helmtest_test.go),
// which says that both name the same renders and the same rejected values.
// It does not say that a check can fail. A grep followed by `|| true`, a
// rejection whose `exit 1` is gone, a recipe line that lost its `set -e`: the
// reader lists each as the check it was, in both files or in one, and the
// comparison is content. So the reader also asks of every check it lists
// whether a failure of it fails the run, block by block, as the shell and
// make and GitHub would have it.

// finding words a problem with the command at pos of the block's script.
func (block helmBlock) finding(pos int, problem string) string {
	index := strings.Count(block.script[:pos], "\n")
	return helmFinding(block.file, block.line+index, block.where, problem, strings.Split(block.script, "\n")[index])
}

// shellWords gives each token of a command its text: an operator as it is, a
// word with its variables left as they are written.
func shellWords(tokens []shellToken) []string {
	words := make([]string, len(tokens))
	for i, token := range tokens {
		words[i] = token.op
		if token.op == "" {
			words[i] = token.expand(nil)
		}
	}
	return words
}

// pipelineStages splits a command at the pipes that are its own, not those
// inside a command substitution.
func pipelineStages(tokens []shellToken) [][]shellToken {
	stages := [][]shellToken{nil}
	depth := 0
	for _, token := range tokens {
		switch token.op {
		case "$(":
			depth++
		case ")":
			depth--
		}
		if token.op == "|" && depth == 0 {
			stages = append(stages, nil)
			continue
		}
		stages[len(stages)-1] = append(stages[len(stages)-1], token)
	}
	return stages
}

var nonZero = regexp.MustCompile(`^[1-9][0-9]*$`)

// failsTheShell reports whether a command ends the shell with a failure,
// whatever came before it: `exit` with a status that is not zero.
func failsTheShell(words []string) bool {
	return len(words) == 2 && words[0] == "exit" && nonZero.MatchString(words[1])
}

// isGrepQuiet reports whether a command is `grep -q`, with or without `--`,
// for a pattern that is not empty: the one grep the checks use, which fails
// when no line matches and so fails on a render of nothing too.
func isGrepQuiet(words []string) bool {
	if len(words) == 4 && words[2] == "--" {
		words = append(words[:2:2], words[3])
	}
	return len(words) == 3 && words[0] == "grep" && words[1] == "-q" && words[2] != ""
}

// isManifestCheck reports whether a command is the manifest check, which
// refuses a render of nothing.
func isManifestCheck(words []string) bool {
	return len(words) == 2 && words[0] == "python3" && words[1] == "tools/check-manifests.py"
}

// openConstruct is an `if` or a `for` the walk is inside of.
type openConstruct struct {
	keyword string
	pos     int
	// must says what the condition of an `if` checks, when it is a helm
	// check: a render that must be refused or a text that must be absent.
	must string
	// branch is the part of an `if` being read: empty for its condition,
	// then or else.
	branch string
	// exits says whether the then-branch ends the shell with a failure, and
	// decided that the first `exit` in it has been read.
	exits, decided bool
}

// ineffectiveHelmChecks reads one block as its shell would and returns a
// finding for every helm check in it whose failure would not fail the block:
//
//   - a render that must succeed, or a line that must be in a render, written
//     as a plain command, where nothing ends the shell when it fails — no
//     `set -e` is in force and it is not the block's last command — or where
//     `||`, `&&` or `&` after it takes its result away;
//   - a render that must be refused, or a text that must be absent, written as
//     the condition of an `if`, whose then-branch does not `exit 1`;
//   - a render read through a pipe that hides helm failing: piped into a
//     condition, or into a reader that does not fail on nothing, or kept from
//     more than the one helm command;
//   - a block whose failure is ignored where it is run.
//
// It knows the shell these checks are written in and reports what it does not
// know rather than passing it. weighed is how many checks it found to ask the
// question of, a loop's once.
func ineffectiveHelmChecks(block helmBlock) (findings []string, weighed int) {
	report := func(pos int, problem string) {
		findings = append(findings, block.finding(pos, problem))
	}
	commands := shellCommands(block.script)
	var open []openConstruct
	errexit := block.errexit
	kept := map[string]bool{}
	unprotected, expectThen := false, false
	previousEnd := ";"

	// plain is a check written as a command of its own, at pos.
	plain := func(index, pos int) {
		weighed++
		command := commands[index]
		switch command.end {
		case "||":
			if index+1 < len(commands) && failsTheShell(shellWords(commands[index+1].tokens)) {
				return
			}
			report(pos, "the `||` after this check takes its failure away, so it can never fail")
			return
		case "&&":
			report(pos, "this check is followed by `&&`, and a command before `&&` does not end the shell when it fails, even under `set -e`; write it on a line of its own")
			return
		case "&":
			report(pos, "this check runs in the background, where its failure is lost")
			return
		}
		last := index == len(commands)-1 && len(open) == 0
		if !errexit && !last && !unprotected {
			// Once for a block: every check after this one is as lost.
			unprotected = true
			report(pos, "a failure of this check is lost: no `set -e` is in force here, so the shell carries on, and it is not the last command, whose result is the block's")
		}
	}

	for index, command := range commands {
		tokens := command.tokens
		words := shellWords(tokens)
		pos := tokens[0].pos
		if expectThen && words[0] != "then" {
			report(pos, "the condition of the `if` is followed by another command, whose result is then the condition's; an `if` of these checks has the one check for its condition")
		}
		condition := false
		expectThen = false
	keywords:
		for len(tokens) > 0 && tokens[0].op == "" {
			top := len(open) - 1
			word := words[0]
			if strings.HasPrefix(word, "(") {
				word = "("
			}
			switch word {
			case "if":
				open = append(open, openConstruct{keyword: "if", pos: pos})
				condition = true
			case "then", "else":
				if top < 0 || open[top].keyword != "if" {
					report(pos, "`"+word+"` belongs to no `if` the reader of the helm checks has seen")
					break keywords
				}
				open[top].branch = word
			case "for":
				open = append(open, openConstruct{keyword: "for", pos: pos})
				tokens = nil
				break keywords
			case "do":
			case "fi", "done":
				keyword := map[string]string{"fi": "if", "done": "for"}[word]
				if top < 0 || open[top].keyword != keyword {
					report(pos, "`"+word+"` closes no `"+keyword+"` the reader of the helm checks has seen")
					break keywords
				}
				closed := open[top]
				open = open[:top]
				if closed.must != "" && !closed.exits {
					report(closed.pos, closed.must+", and nothing fails when it is not: the commands after `then` do not end with `exit 1`")
				}
				switch {
				case len(tokens) > 1 || command.end == "&":
					// `fi | cat` runs the whole `if` in a subshell, which is
					// all an `exit 1` inside it then leaves.
					report(pos, "what follows `"+word+"` runs the commands before it in a shell of their own, whose `exit 1` fails nothing")
				case command.end != ";":
					report(pos, "the `"+command.end+"` after `"+word+"` has the shell carry on when a command before it fails, under `set -e` too")
				}
				tokens = nil
				break keywords
			case "!", "elif", "while", "until", "case", "eval", "{", "(":
				report(pos, "the reader of the helm checks does not understand `"+word+"`; write a render that must succeed as a command of its own, and one that must be refused as `if helm ...; then ...; exit 1; fi`")
				tokens = nil
				break keywords
			default:
				break keywords
			}
			tokens, words = tokens[1:], words[1:]
		}
		top := len(open) - 1
		end := previousEnd
		previousEnd = command.end
		if condition {
			expectThen = true
			if command.end != ";" {
				report(pos, "the condition of the `if` goes on after `"+command.end+"`, so it is not the check's result alone; an `if` of these checks has the one check for its condition")
			}
		}
		if len(tokens) == 0 {
			continue
		}

		switch words[0] {
		case "set":
			// set -e, -eu and -o errexit turn it on, and the same with a
			// plus sign off.
			for i, flag := range words[1:] {
				if len(flag) < 2 || !strings.ContainsRune("-+", rune(flag[0])) || strings.HasPrefix(flag, "--") {
					continue
				}
				named := flag[1:] == "o" && i+2 < len(words) && words[i+2] == "errexit"
				if named || (flag[1:] != "o" && strings.Contains(flag[1:], "e")) {
					errexit = flag[0] == '-'
				}
			}
			continue
		case "exit":
			// The first exit of a then-branch decides what the branch is
			// worth, and only one that runs whatever the command before it
			// returned.
			if top >= 0 && open[top].branch == "then" && !open[top].decided {
				open[top].decided = true
				open[top].exits = failsTheShell(words) && end == ";"
			}
			continue
		}

		// name="$(helm ...)" keeps a render for the lines after it. The
		// assignment fails when helm does only if helm is all there is in
		// the substitution.
		if len(tokens) >= 3 && tokens[1].op == "$(" && tokens[len(tokens)-1].op == ")" && strings.HasSuffix(words[0], "=") {
			inner := tokens[2 : len(tokens)-1]
			if len(inner) == 0 || inner[0].op != "" || words[2] != "helm" {
				continue
			}
			for _, token := range inner {
				if token.op != "" {
					report(pos, "the render is kept from more than the one helm command (`"+token.op+"`), so helm failing is not the assignment failing")
					break
				}
			}
			if condition {
				report(pos, "a kept render is the condition of an `if`; keep it in a command of its own, which fails when helm does")
				continue
			}
			kept[strings.TrimSuffix(words[0], "=")] = true
			plain(index, pos)
			continue
		}
		// helm anywhere else in a substitution is a render nothing answers for.
		for i, token := range tokens {
			if token.op == "$(" && i+1 < len(tokens) && tokens[i+1].op == "" && words[i+1] == "helm" {
				report(pos, "helm runs in a command substitution that is not kept on its own, so its failure is lost; write name=\"$(helm ...)\" and read \"$name\"")
			}
		}

		stages := pipelineStages(tokens)
		first := shellWords(stages[0])
		var reader []string
		if len(stages) > 1 {
			reader = shellWords(stages[1])
		}
		fromHelm := len(first) > 0 && first[0] == "helm"
		fromKept := len(first) == 2 && first[0] == "echo" && strings.HasPrefix(first[1], "$")
		switch {
		case !fromHelm && !fromKept, fromKept && len(stages) == 1:
			continue
		case len(stages) > 2:
			report(pos, "a pipeline answers for its last command only, and this one has more than two; keep the render first and read it with one command")
		case fromHelm && len(stages) == 1 && condition:
			open[top].must = "this render must be refused"
			weighed++
		case fromHelm && len(stages) == 1:
			plain(index, pos)
		case fromHelm && condition:
			report(pos, "a render is piped into a condition, which a render that failed passes as an absence; keep it first (out=\"$(helm ...)\") and read \"$out\"")
		case fromHelm && !isGrepQuiet(reader) && !isManifestCheck(reader):
			report(pos, "helm is piped into `"+strings.Join(reader, " ")+"`, which does not fail when helm does; keep the render first (out=\"$(helm ...)\") and read \"$out\"")
		case fromHelm:
			plain(index, pos)
		case !kept[strings.Trim(first[1], "${}")]:
			if len(reader) > 0 && (reader[0] == "grep" || reader[0] == "python3") {
				report(pos, "`"+first[1]+"` is read, and no render was kept in it by a name=\"$(helm ...)\" before")
			}
		case condition && isGrepQuiet(reader):
			open[top].must = "this text must be absent from the render"
			weighed++
		case condition:
			report(pos, "the condition reads a render with `"+strings.Join(reader, " ")+"`; a text that must be absent is looked for with `grep -q` and a pattern")
		case isGrepQuiet(reader), isManifestCheck(reader), len(reader) == 3 && reader[0] == "python3" && reader[1] == "-c":
			plain(index, pos)
		default:
			report(pos, "a render is read by `"+strings.Join(reader, " ")+"`, which is no check the reader of the helm checks knows: `grep -q` with a pattern, `python3 -c` or tools/check-manifests.py")
		}
	}
	for _, construct := range open {
		report(construct.pos, "this `"+construct.keyword+"` is not closed where the reader of the helm checks looks for its end")
	}
	if weighed > 0 {
		findings = append(findings, block.lost...)
	}
	return findings, weighed
}

// makeDefaultsFindings names what in a Makefile changes how every recipe is
// run, which the reader of the recipe takes to be make's own way: a shell for
// each line, without -e, and a failing line failing the target.
func makeDefaultsFindings(makefile string) []string {
	var findings []string
	for i, line := range strings.Split(makefile, "\n") {
		for target, effect := range map[string]string{
			".IGNORE":     "has make ignore every recipe line that fails",
			".ONESHELL":   "runs a recipe in one shell, where only its last command's result counts",
			".SHELLFLAGS": "changes the shell every recipe line runs in, which the reader of the helm checks takes to be `sh -c`",
		} {
			if regexp.MustCompile(`^` + regexp.QuoteMeta(target) + `\s*(:|\+?=|:=|\?=)`).MatchString(line) {
				findings = append(findings, fmt.Sprintf("Makefile:%d: %s %s:\n\t%s", i+1, target, effect, line))
			}
		}
	}
	return findings
}

// helmCheckFindings returns all that is wrong with the two lists of helm
// checks, a Makefile and a workflow as text: a check whose failure fails
// nothing, a check one list makes and the other does not, and a chart case of
// the Go tests that a list lacks.
func helmCheckFindings(t *testing.T, makefile, workflow string) []string {
	t.Helper()
	recipe := makeRecipeBlocks(makefile, "helm-test")
	if len(recipe) == 0 {
		return []string{"the Makefile has no helm-test target, or the target has no recipe"}
	}
	steps := ciChartBlocks(t, workflow)
	if len(steps) == 0 {
		return []string{"ci.yml has no step that runs helm"}
	}
	findings := makeDefaultsFindings(makefile)
	for _, block := range append(recipe, steps...) {
		ineffective, _ := ineffectiveHelmChecks(block)
		findings = append(findings, ineffective...)
	}
	local, ci := helmCheckLists(t, makefile, workflow)
	findings = append(findings, helmListDifferences(local, ci)...)
	findings = append(findings, chartCasesMissing(local, ci)...)
	// What is wrong with the job is found at each of its steps, and said once.
	seen := map[string]bool{}
	return slices.DeleteFunc(findings, func(finding string) bool {
		again := seen[finding]
		seen[finding] = true
		return again
	})
}

// Every helm check of `make helm-test` and of the chart steps of ci.yml fails
// its run when what it checks does not hold. The comparison of the two lists
// took a check for what it names, so a grep followed by `|| true`, a
// rejection without its `exit 1` and a recipe line without `set -e` passed as
// the checks they had been.
func TestEveryHelmCheckOfMakeAndCIFailsWhenItShould(t *testing.T) {
	makefile, workflow := readHelmLists(t)
	recipe, steps := makeRecipeBlocks(makefile, "helm-test"), ciChartBlocks(t, workflow)
	for _, finding := range makeDefaultsFindings(makefile) {
		t.Error(finding)
	}
	// A reader that lost its way finds nothing to object to, so what it
	// weighed is counted: each plain render and each line looked for, and a
	// rejection once where a loop makes it for many values.
	for name, list := range map[string]struct {
		blocks []helmBlock
		least  int
	}{"the helm-test recipe": {recipe, 86}, "the chart steps of ci.yml": {steps, 88}} {
		weighed := 0
		for _, block := range list.blocks {
			findings, checks := ineffectiveHelmChecks(block)
			weighed += checks
			for _, finding := range findings {
				t.Error(finding)
			}
		}
		if weighed < list.least {
			t.Errorf("only %d checks were read from %s, fewer than the %d it has had; the reader no longer understands it", weighed, name, list.least)
		}
	}
	// GitHub starts a step's shell with -e, and make starts a recipe line's
	// without, which is what the two readers hand on.
	for _, block := range steps {
		if !block.errexit {
			t.Errorf("%s:%d: %s was read as running in a shell without -e", block.file, block.line, block.where)
		}
	}
	for _, block := range recipe {
		if block.errexit {
			t.Errorf("%s:%d: a line of the recipe was read as running in a shell with -e", block.file, block.line)
		}
	}
}

// What a check is worth depends on how its block is run, which the two
// readers have to hand on as make and GitHub have it: a recipe line with the
// lines continued onto it is one shell, started without -e, at the line of the
// Makefile it is written on, with make's prefixes off it; a step's `run` is
// one shell, started with -e unless a `shell:` — the step's, the job's or the
// workflow's — is a command without the flag.
func TestTheHelmCheckReaderKnowsHowEachBlockIsRun(t *testing.T) {
	makefile := "first:\n\ttrue\n\nhelm-test: helm-version\n" +
		"\thelm lint charts/x\n" +
		"\t@# a comment\n" +
		"\t@set -eu; \\\n" +
		"\tout=\"$$(helm template test charts/x)\"; \\\n" +
		"\techo \"$$out\" | grep -q 'kind: Deployment'\n" +
		"\t-@helm template test charts/x --set a=b\n" +
		"\nci: helm-test\n"
	recipe := makeRecipeBlocks(makefile, "helm-test")
	if len(recipe) != 4 {
		t.Fatalf("%d blocks were read from the recipe, want 4: %+v", len(recipe), recipe)
	}
	for i, want := range []helmBlock{
		{line: 5, script: "helm lint charts/x\n"},
		{line: 6, script: "# a comment\n"},
		{line: 7, script: "set -eu; \\\nout=\"$(helm template test charts/x)\"; \\\necho \"$out\" | grep -q 'kind: Deployment'\n"},
		{line: 10, script: "helm template test charts/x --set a=b\n"},
	} {
		got := recipe[i]
		if got.file != "Makefile" || got.where != "the helm-test recipe" || got.line != want.line || got.script != want.script || got.errexit || (len(got.lost) > 0) != (i == 3) {
			t.Errorf("block %d of the recipe was read as %+v", i, got)
		}
	}
	if findings, weighed := ineffectiveHelmChecks(recipe[2]); len(findings) > 0 || weighed != 2 {
		t.Errorf("the block that keeps a render and reads it has %d checks and the findings %q, want 2 and none", weighed, findings)
	}
	if blocks := makeRecipeBlocks(makefile, "absent"); blocks != nil {
		t.Errorf("a target the Makefile does not have was read as %+v", blocks)
	}

	step := func(extra string) string {
		return "      - name: lint\n" + extra + "        run: |\n          helm lint charts/x\n          helm template test charts/x\n"
	}
	for name, tc := range map[string]struct {
		workflow string
		errexit  bool
	}{
		"no shell":                  {"jobs:\n  test:\n    steps:\n" + step(""), true},
		"bash":                      {"jobs:\n  test:\n    steps:\n" + step("        shell: bash\n"), true},
		"sh":                        {"jobs:\n  test:\n    steps:\n" + step("        shell: sh\n"), true},
		"a command with the flag":   {"jobs:\n  test:\n    steps:\n" + step("        shell: bash --noprofile -eo pipefail {0}\n"), true},
		"a command without it":      {"jobs:\n  test:\n    steps:\n" + step("        shell: bash --noprofile {0}\n"), false},
		"another program":           {"jobs:\n  test:\n    steps:\n" + step("        shell: python\n"), false},
		"another program's flag":    {"jobs:\n  test:\n    steps:\n" + step("        shell: pwsh -File {0}\n"), false},
		"sh by its path":            {"jobs:\n  test:\n    steps:\n" + step("        shell: /bin/sh -eu {0}\n"), true},
		"the job's default":         {"jobs:\n  test:\n    defaults:\n      run:\n        shell: bash {0}\n    steps:\n" + step(""), false},
		"the workflow's default":    {"defaults:\n  run:\n    shell: bash {0}\njobs:\n  test:\n    steps:\n" + step(""), false},
		"the step's over a default": {"defaults:\n  run:\n    shell: bash {0}\njobs:\n  test:\n    steps:\n" + step("        shell: bash\n"), true},
	} {
		blocks := ciChartBlocks(t, tc.workflow)
		if len(blocks) != 1 {
			t.Fatalf("%s: %d steps were read, want 1", name, len(blocks))
		}
		block := blocks[0]
		if block.errexit != tc.errexit || block.where != `the step "lint"` || block.script != "helm lint charts/x\nhelm template test charts/x\n" {
			t.Errorf("%s: the step was read as %+v, want errexit %v", name, block, tc.errexit)
		}
		before, _, _ := strings.Cut(tc.workflow, "          helm lint")
		if want := strings.Count(before, "\n") + 1; block.line != want {
			t.Errorf("%s: the step's script was read as starting on line %d, want %d", name, block.line, want)
		}
		// Two renders, the first of which only -e makes count.
		findings, weighed := ineffectiveHelmChecks(block)
		if weighed != 2 || (len(findings) == 0) != tc.errexit {
			t.Errorf("%s: %d checks and the findings %q, want 2 checks and a finding only without -e", name, weighed, findings)
		}
	}
}

// helmCheckMutations are changes that leave a helm check in place and take
// its teeth, or a case, away, each made to one of the two files: the ones a
// review made by hand and found unnoticed, and more of their kind. want is
// what the finding that notices the change says, the file and the line of
// the check among it.
var helmCheckMutations = []struct {
	name, file, old, new string
	want                 []string
}{
	{
		"an absence check of the recipe loses its exit 1", "Makefile",
		"echo \"an empty server.shutdownDelay rendered the flag\" >&2; \\\n\t\texit 1; \\\n",
		"echo \"an empty server.shutdownDelay rendered the flag\" >&2; \\\n",
		[]string{"Makefile:", "the helm-test recipe", "this text must be absent from the render, and nothing fails when it is not", "grep -q -- '--web.shutdown-delay'; then"},
	},
	{
		"a grep of the workflow is followed by || true", "ci.yml",
		"grep -q 'terminationGracePeriodSeconds: 75'\n",
		"grep -q 'terminationGracePeriodSeconds: 75' || true\n",
		[]string{".github/workflows/ci.yml:", `the step "Render the shutdown delay and timeout, their grace period and the exporter credential Secret"`, "the `||` after this check takes its failure away", "grep -q 'terminationGracePeriodSeconds: 75' || true"},
	},
	{
		"a recipe line loses its set -eu", "Makefile",
		"@set -eu; \\\n\tout=\"$$(helm template test charts/prometheus-universal-exporter --set server.shutdownTimeout=1m)\"",
		"@out=\"$$(helm template test charts/prometheus-universal-exporter --set server.shutdownTimeout=1m)\"",
		[]string{"Makefile:", "the helm-test recipe", "no `set -e` is in force here", "--set server.shutdownTimeout=1m"},
	},
	{
		"a case of the recipe renders another value", "Makefile",
		"goGC.percent=200", "goGC.percent=201",
		[]string{"`make helm-test` checks this and ci.yml does not", "--set goGC.percent=201"},
	},
	{
		"a rejection of the workflow loses its exit 1", "ci.yml",
		"echo \"helm template accepted an Ingress without the Service it routes to\" >&2\n            exit 1\n",
		"echo \"helm template accepted an Ingress without the Service it routes to\" >&2\n",
		[]string{".github/workflows/ci.yml:", "this render must be refused, and nothing fails when it is not", "--set ingress.enabled=true --set service.enabled=false"},
	},
	{
		"a rejection of the workflow no longer runs helm", "ci.yml",
		"if helm template test charts/prometheus-universal-exporter --set replicaCount=many >/dev/null 2>&1; then",
		"if false; then",
		[]string{"`make helm-test` checks this and ci.yml does not", "--set replicaCount=many"},
	},
	{
		"a rejection of the recipe no longer runs helm", "Makefile",
		"@if helm template test charts/prometheus-universal-exporter --set ingress.enabled=true --set service.enabled=false >/dev/null 2>&1; then",
		"@if false; then",
		[]string{"`make helm-test` does not make this check of the chart", "--set ingress.enabled=true --set service.enabled=false"},
	},
	{
		"a pattern of the workflow is weakened to match anything", "ci.yml",
		"grep -q 'type: Recreate'", "grep -q 'type: '",
		[]string{"ci.yml does not make this check of the chart", `renders a line matching "type: Recreate"`},
	},
	{
		"a pattern of the recipe is weakened", "Makefile",
		"grep -q 'kind: PodMonitor'", "grep -q 'kind: Pod'",
		[]string{"ci.yml checks this and `make helm-test` does not", `renders a line matching "kind: PodMonitor"`},
	},
	{
		"the manifest check of the workflow becomes cat", "ci.yml",
		" | python3 tools/check-manifests.py", " | cat",
		[]string{".github/workflows/ci.yml:", `the step "Check that every rendered manifest is its own document"`, "helm is piped into `cat`, which does not fail when helm does"},
	},
	{
		"a render of the recipe is followed by || :", "Makefile",
		"\thelm lint charts/prometheus-universal-exporter\n",
		"\thelm lint charts/prometheus-universal-exporter || :\n",
		[]string{"Makefile:", "the `||` after this check takes its failure away", "helm lint charts/prometheus-universal-exporter || :"},
	},
	{
		"a recipe line starts with make's -", "Makefile",
		"\thelm lint charts/prometheus-universal-exporter\n",
		"\t-helm lint charts/prometheus-universal-exporter\n",
		[]string{"Makefile:", "the line starts with `-`, which has make ignore its failure", "helm lint charts/prometheus-universal-exporter"},
	},
	{
		"a quiet recipe line starts with make's -", "Makefile",
		"\t@set -eu; \\\n\tout=\"$$(helm template test charts/prometheus-universal-exporter --set strategy.type=Recreate)\"",
		"\t@-set -eu; \\\n\tout=\"$$(helm template test charts/prometheus-universal-exporter --set strategy.type=Recreate)\"",
		[]string{"Makefile:", "the line starts with `-`, which has make ignore its failure", "@-set -eu;"},
	},
	{
		"a recipe line carries on after its check", "Makefile",
		"\thelm lint charts/prometheus-universal-exporter\n",
		"\thelm lint charts/prometheus-universal-exporter; echo linted\n",
		[]string{"Makefile:", "no `set -e` is in force here", "helm lint charts/prometheus-universal-exporter; echo linted"},
	},
	{
		"a recipe line turns set -e off again", "Makefile",
		"@set -eu; \\\n\tout=\"$$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\"",
		"@set -eu; set +e; \\\n\tout=\"$$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\"",
		[]string{"Makefile:", "no `set -e` is in force here", "--set goGC.percent=400"},
	},
	{
		"the Makefile ignores every failing line", "Makefile",
		"\nhelm-test: helm-version\n", "\n.IGNORE:\nhelm-test: helm-version\n",
		[]string{"Makefile:", ".IGNORE has make ignore every recipe line that fails"},
	},
	{
		"a step of the workflow continues on error", "ci.yml",
		"      - name: Render the Recreate strategy without a rollingUpdate\n",
		"      - name: Render the Recreate strategy without a rollingUpdate\n        continue-on-error: true\n",
		[]string{".github/workflows/ci.yml:", `the step "Render the Recreate strategy without a rollingUpdate"`, "the step has continue-on-error", "\tcontinue-on-error: true"},
	},
	{
		"the job of the workflow continues on error", "ci.yml",
		"  test:\n    runs-on: ubuntu-latest\n",
		"  test:\n    runs-on: ubuntu-latest\n    continue-on-error: true\n",
		[]string{".github/workflows/ci.yml:8: the job test: the job has continue-on-error", "\tcontinue-on-error: true"},
	},
	{
		"a step of the workflow never runs", "ci.yml",
		"      - name: Render the Recreate strategy without a rollingUpdate\n        if: steps.changes.outputs.chart == 'true'\n",
		"      - name: Render the Recreate strategy without a rollingUpdate\n        if: false\n",
		[]string{".github/workflows/ci.yml:", `the step "Render the Recreate strategy without a rollingUpdate"`, "the step does not run whenever the chart changed", "\tif: false"},
	},
	{
		"a step of the workflow runs in a shell without -e", "ci.yml",
		"      - name: Package the chart\n",
		"      - name: Package the chart\n        shell: bash {0}\n",
		[]string{".github/workflows/ci.yml:", `the step "Package the chart"`, "no `set -e` is in force here", "helm package charts/prometheus-universal-exporter --destination dist"},
	},
	{
		"a rejection of the workflow exits with 0", "ci.yml",
		"echo \"helm template accepted a goGC.percent of 0\" >&2\n            exit 1\n",
		"echo \"helm template accepted a goGC.percent of 0\" >&2\n            exit 0\n",
		[]string{".github/workflows/ci.yml:", "this render must be refused, and nothing fails when it is not", "--set goGC.percent=0 "},
	},
	{
		"a rejection of the recipe exits only if its message fails", "Makefile",
		"echo \"helm template accepted a goGC.percent of 0\" >&2; \\\n\t\texit 1; \\\n",
		"echo \"helm template accepted a goGC.percent of 0\" >&2 || \\\n\t\texit 1; \\\n",
		[]string{"Makefile:", "this render must be refused, and nothing fails when it is not", "--set goGC.percent=0 "},
	},
	{
		"a rejection of the recipe holds only with something false", "Makefile",
		"@if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then",
		"@if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1 && false; then",
		[]string{"Makefile:", "the condition of the `if` goes on after `&&`", "--set goGC.percent=0 "},
	},
	{
		"a rejection of the workflow has another command for its condition", "ci.yml",
		"if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then",
		"if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; false; then",
		[]string{".github/workflows/ci.yml:", "the condition of the `if` is followed by another command"},
	},
	{
		"an absence check of the workflow reads a pipe", "ci.yml",
		"if echo \"$out\" | grep -q rollingUpdate; then",
		"if helm template test charts/prometheus-universal-exporter --set strategy.type=Recreate | grep -q rollingUpdate; then",
		[]string{".github/workflows/ci.yml:", "a render is piped into a condition, which a render that failed passes as an absence"},
	},
	{
		"a render of the recipe is kept whatever helm returned", "Makefile",
		"out=\"$$(helm template test charts/prometheus-universal-exporter --set strategy.type=Recreate)\"",
		"out=\"$$(helm template test charts/prometheus-universal-exporter --set strategy.type=Recreate || true)\"",
		[]string{"Makefile:", "the render is kept from more than the one helm command (`||`)"},
	},
	{
		"a grep of the workflow passes on any other line", "ci.yml",
		"echo \"$out\" | grep -q 'name: GOGC'", "echo \"$out\" | grep -qv 'name: GOGC'",
		[]string{".github/workflows/ci.yml:", "a render is read by `grep -qv name: GOGC`, which is no check the reader of the helm checks knows"},
	},
	{
		"a grep of the recipe looks for nothing", "Makefile",
		"echo \"$$out\" | grep -q 'value: \"400\"'", "echo \"$$out\" | grep -q ''",
		[]string{"Makefile:", "which is no check the reader of the helm checks knows"},
	},
	{
		"a rejection of the workflow runs in a pipeline", "ci.yml",
		"echo \"helm template accepted a goGC.percent of 0\" >&2\n            exit 1\n          fi\n",
		"echo \"helm template accepted a goGC.percent of 0\" >&2\n            exit 1\n          fi | cat\n",
		[]string{".github/workflows/ci.yml:", "what follows `fi` runs the commands before it in a shell of their own"},
	},
	{
		"a loop of rejections of the recipe is followed by || true", "Makefile",
		"\t\t\techo \"helm template accepted the invalid server.listenAddress '$$address'\" >&2; \\\n\t\t\texit 1; \\\n\t\tfi; \\\n\tdone\n",
		"\t\t\techo \"helm template accepted the invalid server.listenAddress '$$address'\" >&2; \\\n\t\t\texit 1; \\\n\t\tfi; \\\n\tdone || true\n",
		[]string{"Makefile:", "the `||` after `done` has the shell carry on"},
	},
	{
		"a render that must be refused is written behind !", "ci.yml",
		"          if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then\n            echo \"helm template accepted a goGC.percent of 0\" >&2\n            exit 1\n          fi\n",
		"          ! helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1\n",
		[]string{".github/workflows/ci.yml:", "does not understand `!`"},
	},
}

// A check made toothless, in one file or the other, is noticed: each change
// of helmCheckMutations is made to a copy of the file's text, in memory, and
// the reader has to report it, naming the file and saying what is wrong with
// which line. The files themselves have nothing reported, and are not written.
func TestAHelmCheckMadeToothlessIsNoticed(t *testing.T) {
	makefile, workflow := readHelmLists(t)
	if findings := helmCheckFindings(t, makefile, workflow); len(findings) > 0 {
		t.Fatalf("the helm checks as they are have findings, so a change to them would not be told from none:\n%s", strings.Join(findings, "\n"))
	}
	for _, mutation := range helmCheckMutations {
		t.Run(mutation.name, func(t *testing.T) {
			changedMakefile, changedWorkflow := makefile, workflow
			text := &changedWorkflow
			if mutation.file == "Makefile" {
				text = &changedMakefile
			}
			if !strings.Contains(*text, mutation.old) {
				t.Fatalf("%s no longer holds the text this change is made to, so the test changes nothing:\n%s", mutation.file, mutation.old)
			}
			*text = strings.Replace(*text, mutation.old, mutation.new, 1)
			findings := helmCheckFindings(t, changedMakefile, changedWorkflow)
			for _, finding := range findings {
				noticed := true
				for _, want := range mutation.want {
					noticed = noticed && strings.Contains(finding, want)
				}
				if noticed {
					return
				}
			}
			t.Errorf("the change is not noticed: no finding says all of %q\n\n%d findings:\n%s", mutation.want, len(findings), strings.Join(findings, "\n"))
		})
	}
}
