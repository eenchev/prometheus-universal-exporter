package transform

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A script that fails answers with its traceback: the frames of the script,
// innermost last, and then the exception's own line, its type and its
// message (the worker's script_error). The worker keeps the five innermost
// frames of each exception, and that is all that bounds the text: the
// message is whatever the script, or a library it called, made of the data —
// `raise Exception(text)` with the whole response, a KeyError or a
// JSONDecodeError quoting it — a frame shows its source line however long
// that is, and exceptions raised from one another are each written out, a
// chain of four hundred in 119 kB. The text went whole into the transform's
// error, and that, cut at the bound of every failure
// (model.MaxFailureBytes), was the start of it: the first frames of the
// first exception, and of a message as long as the response nothing but
// "Traceback (most recent call last)" and its beginning.
//
// So a text longer than scriptErrorBytes is shown by what says most
// (shownScriptError): the exception's own line and the frames before it,
// the last of them first, each line by its start.

const (
	// scriptErrorBytes is how long a script's error may be before it is
	// shown in part, and how much of it is then shown: with what the
	// transform says before it, it is within model.MaxFailureBytes.
	scriptErrorBytes = 1500
	// scriptLineBytes is how much of one line of such an error is shown.
	scriptLineBytes = 200
	// scriptFrameLine is how the line of a traceback that names a frame
	// begins, as Python writes it.
	scriptFrameLine = "  File "
)

// linesMark is what stands where lines of a script's error were left out:
// how many lines that part of it had, as model.CutMark says how many bytes a
// text had, and the mark of something measured in the text the failure is
// recognised by.
func linesMark(whole int, recognised bool) string {
	if recognised {
		return "... (" + model.MovingMark + " lines)"
	}
	return "... (" + strconv.Itoa(whole) + " lines)"
}

// shownScriptError is the error a script failed with, text, as the
// transform's error shows it: whole when it is no longer than
// scriptErrorBytes, as nearly every traceback is, and otherwise
//
//   - each line by its first scriptLineBytes bytes and its length;
//   - of the lines from the exception's own on — its message, when that has
//     several lines — as many of the first as half of scriptErrorBytes
//     holds, and all of scriptErrorBytes where there is no frame before them;
//   - of the lines before it, the frames and what Python says between the
//     exceptions of a chain, as many of the last as the rest holds.
//
// Where lines are left out stands how many lines that part had. The
// exception's own line is the first after the last frame that is not
// indented, which is where Python writes it; a text with no frame, as the
// message of fail() is none, is all message.
//
// What is kept is decided by the lengths of the lines as the failure is
// recognised by them, in which every length that was measured is the mark
// (model.MovingMark): so the same failure of a longer message, or of a
// longer chain that ends alike, is shown by the same lines and is one
// failure to the log. The text itself is longer than that by the digits of
// the lengths, a few bytes a line.
func shownScriptError(text string) model.QuotedValue {
	if len(text) <= scriptErrorBytes {
		return model.ShownAs(text, text)
	}
	lines := strings.Split(text, "\n")
	// own is the exception's own line: the first that is not indented
	// after the last line naming a frame.
	own := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], scriptFrameLine) {
			own = i + 1
			for own < len(lines)-1 && strings.HasPrefix(lines[own], " ") {
				own++
			}
			break
		}
	}
	own = min(own, len(lines)-1)
	// room is what the message may take, and then what is left for the
	// lines before it; each line takes its length and its line break.
	room := scriptErrorBytes
	if own > 0 {
		room /= 2
	}
	used := 0
	message := make([]model.QuotedValue, 0, 8)
	for _, line := range lines[own:] {
		shown := model.Shown(line, scriptLineBytes)
		if len(message) > 0 && used+len(shown.Same())+1 > room {
			break
		}
		used += len(shown.Same()) + 1
		message = append(message, shown)
	}
	// The lines before it, from the last, while they fit in what is left.
	first := own
	for first > 0 {
		shown := model.Shown(lines[first-1], scriptLineBytes)
		if used+len(shown.Same())+1 > scriptErrorBytes {
			break
		}
		used += len(shown.Same()) + 1
		first--
	}
	var said, same strings.Builder
	write := func(text, recognised string) {
		if said.Len() > 0 {
			said.WriteByte('\n')
			same.WriteByte('\n')
		}
		said.WriteString(text)
		same.WriteString(recognised)
	}
	if first > 0 {
		write(linesMark(own, false), linesMark(own, true))
	}
	for _, line := range lines[first:own] {
		shown := model.Shown(line, scriptLineBytes)
		write(shown.String(), shown.Same())
	}
	for _, shown := range message {
		write(shown.String(), shown.Same())
	}
	if left := len(lines) - own; len(message) < left {
		write(linesMark(left, false), linesMark(left, true))
	}
	return model.ShownAs(said.String(), same.String())
}

// shownByWorker is the error of a script that failed as the worker made it
// ready to show, parts: the texts and the numbers of what shownScriptError
// makes of the whole traceback, in their order. A traceback that may be
// longer than limits.max_output_bytes is not sent whole to be shown here —
// it was, and the line it made was refused as output over the limit, which
// said nothing of the exception and cost the worker. The worker shows it by
// the same rules (the launcher's cut; scripterrorcut_test.go compares the
// two) and sends what is shown. Each number is one that was measured, the
// length of a line or how many lines a part had, so it is written as it is
// and is the mark in what the failure is recognised by.
//
// Parts that come to more than a worker shows of any error
// (shownByWorkerBytes) are no worker's: a script can write to the answers
// itself, as it could always write an error of its own there, and one of a
// megabyte in parts was a transform's error of a megabyte, where every
// error a worker sent was cut here. Such a text is an error's text like
// another and is shown as one is, so nothing this returns is longer than
// that.
func shownByWorker(parts []any) model.QuotedValue {
	var said, same strings.Builder
	for _, part := range parts {
		switch part := part.(type) {
		case string:
			said.WriteString(part)
			same.WriteString(part)
		case json.Number:
			said.WriteString(part.String())
			same.WriteString(model.MovingMark)
		}
	}
	if said.Len() > shownByWorkerBytes {
		return shownScriptError(strings.TrimSpace(said.String()))
	}
	return model.ShownAs(said.String(), same.String())
}

// shownByWorkerBytes is how long what is shown of a script's error can be,
// by shownScriptError and so by a worker: the lines it keeps take
// scriptErrorBytes at most, each with its line break, which the last has
// none of; before them and after them may stand how many lines a part had;
// and each number, which takes one byte where the lines are counted, is as
// many digits as a length has, nineteen at most. The numbers are those two
// and the length of each line that is cut, which takes its first
// scriptLineBytes bytes, or up to three fewer, and the mark of its length:
// seven lines at most. That is 1,689 bytes.
const shownByWorkerBytes = scriptErrorBytes - 1 + 2*len("... (# lines)\n") +
	(2+scriptErrorBytes/(scriptLineBytes-utf8.UTFMax+1+len("... (# bytes)\n")))*(len("9223372036854775807")-len(model.MovingMark))

// stderrShownBytes is how much of what a worker wrote to stderr an error
// shows. The worker keeps the last pythonStderrTail bytes of it, 4,096, and
// an error that ended with all of them was over the bound of every failure
// (model.MaxFailureBytes) by itself: cut there, it lost the end of what
// CPython wrote, which is where it says why it stopped, and the hint that
// follows it about limits.max_script_memory (memoryHint). With the longest
// of what the errors say before and after it, this much is within the
// bound.
const stderrShownBytes = 1500

// shownStderr is tail, the end of what a worker wrote to stderr, as an error
// shows it: whole when it is no longer than stderrShownBytes, and otherwise
// its last stderrShownBytes bytes, or up to three fewer from a character's
// start, after "... ".
// Nothing is said of how much stood before: the worker does not keep how
// much it wrote, and a number there would be another on every run.
func shownStderr(tail string) string {
	if len(tail) <= stderrShownBytes {
		return tail
	}
	from := len(tail) - stderrShownBytes
	for skipped := 0; skipped < utf8.UTFMax-1 && !utf8.RuneStart(tail[from]); skipped++ {
		from++
	}
	return "... " + tail[from:]
}
