//go:build !select_request_types || request_type_http

package exporter

import (
	"math"
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
			`<div class="gutter"><div class="gutter-lines" id="collector-gutter"></div></div>`,
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
			`const shift = "translateY(" + -from.scrollTop + "px)";`,
			"gutterLines.style.transform = shift;",
			"bandLayer.style.transform = shift;",
			`const count = editorArea.value.split("\n").length;`,
			"drawEditor();\nshowCollector();",
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
		`gutterLines.children[k].classList.add("marked");`,
		`band.style.top = pad + k * height + "px";`,
		`button.addEventListener("click", () => jumpTo(Number(m[1])));`,
		"editorArea.setSelectionRange(start, start + lines[n - 1].length);",
		"marked = [];\n  drawSoon();\n  if ($(\"collector\").readOnly) return;",
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

// The collector's YAML is highlighted by a layer behind the textarea, laid
// out as the textarea's text is and scrolled with it as a scroller of its
// own, while the textarea's text is transparent over it, its caret and
// selection drawn; the layer is drawn again once a frame as the text is
// typed, a line read as before not read again, and with textContent
// alone. The tokens' colours are the theme's, in light and in both dark
// rules.
func TestTheDebugPageEditorHighlightsTheYAML(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for _, path := range []string{"/debug", "/debug?collector=rooms"} {
		page := getPage(t, server, path)
		requireContains(t, page,
			`<div class="text"><div class="bands" id="collector-bands" aria-hidden="true"></div>
<div class="yaml-view" id="collector-view" aria-hidden="true"><div class="yaml-lines" id="collector-lines"></div></div>
<textarea id="collector"`,
			// The layer's text is laid out as the textarea's.
			".editor .yaml-view { position: absolute; inset: 0; overflow: auto; scrollbar-color: transparent transparent; pointer-events: none; color: var(--fg); font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 13px; font-weight: normal; line-height: var(--editor-line); tab-size: 2; white-space: pre; overflow-wrap: normal; }",
			".editor .yaml-lines { width: max-content; min-width: 100%; padding: var(--editor-pad) 8px; }",
			".editor:not(.readonly):not(.folded) .yaml-lines { padding-right: 0; }",
			".editor .yaml-lines > div { height: var(--editor-line); }",
			"textarea { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 13px; resize: vertical; tab-size: 2; }",
			".editor textarea { color: transparent; caret-color: var(--fg); }",
			".editor textarea::selection { color: transparent; background: var(--editor-selection); }",
			".editor.readonly textarea, .editor.folded textarea { display: none; }",
		)
		// The JSON viewer's colours where a token is one of JSON's.
		for _, token := range []string{"key", "string", "number", "boolean", "null", "punct"} {
			requireContains(t, page, ".y-"+token+" { color: var(--json-"+token+"); }")
		}
		for _, token := range []string{"comment", "dash", "indicator", "block", "anchor", "tag"} {
			requireContains(t, page, ".y-"+token+" { color: var(--yaml-"+token+"); }")
		}
		requireContains(t, page,
			".y-param { color: var(--yaml-param); background: var(--yaml-param-bg);",
			".y-env { color: var(--yaml-env); background: var(--yaml-env-bg);",
			".y-withheld { color: var(--yaml-withheld); background: var(--yaml-withheld-bg);")
		script := debugPageScript(page)
		requireContains(t, script,
			// The tokenizer: placeholders, ${NAME} and what is withheld;
			// booleans, null and numbers; a block scalar's lines; a
			// quoted scalar and a flow collection going on; never throwing.
			`const yamlSpecial = /(\{\{[^{}\n]*\}\}|\$\{[A-Za-z_][A-Za-z0-9_]*\}|<redacted>)/;`,
			`out.push(i % 2 === 0 ? [cls, parts[i]] : [parts[i][0] === "{" ? "y-param" : parts[i][0] === "$" ? "y-env" : "y-withheld", parts[i]]);`,
			`yamlPush(out, yamlBoolean.test(word) ? "y-boolean" : yamlNull.test(word) ? "y-null" : yamlNumber.test(word) ? "y-number" : "y-string", word);`,
			`const block = /^[|>][-+1-9]*(?=[ \t]|$)/.exec(line.slice(pos));`,
			`out.push(["y-indicator", block[0]]);`,
			"state.block = parent;",
			`if (indent > state.block) { yamlPush(out, "y-block", line); info.kind = "scalar"; return info; }`,
			`if (end < 0) { yamlPush(out, "y-string", line.slice(pos)); state.quote = ch; return; }`,
			"yamlRest(line, yamlFlow(line, 0, out, state), out);",
			`out.push([line[pos] === "!" ? "y-tag" : "y-anchor", line.slice(pos, end)]);`,
			`out.push(["y-dash", "-"]);`,
			`out.push(["y-key", line.slice(pos, key.end)]);`,
			"read = { info: yamlLine(line, state) };\n      } catch (error) {",
			"let read = yamlKnown.get(sig) || known.get(sig);",
			// Drawn with textContent, the rows that changed alone.
			`for (const [cls, text] of yamlInfo[row.line].tokens) div.append(cls ? jsonSpan(cls, text) : text);`,
			"while (start < same && sigs[start] === viewSigs[start]) start++;",
			// Scrolled with the textarea, down and sideways.
			"yamlView.scrollTop = editorArea.scrollTop;\n    yamlView.scrollLeft = editorArea.scrollLeft;",
			`editorArea.addEventListener("scroll", followScroll);`,
			// Once a frame as it is typed.
			"if (!drawFrame) drawFrame = requestAnimationFrame(() => { drawFrame = 0; drawEditor(); });",
			"marked = [];\n  drawSoon();",
		)
		for _, never := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function"} {
			if strings.Contains(script, never) {
				t.Errorf("the script has %s", never)
			}
		}
	}
	page := getPage(t, server, "/debug")
	themes := map[string]*regexp.Regexp{
		"light":            regexp.MustCompile(`:root \{ color-scheme: light dark; ([^}]*)\}`),
		"dark (preferred)": regexp.MustCompile(`:root:not\(\[data-theme="light"\]\) \{ color-scheme: dark; ([^}]*)\}`),
		"dark (chosen)":    regexp.MustCompile(`:root\[data-theme="dark"\] \{ color-scheme: dark; ([^}]*)\}`),
	}
	colours := map[string]map[string]string{}
	for theme, rule := range themes {
		m := rule.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("no %s rule", theme)
		}
		colours[theme] = map[string]string{}
		for _, token := range []string{"json-key", "json-string", "json-number", "json-boolean", "json-null", "json-punct", "yaml-comment", "yaml-dash", "yaml-indicator", "yaml-block", "yaml-anchor", "yaml-tag", "yaml-param", "yaml-param-bg", "yaml-env", "yaml-env-bg", "yaml-withheld", "yaml-withheld-bg"} {
			c := regexp.MustCompile(`--` + token + `: (#[0-9a-f]{6});`).FindStringSubmatch(m[1])
			if c == nil {
				t.Errorf("the %s theme has no --%s", theme, token)
				continue
			}
			colours[theme][token] = c[1]
		}
		if !strings.Contains(m[1], "--editor-selection: rgba(") {
			t.Errorf("the %s theme has no --editor-selection", theme)
		}
	}
	for token, light := range colours["light"] {
		if colours["dark (preferred)"][token] != colours["dark (chosen)"][token] || light == colours["dark (chosen)"][token] {
			t.Errorf("--%s is %s light, %s and %s dark", token, light, colours["dark (preferred)"][token], colours["dark (chosen)"][token])
		}
	}
	// Each colour reads at 4.5:1 on the editor's backgrounds and the band
	// of a marked line, and a tinted token on its tint.
	backgrounds := map[string][]string{"light": {"#ffffff", "#f3f4f6", "#fef2f2"}, "dark (chosen)": {"#16181b", "#23262b", "#2c1517"}}
	for theme, on := range backgrounds {
		for token, colour := range colours[theme] {
			if strings.HasSuffix(token, "-bg") {
				continue
			}
			against := on
			if tint, ok := colours[theme][token+"-bg"]; ok {
				against = append(slices.Clone(on), tint)
			}
			for _, bg := range against {
				if ratio := contrastRatio(colour, bg); ratio < 4.5 {
					t.Errorf("%s: --%s %s on %s is %.2f:1", theme, token, colour, bg, ratio)
				}
			}
		}
	}
}

// contrastRatio is the WCAG contrast of two #rrggbb colours.
func contrastRatio(a, b string) float64 {
	luminance := func(hex string) float64 {
		var rgb [3]float64
		for i := range rgb {
			v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
			c := float64(v) / 255
			if c <= 0.03928 {
				rgb[i] = c / 12.92
			} else {
				rgb[i] = math.Pow((c+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
	}
	x, y := luminance(a)+0.05, luminance(b)+0.05
	return max(x, y) / min(x, y)
}

// A block — a key with lines indented under it, a list item with more of
// it below, a block scalar — folds behind a button in the gutter, which
// says whether it is expanded and what it folds, one of them reached by
// Tab and the arrow keys going on to the others; the block's first line
// says how many lines it hides, each line keeps its own number and a
// hidden line's mark goes on its fold's row. Expand all and Collapse all
// stand with Edit a copy. The playground is shown by the layer alone while
// a block of it is folded, the textarea hidden but holding the whole text,
// which Run sends, and a click, Enter or Expand all edits it again.
func TestTheDebugPageEditorFoldsBlocks(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	for _, path := range []string{"/debug", "/debug?collector=rooms"} {
		page := getPage(t, server, path)
		requireContains(t, page,
			`<div class="editor-tools">
<button type="button" class="secondary" data-yaml-all="open" aria-controls="collector-editor">Expand all</button><button type="button" class="secondary" data-yaml-all="close" aria-controls="collector-editor">Collapse all</button>
<button type="button" class="secondary" id="edit-copy"`,
			".editor-tools button { margin: 0; }",
			".editor .gutter-lines button.fold { position: absolute; left: 0; top: 0; width: 14px; height: var(--editor-line);",
			".editor .gutter-lines button.fold:focus-visible { outline: 2px solid var(--accent);",
			".editor.readonly .yaml-view, .editor.folded .yaml-view { position: relative; inset: auto; height: 360px;",
			".editor .yaml-more { margin-left: 1ch; padding: 0 6px; border: 1px dashed var(--line); border-radius: 3px; color: var(--muted); cursor: pointer; user-select: none; }",
			`<p class="note editor-hint" id="fold-note" role="status" hidden>`,
		)
		script := debugPageScript(page)
		requireContains(t, script,
			// Where a block ends.
			"if (next.kind === \"scalar\" || next.indent > at.indent || (at.open && next.dash && next.indent === at.indent)) { ends[i] = j; continue; }",
			"info.open = !info.dash && out.slice(before).every(([cls]) => cls === null || cls === \"y-comment\");",
			// The rows, a fold's first line saying how many it hides.
			"viewRows.push({ line: i, hidden: yamlEnds[i] - i });",
			"for (let j = i + 1; j <= yamlEnds[i]; j++) rowOf[j] = viewRows.length - 1;",
			`const hiddenText = (count) => "… " + count + (count === 1 ? " line" : " lines");`,
			`const more = jsonSpan("yaml-more", hiddenText(row.hidden));`,
			`if (event.target.closest(".yaml-more")) { refold(() => folds.delete(row.line), row.line); return; }`,
			// The gutter: a button, and the line's own number.
			`const number = document.createElement("div"), toggle = document.createElement("button"), text = jsonSpan("", "");`,
			`text.setAttribute("aria-hidden", "true");`,
			"toggle.hidden = end < 0;",
			"toggle.tabIndex = k === stop ? 0 : -1;",
			`toggle.textContent = open ? "▾" : "▸";`,
			`toggle.setAttribute("aria-expanded", String(open));`,
			`toggle.setAttribute("aria-label", "Lines " + (row.line + 2) + " to " + (end + 1) + ", under line " + (row.line + 1));`,
			"number.lastChild.textContent = String(row.line + 1);",
			`const toggle = event.target.closest("button.fold");`,
			"refold(() => { if (folds.has(row.line)) folds.delete(row.line); else folds.add(row.line); }, row.line);",
			// The keyboard goes from one button to another, the gutter
			// never scrolled by the focus.
			`!["ArrowUp", "ArrowDown", "Home", "End"].includes(event.key)) return;`,
			"buttons[to].tabIndex = 0;",
			"buttons[to].focus({ preventScroll: true });",
			"gutter.scrollTop = 0;",
			// A mark of a hidden line, on its fold's row.
			"const at = [...new Set(marked.filter((n) => n <= rowOf.length).map((n) => rowOf[n - 1]))];",
			// Expand all and Collapse all.
			"if (!open) for (let i = 0; i < yamlEnds.length; i++) if (yamlEnds[i] > i) folds.add(i);",
			`button.disabled = button.dataset.yamlAll === "open" ? folds.size === 0 : !yamlEnds.some((end) => end >= 0);`,
			// The playground, folded, read-only; edited again by a click,
			// Enter or a link to a line.
			"const folded = !editorArea.readOnly && folds.size > 0;\n  editorBox.classList.toggle(\"folded\", folded);\n  $(\"fold-note\").hidden = !folded;",
			"editAt(row.line, columnAt(div, event.clientX, event.clientY));",
			"editAt(topLine(), 0);",
			"if (folds.size > 0) { folds.clear(); drawEditor(); }",
			// What runs is the textarea's text, whatever is folded.
			`else request.collector = $("collector").value;`,
			"function setEditor(text) {\n  editorArea.value = text;\n  folds.clear();",
		)
	}
}
