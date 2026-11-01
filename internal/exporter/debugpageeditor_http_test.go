//go:build !select_request_types || request_type_http

package exporter

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The collector's editor numbers its lines in a gutter beside the textarea,
// the read-only view of a configured collector's as the playground's, laid
// out by one line height and following the textarea's scroll; the textarea
// does not wrap, so a long line scrolls sideways and the gutter stays where
// it is. The gutter's numbers are out of the flow, so a long YAML does not
// make the editor taller than its textarea.
func TestTheDebugPageEditorNumbersItsLines(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for _, path := range []string{"/debug", "/debug?collector=rooms"} {
		page := getPage(t, server, path)
		requireContains(t, page,
			`<div class="gutter" aria-hidden="true"><div class="gutter-lines" id="collector-gutter"></div></div>`,
			`<div class="text"><div class="bands" id="collector-bands" aria-hidden="true"></div>`,
			`<textarea id="collector" spellcheck="false" autocomplete="off" wrap="off"`,
			".editor { --editor-line: 20px; --editor-pad: 6px;",
			"line-height: var(--editor-line); text-align: right;",
			".editor .gutter-lines { position: absolute; top: 0; left: 4px; right: 6px; padding-top: var(--editor-pad);",
			".editor .gutter-lines div { height: var(--editor-line);",
			"padding: var(--editor-pad) 8px; border: 0; border-radius: 0; background: transparent; line-height: var(--editor-line); white-space: pre; overflow-wrap: normal; overflow: auto;",
		)
		script := debugPageScript(page)
		requireContains(t, script,
			`editorArea.addEventListener("scroll", followScroll);`,
			`const shift = "translateY(" + -editorArea.scrollTop + "px)";`,
			"gutterLines.style.transform = shift;",
			"bandLayer.style.transform = shift;",
			`const count = editorArea.value.split("\n").length;`,
			"drawGutter();\nshowCollector();",
		)
	}
	requireContains(t, getPage(t, server, "/debug?collector=rooms"), `<div class="editor readonly" id="collector-editor">`)
	requireContains(t, getPage(t, server, "/debug"), `<div class="editor" id="collector-editor">`)
}

// The lines a refusal of the playground names, as the loader names them,
// "line 7: …", counted from the collector's first line, are marked in the
// gutter and by a band behind each, and the error's "line N" is a button
// that selects that line; an edit, or a run that is not refused, clears the
// marks, and a configured collector's run marks nothing.
func TestTheDebugPageMarksTheLinesAnErrorNames(t *testing.T) {
	_, err := config.ParseCollector([]byte("name: x\nrequest:\n  type: http\nmetrics:\n  - name: a\n    expression: .a\n    bogus: 1\n  - name: b\n    expresion: .b\n"), refusedOnDebugPage)
	if err == nil {
		t.Fatal("the collector loaded")
	}
	// The page's own reading of an error's lines.
	var lines []int
	for _, m := range regexp.MustCompile(`\bline (\d+)\b`).FindAllStringSubmatch(err.Error(), -1) {
		n, _ := strconv.Atoi(m[1])
		lines = append(lines, n)
	}
	if !slices.Equal(lines, []int{7, 9}) {
		t.Fatalf("the loader's error %q names lines %v, want 7 and 9", err, lines)
	}
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	script := debugPageScript(getPage(t, server, "/debug"))
	requireContains(t, script,
		`const lineNamed = /\bline (\d+)\b/g;`,
		`const ours = sent !== undefined && sent === editorArea.value && $("configured").value === "";`,
		"showError(series, answer.error, ours);",
		"markLines(ours ? linesNamed(answer.error) : []);",
		`gutterLines.children[n - 1].classList.add("marked");`,
		`band.style.top = pad + (n - 1) * height + "px";`,
		`button.addEventListener("click", () => jumpTo(Number(m[1])));`,
		"editorArea.setSelectionRange(start, start + lines[n - 1].length);",
		"markLines([]);\n  drawGutter();\n  if ($(\"collector\").readOnly) return;",
		"show(answer, response, Math.round(performance.now() - started), request.collector);",
	)
	if strings.Contains(script, "innerHTML") || strings.Contains(script, "insertAdjacentHTML") {
		t.Error("the script writes HTML")
	}
}

// Ctrl+Enter, or Cmd+Enter, runs from anywhere on the page, as Run does,
// which says so in its title, its aria-keyshortcuts and a hint beside it;
// Enter with either in a parameter's value adds no row.
func TestTheDebugPageRunsOnCtrlEnter(t *testing.T) {
	page := getPage(t, debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}), "/debug")
	requireContains(t, page,
		`<button type="button" id="run" title="Run (Ctrl+Enter, or ⌘+Enter on a Mac, from anywhere on the page)" aria-keyshortcuts="Control+Enter Meta+Enter">Run</button>`,
		`<p class="note run-hint" id="run-hint">Ctrl+Enter runs, ⌘+Enter on a Mac</p>`,
	)
	requireContains(t, debugPageScript(page),
		"document.addEventListener(\"keydown\", (event) => {\n  if (event.key !== \"Enter\" || !(event.ctrlKey || event.metaKey) || event.altKey || event.shiftKey || event.isComposing) return;\n  event.preventDefault();\n  run();\n});",
		`if (event.key !== "Enter" || event.ctrlKey || event.metaKey || rows[rows.length - 1] !== row) return;`,
	)
}

// In the playground Tab puts in two spaces, or indents the lines selected,
// and Shift+Tab outdents them by two; a read-only editor takes Tab as any
// field does, and Esc, then Tab, leaves the playground's, as the hint below
// the editor says.
func TestTheDebugPageEditorIndentsOnTab(t *testing.T) {
	page := getPage(t, debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}}), "/debug")
	requireContains(t, page, `<p class="note editor-hint" id="editor-hint">In the playground Tab indents by two spaces and Shift+Tab outdents; Esc, then Tab, leaves the editor.</p>`)
	requireContains(t, debugPageScript(page),
		`editorArea.setAttribute("aria-describedby", "editor-hint");`,
		`if (event.key === "Escape") { escaped = true; return; }`,
		`if (event.key !== "Tab" || leave || editorArea.readOnly || event.ctrlKey || event.altKey || event.metaKey) return;`,
		"indent(event.shiftKey);",
		`if (!out && !many) { edit(start, end, "  ", start + 2, start + 2); return; }`,
		`(line.startsWith("  ") ? 2 : line.startsWith(" ") ? 1 : 0)`,
		`done = document.execCommand("insertText", false, text);`,
	)
}
