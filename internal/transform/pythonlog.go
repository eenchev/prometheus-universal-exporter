package transform

import (
	"log/slog"
	"strings"
)

// pythonLogMark begins a line a script wrote with log(...) among what it
// printed: the mark, the level, a NUL, the message, a NUL
// (pythonworker.go). A script's own output holds it only if the script
// writes it there, which then logs what it meant to print.
const pythonLogMark = "\x00pue-log:"

// pythonLogLine is one call of a script's log(...).
type pythonLogLine struct {
	level slog.Level
	text  string
}

// pythonLogLines parts what a run wrote into what it printed and the lines
// it logged, in the order it logged them. A line the 4 KiB of a run's
// output ends within is kept as far as it goes.
func pythonLogLines(output string) (printed string, logged []pythonLogLine) {
	if !strings.Contains(output, pythonLogMark) {
		return output, nil
	}
	var rest strings.Builder
	for {
		before, after, found := strings.Cut(output, pythonLogMark)
		rest.WriteString(before)
		if !found {
			return rest.String(), logged
		}
		name, after, _ := strings.Cut(after, "\x00")
		text, after, _ := strings.Cut(after, "\x00")
		level, known := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warning": slog.LevelWarn, "error": slog.LevelError}[name]
		if known {
			logged = append(logged, pythonLogLine{level, text})
		}
		output = after
	}
}
