package transform

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A failed script answers with its traceback: the script's frames,
// innermost last, then the exception's own line with its type and message
// (the worker's script_error). The worker keeps the five innermost frames
// of each exception, and nothing else bounds the text:
//   - The message is whatever the script, or a library it called, made of
//     the data: `raise Exception(text)` with the whole response, or a
//     KeyError or JSONDecodeError quoting it.
//   - A frame shows its source line however long it is.
//   - Chained exceptions are each written out: a chain of four hundred
//     took 119 kB.
//
// The whole text went into the transform's error. Cut at the limit of every
// failure (model.MaxFailureBytes), only its start remained: the first
// frames of the first exception, and, for a message as long as the
// response, just "Traceback (most recent call last)" and its beginning.
//
// So a text longer than scriptErrorBytes is shown by its most telling
// parts (shownScriptError): the exception's own line and the frames before
// it, the last of them first, each line by its start.

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

// linesMark is the text that replaces lines left out of a script's error.
// It says how many lines that part had, as model.CutMark says how many
// bytes a text had. In the text a failure is recognised by, it is the mark
// of something measured.
func linesMark(whole int, recognised bool) string {
	if recognised {
		return "... (" + model.MovingMark + " lines)"
	}
	return "... (" + strconv.Itoa(whole) + " lines)"
}

// shownScriptError returns text, the error a script failed with, as the
// transform's error shows it. It is whole when it is no longer than
// scriptErrorBytes, as nearly every traceback is. Otherwise it shows:
//
//   - each line by its first scriptLineBytes bytes and its length;
//   - from the exception's own line on (its message, when that has several
//     lines), as many of the first lines as fit in half of
//     scriptErrorBytes, or in all of it when no frame precedes them;
//   - of the lines before it (the frames, and what Python writes between
//     the exceptions of a chain), as many of the last as the rest holds.
//
// Where lines are left out, the text says how many lines that part had.
// The exception's own line is the first unindented line after the last
// frame, which is where Python writes it. A text with no frame, such as
// fail()'s message, is all message.
//
// What is kept is decided by the line lengths as seen in the text the
// failure is recognised by, where every measured length is the mark
// (model.MovingMark). So the same failure with a longer message, or with a
// longer chain that ends the same way, is shown by the same lines and is
// one failure to the log. The text itself is longer than that by the
// digits of the lengths, a few bytes per line.
func shownScriptError(text string) model.QuotedValue {
	if len(text) <= scriptErrorBytes {
		return model.ShownAs(text, text).ForCaller(scriptCallerText(text))
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
	return model.ShownAs(said.String(), same.String()).ForCaller(scriptCallerText(text))
}

// shownByWorker returns the error of a failed script that the worker
// prepared for display as parts: the texts and numbers that
// shownScriptError makes of the whole traceback, in order. A traceback
// that may be longer than limits.max_output_bytes is not sent whole to be
// shown here. It used to be, and its line was refused as output over the
// limit, which said nothing about the exception and cost the worker. The
// worker applies the same rules (the launcher's cut;
// scripterrorcut_test.go compares the two) and sends the result. Each
// number was measured, either a line's length or a part's line count. So
// it is written as is, and is the mark in the text the failure is
// recognised by.
//
// Parts that add up to more than a worker shows of any error
// (shownByWorkerBytes) did not come from a worker. A script can write to
// the answers itself, as it always could write its own error there. A
// megabyte of parts became a megabyte transform error, while every error a
// worker sent was cut here. Such a text is treated like any error's text,
// so nothing this returns is longer than that.
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
	return model.ShownAs(said.String(), same.String()).ForCaller(scriptCallerText(said.String()))
}

// scriptFrame is the name a script's own frames have in a traceback, as the
// worker compiles it.
const scriptFrame = `"<collector-python>"`

// scriptCallerText returns what a probe's caller is told of a script's
// error, text: the exception type and the script line it came from, as in
// "ValueError at line 3 of the script". The message is withheld, because
// it is what the script, or a library it called, made of the data: a
// KeyError quotes a key of the response, a JSONDecodeError the response
// itself. The frame lines, which include the script's source, are withheld
// too. An error the exporter's worker raised after the script has no frame
// and is reported by its type alone. A text whose exception line names no
// type, such as fail()'s message, is withheld entirely.
func scriptCallerText(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	own, scriptLine := -1, ""
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], scriptFrameLine) {
			continue
		}
		if own < 0 {
			own = i + 1
			for own < len(lines)-1 && strings.HasPrefix(lines[own], " ") {
				own++
			}
		}
		// The innermost of the script's own frames of the last
		// exception: the frames after it are a library's.
		if rest, ok := strings.CutPrefix(lines[i], scriptFrameLine+scriptFrame+", line "); ok {
			if n, _, _ := strings.Cut(rest, ","); n != "" && strings.Trim(n, "0123456789") == "" {
				scriptLine = n
			}
			break
		}
	}
	if own < 0 {
		own = 0
	}
	if own >= len(lines) {
		return model.WithheldMark
	}
	kind, _, typed := strings.Cut(lines[own], ":")
	if !typed || !pythonName(kind) {
		return model.WithheldMark
	}
	shown := kind
	if scriptLine != "" {
		shown += " at line " + scriptLine + " of the script"
	}
	return shown + "; the rest is in the exporter's log"
}

// pythonName reports whether s reads as the name of an exception's type,
// dotted as Python writes one of a module: json.decoder.JSONDecodeError.
func pythonName(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" || part[0] >= '0' && part[0] <= '9' {
			return false
		}
		for _, r := range part {
			letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			if r != '_' && !letter && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

// shownByWorkerBytes is the maximum length of what is shown of a script's
// error by shownScriptError, and so by a worker:
//   - The kept lines take at most scriptErrorBytes, each with its line
//     break, except the last.
//   - Before and after them may stand a count of a part's lines.
//   - Each number takes one byte where lines are counted, and as many
//     digits as a length has, nineteen at most. The numbers are those two
//     counts plus the length of each cut line. A cut line keeps its first
//     scriptLineBytes bytes, or up to three fewer, plus the mark of its
//     length: seven lines at most.
//
// That is 1,689 bytes.
const shownByWorkerBytes = scriptErrorBytes - 1 + 2*len("... (# lines)\n") +
	(2+scriptErrorBytes/(scriptLineBytes-utf8.UTFMax+1+len("... (# bytes)\n")))*(len("9223372036854775807")-len(model.MovingMark))

// stderrShownBytes is how much of a worker's stderr an error shows. The
// worker keeps the last pythonStderrTail bytes, 4,096. An error ending with
// all of them exceeded the limit of every failure (model.MaxFailureBytes)
// by itself. Cut there, it lost the end of what CPython wrote, which says
// why it stopped, and the following hint about limits.max_script_memory
// (memoryHint). With the longest text the errors put before and after it,
// this amount fits within the limit.
const stderrShownBytes = 1500

// shownStderr returns tail, the end of a worker's stderr, as an error shows
// it: whole when it is no longer than stderrShownBytes, otherwise its last
// stderrShownBytes bytes (or up to three fewer, to start on a character),
// after "... ".
// It does not say how much came before: the worker does not track how much
// it wrote, and a number there would differ on every run.
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
