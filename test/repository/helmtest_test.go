package repository

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// `make helm-test` and the chart steps of ci.yml are the same checks written
// twice, in a Makefile recipe and in a workflow's shell, and the Makefile says
// that a local run means a CI run. The tests below read both as the shell
// would, far enough to list what each establishes — which helm command lines
// must succeed, which must fail, and which lines must or must not be in what
// they render — and compare the lists.

// shellToken is a word or an operator of a shell script. A word keeps its
// pieces apart, since a variable is expanded in the unquoted and double-quoted
// ones only. pos is where the token starts in the script, which is how a
// finding names the line it is about.
type shellToken struct {
	op     string
	pieces []shellPiece
	pos    int
}

type shellPiece struct {
	text       string
	expandable bool
}

// lexShell splits a script into words and the operators the checks need:
// command separators — `;` and the end of a line, `&&`, `||` and `&`, each
// kept as what it is, since a check is only worth what follows it lets it be —
// pipes and command substitutions. Redirections are dropped, and so are
// comments; a here-string is kept, as `<<<` and the word after it, since what
// it hands a command is what the command reads, and of a here-document the
// `<<`, since the lines after it are not commands. It knows the quoting rules and
// nothing of the grammar, which is enough for scripts as plain as these.
func lexShell(script string) []shellToken {
	return lexShellAt(script, 0)
}

// lexShellAt is lexShell for a piece of a script that starts at base in the
// whole of it.
func lexShellAt(script string, base int) []shellToken {
	var tokens []shellToken
	var word []shellPiece
	inWord := false
	start, i := 0, 0
	flush := func() {
		if inWord {
			tokens = append(tokens, shellToken{pieces: word, pos: base + start})
		}
		word, inWord = nil, false
	}
	add := func(text string, expandable bool) {
		if !inWord {
			start = i
		}
		inWord = true
		if last := len(word) - 1; last >= 0 && word[last].expandable == expandable {
			word[last].text += text
		} else if text != "" {
			word = append(word, shellPiece{text, expandable})
		}
	}
	op := func(name string) {
		flush()
		tokens = append(tokens, shellToken{op: name, pos: base + i})
	}
	// skipWord consumes the target of a redirection.
	skipWord := func(i int) int {
		for i < len(script) && (script[i] == ' ' || script[i] == '\t') {
			i++
		}
		for i < len(script) && !strings.ContainsRune(" \t\n;|)", rune(script[i])) {
			i++
		}
		return i
	}
	for i < len(script) {
		c := script[i]
		switch {
		case c == ' ' || c == '\t':
			flush()
			i++
		case c == '\n':
			op(";")
			i++
		case c == '#' && !inWord:
			for i < len(script) && script[i] != '\n' {
				i++
			}
		case c == '\\' && i+1 < len(script):
			// A line continuation vanishes; any other escaped character is itself.
			if script[i+1] != '\n' {
				add(string(script[i+1]), false)
			}
			i += 2
		case c == '\'':
			end := strings.IndexByte(script[i+1:], '\'')
			if end < 0 {
				end = len(script) - i - 1
			}
			add(script[i+1:i+1+end], false)
			i += end + 2
		case c == '"':
			if !inWord {
				start = i
			}
			inWord = true
			i++
			for i < len(script) && script[i] != '"' {
				switch {
				case script[i] == '\\' && i+1 < len(script):
					if script[i+1] != '\n' {
						add(string(script[i+1]), false)
					}
					i += 2
				case strings.HasPrefix(script[i:], "$("):
					// out="$(helm ...)": the substitution's own words follow, up
					// to its closing parenthesis.
					end := matchingParen(script, i+2)
					op("$(")
					tokens = append(tokens, lexShellAt(script[i+2:end], base+i+2)...)
					i = end
					op(")")
					i++
				default:
					add(string(script[i]), true)
					i++
				}
			}
			i++
		case strings.HasPrefix(script[i:], "$("):
			end := matchingParen(script, i+2)
			op("$(")
			tokens = append(tokens, lexShellAt(script[i+2:end], base+i+2)...)
			i = end
			op(")")
			i++
		case c == ';':
			op(";")
			i++
		case strings.HasPrefix(script[i:], "&&") || strings.HasPrefix(script[i:], "||"):
			op(script[i : i+2])
			i += 2
		case c == '|':
			op("|")
			i++
		case c == '&' && !strings.HasPrefix(script[i:], "&>"):
			op("&")
			i++
		case strings.HasPrefix(script[i:], "<<<"):
			op("<<<")
			i += 3
		case c == '>' || c == '<' || c == '&':
			// `>/dev/null`, `>&2`, `2>&1` and `>> "$GITHUB_OUTPUT"`: a file
			// descriptor written before the operator belongs to it.
			if inWord && len(word) == 1 && regexp.MustCompile(`^[0-9]+$`).MatchString(word[0].text) {
				word, inWord = nil, false
			}
			flush()
			if strings.HasPrefix(script[i:], "<<") {
				// A here-document: its delimiter is skipped as a
				// redirection's target is, and the operator kept, since
				// the lines that follow are then no commands.
				op("<<")
				i += len("<<")
				if i < len(script) && script[i] == '-' {
					i++
				}
			}
			for i < len(script) && (script[i] == '>' || script[i] == '<' || script[i] == '&') {
				i++
			}
			i = skipWord(i)
		default:
			add(string(c), true)
			i++
		}
	}
	flush()
	return tokens
}

// matchingParen returns the index of the parenthesis that closes a command
// substitution whose body starts at from, skipping quoted text.
func matchingParen(script string, from int) int {
	depth := 1
	for i := from; i < len(script); i++ {
		switch script[i] {
		case '\'':
			if end := strings.IndexByte(script[i+1:], '\''); end >= 0 {
				i += end + 1
			}
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(script)
}

var shellVariable = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// expand gives a word its text, with the variables that are known replaced; an
// unknown one stays as it is written, which is how `"$out"` and `"$package"`
// are recognised.
func (token shellToken) expand(variables map[string]string) string {
	var b strings.Builder
	for _, piece := range token.pieces {
		if !piece.expandable {
			b.WriteString(piece.text)
			continue
		}
		b.WriteString(shellVariable.ReplaceAllStringFunc(piece.text, func(reference string) string {
			if value, ok := variables[shellVariable.FindStringSubmatch(reference)[1]]; ok {
				return value
			}
			return reference
		}))
	}
	return b.String()
}

var wholeVariable = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*|\{[A-Za-z_][A-Za-z0-9_]*\})$`)

// variableName returns the name of the variable a word is, `$name` or
// `${name}`, and nothing for any other word.
func variableName(word string) string {
	if !wholeVariable.MatchString(word) {
		return ""
	}
	return strings.Trim(word, "${}")
}

// printedVariable returns the name of the variable a command prints whole, a
// line of it after the other, which is how a kept render is handed to what
// reads it: `echo "$name"`, or `printf '%s\n' "$name"`, which is the same
// text. Both readers of the helm checks ask it, so that a check one of them
// weighs is a check the other lists.
func printedVariable(words []string) string {
	if len(words) == 3 && words[0] == "printf" && words[1] == `%s\n` {
		return variableName(words[2])
	}
	if len(words) == 2 && words[0] == "echo" {
		return variableName(words[1])
	}
	return ""
}

var literalAssignment = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// shellCommand is one command of a script, a pipeline included, with the
// operator that ends it: `;` for that or the end of a line, `&&`, `||` or `&`.
type shellCommand struct {
	tokens []shellToken
	end    string
}

// shellCommands splits a script into its commands. A command substitution
// stays whole inside the command that holds it.
func shellCommands(script string) []shellCommand {
	var commands []shellCommand
	var current []shellToken
	depth := 0
	for _, token := range lexShell(script) {
		switch token.op {
		case "$(":
			depth++
		case ")":
			depth--
		}
		if depth == 0 && (token.op == ";" || token.op == "&&" || token.op == "||" || token.op == "&") {
			if len(current) > 0 {
				commands = append(commands, shellCommand{current, token.op})
			}
			current = nil
			continue
		}
		current = append(current, token)
	}
	if len(current) > 0 {
		commands = append(commands, shellCommand{current, ";"})
	}
	return commands
}

// helmChecks lists what a script establishes about helm, one sentence each, as
// a set. variables holds what the script's environment defines.
func helmChecks(script string, variables map[string]string) map[string]bool {
	var commands [][]shellToken
	for _, command := range shellCommands(script) {
		commands = append(commands, command.tokens)
	}
	checks := map[string]bool{}
	bound := map[string]string{}
	for name, value := range variables {
		bound[name] = value
	}
	runHelmCommands(commands, bound, map[string]string{}, checks)
	return checks
}

// runHelmCommands walks the commands of a script, a `for` loop's body once for
// each of its words, and records every check it recognises. captured holds the
// helm command line behind each `name="$(helm ...)"`. A variable given a
// value written out (`chart=charts/x`) is replaced by it in the commands
// after, so a command line means the same with the value in a variable.
func runHelmCommands(commands [][]shellToken, variables, captured map[string]string, checks map[string]bool) {
	for i := 0; i < len(commands); i++ {
		command := commands[i]
		words := func() []string {
			out := make([]string, len(command))
			for j, token := range command {
				out[j] = token.op
				if token.op == "" {
					out[j] = token.expand(variables)
				}
			}
			return out
		}()
		// Keywords that only introduce the command behind them.
		condition := false
		for len(words) > 0 && command[0].op == "" && (words[0] == "if" || words[0] == "then" || words[0] == "do" || words[0] == "else" || words[0] == "!") {
			condition = condition || words[0] == "if"
			words, command = words[1:], command[1:]
		}
		if len(words) == 0 {
			continue
		}
		if words[0] == "for" && len(words) >= 3 && words[2] == "in" {
			// The body runs to the `done` that closes this loop.
			end, depth := i+1, 1
			for ; end < len(commands); end++ {
				first := commands[end][0].expand(nil)
				if first == "for" {
					depth++
				}
				if first == "done" {
					if depth--; depth == 0 {
						break
					}
				}
			}
			for _, value := range words[3:] {
				inner := map[string]string{words[1]: value}
				for name, old := range variables {
					if name != words[1] {
						inner[name] = old
					}
				}
				runHelmCommands(commands[i+1:end], inner, captured, checks)
			}
			i = end
			continue
		}
		// name="$(helm ...)" keeps a render for the lines after it.
		if len(command) >= 3 && command[1].op == "$(" && command[len(command)-1].op == ")" && strings.HasSuffix(words[0], "=") {
			if inner := words[2 : len(words)-1]; len(inner) > 0 && inner[0] == "helm" {
				line := helmCommandLine(inner)
				captured[strings.TrimSuffix(words[0], "=")] = line
				checks["succeeds: "+line] = true
			}
			continue
		}
		if match := literalAssignment.FindStringSubmatch(words[0]); match != nil && len(command) == 1 {
			variables[match[1]] = match[2]
			continue
		}
		// A pipeline: helm, or the echo of a kept render, and what reads it.
		stage, rest := words, []string(nil)
		for j, token := range command {
			if token.op == "|" {
				stage, rest = words[:j], words[j+1:]
				break
			}
		}
		var line string
		switch {
		case len(stage) > 0 && stage[0] == "helm":
			line = helmCommandLine(stage)
		case captured[printedVariable(stage)] != "":
			line = captured[printedVariable(stage)]
		default:
			continue
		}
		switch {
		case len(rest) == 0 && condition:
			checks["fails: "+line] = true
		case len(rest) == 0:
			checks["succeeds: "+line] = true
		case len(rest) >= 3 && rest[0] == "grep" && rest[1] == "-q":
			pattern := rest[len(rest)-1]
			if condition {
				checks["renders no line matching "+strconv.Quote(pattern)+": "+line] = true
			} else {
				checks["renders a line matching "+strconv.Quote(pattern)+": "+line] = true
			}
		case len(rest) == 3 && rest[0] == "python3" && rest[1] == "-c":
			checks["renders what this Python accepts: "+line+"\n"+strings.TrimSpace(rest[2])] = true
		default:
			checks["renders what `"+strings.Join(rest, " ")+"` accepts: "+line] = true
		}
	}
}

// helmCommandLine writes a helm command as one line. The directory a package
// is written to is the one thing the Makefile and the workflow choose
// differently, a temporary directory against the runner's checkout.
func helmCommandLine(words []string) string {
	out := make([]string, len(words))
	for i, word := range words {
		switch {
		case i > 0 && words[i-1] == "--destination":
			out[i] = "<directory>"
		case word == "" || strings.ContainsAny(word, " \t\n"):
			out[i] = strconv.Quote(word)
		default:
			out[i] = word
		}
	}
	return strings.Join(out, " ")
}

// helmBlock is a piece of one of the two lists that runs in a shell of its
// own: a line of the Makefile's recipe with the lines continued onto it, or
// the `run` of a workflow step. A failing command in one block does not stop
// the next from being read, so what a check is worth is decided block by
// block.
type helmBlock struct {
	// file and where name the block in a finding, and line is the line of
	// the file its script starts on.
	file, where string
	line        int
	script      string
	// errexit says whether the block's shell is started so that a failing
	// command ends it, as `set -e` has it.
	errexit bool
	// lost holds a finding for each reason a failure of the block fails
	// nothing, whatever its script does.
	lost []string
}

// helmFinding words a finding about one of the two lists: the file and line,
// the recipe or step, what is wrong, and the line itself.
func helmFinding(file string, line int, where, problem, text string) string {
	return fmt.Sprintf("%s:%d: %s: %s:\n\t%s", file, line, where, problem, strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "\\")))
}

// makeRecipeBlocks returns a Makefile target's recipe as the shell receives
// it, a block for each line make runs in a shell of its own: the recipe
// prefix and make's own prefixes taken off, `$$` made the `$` it stands for.
// make starts that shell without -e, and fails the target on what the shell
// returns: the result of the line's last command.
func makeRecipeBlocks(makefile, target string) []helmBlock {
	before, after, found := strings.Cut("\n"+makefile, "\n"+target+":")
	if !found {
		return nil
	}
	first := strings.Count(before, "\n") + 2
	_, after, _ = strings.Cut(after, "\n")
	var blocks []helmBlock
	continued := false
	for i, line := range strings.SplitAfter(after, "\n") {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		line = strings.TrimPrefix(line, "\t")
		if !continued {
			block := helmBlock{file: "Makefile", where: "the " + target + " recipe", line: first + i}
			// @ keeps make from printing the line and + runs it under -n
			// too; - has make carry on when the line fails.
			written := line
			for line != "" && strings.ContainsRune("@+- \t", rune(line[0])) {
				if line[0] == '-' {
					block.lost = append(block.lost, helmFinding(block.file, block.line, block.where, "the line starts with `-`, which has make ignore its failure", written))
				}
				line = line[1:]
			}
			blocks = append(blocks, block)
		}
		continued = strings.HasSuffix(strings.TrimRight(line, "\n"), "\\")
		blocks[len(blocks)-1].script += strings.ReplaceAll(line, "$$", "$")
	}
	return blocks
}

// scriptOf joins the scripts of blocks, each on lines of its own.
func scriptOf(blocks []helmBlock) string {
	var script strings.Builder
	for _, block := range blocks {
		script.WriteString(block.script)
		if !strings.HasSuffix(block.script, "\n") {
			script.WriteString("\n")
		}
	}
	return script.String()
}

// makeRecipe returns a Makefile target's recipe as one script.
func makeRecipe(t *testing.T, makefile, target string) string {
	t.Helper()
	blocks := makeRecipeBlocks(makefile, target)
	if len(blocks) == 0 {
		t.Fatalf("the Makefile has no %s target, or the target has no recipe", target)
	}
	return scriptOf(blocks)
}

// makeDefinitions returns the multi-line variables the Makefile defines and
// exports, which a recipe reads from its environment.
func makeDefinitions(makefile string) map[string]string {
	definitions := map[string]string{}
	for _, match := range regexp.MustCompile(`(?ms)^define ([A-Za-z_][A-Za-z0-9_]*)\n(.*?)\nendef$`).FindAllStringSubmatch(makefile, -1) {
		if strings.Contains(makefile, "\nexport "+match[1]+"\n") {
			definitions[match[1]] = match[2]
		}
	}
	return definitions
}

// chartStepCondition is the `if` of the chart steps of ci.yml: they run
// whenever the chart, or a file its checks render, changed.
const chartStepCondition = "steps.changes.outputs.chart == 'true'"

// ciChartBlocks returns the shell of every step of the workflow that runs
// helm, a block for each step. GitHub runs a step that names no shell, or
// names bash or sh, in a shell started with -e; a `shell:` written as a
// command says itself whether it is, and one that is not — the step's, the
// job's default or the workflow's — is reported where it is written. A step,
// or its job, with continue-on-error fails nothing, and neither does a step
// whose `if` is not the chart steps' own, or a job with an `if` at all: the
// chart steps' condition reads a step's output, which no job can.
func ciChartBlocks(t *testing.T, workflow string) []helmBlock {
	t.Helper()
	type defaults struct {
		Run struct {
			Shell yaml.Node `yaml:"shell"`
		} `yaml:"run"`
	}
	var parsed struct {
		Defaults defaults `yaml:"defaults"`
		Jobs     map[string]struct {
			Defaults        defaults  `yaml:"defaults"`
			If              yaml.Node `yaml:"if"`
			ContinueOnError yaml.Node `yaml:"continue-on-error"`
			Steps           []struct {
				ID   string `yaml:"id"`
				With struct {
					Filters string `yaml:"filters"`
				} `yaml:"with"`
				Name            string    `yaml:"name"`
				Run             yaml.Node `yaml:"run"`
				Shell           yaml.Node `yaml:"shell"`
				If              yaml.Node `yaml:"if"`
				ContinueOnError yaml.Node `yaml:"continue-on-error"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &parsed); err != nil {
		t.Fatal(err)
	}
	job := parsed.Jobs["test"]
	var blocks []helmBlock
	// chartChanges says whether a step read so far, with the id the chart
	// steps' condition names, gives the output it asks for whenever a file
	// of the chart changed.
	chartChanges := false
	for _, step := range job.Steps {
		if step.ID == "changes" {
			var filters map[string][]string
			_ = yaml.Unmarshal([]byte(step.With.Filters), &filters)
			chartChanges = slices.Contains(filters["chart"], "charts/**")
		}
		if !regexp.MustCompile(`(?m)(^|[\s(])helm (lint|template|package|install)\b`).MatchString(step.Run.Value) {
			continue
		}
		const file = ".github/workflows/ci.yml"
		block := helmBlock{file: file, where: "the step " + strconv.Quote(step.Name), line: step.Run.Line, script: step.Run.Value}
		if step.Name == "" {
			line, _, _ := strings.Cut(step.Run.Value, "\n")
			block.where = "the step that runs " + strconv.Quote(line)
		}
		if step.Run.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			// A block scalar's text starts on the line after its indicator.
			block.line++
		}
		shell, owner := step.Shell, block.where
		if shell.Kind == 0 {
			shell, owner = job.Defaults.Run.Shell, "the job test"
		}
		if shell.Kind == 0 {
			shell, owner = parsed.Defaults.Run.Shell, "the workflow"
		}
		block.errexit = shellEndsOnFailure(shell.Value)
		if !block.errexit {
			block.lost = append(block.lost, helmFinding(file, shell.Line, owner, "the shell is not bash or sh started with -e, as GitHub starts the shell of a step that names none, so a failing check does not end a chart step; leave `shell:` out, or write it as `bash -e {0}`", "shell: "+shell.Value))
		}
		if node := step.ContinueOnError; node.Kind != 0 && node.Value != "false" {
			block.lost = append(block.lost, helmFinding(file, node.Line, block.where, "the step has continue-on-error, so the job passes when it fails", "continue-on-error: "+node.Value))
		}
		if node := job.ContinueOnError; node.Kind != 0 && node.Value != "false" {
			block.lost = append(block.lost, helmFinding(file, node.Line, "the job test", "the job has continue-on-error, so the workflow passes when a chart step fails", "continue-on-error: "+node.Value))
		}
		if node := step.If; node.Kind != 0 && node.Value != chartStepCondition {
			block.lost = append(block.lost, helmFinding(file, node.Line, block.where, "the step does not run whenever the chart changed, as the chart steps do with `if: "+chartStepCondition+"`", "if: "+node.Value))
		}
		if node := step.If; node.Value == chartStepCondition && !chartChanges {
			block.lost = append(block.lost, helmFinding(file, node.Line, block.where, "the step runs when steps.changes.outputs.chart is true, and no step before it with the id `changes` sets that for a change under charts/**: the `filters` of that step need a `chart` list that holds 'charts/**'", "if: "+node.Value))
		}
		if node := job.If; node.Kind != 0 {
			block.lost = append(block.lost, helmFinding(file, node.Line, "the job test", "the job has an `if`, so the chart steps do not run whenever the chart changed; the one condition they have is each step's `if: "+chartStepCondition+"`, which a job cannot have", "if: "+node.Value))
		}
		blocks = append(blocks, block)
	}
	return blocks
}

var shellFlags = regexp.MustCompile(`^-[euxo]+$`)

// shellEndsOnFailure reports whether a workflow step's shell ends at a
// command that fails: GitHub's own bash and sh do, started as `bash -e {0}`
// and `sh -e {0}`, and a shell given as a command does when it has the flag.
// Such a command is bash or sh, the options GitHub itself gives them and the
// script, `{0}`, last; with any other word — `-n`, which runs nothing, or `-c`
// and a command of its own — it is not a shell the checks are known to run in.
func shellEndsOnFailure(shell string) bool {
	words := strings.Fields(shell)
	if len(words) == 0 || shell == "bash" || shell == "sh" {
		return true
	}
	if program := filepath.Base(words[0]); program != "bash" && program != "sh" {
		// Another program's flags are its own, and its script is not shell.
		return false
	}
	ends := false
	for i := 1; i < len(words); i++ {
		word := words[i]
		switch {
		case word == "{0}" && i == len(words)-1, word == "--noprofile", word == "--norc":
		case shellFlags.MatchString(word):
			ends = ends || strings.Contains(word, "e")
			if strings.Contains(word, "o") {
				// -o takes the name of its option from the next word.
				if i++; i == len(words) || !slices.Contains([]string{"errexit", "nounset", "pipefail", "xtrace"}, words[i]) {
					return false
				}
				ends = ends || words[i] == "errexit"
			}
		default:
			return false
		}
	}
	return ends && words[len(words)-1] == "{0}"
}

// readHelmLists reads the two files the helm checks are written in.
func readHelmLists(t *testing.T) (makefile, workflow string) {
	t.Helper()
	for name, into := range map[string]*string{"Makefile": &makefile, ".github/workflows/ci.yml": &workflow} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		*into = string(raw)
	}
	return makefile, workflow
}

// The reader above is only worth trusting if it reads the shell these files
// use: a loop over quoted and unquoted words, a variable inside an escaped
// double-quoted JSON value, a render kept in a variable and looked at twice, a
// pipeline in a condition, a line continuation, and the Makefile's own
// escaping of all of that.
func TestTheHelmCheckReaderUnderstandsTheShellTheChecksUse(t *testing.T) {
	const chart = "charts/x"
	script := `set -eu
# helm template never-run charts/x
helm lint charts/x
out="$(helm template test charts/x --set a=1m --set-json 'list=[{"name":"t"}]')"
echo "$out" | grep -q -- '--flag=1m'
if echo "$out" | grep -q gone; then
  echo "helm template rendered what it should not" >&2
  exit 1
fi
for bad in ':http' 'a=b' plain; do
  if helm template test charts/x --set "value=$bad" --set-json "args=[\"$bad\"]" >/dev/null 2>&1; then
    echo "helm template accepted $bad" >&2
    exit 1
  fi
done
helm template test charts/x \
  --set many=true \
  | python3 tools/check.py
helm package charts/x --destination dist
package=$(ls dist/x-*.tgz)
helm install test "$package" --dry-run=client >/dev/null
echo "$out" | python3 -c '
import sys
sys.exit(0)
'
`
	want := []string{
		"succeeds: helm lint " + chart,
		`succeeds: helm template test charts/x --set a=1m --set-json list=[{"name":"t"}]`,
		`renders a line matching "--flag=1m": helm template test charts/x --set a=1m --set-json list=[{"name":"t"}]`,
		`renders no line matching "gone": helm template test charts/x --set a=1m --set-json list=[{"name":"t"}]`,
		`fails: helm template test charts/x --set value=:http --set-json args=[":http"]`,
		`fails: helm template test charts/x --set value=a=b --set-json args=["a=b"]`,
		`fails: helm template test charts/x --set value=plain --set-json args=["plain"]`,
		"renders what `python3 tools/check.py` accepts: helm template test charts/x --set many=true",
		"succeeds: helm package charts/x --destination <directory>",
		"succeeds: helm install test $package --dry-run=client",
		"renders what this Python accepts: helm template test charts/x --set a=1m --set-json list=[{\"name\":\"t\"}]\nimport sys\nsys.exit(0)",
	}
	sort.Strings(want)
	if got := sortedKeys(helmChecks(script, nil)); !equalStrings(got, want) {
		t.Fatalf("read from the workflow's shell:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The same checks as a Makefile writes them.
	makefile := "CHECK := 1\n" +
		"define PYTHON_CHECK\nimport sys\nsys.exit(0)\nendef\nexport PYTHON_CHECK\n\n" +
		"other:\n\thelm lint charts/other\n\n" +
		"helm-test: helm-version\n" +
		"\thelm lint charts/x\n" +
		"\t@# helm template never-run charts/x\n" +
		"\t@set -eu; \\\n" +
		"\tout=\"$$(helm template test charts/x --set a=1m --set-json 'list=[{\"name\":\"t\"}]')\"; \\\n" +
		"\techo \"$$out\" | grep -q -- '--flag=1m'; \\\n" +
		"\tif echo \"$$out\" | grep -q gone; then \\\n" +
		"\t\techo \"helm template rendered what it should not\" >&2; \\\n" +
		"\t\texit 1; \\\n" +
		"\tfi; \\\n" +
		"\techo \"$$out\" | python3 -c \"$$PYTHON_CHECK\"\n" +
		"\t@for bad in ':http' 'a=b' plain; do \\\n" +
		"\t\tif helm template test charts/x --set \"value=$$bad\" --set-json \"args=[\\\"$$bad\\\"]\" >/dev/null 2>&1; then \\\n" +
		"\t\t\techo \"helm template accepted $$bad\" >&2; \\\n" +
		"\t\t\texit 1; \\\n" +
		"\t\tfi; \\\n" +
		"\tdone\n" +
		"\thelm template test charts/x --set many=true | python3 tools/check.py\n" +
		"\t@set -e; dist=$$(mktemp -d); \\\n" +
		"\thelm package charts/x --destination \"$$dist\" >/dev/null; \\\n" +
		"\tpackage=$$(ls \"$$dist\"/x-*.tgz); \\\n" +
		"\thelm install test \"$$package\" --dry-run=client >/dev/null; \\\n" +
		"\trm -rf \"$$dist\"\n" +
		"\nci: helm-test\n\thelm lint charts/after\n"
	got := sortedKeys(helmChecks(makeRecipe(t, makefile, "helm-test"), makeDefinitions(makefile)))
	if !equalStrings(got, want) {
		t.Fatalf("read from the Makefile's recipe:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// helmCheckLists reads what each of the two lists checks: the helm-test
// recipe of a Makefile and the chart steps of a workflow, both as text.
func helmCheckLists(t *testing.T, makefile, workflow string) (local, ci map[string]bool) {
	t.Helper()
	return helmChecks(scriptOf(makeRecipeBlocks(makefile, "helm-test")), makeDefinitions(makefile)),
		helmChecks(scriptOf(ciChartBlocks(t, workflow)), nil)
}

// helmListDifferences names every check one of the two lists makes and the
// other does not.
func helmListDifferences(local, ci map[string]bool) []string {
	var findings []string
	for _, check := range sortedKeys(ci) {
		if !local[check] {
			findings = append(findings, "ci.yml checks this and `make helm-test` does not, so a local run would not mean a CI run:\n"+check)
		}
	}
	for _, check := range sortedKeys(local) {
		if !ci[check] {
			findings = append(findings, "`make helm-test` checks this and ci.yml does not, so CI would pass what a local run fails:\n"+check)
		}
	}
	return findings
}

// A chart change that passes `make helm-test`, or `make ci`, must not then
// fail CI on a case only the workflow knows, and the other way round. Both
// lists are compared whole: every render, every rejected value — the loopback
// listen addresses and the shutdown, grace period and webAuth values among
// them — every line looked for and the Python check of the webAuth monitor.
func TestMakeHelmTestChecksWhatTheCIWorkflowChecks(t *testing.T) {
	makefile, workflow := readHelmLists(t)
	local, ci := helmCheckLists(t, makefile, workflow)

	// Neither list may be empty or thin because the reader lost its way.
	kinds := map[string]int{}
	for check := range ci {
		kind, _, _ := strings.Cut(check, ":")
		kind, _, _ = strings.Cut(kind, ` "`)
		kinds[kind]++
	}
	for kind, least := range map[string]int{
		"succeeds": 21, "fails": 42, "renders a line matching": 22, "renders no line matching": 6,
		"renders what this Python accepts": 1, "renders what `python3 tools/check-manifests.py` accepts": 2,
	} {
		if kinds[kind] < least {
			t.Errorf("only %d checks of the kind %q were read from ci.yml, fewer than the %d it has had; the reader no longer understands the workflow", kinds[kind], kind, least)
		}
	}
	for _, check := range []string{
		"fails: helm template test charts/prometheus-universal-exporter --set server.listenAddress=127.0.0.1:8080",
		"fails: helm template test charts/prometheus-universal-exporter --set server.listenAddress=[::1]:8080",
		"fails: helm template test charts/prometheus-universal-exporter --set server.shutdownDelay=-1s",
		"fails: helm template test charts/prometheus-universal-exporter --set terminationGracePeriodSeconds=10",
		"fails: helm template test charts/prometheus-universal-exporter --set webAuth.enabled=true",
		`fails: helm template test charts/prometheus-universal-exporter --set-json extraArgs=["--dry-run"]`,
		`renders a line matching "terminationGracePeriodSeconds: 75": helm template test charts/prometheus-universal-exporter --set server.shutdownTimeout=1m`,
		`renders no line matching "terminationGracePeriodSeconds": helm template test charts/prometheus-universal-exporter`,
		"succeeds: helm lint charts/prometheus-universal-exporter",
	} {
		if !ci[check] {
			t.Errorf("ci.yml was not read as making this check, which it makes:\n%s", check)
		}
	}

	for _, finding := range helmListDifferences(local, ci) {
		t.Error(finding)
	}
}

// chartCasesMissing names each check of the chart that the Go tests make, and
// that both lists have to make too, which one of them lacks.
func chartCasesMissing(local, ci map[string]bool) []string {
	const template = "helm template test charts/prometheus-universal-exporter "
	monitor := func(kind string) string {
		return template + `--set-json monitors=[{"name":"t","enabled":true,"type":"` + kind + `","collector":"example","interval":"30s","scrapeTimeout":"10s","port":"grpc","namespaceSelector":{"matchNames":["a"]}}]`
	}
	const (
		recreate = template + "--set strategy.type=Recreate"
		indented = template + `--set-file config.data.extra\.yaml=testdata/chart/indented-first-line.yaml`
		parent   = "helm template test $tree/testdata/chart/parent --set prometheus-universal-exporter.enabled=true"
		shadow   = `--set-json extraVolumes=[{"name":"shadow","configMap":{"name":"shadow"}}] --set-json extraVolumeMounts=[{"name":"shadow","mountPath":"/etc/prometheus-universal-exporter/"}]`
		target   = template + "--set goGC.percent=400"
		numbers  = template + "--set-json replicaCount=2000000 --set-json server.probeMaxConcurrent=2e6 --set-json server.pythonMaxWorkers=2000000.0"
	)
	want := []string{
		"succeeds: " + recreate,
		`renders a line matching "type: Recreate": ` + recreate,
		`renders no line matching "rollingUpdate": ` + recreate,

		"succeeds: " + indented,
		`renders a line matching "^      # A file whose first line is indented": ` + indented,
		"renders what `python3 tools/check-manifests.py` accepts: " + indented,

		"succeeds: helm dependency build $tree/testdata/chart/parent",
		"succeeds: " + parent,
		`renders a line matching "^# Source: parent/charts/prometheus-universal-exporter/templates/deployment.yaml$": ` + parent,
		`renders a line matching "replicas: 2": ` + parent,

		"fails: " + template + `--set-json monitors=[{"name":"t","enabled":true,"type":"service","collector":"example","auth":{"enabled":true,"secretName":"s"}}]`,
		"fails: " + template + "--set selfMetrics.enabled=true --set selfMetrics.interval=10s --set selfMetrics.scrapeTimeout=5m",
		"fails: " + template + "--set ingress.enabled=true --set service.enabled=false",
		"fails: " + template + "--set webAuth.enabled=true --set webAuth.secretName=s --set webAuth.mountPath=/etc/prometheus-universal-exporter/",
		"fails: " + template + shadow,

		// The garbage collector's target: rendered as GOGC, absent by
		// default, and refused where it would collect nothing, is no target
		// or has two sources.
		"succeeds: " + target,
		`renders a line matching "name: GOGC": ` + target,
		`renders a line matching "value: \"400\"": ` + target,
		`renders no line matching "GOGC": ` + strings.TrimSpace(template),
		"fails: " + template + "--set-string goGC.percent=off --set goMemLimit.enabled=false",
		"fails: " + template + "--set goGC.percent=0",
		"fails: " + template + `--set goGC.percent=200 --set-json env=[{"name":"GOGC","value":"50"}]`,

		// A whole number is rendered as the whole number it is, however
		// it is written, where a template printed 2e+06 by itself; one
		// past what a count may be is refused.
		"succeeds: " + numbers,
		`renders a line matching "replicas: 2000000$": ` + numbers,
		`renders a line matching "--probe.max-concurrent=2000000\"": ` + numbers,
		`renders a line matching "--python.max-workers=2000000\"": ` + numbers,
		`renders no line matching "e+0": ` + numbers,
		"fails: " + template + "--set-json replicaCount=2147483648",

		// A monitor's port is a port's name, never its number.
		"fails: " + template + `--set-json monitors=[{"name":"t","enabled":true,"type":"service","collector":"example","port":"9115"}]`,
	}
	for _, kind := range []string{"service", "pod"} {
		want = append(want,
			"succeeds: "+monitor(kind),
			`renders a line matching "port: \"grpc\"": `+monitor(kind),
			`renders a line matching "namespaceSelector:": `+monitor(kind),
			`renders a line matching "^    - a$": `+monitor(kind),
		)
	}
	want = append(want, `renders a line matching "kind: PodMonitor": `+monitor("pod"))
	var findings []string
	for _, check := range want {
		if !local[check] {
			findings = append(findings, "`make helm-test` does not make this check of the chart"+chartCaseAdvice(check, local))
		}
		if !ci[check] {
			findings = append(findings, "ci.yml does not make this check of the chart"+chartCaseAdvice(check, ci))
		}
	}
	return findings
}

// chartCaseAdvice says, of a chart case a list lacks, why the list has to
// make it and what to write: the check as the comparison reads it, and beside
// it the check the list makes with every word of it and more — an option
// added to its helm command, say — when there is one, the nearest first,
// since that is then the check that was meant.
func chartCaseAdvice(check string, made map[string]bool) string {
	advice := ", which the Go tests make and are skipped without helm, so both lists have to make it too, with this command line and this pattern (chartCasesMissing in test/repository/helmtest_test.go lists them; a case changed on purpose is changed there and in both lists):\n" + check
	words := strings.Fields(check)
	nearest, extra := "", 0
	for _, other := range sortedKeys(made) {
		otherWords := strings.Fields(other)
		if otherWords[0] != words[0] || (nearest != "" && len(otherWords)-len(words) >= extra) {
			continue
		}
		if !slices.ContainsFunc(words, func(word string) bool { return !slices.Contains(otherWords, word) }) {
			nearest, extra = other, len(otherWords)-len(words)
		}
	}
	if nearest != "" {
		advice += "\nthe list makes this one, which is another check to the comparison:\n" + nearest
	}
	return advice
}

// The chart refuses and renders things that only the Go tests looked at: the
// Recreate strategy without a rollingUpdate, a monitor's port and namespaces,
// a configuration file whose first line is indented, the chart as a dependency
// of a parent chart, the garbage collector's target, whole numbers written
// out however they were given, and the refusals of a count past 2147483647,
// of a monitor's auth without a type, of a monitor's port given as a number, of a
// scrape timeout longer than its interval, of an Ingress without the Service,
// of a mount at the configuration directory written with a trailing slash and
// of a garbage collector target that is off with no memory limit, 0 or beside
// GOGC in env. The Go tests are skipped without helm, and neither `make
// helm-test` nor the chart steps of ci.yml are, so each case has to be in both
// lists too, a render kept and looked at rather than piped, since a pipeline
// hides helm failing. The garbage collector's cases were in neither list.
func TestMakeHelmTestAndCICheckTheChartCasesTheGoTestsCover(t *testing.T) {
	makefile, workflow := readHelmLists(t)
	for _, finding := range chartCasesMissing(helmCheckLists(t, makefile, workflow)) {
		t.Error(finding)
	}
}

// fakeTool writes an executable shell script into dir.
func fakeTool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // a script has to be executable
		t.Fatal(err)
	}
}

// runMake runs make targets from the repository root with dir first on PATH.
func runMake(t *testing.T, dir string, targets ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("no make")
	}
	cmd := exec.Command("make", append([]string{"--no-print-directory"}, targets...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_BIN="+dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func pinnedHelmVersion(t *testing.T) string {
	t.Helper()
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^HELM_VERSION[ \t]*:?=[ \t]*(v[0-9]+\.[0-9]+\.[0-9]+)$`).FindSubmatch(makefile)
	if match == nil {
		t.Fatal("the Makefile no longer pins HELM_VERSION")
	}
	return string(match[1])
}

// helm built from its source by `go install` reports its release line, "v4.3":
// the full version is a variable (internal/version.version) that helm's release
// build sets with -X, and `make helm-version` rightly accepts only the full
// one. So `make helm-install` has to set it too, or it installs a helm that
// `make helm-test` then refuses. The real install needs the network, so a
// stand-in for `go` builds what the real one would: a helm that reports the
// version stamped into it, and the release line when none is.
func TestMakeHelmInstallInstallsAHelmThatHelmVersionAccepts(t *testing.T) {
	pinned := pinnedHelmVersion(t)
	dir := t.TempDir()
	fakeTool(t, dir, "go", `[ "$1" = install ] || { echo "unexpected: go $*" >&2; exit 1; }
shift
stamp= package=
while [ $# -gt 0 ]; do
	flags=
	case "$1" in
		-ldflags) shift; flags="$1" ;;
		-ldflags=*) flags="${1#-ldflags=}" ;;
		helm.sh/helm/v4/cmd/helm@*) package="$1" ;;
	esac
	case "$flags" in
		*"-X helm.sh/helm/v4/internal/version.version="*)
			stamp="${flags##*-X helm.sh/helm/v4/internal/version.version=}"
			stamp="${stamp%% *}" ;;
	esac
	shift
done
[ -n "$package" ] || { echo "go install was not asked for helm" >&2; exit 1; }
release="${package##*@}"
printf '#!/bin/sh\necho "%s"\n' "${stamp:-${release%.*}}" > "$FAKE_BIN/helm"
chmod +x "$FAKE_BIN/helm"
`)
	out, err := runMake(t, dir, "helm-install", "helm-version")
	if err != nil {
		t.Fatalf("`make helm-install helm-version` failed, so the helm it installs is one `make helm-test` refuses: %v\n%s", err, out)
	}
	installed, err := exec.Command(filepath.Join(dir, "helm"), "version", "--short").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(installed)); got != pinned {
		t.Fatalf("the installed helm reports %q, want the pinned %s", got, pinned)
	}
}

// The version check itself stays strict: the release line a source build
// reports, another patch release and another minor are refused with the
// command that fixes it, and a release build's commit suffix is no difference.
func TestMakeHelmVersionAcceptsOnlyThePinnedRelease(t *testing.T) {
	pinned := pinnedHelmVersion(t)
	line := pinned[:strings.LastIndex(pinned, ".")]
	for reported, accepted := range map[string]bool{
		pinned:               true,
		pinned + "+g1234abc": true,
		line:                 false,
		pinned + "1":         false,
		line + ".99":         false,
		"v3.16.4+gabcdef0":   false,
	} {
		t.Run(reported, func(t *testing.T) {
			dir := t.TempDir()
			fakeTool(t, dir, "helm", `echo "`+reported+`"`+"\n")
			out, err := runMake(t, dir, "helm-version")
			if accepted {
				if err != nil {
					t.Fatalf("a helm reporting %s was refused: %v\n%s", reported, err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("a helm reporting %s was accepted", reported)
			}
			want := "helm is " + strings.SplitN(reported, "+", 2)[0] + " but CI runs " + pinned + "; run: make helm-install"
			if !strings.Contains(out, want) {
				t.Fatalf("the refusal does not say %q:\n%s", want, out)
			}
		})
	}
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
