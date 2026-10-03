package repository

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
// make and GitHub would have it — and whether the block gets to the check at
// all: an `exit 0` above it, an `if false` around it, a `trap` that leaves
// with 0, a function named helm and a job that never runs each left every
// check as it was written, and none of them made.

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

// failsTheShell reports whether a command ends the shell with a failure,
// whatever came before it: `exit` with a status from 1 to 255. A shell keeps
// the low eight bits of a status, so `exit 256` is `exit 0`.
func failsTheShell(words []string) bool {
	if len(words) != 2 || words[0] != "exit" {
		return false
	}
	status, err := strconv.ParseUint(words[1], 10, 8)
	return err == nil && status != 0 && !strings.HasPrefix(words[1], "0")
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

// openConstruct is an `if`, a `for` or the `{ }` after a check's `||` that
// the walk is inside of.
type openConstruct struct {
	keyword string
	pos     int
	// must says what the condition of an `if` checks, when it is a helm
	// check: a render that must be refused or a text that must be absent.
	must string
	// branch is the part of an `if` being read: empty for its condition,
	// then or else.
	branch string
	// exits says whether the then-branch, or the `{ }`, ends the shell with a
	// failure, and decided that the first `exit` in it has been read.
	exits, decided bool
	// seen is how many findings there were when an `if` was opened, and
	// reported that its condition added one: what is wrong with it has been
	// said, and the `exit` of its then-branch is not held against it again.
	seen     int
	reported bool
}

// loopsOverValues reports whether a `for` is the loop the checks use: `for
// name in` and at least one value, each written out, so that the body runs
// and runs for those values. A variable or a command among them can be
// nothing, and a loop over nothing makes no check.
func loopsOverValues(tokens []shellToken) bool {
	if len(tokens) < 4 || tokens[2].expand(nil) != "in" {
		return false
	}
	for _, token := range tokens[1:] {
		if token.op != "" {
			return false
		}
		for _, piece := range token.pieces {
			if piece.expandable && strings.ContainsAny(piece.text, "$`") {
				return false
			}
		}
	}
	return true
}

// writeItAs is the advice for a render looked at in a way the reader does not
// take for a check: the one way it does.
func writeItAs(variable, pattern string) string {
	if variable == "" {
		variable = "out"
	}
	if pattern == "" {
		pattern = "<pattern>"
	}
	return `write it as: echo "$` + variable + `" | grep -q -- '` + strings.ReplaceAll(pattern, "'", `'\''`) + `'`
}

// grepPattern returns the pattern of a grep command: its first word that is
// not an option.
func grepPattern(words []string) string {
	for _, word := range words[1:] {
		if !strings.HasPrefix(word, "-") {
			return word
		}
	}
	return ""
}

// unknownRead names a way of looking at a render that is no check to either
// reader, and says what to write instead: a here-string, a `grep` that reads
// a file, a kept render compared by `[[`, `[` or `test`, and a kept render
// printed in another way than printedVariable knows. Left alone, each would
// only show as a check the other list makes and this one does not.
func unknownRead(words []string, kept map[string]bool) string {
	if i := slices.Index(words, "<<<"); i >= 0 {
		variable := ""
		if i+1 < len(words) {
			variable = variableName(words[i+1])
		}
		return "a here-string (`<<<`) is not a way to read a render that the reader of the helm checks knows, and the sh that make runs a recipe in does not have it; " + writeItAs(variable, grepPattern(words[:i]))
	}
	stage := words
	if i := slices.Index(words, "|"); i >= 0 {
		stage = words[:i]
	}
	keptWord := func(word string) bool { return kept[variableName(word)] }
	switch words[0] {
	case "grep":
		return "this `grep` reads a file or what the step is given, not a kept render, so the reader of the helm checks cannot tell which render it looks at and takes it for no check; keep the render (out=\"$(helm ...)\") and " + writeItAs("", grepPattern(stage))
	case "[[", "[", "test":
		for i, word := range words {
			if keptWord(word) && i+2 < len(words) && slices.Contains([]string{"=", "==", "!=", "=~"}, words[i+1]) {
				return "a kept render is compared with `" + words[0] + "`, which holds the whole render against a pattern of the shell's rather than looking for a line, and is no check the reader of the helm checks knows; " + writeItAs(variableName(word), strings.Trim(words[i+2], "*"))
			}
		}
	case "echo", "printf":
		if i := slices.IndexFunc(stage, keptWord); i >= 0 && len(stage) < len(words) && printedVariable(stage) == "" {
			return "a kept render is printed with `" + strings.Join(stage, " ") + "`, which the reader of the helm checks does not know to hand the reader the render as it is; " + writeItAs(variableName(stage[i]), grepPattern(words[len(stage)+1:]))
		}
	}
	return ""
}

var functionDefinition = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*\(\)`)

// unfollowed names a command after which the reader cannot say what the
// commands of the block are, or whether they run: a function or an alias,
// which can stand in for `helm`, `grep`, `python3` or `exit`; a file of
// commands read in, and a PATH or a `hash` of another's choosing, which can
// too; `break` and `continue`, which leave a loop before its checks; and
// `return`, `exec` and a `trap`, which end the block or change what it ends
// with. The one trap it knows is the clean-up both lists have, `trap 'rm -rf
// "$dir"' EXIT`: an `rm` alone, which leaves the block's result as it was.
func unfollowed(script string, tokens []shellToken, words []string) string {
	switch {
	case words[0] == "function", words[0] == "alias", functionDefinition.MatchString(words[0]),
		len(tokens) > 1 && tokens[1].op == "" && script[tokens[1].pos] == '(':
		return "a function or an alias can stand in for `helm`, `grep`, `python3` or `exit`, and the reader of the helm checks does not follow one; write the commands out where they run"
	case words[0] == "." || words[0] == "source":
		return "`" + words[0] + "` reads commands the reader of the helm checks does not see, which can define a `helm` or a `grep` of their own; write the commands out"
	case strings.HasPrefix(words[0], "PATH=") || words[0] == "export" && len(words) > 1 && (words[1] == "PATH" || strings.HasPrefix(words[1], "PATH=")):
		return "a PATH set in the block decides which `helm`, `grep` and `python3` the checks run, which the reader of the helm checks cannot follow; leave PATH as the step or the recipe is given it"
	case words[0] == "hash":
		return "`hash` can name another program as `helm`, `grep` or `python3`, which the reader of the helm checks cannot follow; leave the commands to be found on the PATH"
	case words[0] == "break" || words[0] == "continue":
		return "`" + words[0] + "` leaves the loop, or this turn of it, before the checks after it are made; a loop of checks makes each for every value"
	case words[0] == "return":
		return "`return` outside a function is an error in one shell and ends the block in another, so the checks after it cannot be counted on; a block ends at its last command"
	case words[0] == "exec":
		return "`exec` replaces the shell, so no check after it is made and its result is the block's; run the command without it"
	case words[0] == "trap":
		if len(words) == 3 && words[2] == "EXIT" {
			if action := shellCommands(words[1]); len(action) == 1 && action[0].end == ";" && action[0].tokens[0].expand(nil) == "rm" && !slices.ContainsFunc(action[0].tokens, func(token shellToken) bool { return token.op != "" }) {
				return ""
			}
		}
		return "a `trap` runs its commands when the shell ends or a command fails, and those can end the block with another result than its checks'; the one trap the reader of the helm checks knows is a clean-up, `trap 'rm -rf \"$dir\"' EXIT`"
	}
	return ""
}

// ineffectiveHelmChecks reads one block as its shell would and returns a
// finding for every helm check in it whose failure would not fail the block,
// or that is not made at all:
//
//   - a render that must succeed, or a line that must be in a render, written
//     as a plain command, where nothing ends the shell when it fails — no
//     `set -e` is in force and it is not the block's last command — or where
//     `||`, `&&` or `&` after it takes its result away. `|| exit 1` and
//     `|| { ...; exit 1; }` do not: the failure ends the shell there;
//   - a render that must be refused, or a text that must be absent, written as
//     the condition of an `if`, whose then-branch does not `exit 1`;
//   - a render read through a pipe that hides helm failing: piped into a
//     condition, or into a reader that does not fail on nothing, or kept from
//     more than the one helm command;
//   - a check that is not made on every run of its block. A check stands at
//     the top of the block, in the body of a `for` over values written out
//     (loopsOverValues), or is the condition of an `if` that stands there;
//     one inside a branch of an `if`, whatever the condition, in the `{ }`
//     after a `||`, or after `&&` or `||` is made only when something else
//     went one way, and one after an `exit` is not made. So an `exit`, of any
//     status, belongs to a check — in the then-branch of the `if` whose
//     condition the check is, or after its `||` — and any other is reported;
//   - a block whose commands the reader cannot follow (unfollowed), or whose
//     `set` turns `-e` off or sets an option the reader does not know;
//   - a block whose failure is ignored where it is run.
//
// It knows the shell these checks are written in and reports what it does not
// know rather than passing it, saying what to write where a render is looked
// at in another way than the checks do (unknownRead). weighed is how many
// checks it found to ask the question of, a loop's once.
func ineffectiveHelmChecks(block helmBlock) (findings []string, weighed int) {
	report := func(pos int, problem string) {
		findings = append(findings, block.finding(pos, problem))
	}
	commands := shellCommands(block.script)
	var open []openConstruct
	errexit := block.errexit
	kept := map[string]bool{}
	unprotected, expectThen := false, false
	previousEnd, before := ";", ";"
	// guard is the command a check hands its failure to with `||`, and
	// guardPos where that check is.
	guard, guardPos := -1, 0

	// everyRun reports a check, at pos, that an enclosing `if` or `{ }` has
	// made only on some runs of the block. A check that is the condition of
	// an `if` is not inside that `if`.
	everyRun := func(pos int, condition bool) {
		enclosing := open
		if condition {
			enclosing = open[:len(open)-1]
		}
		for _, construct := range enclosing {
			if construct.keyword == "for" {
				continue
			}
			inside := "a branch of the `if`"
			if construct.keyword == "{" {
				inside = "the `{ }` after the `||`"
			}
			report(pos, fmt.Sprintf("this check is in %s of line %d, so it is made only when that is run, not on every run of the block; a check stands at the top of its block, in a `for` over values written out, or is the condition of an `if` that stands there", inside, block.line+strings.Count(block.script[:construct.pos], "\n")))
			return
		}
	}

	// plain is a check written as a command of its own, at pos.
	plain := func(index, pos int) {
		weighed++
		everyRun(pos, false)
		if before == "&&" || before == "||" {
			report(pos, "this check comes after `"+before+"`, so it is made only when the command before that went one way; write it on a line of its own")
		}
		command := commands[index]
		switch command.end {
		case "||":
			if index+1 < len(commands) {
				next := commands[index+1].tokens
				if failsTheShell(shellWords(next)) || next[0].op == "" && next[0].expand(nil) == "{" {
					guard, guardPos = index+1, pos
					return
				}
			}
			report(pos, "the `||` after this check takes its failure away, so it can never fail; what may follow a check's `||` is `exit 1`, alone or last in `{ echo \"...\" >&2; exit 1; }`")
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
		before, previousEnd = previousEnd, command.end
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
			case "if", "for":
				if before == "&&" || before == "||" {
					report(pos, "this `"+word+"` comes after `"+before+"`, so its checks are made only when the command before that went one way; write it on a line of its own")
				}
				open = append(open, openConstruct{keyword: word, pos: pos, seen: len(findings)})
				if word == "for" {
					if !loopsOverValues(tokens) {
						report(pos, "this loop is not one over values written out, `for name in value ...; do` with at least one value and no variable or command among them, so the reader of the helm checks cannot say that its checks are made: a loop over nothing makes none")
					}
					tokens = nil
					break keywords
				}
				condition = true
			case "then", "else":
				if top < 0 || open[top].keyword != "if" {
					report(pos, "`"+word+"` belongs to no `if` the reader of the helm checks has seen")
					break keywords
				}
				if word == "then" {
					open[top].reported = len(findings) > open[top].seen
				}
				open[top].branch = word
			case "do":
			case "{":
				if index != guard {
					report(pos, "the reader of the helm checks does not understand `{` but after the `||` of a check, as `check || { echo \"...\" >&2; exit 1; }`; write a render that must succeed as a command of its own, and one that must be refused as `if helm ...; then ...; exit 1; fi`")
					tokens = nil
					break keywords
				}
				open = append(open, openConstruct{keyword: "{", pos: guardPos})
				// The first command in it runs whenever the `{ }` does, and
				// an `exit` there is the `{ }`'s own.
				guard, before = -1, ";"
			case "fi", "done", "}":
				keyword := map[string]string{"fi": "if", "done": "for", "}": "{"}[word]
				if top < 0 || open[top].keyword != keyword {
					if word == "}" {
						// The end of a function, which is reported where it starts.
						break keywords
					}
					report(pos, "`"+word+"` closes no `"+keyword+"` the reader of the helm checks has seen")
					break keywords
				}
				closed := open[top]
				open = open[:top]
				switch {
				case closed.exits:
				case closed.keyword == "{":
					report(closed.pos, "the commands in the `{ }` after the `||` of this check do not end the shell with a failure, so the `||` takes the check's failure away and it can never fail; end them with `exit 1`")
				case closed.must != "":
					report(closed.pos, closed.must+", and nothing fails when it is not: the commands after `then` do not end with `exit 1`")
				}
				switch {
				case len(tokens) > 1 || command.end == "&":
					// `fi | cat` runs the whole `if` in a subshell, which is
					// all an `exit 1` inside it then leaves.
					report(pos, "what follows `"+word+"` runs the commands before it in a shell of their own, whose `exit 1` fails nothing")
				case command.end != ";" && word != "}":
					report(pos, "the `"+command.end+"` after `"+word+"` has the shell carry on when a command before it fails, under `set -e` too")
				}
				tokens = nil
				break keywords
			case "!", "elif", "while", "until", "case", "eval", "(":
				report(pos, "the reader of the helm checks does not understand `"+word+"`; write a render that must succeed as a command of its own, and one that must be refused as `if helm ...; then ...; exit 1; fi`")
				tokens = nil
				break keywords
			default:
				break keywords
			}
			tokens, words = tokens[1:], words[1:]
		}
		top := len(open) - 1
		if len(tokens) > 0 {
			// A finding names the line of the command, not of a `then` or a
			// `do` a continued line has before it.
			pos = tokens[0].pos
		}
		if condition {
			expectThen = true
			if command.end != ";" {
				report(pos, "the condition of the `if` goes on after `"+command.end+"`, so it is not the check's result alone; an `if` of these checks has the one check for its condition")
			}
		}
		if len(tokens) == 0 {
			continue
		}

		if slices.Contains(words, "<<") {
			// The lexer reads the lines of a here-document as the commands
			// they look like, and the shell runs none of them.
			report(pos, "the lines of a here-document (`<<`) are text handed to a command, not commands the shell runs, and the reader of the helm checks cannot tell them from the block's own; write the checks as commands of the block")
			continue
		}
		if problem := unfollowed(block.script, tokens, words); problem != "" {
			report(pos, problem)
			continue
		}
		switch words[0] {
		case "set":
			// set -e, -eu and -o errexit turn it on, and the same with a
			// plus sign off, which no block of checks has a use for. The
			// other options the checks are known under are -u, -x, -v and
			// pipefail; -n, for one, has the shell run nothing.
			for i := 1; i < len(words) && words[i] != "--"; i++ {
				flag := words[i]
				if len(flag) < 2 || !strings.ContainsRune("-+", rune(flag[0])) {
					continue
				}
				for _, letter := range flag[1:] {
					option := string(letter)
					if letter == 'o' && i+1 < len(words) {
						i++
						option = words[i]
					}
					switch option {
					case "e", "errexit":
						if errexit = flag[0] == '-'; !errexit {
							report(pos, "`set "+flag+"` turns `set -e` off, so a check that fails after it does not end the shell; leave `set -e` on for the whole block")
						}
					case "u", "nounset", "x", "xtrace", "v", "verbose", "pipefail":
					default:
						report(pos, "the reader of the helm checks does not know what `set "+flag[:1]+option+"` does to the checks after it; the options it knows are -e, -u, -x, -v and -o pipefail")
					}
				}
			}
			continue
		case "exit":
			switch {
			case index == guard:
				// `check || exit 1`, which the check has answered for, unless
				// the whole of it is sent to the background.
				if command.end == "&" {
					report(guardPos, "this check and the `exit` after its `||` run in the background, where its failure is lost")
				}
			case top >= 0 && (open[top].keyword == "{" || open[top].branch == "then" && (open[top].must != "" || open[top].reported)):
				// The first exit of a then-branch, or of the `{ }` after a
				// `||`, decides what it is worth, and only one that runs
				// whatever the command before it returned.
				if !open[top].decided {
					open[top].decided = true
					open[top].exits = failsTheShell(words) && before == ";"
				}
			default:
				report(pos, "this `exit` belongs to no check: it is neither in the `then` branch of an `if` whose condition is a helm check nor after the `||` of one. Anywhere else an `exit` ends the block before the checks after it are made, whatever its status, or fails it only when something else went one way; write a refusal as `if helm ...; then ...; exit 1; fi`, and let a block end at its last command")
			}
			continue
		}
		if problem := unknownRead(words, kept); problem != "" {
			report(pos, problem)
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
		// advice is what to write in place of a grep that is not the checks'.
		advice := ""
		if len(reader) > 0 && reader[0] == "grep" && reader[len(reader)-1] != "" && !strings.HasPrefix(reader[len(reader)-1], "-") {
			advice = "; " + writeItAs(printedVariable(first), reader[len(reader)-1])
		}
		fromHelm := len(first) > 0 && first[0] == "helm"
		fromKept := printedVariable(first) != ""
		switch {
		case !fromHelm && !fromKept, fromKept && len(stages) == 1:
			continue
		case len(stages) > 2:
			report(pos, "a pipeline answers for its last command only, and this one has more than two; keep the render first and read it with one command")
		case fromHelm && len(stages) == 1 && condition:
			open[top].must = "this render must be refused"
			weighed++
			everyRun(pos, true)
		case fromHelm && len(stages) == 1:
			plain(index, pos)
		case fromHelm && condition:
			report(pos, "a render is piped into a condition, which a render that failed passes as an absence; keep it first (out=\"$(helm ...)\") and read \"$out\"")
		case fromHelm && !isGrepQuiet(reader) && !isManifestCheck(reader):
			report(pos, "helm is piped into `"+strings.Join(reader, " ")+"`, which does not fail when helm does; keep the render first (out=\"$(helm ...)\") and read \"$out\"")
		case fromHelm:
			plain(index, pos)
		case !kept[printedVariable(first)]:
			if len(reader) > 0 && (reader[0] == "grep" || reader[0] == "python3") {
				report(pos, "`"+first[len(first)-1]+"` is read, and no render was kept in it by a name=\"$(helm ...)\" before")
			}
		case condition && isGrepQuiet(reader):
			open[top].must = "this text must be absent from the render"
			weighed++
			everyRun(pos, true)
		case condition:
			report(pos, "the condition reads a render with `"+strings.Join(reader, " ")+"`; a text that must be absent is looked for with `grep -q` and a pattern"+advice)
		case isGrepQuiet(reader), isManifestCheck(reader), len(reader) == 3 && reader[0] == "python3" && reader[1] == "-c":
			plain(index, pos)
		default:
			report(pos, "a render is read by `"+strings.Join(reader, " ")+"`, which is no check the reader of the helm checks knows: `grep -q` with a pattern, `python3 -c` or tools/check-manifests.py"+advice)
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

// makeDefaults are the special targets and variables of a Makefile that
// change how every recipe is run, with what each does.
var makeDefaults = map[string]string{
	".IGNORE":     "has make ignore every recipe line that fails",
	".ONESHELL":   "runs a recipe in one shell, where only its last command's result counts",
	".SHELLFLAGS": "changes the shell every recipe line runs in, which the reader of the helm checks takes to be `sh -c`",
	"MAKEFLAGS":   "can have make ignore every recipe line that fails (-i) or run none (-n), which the reader of the helm checks cannot follow",
	"PATH":        "decides which `helm`, `grep` and `python3` every recipe line runs, which the reader of the helm checks cannot follow",
}

var (
	makeDefault = regexp.MustCompile(`^(?:(?:export|override)\s+)*(\.IGNORE|\.ONESHELL|\.SHELLFLAGS|MAKEFLAGS|PATH)\s*(?::|\+?=|\?=)`)
	makeShell   = regexp.MustCompile(`^(?:(?:export|override)\s+)*SHELL\s*[:+?!]?=(.*)$`)
	// helmTestRule is a line that gives the helm-test target a rule, or a
	// variable of its own.
	helmTestRule = regexp.MustCompile(`^(?:[^\t#:=]*\s)?helm-test(?:\s[^:=]*)?:`)
)

// makeDefaultsFindings names what in a Makefile changes how every recipe is
// run, which the reader of the recipe takes to be make's own way: a shell for
// each line, sh or bash without -e, with the PATH make was given, and a
// failing line failing the target. It also names a second rule for the
// helm-test target: make runs the recipe written last, and the reader reads
// the first.
func makeDefaultsFindings(makefile string) []string {
	var findings []string
	rules := 0
	for i, line := range strings.Split(makefile, "\n") {
		if match := makeDefault.FindStringSubmatch(line); match != nil {
			findings = append(findings, fmt.Sprintf("Makefile:%d: %s %s:\n\t%s", i+1, match[1], makeDefaults[match[1]], line))
		}
		if match := makeShell.FindStringSubmatch(line); match != nil {
			if shell := strings.TrimSpace(match[1]); strings.ContainsAny(shell, " \t") || (filepath.Base(shell) != "sh" && filepath.Base(shell) != "bash") {
				findings = append(findings, fmt.Sprintf("Makefile:%d: SHELL is the program every recipe line is run by, which the reader of the helm checks takes to be sh or bash with no option of its own; another program need not run the checks at all:\n\t%s", i+1, line))
			}
		}
		if helmTestRule.MatchString(line) {
			if rules++; rules > 1 {
				findings = append(findings, fmt.Sprintf("Makefile:%d: the helm-test target is given a second rule; make runs the recipe written last and applies a variable set here to every line of it, and the reader of the helm checks reads the first rule only, so write the target once:\n\t%s", i+1, line))
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
// workflow's — is a command without the flag, or one with a word GitHub's own
// command for bash and sh does not have: `bash -n -e {0}` has the flag and
// runs nothing, and passed as a shell that ends on a failure. Such a `shell:`
// is reported where it is written, the block's checks aside.
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
		"GitHub's own for bash":     {"jobs:\n  test:\n    steps:\n" + step("        shell: bash --noprofile --norc -eo pipefail {0}\n"), true},
		"errexit by its name":       {"jobs:\n  test:\n    steps:\n" + step("        shell: bash -o errexit {0}\n"), true},
		"the flag and no script":    {"jobs:\n  test:\n    steps:\n" + step("        shell: bash -e\n"), false},
		"the flag and -n":           {"jobs:\n  test:\n    steps:\n" + step("        shell: bash -n -e {0}\n"), false},
		"the flag in one with -n":   {"jobs:\n  test:\n    steps:\n" + step("        shell: bash -en {0}\n"), false},
		"the flag and a command":    {"jobs:\n  test:\n    steps:\n" + step("        shell: bash -e -c true {0}\n"), false},
		"an option with no name":    {"jobs:\n  test:\n    steps:\n" + step("        shell: bash -eo {0}\n"), false},
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
		_, shell, _ := strings.Cut(tc.workflow, "shell: ")
		shell, _, _ = strings.Cut(shell, "\n")
		if named := strings.Contains(strings.Join(findings, "\n"), "the shell is not bash or sh started with -e, as GitHub starts the shell of a step that names none, so a failing check does not end a chart step; leave `shell:` out, or write it as `bash -e {0}`:\n\tshell: "+shell); named == tc.errexit {
			t.Errorf("%s: the findings %q, want the shell named in one only without -e", name, findings)
		}
	}
}

// The garbage collector's checks as each file writes them, and a refusal of
// the workflow: the text several of the changes below are made to.
const (
	workflowGCChecks = "          out=\"$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\"\n" +
		"          echo \"$out\" | grep -q 'name: GOGC'\n" +
		"          echo \"$out\" | grep -q 'value: \"400\"'\n"
	recipeGCChecks = "\tout=\"$$(helm template test charts/prometheus-universal-exporter --set goGC.percent=400)\"; \\\n" +
		"\techo \"$$out\" | grep -q 'name: GOGC'; \\\n" +
		"\techo \"$$out\" | grep -q 'value: \"400\"'; \\\n"
	workflowGCRefusal = "          if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then\n" +
		"            echo \"helm template accepted a goGC.percent of 0\" >&2\n" +
		"            exit 1\n" +
		"          fi\n"
	gcStep = `the step "Render the garbage collector's target"`
)

// helmCheckMutations are changes that leave a helm check in place and take
// its teeth, or a case, away, each made to one of the two files: the ones two
// reviews made by hand and found unnoticed, and more of their kind. The
// second review's are from the first comment on: the reader weighed each
// check where it stood and did not ask whether the block reaches it, so an
// early `exit 0`, an `if false` around the checks, a `trap 'exit 0' EXIT`,
// `helm` as a function and an `if: false` on the job all passed. want is
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
	// A check that is never made: the block ends before it, or it stands
	// where the block does not always go.
	{
		"a step of the workflow exits with 0 before its checks", "ci.yml",
		workflowGCChecks, "          exit 0\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "this `exit` belongs to no check", "\texit 0"},
	},
	{
		"a recipe line exits with 0 before its checks", "Makefile",
		recipeGCChecks, "\texit 0; \\\n" + recipeGCChecks,
		[]string{"Makefile:", "the helm-test recipe", "this `exit` belongs to no check", "\texit 0;"},
	},
	{
		"a step of the workflow exits behind a command that never fails", "ci.yml",
		workflowGCChecks, "          true || exit 0\n          exit\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "this `exit` belongs to no check", "\texit"},
	},
	{
		"a step of the workflow returns before its checks", "ci.yml",
		workflowGCChecks, "          return 0 2>/dev/null\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "`return` outside a function", "return 0 2>/dev/null"},
	},
	{
		"a step of the workflow replaces its shell before its checks", "ci.yml",
		workflowGCChecks, "          exec true\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "`exec` replaces the shell", "exec true"},
	},
	{
		"the checks of a step are wrapped in if false", "ci.yml",
		workflowGCChecks, "          if false; then\n" + workflowGCChecks + "          fi\n",
		[]string{".github/workflows/ci.yml:", gcStep, "this check is in a branch of the `if` of line", "echo \"$out\" | grep -q 'value: \"400\"'"},
	},
	{
		"the checks of a recipe line are wrapped in if false", "Makefile",
		recipeGCChecks, "\tif false; then \\\n" + recipeGCChecks + "\tfi; \\\n",
		[]string{"Makefile:", "the helm-test recipe", "this check is in a branch of the `if` of line", "--set goGC.percent=400"},
	},
	{
		"the checks of a step are made only where a variable is set", "ci.yml",
		workflowGCChecks, "          if [ -n \"${CHECK_GC:-}\" ]; then\n" + workflowGCChecks + "          fi\n",
		[]string{".github/workflows/ci.yml:", gcStep, "this check is in a branch of the `if` of line", "--set goGC.percent=400"},
	},
	{
		"the checks of a step are in the else of a condition that holds", "ci.yml",
		workflowGCChecks, "          if true; then\n            :\n          else\n" + workflowGCChecks + "          fi\n",
		[]string{".github/workflows/ci.yml:", gcStep, "this check is in a branch of the `if` of line", "--set goGC.percent=400"},
	},
	{
		"a check is moved into the then-branch of a refusal", "ci.yml",
		"            echo \"helm template accepted a goGC.percent of 0\" >&2\n",
		"            helm template test charts/prometheus-universal-exporter --set goGC.percent=300 >/dev/null\n            echo \"helm template accepted a goGC.percent of 0\" >&2\n",
		[]string{".github/workflows/ci.yml:", "this check is in a branch of the `if` of line", "--set goGC.percent=300"},
	},
	{
		"the exit 1 of a refusal runs only in an if false", "ci.yml",
		workflowGCRefusal, strings.Replace(workflowGCRefusal, "            exit 1\n", "            if false; then\n              exit 1\n            fi\n", 1),
		[]string{".github/workflows/ci.yml:", "this `exit` belongs to no check", "\texit 1"},
	},
	{
		"the checks of a step are in a loop over nothing", "ci.yml",
		workflowGCChecks, "          for never in; do\n" + workflowGCChecks + "          done\n",
		[]string{".github/workflows/ci.yml:", gcStep, "this loop is not one over values written out", "for never in; do"},
	},
	{
		"a loop of refusals runs over a variable", "Makefile",
		"@for address in ':http' '9115' '0.0.0.0' ':0' '127.0.0.1:8080' 'localhost:8080' '[::1]:8080'; do",
		"@for address in $$ADDRESSES; do",
		[]string{"Makefile:", "the helm-test recipe", "this loop is not one over values written out", "for address in $ADDRESSES; do"},
	},
	{
		"a loop of refusals of the recipe moves on before its check", "Makefile",
		"\t@for address in ':http' '9115' '0.0.0.0' ':0' '127.0.0.1:8080' 'localhost:8080' '[::1]:8080'; do \\\n",
		"\t@for address in ':http' '9115' '0.0.0.0' ':0' '127.0.0.1:8080' 'localhost:8080' '[::1]:8080'; do \\\n\t\tcontinue; \\\n",
		[]string{"Makefile:", "the helm-test recipe", "`continue` leaves the loop, or this turn of it, before the checks after it are made", "\tcontinue;"},
	},
	{
		"a loop of refusals of the workflow ends before its check", "ci.yml",
		"          for oneshot in --dry-run --config.schema --config.collector-file-schema --static-targets-file-schema --version --help; do\n",
		"          for oneshot in --dry-run --config.schema --config.collector-file-schema --static-targets-file-schema --version --help; do\n            break\n",
		[]string{".github/workflows/ci.yml:", "`break` leaves the loop, or this turn of it, before the checks after it are made", "\tbreak"},
	},
	{
		"the checks of a step are in a while that never runs", "ci.yml",
		workflowGCChecks, "          while false; do\n" + workflowGCChecks + "          done\n",
		[]string{".github/workflows/ci.yml:", gcStep, "does not understand `while`"},
	},
	{
		"a render of the recipe runs only if true fails", "Makefile",
		"\thelm lint charts/prometheus-universal-exporter\n",
		"\ttrue || helm lint charts/prometheus-universal-exporter\n",
		[]string{"Makefile:", "this check comes after `||`", "true || helm lint charts/prometheus-universal-exporter"},
	},
	{
		"a refusal of the workflow runs only if false succeeds", "ci.yml",
		"          if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then\n",
		"          false && if helm template test charts/prometheus-universal-exporter --set goGC.percent=0 >/dev/null 2>&1; then\n",
		[]string{".github/workflows/ci.yml:", "this `if` comes after `&&`", "--set goGC.percent=0 "},
	},
	{
		"a refusal of the workflow exits with 256, which a shell takes for 0", "ci.yml",
		workflowGCRefusal, strings.Replace(workflowGCRefusal, "exit 1\n", "exit 256\n", 1),
		[]string{".github/workflows/ci.yml:", "this render must be refused, and nothing fails when it is not", "--set goGC.percent=0"},
	},
	{
		"a grep of the workflow hands its failure to an exit with 256", "ci.yml",
		"grep -q 'terminationGracePeriodSeconds: 75'\n",
		"grep -q 'terminationGracePeriodSeconds: 75' || { echo \"no grace period\" >&2; exit 256; }\n",
		[]string{".github/workflows/ci.yml:", "grep -q 'terminationGracePeriodSeconds: 75' || {"},
	},
	{
		"a grep of the workflow is followed by || exit 256", "ci.yml",
		"grep -q 'terminationGracePeriodSeconds: 75'\n",
		"grep -q 'terminationGracePeriodSeconds: 75' || exit 256\n",
		[]string{".github/workflows/ci.yml:", "the `||` after this check takes its failure away", "grep -q 'terminationGracePeriodSeconds: 75' || exit 256"},
	},
	{
		"the checks of a step are the lines of a here-document", "ci.yml",
		workflowGCChecks, "          cat >/dev/null <<'EOF'\n" + workflowGCChecks + "          EOF\n",
		[]string{".github/workflows/ci.yml:", gcStep, "the lines of a here-document (`<<`) are text handed to a command", "cat >/dev/null <<'EOF'"},
	},
	{
		"the checks of a recipe line are the lines of a here-document", "Makefile",
		recipeGCChecks, "\tcat >/dev/null <<-EOF; \\\n" + recipeGCChecks + "\tEOF\n",
		[]string{"Makefile:", "the helm-test recipe", "the lines of a here-document (`<<`) are text handed to a command", "cat >/dev/null <<-EOF;"},
	},
	// What ends the block, or runs in place of a command, behind the
	// reader's back.
	{
		"a step of the workflow traps its exit and leaves with 0", "ci.yml",
		workflowGCChecks, "          trap 'exit 0' EXIT\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "a `trap` runs its commands when the shell ends", "trap 'exit 0' EXIT"},
	},
	{
		"the clean-up trap of the recipe leaves with 0", "Makefile",
		"trap 'rm -rf \"$$tree\"' EXIT;", "trap 'rm -rf \"$$tree\"; exit 0' EXIT;",
		[]string{"Makefile:", "the helm-test recipe", "a `trap` runs its commands when the shell ends", "exit 0' EXIT;"},
	},
	{
		"a step of the workflow traps a failing command", "ci.yml",
		workflowGCChecks, "          trap 'exit 0' ERR\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "a `trap` runs its commands when the shell ends", "trap 'exit 0' ERR"},
	},
	{
		"helm is a function of the step", "ci.yml",
		workflowGCChecks, "          helm() { true; }\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "a function or an alias can stand in for `helm`, `grep`, `python3` or `exit`", "helm() { true; }"},
	},
	{
		"python3 is a function of the step, written with the keyword", "ci.yml",
		workflowGCChecks, "          function python3 {\n            :\n          }\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "a function or an alias can stand in", "function python3 {"},
	},
	{
		"grep is an alias in a recipe line", "Makefile",
		recipeGCChecks, "\talias grep=true; \\\n" + recipeGCChecks,
		[]string{"Makefile:", "the helm-test recipe", "a function or an alias can stand in", "alias grep=true;"},
	},
	{
		"a recipe line puts another helm first on its PATH", "Makefile",
		recipeGCChecks, "\tPATH=\"$$PWD/bin:$$PATH\"; \\\n" + recipeGCChecks,
		[]string{"Makefile:", "the helm-test recipe", "a PATH set in the block decides which `helm`"},
	},
	{
		"a step of the workflow names another program as helm", "ci.yml",
		workflowGCChecks, "          hash -p /usr/bin/true helm\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "`hash` can name another program as `helm`", "hash -p /usr/bin/true helm"},
	},
	{
		"a step of the workflow reads its commands from a file", "ci.yml",
		workflowGCChecks, "          . ./tools/helm-functions.sh\n" + workflowGCChecks,
		[]string{".github/workflows/ci.yml:", gcStep, "reads commands the reader of the helm checks does not see"},
	},
	{
		"a step of the workflow turns set -e off before its last check", "ci.yml",
		"          echo \"$out\" | grep -q '^      # A file whose first line is indented'\n",
		"          echo \"$out\" | grep -q '^      # A file whose first line is indented'\n          set +e\n",
		[]string{".github/workflows/ci.yml:", "`set +e` turns `set -e` off", "\tset +e"},
	},
	{
		"a step of the workflow has its shell run nothing", "ci.yml",
		"          set -eu\n          # goGC.percent renders", "          set -eun\n          # goGC.percent renders",
		[]string{".github/workflows/ci.yml:", gcStep, "does not know what `set -n` does to the checks after it", "set -eun"},
	},
	// A failure handed on with `||`, to something that does not fail.
	{
		"a grep of the workflow hands its failure to an exit 0", "ci.yml",
		"echo \"$out\" | grep -q 'name: GOGC'\n", "echo \"$out\" | grep -q 'name: GOGC' || exit 0\n",
		[]string{".github/workflows/ci.yml:", gcStep, "the `||` after this check takes its failure away", "grep -q 'name: GOGC' || exit 0"},
	},
	{
		"a grep of the workflow hands its failure to a message", "ci.yml",
		"echo \"$out\" | grep -q 'name: GOGC'\n", "echo \"$out\" | grep -q 'name: GOGC' || { echo \"goGC.percent did not render GOGC\" >&2; }\n",
		[]string{".github/workflows/ci.yml:", gcStep, "the commands in the `{ }` after the `||` of this check do not end the shell with a failure", "grep -q 'name: GOGC' || {"},
	},
	{
		"a grep of the recipe hands its failure to a message and an exit 0", "Makefile",
		"echo \"$$out\" | grep -q 'name: GOGC'; \\\n", "echo \"$$out\" | grep -q 'name: GOGC' || { echo \"goGC.percent did not render GOGC\" >&2; exit 0; }; \\\n",
		[]string{"Makefile:", "the helm-test recipe", "the commands in the `{ }` after the `||` of this check do not end the shell with a failure", "grep -q 'name: GOGC' || {"},
	},
	{
		"a grep of the workflow hands its failure to an exit in a pipeline", "ci.yml",
		"echo \"$out\" | grep -q 'name: GOGC'\n", "echo \"$out\" | grep -q 'name: GOGC' || { echo \"goGC.percent did not render GOGC\" >&2; exit 1; } | cat\n",
		[]string{".github/workflows/ci.yml:", gcStep, "what follows `}` runs the commands before it in a shell of their own"},
	},
	// How the block is run.
	{
		"the job of the workflow never runs", "ci.yml",
		"  test:\n    runs-on: ubuntu-latest\n", "  test:\n    if: false\n    runs-on: ubuntu-latest\n",
		[]string{".github/workflows/ci.yml:7: the job test: the job has an `if`, so the chart steps do not run whenever the chart changed", "\tif: false"},
	},
	{
		"the job of the workflow runs for one event only", "ci.yml",
		"  test:\n    runs-on: ubuntu-latest\n", "  test:\n    runs-on: ubuntu-latest\n    if: github.event_name == 'workflow_dispatch'\n",
		[]string{".github/workflows/ci.yml:8: the job test: the job has an `if`", "\tif: github.event_name == 'workflow_dispatch'"},
	},
	{
		"the chart filter of the workflow no longer names the chart", "ci.yml",
		"            chart:\n              - 'charts/**'\n", "            chart:\n              - 'chart/**'\n",
		[]string{".github/workflows/ci.yml:", "no step before it with the id `changes` sets that for a change under charts/**"},
	},
	{
		"the workflow's steps run in a shell without -e", "ci.yml",
		"jobs:\n  test:\n", "defaults:\n  run:\n    shell: bash {0}\njobs:\n  test:\n",
		[]string{".github/workflows/ci.yml:7: the workflow: the shell is not bash or sh started with -e", "\tshell: bash {0}"},
	},
	{
		"the job's steps run in a shell without -e", "ci.yml",
		"  test:\n    runs-on: ubuntu-latest\n", "  test:\n    runs-on: ubuntu-latest\n    defaults:\n      run:\n        shell: sh {0}\n",
		[]string{".github/workflows/ci.yml:10: the job test: the shell is not bash or sh started with -e", "\tshell: sh {0}"},
	},
	{
		"a step of the workflow has a shell that reads its script and runs nothing", "ci.yml",
		"      - name: Package the chart\n", "      - name: Package the chart\n        shell: bash -n -e {0}\n",
		[]string{".github/workflows/ci.yml:", `the step "Package the chart"`, "the shell is not bash or sh started with -e", "\tshell: bash -n -e {0}"},
	},
	{
		"the Makefile runs its recipes with a program that does nothing", "Makefile",
		"\nhelm-test: helm-version\n", "\nSHELL := /usr/bin/true\nhelm-test: helm-version\n",
		[]string{"Makefile:", "SHELL is the program every recipe line is run by", "SHELL := /usr/bin/true"},
	},
	{
		"the Makefile has make ignore failing lines by its flags", "Makefile",
		"\nhelm-test: helm-version\n", "\nMAKEFLAGS += --ignore-errors\nhelm-test: helm-version\n",
		[]string{"Makefile:", "MAKEFLAGS can have make ignore every recipe line that fails", "MAKEFLAGS += --ignore-errors"},
	},
	{
		"the Makefile puts another helm first on the PATH", "Makefile",
		"\nhelm-test: helm-version\n", "\nexport PATH := $(CURDIR)/bin:$(PATH)\nhelm-test: helm-version\n",
		[]string{"Makefile:", "PATH decides which `helm`, `grep` and `python3` every recipe line runs"},
	},
	{
		"the helm-test target is given a second recipe", "Makefile",
		"\nci: fmt-check", "\nhelm-test:\n\t@true\n\nci: fmt-check",
		[]string{"Makefile:", "the helm-test target is given a second rule", "\thelm-test:"},
	},
	{
		"the helm-test target is given a shell of its own", "Makefile",
		"\nci: fmt-check", "\nhelm-test: SHELL = /usr/bin/true\n\nci: fmt-check",
		[]string{"Makefile:", "the helm-test target is given a second rule", "\thelm-test: SHELL = /usr/bin/true"},
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
