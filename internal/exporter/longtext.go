package exporter

import (
	"html/template"
	"strings"
	"unicode/utf8"
)

// A page cell of free text — an error, which can quote a whole URL or a
// target's answer, or a target's address — has no length of its own, and one
// long value would make its table as tall as the text. Past
// longPreviewRunes characters or longPreviewLines lines a cell shows the
// start of it, and the whole text one click away: the template "long"
// writes a <details> whose summary is the start and whose body is the whole
// text, so a page without JavaScript opens it in place, and a page's script
// may show the body in a dialog instead (the status page does). Shorter
// text is written as it is.
const (
	longPreviewRunes = 160
	longPreviewLines = 3
)

// longText is a cell's text as the template "long" writes it: Text whole,
// and, when it is Long, Preview, its start. Kind names the text in the
// control that shows it whole, "Show the whole <Kind>".
type longText struct {
	Kind, Text, Preview string
	Long                bool
}

// clipLong is the template function long: text as a cell writes it.
func clipLong(kind, text string) longText {
	preview, cut := text, false
	if lines := strings.SplitN(text, "\n", longPreviewLines+1); len(lines) > longPreviewLines {
		preview, cut = strings.Join(lines[:longPreviewLines], "\n"), true
	}
	if utf8.RuneCountInString(preview) > longPreviewRunes {
		preview, cut = string([]rune(preview)[:longPreviewRunes]), true
	}
	if !cut {
		return longText{Kind: kind, Text: text}
	}
	return longText{Kind: kind, Text: text, Preview: strings.TrimRight(preview, " \t\n") + "…", Long: true}
}

// pageFuncs are the functions the pages' templates may call.
var pageFuncs = template.FuncMap{"long": clipLong}

// longTemplate writes a cell's text, {{template "long" (long "error" .Err)}}.
// The summary's label says what it does, since its text is only the start
// of the value; the body is a <pre> so the whole text keeps its lines.
const longTemplate = `{{define "long"}}{{if .Long}}<details class="long"><summary aria-label="Show the whole {{.Kind}}" data-kind="{{.Kind}}"><span class="preview">{{.Preview}}</span></summary><pre class="whole">{{.Text}}</pre></details>{{else}}{{.Text}}{{end}}{{end}}`

// longStyle is the part of pageStyle for longTemplate: the start is cut at
// three lines as well, however narrow the cell, and the summary says it
// opens, or, opened in place without a script, closes. A <details> is a
// block of its own, so a line break after it would leave a blank line.
const longStyle = `
details.long > summary { list-style: none; cursor: pointer; }
details.long > summary::-webkit-details-marker { display: none; }
details.long > summary .preview { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 3; line-clamp: 3; overflow: hidden; white-space: pre-wrap; }
details.long > summary::after { content: "Show all"; display: inline-block; margin-top: 2px; color: var(--accent); font-family: system-ui, -apple-system, "Segoe UI", sans-serif; font-size: 12px; text-decoration: underline; }
details.long[open] > summary .preview { display: none; }
details.long[open] > summary::after { content: "Show less"; }
details.long > summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; border-radius: 4px; }
details.long + br { display: none; }
details.long > pre.whole { margin: 0; font: inherit; white-space: pre-wrap; overflow-wrap: anywhere; }
`
