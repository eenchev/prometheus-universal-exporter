package exporter

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// A chained collector's trip (fetch/chain.go) is drawn as a workflow: one
// block per step, in order, with arrows between them. The diagram is shown
// in the debug report of a probe, of the debug page's run and of a static
// target's debug run, on the status page for each chained collector's last
// trip, and on the static target status page for each chained target's
// last scrape. The text debug report gives the same chain as one line per
// step (chainLines).
//
// Each block shows the step's name, its method and path with credentials
// masked, its state, its status and its duration, and the names of the
// values it read; the debug report adds their values, a secret one as ***.
// A fan-out is a stacked block saying how many items it asked for, how many
// went through, failed and were cut, and up to 5 failed items. The states:
//
//   - ok: a green border;
//   - failed, or timed out: a red border and tint, with the error, and for
//     a timeout whose deadline ran out;
//   - a fan-out with failures: red under on_item_error: fail, amber under
//     skip;
//   - cut by max_items: an amber note under the block;
//   - not reached: a grey dashed border.
//
// A trip answered from the cache is one grey block. Each state also has its
// word in the block, so the diagram reads in print and for a reader who
// cannot tell the colours apart. The diagram is an SVG the exporter writes,
// with no script, followed by its text equivalent, an ordered list of the
// steps and their states, for screen readers. In the debug report each
// block links to its step's details below it.

// chainDiagramOptions say what a diagram shows beyond each step's state.
type chainDiagramOptions struct {
	// values shows the values each step read, a secret one as ***, as the
	// debug report does; the status pages show their names alone.
	values bool
	// links makes each block a link to the element of id chainStepID.
	links bool
	// cached, when set, draws the trip as answered from the cache, fetched
	// then.
	cached time.Time
}

// The diagram's geometry, in user units.
const (
	diagramWidth  = 560
	blockWidth    = 520
	blockLeft     = 20
	lineHeight    = 16
	blockPadding  = 10
	arrowHeight   = 26
	stackOffset   = 5
	maxErrorLines = 3
	maxShownItems = 5
	// lineChars is the most characters of a block's line. A line runs
	// blockWidth less a padding on either side, 500 units, in 12px
	// monospace, whose character is 0.6em, 7.2 units, in Menlo, Consolas
	// and the browsers' own monospace alike: 69 characters fill it, and 66
	// leave a margin for a wider font.
	lineChars = 66
)

// chainStepID is the id of the element of a step's details in a debug
// report, which its block links to.
func chainStepID(name string) string { return "chain-step-" + name }

// chainBlock is one block of a diagram: its state's class and word, and its
// lines of text.
type chainBlock struct {
	class, word string
	lines       []string
	stacked     bool
	note        string // the amber note under it, for a cut fan-out
	link        string
}

// diagramNode is an element of a diagram: its tag, its attributes in
// order, its text and its children. The status pages write it as markup
// (html); the debug page's script, which writes no markup, makes the same
// elements from it, each text set as text.
type diagramNode struct {
	Tag      string        `json:"tag"`
	Attrs    [][2]string   `json:"attrs,omitempty"`
	Text     string        `json:"text,omitempty"`
	Children []diagramNode `json:"children,omitempty"`
}

// node is a diagramNode of tag with attributes given as name and value in
// turn.
func node(tag string, attrs ...string) diagramNode {
	n := diagramNode{Tag: tag}
	for i := 0; i+1 < len(attrs); i += 2 {
		n.Attrs = append(n.Attrs, [2]string{attrs[i], attrs[i+1]})
	}
	return n
}

// html writes n as markup, every attribute value and text escaped.
func (n diagramNode) html(b *strings.Builder) {
	fmt.Fprintf(b, "<%s", n.Tag)
	for _, a := range n.Attrs {
		fmt.Fprintf(b, ` %s="%s"`, a[0], template.HTMLEscapeString(a[1]))
	}
	b.WriteString(">")
	b.WriteString(template.HTMLEscapeString(n.Text))
	for _, child := range n.Children {
		child.html(b)
	}
	fmt.Fprintf(b, "</%s>", n.Tag)
}

// chainDiagram is the diagram of report, with its text equivalent, as
// markup for a page's template.
func chainDiagram(report *fetch.ChainReport, options chainDiagramOptions) template.HTML {
	if report == nil {
		return ""
	}
	var b strings.Builder
	for _, n := range chainDiagramNodes(report, options) {
		n.html(&b)
	}
	return template.HTML(b.String()) // #nosec G203 -- each text and attribute value is escaped (diagramNode.html)
}

// chainDiagramNodes are the diagram of report, an svg element, and its text
// equivalent, an ordered list.
func chainDiagramNodes(report *fetch.ChainReport, options chainDiagramOptions) []diagramNode {
	blocks := chainBlocks(report, options)
	height := 0
	for i, block := range blocks {
		height += blockHeight(block)
		if block.stacked {
			height += 2 * stackOffset
		}
		if block.note != "" {
			height += lineHeight + 4
		}
		if i < len(blocks)-1 {
			height += arrowHeight
		}
	}
	height += 2 * blockPadding
	itoa := strconv.Itoa
	svg := node("svg", "class", "chain-diagram", "xmlns", "http://www.w3.org/2000/svg", "viewBox", fmt.Sprintf("0 0 %d %d", diagramWidth, height), "width", itoa(diagramWidth), "height", itoa(height),
		"role", "img", "aria-label", "The steps of the chain, in order; the list after the diagram says the same in words")
	style := node("style")
	style.Text = chainDiagramStyle
	marker := node("marker", "id", "chain-arrow", "viewBox", "0 0 10 10", "refX", "9", "refY", "5", "markerWidth", "7", "markerHeight", "7", "orient", "auto-start-reverse")
	marker.Children = []diagramNode{node("path", "d", "M0,0 L10,5 L0,10 z", "class", "chain-arrowhead")}
	defs := node("defs")
	defs.Children = []diagramNode{marker}
	svg.Children = append(svg.Children, style, defs)
	y := blockPadding
	for i, block := range blocks {
		h := blockHeight(block)
		var shapes []diagramNode
		if block.stacked {
			// The cards behind it, below and to its right.
			for offset := 2 * stackOffset; offset > 0; offset -= stackOffset {
				shapes = append(shapes, node("rect", "class", "chain-block chain-"+block.class+" chain-stack", "x", itoa(blockLeft+offset), "y", itoa(y+offset), "width", itoa(blockWidth-2*stackOffset), "height", itoa(h), "rx", "6"))
			}
		}
		if block.stacked {
			// The cards behind do not show through a tinted block.
			shapes = append(shapes, node("rect", "class", "chain-base", "x", itoa(blockLeft), "y", itoa(y), "width", itoa(blockWidth), "height", itoa(h), "rx", "6"))
		}
		shapes = append(shapes, node("rect", "class", "chain-block chain-"+block.class, "x", itoa(blockLeft), "y", itoa(y), "width", itoa(blockWidth), "height", itoa(h), "rx", "6"))
		for j, line := range block.lines {
			class := "chain-text"
			if j == 0 {
				class += " chain-title"
			}
			text := node("text", "class", class, "x", itoa(blockLeft+blockPadding), "y", itoa(y+blockPadding+(j+1)*lineHeight-4))
			text.Text = line
			shapes = append(shapes, text)
		}
		if block.link != "" {
			link := node("a", "href", "#"+block.link)
			link.Children = shapes
			shapes = []diagramNode{link}
		}
		svg.Children = append(svg.Children, shapes...)
		y += h
		if block.stacked {
			y += 2 * stackOffset
		}
		if block.note != "" {
			note := node("text", "class", "chain-text chain-note", "x", itoa(blockLeft+blockPadding), "y", itoa(y+lineHeight))
			note.Text = block.note
			svg.Children = append(svg.Children, note)
			y += lineHeight + 4
		}
		if i < len(blocks)-1 {
			x := itoa(blockLeft + blockWidth/2)
			svg.Children = append(svg.Children, node("line", "class", "chain-arrow", "x1", x, "y1", itoa(y+2), "x2", x, "y2", itoa(y+arrowHeight-2), "marker-end", "url(#chain-arrow)"))
			y += arrowHeight
		}
	}
	list := node("ol", "class", "chain-steps", "style", "position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0)")
	for _, block := range blocks {
		item := node("li")
		item.Text = strings.Join(block.lines, "; ") + noteText(block.note)
		list.Children = append(list.Children, item)
	}
	return []diagramNode{svg, list}
}

// noteText is a block's note as its text equivalent ends with it.
func noteText(note string) string {
	if note == "" {
		return ""
	}
	return "; " + note
}

// blockHeight is the height of a block.
func blockHeight(block chainBlock) int {
	return len(block.lines)*lineHeight + 2*blockPadding
}

// chainDiagramStyle draws the states in the colours of the page's theme,
// --good, --bad and --muted, and its text colour. Where a page sets no
// theme, the text and the block a stack's cards hide behind are the
// browser's own text and background, Canvas and CanvasText, which always
// read against each other. Amber is the diagram's own: #9a6700 in a light
// theme and #d29922 in a dark one (light-dark), each 4.5:1 or more against
// the page and the panel, where one amber for both stood out from neither
// a white page nor a dark one; a browser without light-dark keeps #9a6700.
const chainDiagramStyle = `
.chain-block{fill:var(--panel,transparent);stroke-width:2}
.chain-ok{stroke:var(--good,#1a7f37)}
.chain-failed{stroke:var(--bad,#cf222e);fill:var(--danger-bg,rgba(207,34,46,.12))}
.chain-warned{stroke:#9a6700;stroke:light-dark(#9a6700,#d29922);fill:rgba(191,135,0,.12)}
.chain-unreached{stroke:var(--muted,#8c959f);stroke-dasharray:6 4}
.chain-cached{stroke:var(--muted,#8c959f);fill:rgba(140,149,159,.12)}
.chain-stack{opacity:.55}
.chain-base{fill:var(--bg,Canvas);stroke:none}
.chain-text{fill:var(--fg,CanvasText);font:12px ui-monospace,SFMono-Regular,Menlo,monospace}
.chain-title{font-weight:bold}
.chain-note{fill:#9a6700;fill:light-dark(#9a6700,#d29922)}
.chain-arrow{stroke:var(--fg,currentColor);stroke-width:1.5}
.chain-arrowhead{fill:var(--fg,currentColor)}
`

// chainBlocks are the blocks of report's diagram.
func chainBlocks(report *fetch.ChainReport, options chainDiagramOptions) []chainBlock {
	if !options.cached.IsZero() {
		return []chainBlock{{class: "cached", word: "from the cache", lines: []string{
			"from the cache, fetched " + options.cached.Format("15:04:05"),
			fmt.Sprintf("a chain of %d requests, none sent for this answer", len(report.Steps)),
		}}}
	}
	blocks := make([]chainBlock, 0, len(report.Steps))
	for i := range report.Steps {
		blocks = append(blocks, stepBlock(&report.Steps[i], options))
	}
	return blocks
}

// stepBlock is the block of one step.
func stepBlock(step *fetch.ChainStepReport, options chainDiagramOptions) chainBlock {
	block := chainBlock{class: "ok", word: "ok"}
	if options.links && step.Reached() {
		block.link = chainStepID(step.Name)
	}
	title := step.Name + "  " + step.Method
	if step.URL != "" {
		title += " " + pathOf(step.URL)
	}
	block.lines = append(block.lines, fitted(title))
	switch step.State {
	case fetch.ChainStepNotReached:
		block.class, block.word = "unreached", "not reached"
		block.lines = append(block.lines, "not reached")
		block.stacked = step.ForEach
		return block
	case fetch.ChainStepTimedOut:
		block.class, block.word = "failed", "timed out"
	case fetch.ChainStepFailed:
		block.class, block.word = "failed", "failed"
		if step.ForEach && step.OnItemError == model.ChainItemSkip && step.ItemsFailed > 0 {
			block.class, block.word = "warned", "failed items skipped"
		}
	}
	state := block.word
	if step.State == fetch.ChainStepTimedOut {
		state = "timed out after " + step.Duration.Round(100*time.Millisecond).String()
		if step.Deadline != "" {
			state += ", " + step.Deadline + " ran out"
		}
	}
	if step.Status != 0 && !step.ForEach {
		state += fmt.Sprintf("  status %d", step.Status)
	}
	state += "  " + step.Duration.Round(time.Millisecond).String()
	block.lines = append(block.lines, fitted(state))
	if len(step.Values) > 0 {
		block.lines = append(block.lines, fitted("read: "+shownValues(step.Values, options.values)))
	}
	if step.ForEach {
		block.stacked = true
		block.lines = append(block.lines, fmt.Sprintf("%d items: %d went through, %d failed", len(step.Items), len(step.Items)-step.ItemsFailed, step.ItemsFailed))
		shown := 0
		for _, item := range step.Items {
			if item.Error == "" {
				continue
			}
			if shown == maxShownItems {
				block.lines = append(block.lines, fmt.Sprintf("and %d more", step.ItemsFailed-shown))
				break
			}
			block.lines = append(block.lines, fitted(fmt.Sprintf("item %d (%s): %s", item.Index+1, item.Each, item.Error)))
			shown++
		}
		if step.ItemsCut > 0 {
			block.note = fmt.Sprintf("cut at %d of %d (max_items)", step.ItemsTotal-step.ItemsCut, step.ItemsTotal)
		}
	}
	if block.class != "ok" && step.Error != "" && (!step.ForEach || step.State == fetch.ChainStepTimedOut) {
		block.lines = append(block.lines, wrapped(step.Error, lineChars, maxErrorLines)...)
	}
	return block
}

// pathOf is a masked URL's path and query, which a block shows: the origin
// is the target's for every step.
func pathOf(shown string) string {
	if i := strings.Index(shown, "://"); i >= 0 {
		if j := strings.IndexByte(shown[i+3:], '/'); j >= 0 {
			return shown[i+3+j:]
		}
		return "/"
	}
	return shown
}

// shownValues are the values a step read: their names, or with values
// their values too, a secret one as ***.
func shownValues(values []fetch.ChainValueReport, withValues bool) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		if !withValues {
			parts = append(parts, v.Name)
			continue
		}
		parts = append(parts, v.Name+"="+shownChainValue(v))
	}
	return strings.Join(parts, ", ")
}

// shownChainValue is a value as a report shows it: *** for a secret one,
// "(not found)" for an optional one not found.
func shownChainValue(v fetch.ChainValueReport) string {
	switch {
	case v.Secret:
		return "***"
	case !v.Found:
		return "(not found)"
	}
	return shownStart(v.Text, 64)
}

// fitted is a line of a block cut to lineChars characters, the last of them
// … when it goes on, so it stays inside the block.
func fitted(line string) string {
	runes := []rune(line)
	if len(runes) <= lineChars {
		return line
	}
	return string(runes[:lineChars-1]) + "…"
}

// wrapped is text in lines of at most width characters, at most lines of
// them, the last ending with … when text goes on, within the width.
func wrapped(text string, width, lines int) []string {
	var out []string
	runes := []rune(text)
	for len(runes) > 0 && len(out) < lines {
		n := min(width, len(runes))
		if n < len(runes) {
			if space := strings.LastIndex(string(runes[:n]), " "); space > width/2 {
				n = len([]rune(string(runes[:n])[:space])) + 1
			}
		}
		out = append(out, strings.TrimSpace(string(runes[:n])))
		runes = runes[n:]
	}
	if len(runes) > 0 && len(out) > 0 {
		last := []rune(out[len(out)-1])
		out[len(out)-1] = string(last[:min(len(last), width-1)]) + "…"
	}
	return out
}

// chainLines are the chain of a trip as the text debug report gives it: a
// line for each step, its name, its state in capitals when it is not ok,
// and what went wrong or how it went; then its values and, for a fan-out,
// its first items.
func chainLines(report *fetch.ChainReport) []string {
	width := 0
	for _, step := range report.Steps {
		width = max(width, len(step.Name))
	}
	var out []string
	for _, step := range report.Steps {
		state := strings.ToUpper(step.State)
		if step.State == fetch.ChainStepOK {
			state = "ok"
		}
		line := fmt.Sprintf("  %-*s  %-11s", width, step.Name, state)
		switch {
		case !step.Reached():
		case step.ForEach && step.State != fetch.ChainStepTimedOut:
			line += fmt.Sprintf(" %d items, %d failed", len(step.Items), step.ItemsFailed)
			if step.OnItemError == model.ChainItemSkip && step.ItemsFailed > 0 {
				line += " and skipped"
			}
			line += ", in " + step.Duration.Round(time.Millisecond).String()
		case step.Error != "":
			line += " " + step.Error
		default:
			line += fmt.Sprintf(" %s %s -> %d in %s, %d bytes", step.Method, step.URL, step.Status, step.Duration.Round(time.Millisecond), step.Bytes)
		}
		out = append(out, strings.TrimRight(line, " "))
		if !step.Reached() {
			continue
		}
		indent := strings.Repeat(" ", width+4)
		if len(step.Values) > 0 {
			out = append(out, indent+"read: "+shownValues(step.Values, true))
		}
		if step.ItemsCut > 0 {
			out = append(out, fmt.Sprintf("%scut at %d of %d (max_items)", indent, step.ItemsTotal-step.ItemsCut, step.ItemsTotal))
		}
		for i, item := range step.Items {
			if i == maxShownItems {
				out = append(out, fmt.Sprintf("%sand %d more items, %d of them failed", indent, len(step.Items)-i, failedAfter(step.Items, i)))
				break
			}
			outcome := fmt.Sprintf("%d in %s", item.Status, item.Duration.Round(time.Millisecond))
			if item.Error != "" {
				outcome = item.Error
			}
			out = append(out, fmt.Sprintf("%sitem %d (%s): %s %s -> %s", indent, item.Index+1, shownStart(item.Each, 64), step.Method, item.URL, outcome))
		}
	}
	return out
}

// chainVia is what the Requests section of a debug report adds to a
// request a chain made: its step, and its fan-out item.
func chainVia(req fetch.TracedRequest) string {
	switch {
	case req.Step == "":
		return ""
	case req.Item > 0:
		return fmt.Sprintf(" (step %s, item %d)", req.Step, req.Item)
	}
	return " (step " + req.Step + ")"
}

// failedAfter counts the items from i on that failed.
func failedAfter(items []fetch.ChainItemReport, i int) int {
	n := 0
	for _, item := range items[i:] {
		if item.Error != "" {
			n++
		}
	}
	return n
}

// debugChainStep is a step's details in the debug page's report, under
// the id its block links to: its title and its lines, which the page sets
// as text.
type debugChainStep struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Lines []string `json:"lines"`
}

// chainDetails are the details of each step of report below its diagram in
// the debug page's report: what it sent and its status, an error, the values
// it read, then its answer laid out as the page's Response is, its headers,
// a blank line and the start of its body; for a fan-out, each item, those
// that failed first. configured says which query values the configuration
// set, which are shown (fetch.ConfiguredQuery).
func chainDetails(report *fetch.ChainReport, configured func(name, value string) bool) []debugChainStep {
	var out []debugChainStep
	for _, step := range report.Steps {
		if !step.Reached() {
			continue
		}
		var lines []string
		answered := !step.ForEach && step.Status != 0
		if !step.ForEach && !answered {
			lines = append(lines, fmt.Sprintf("%s %s -> no answer", step.Method, fetch.RedactRequestURL(step.URL, configured)))
		} else if answered {
			status := strings.TrimSpace(fmt.Sprintf("%d %s", step.Status, http.StatusText(step.Status)))
			lines = append(lines, fmt.Sprintf("%s %s -> %s in %s, %d bytes", step.Method, fetch.RedactRequestURL(step.URL, configured), status, step.Duration.Round(time.Millisecond), step.Bytes))
		}
		if step.Error != "" {
			lines = append(lines, "error: "+step.Error)
		}
		if len(step.Values) > 0 {
			lines = append(lines, "read: "+shownValues(step.Values, true))
		}
		if answered {
			if headers := headerLines(step.Headers); len(headers) > 0 {
				lines = append(lines, "")
				lines = append(lines, headers...)
			}
			if step.Excerpt != "" {
				lines = append(lines, "", shownStart(step.Excerpt, 512))
			}
		}
		if step.ItemsCut > 0 {
			lines = append(lines, fmt.Sprintf("cut at %d of %d (max_items)", step.ItemsTotal-step.ItemsCut, step.ItemsTotal))
		}
		items := append([]fetch.ChainItemReport(nil), step.Items...)
		// The items that failed first, each group in order.
		sortItemsFailedFirst(items)
		for _, item := range items {
			outcome := fmt.Sprintf("%d in %s", item.Status, item.Duration.Round(time.Millisecond))
			if item.Error != "" {
				outcome = item.Error
			}
			lines = append(lines, fmt.Sprintf("item %d (%s): %s %s -> %s", item.Index+1, shownStart(item.Each, 64), step.Method, fetch.RedactRequestURL(item.URL, configured), outcome))
		}
		out = append(out, debugChainStep{ID: chainStepID(step.Name), Title: step.Name + ": " + step.State, Lines: lines})
	}
	return out
}

// sortItemsFailedFirst puts the items that failed first, keeping the order
// of each group.
func sortItemsFailedFirst(items []fetch.ChainItemReport) {
	failed := make([]fetch.ChainItemReport, 0, len(items))
	var rest []fetch.ChainItemReport
	for _, item := range items {
		if item.Error != "" {
			failed = append(failed, item)
		} else {
			rest = append(rest, item)
		}
	}
	copy(items, append(failed, rest...))
}
