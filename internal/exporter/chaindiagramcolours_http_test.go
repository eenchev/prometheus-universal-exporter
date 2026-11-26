//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
)

// The workflow diagram's amber, of a fan-out whose failed items were skipped
// and of a cut fan-out's note, reads against the page and the panel at 4.5:1
// or more in both themes: a darker amber in the light theme and a lighter one
// in the dark (light-dark), with the darker one for a browser without
// light-dark. Where a page sets no theme, the text and the block the stack's
// cards hide behind are the browser's own text and background, which read
// against each other, not the page's text on white.
func TestTheWorkflowDiagramsAmberReadsInBothThemes(t *testing.T) {
	for _, rule := range []string{
		".chain-warned{stroke:#9a6700;stroke:light-dark(#9a6700,#d29922);",
		".chain-note{fill:#9a6700;fill:light-dark(#9a6700,#d29922)}",
		".chain-base{fill:var(--bg,Canvas);",
		".chain-text{fill:var(--fg,CanvasText);",
	} {
		if !strings.Contains(chainDiagramStyle, rule) {
			t.Errorf("the diagram's style lacks %q", rule)
		}
	}
	colour := func(theme, name string) string {
		return regexp.MustCompile(`--` + name + `: (#[0-9a-f]{6});`).FindStringSubmatch(theme)[1]
	}
	for amber, theme := range map[string]string{"#9a6700": pagekit.LightColours, "#d29922": pagekit.DarkColours} {
		for _, back := range []string{"bg", "panel"} {
			if r := contrastRatio(amber, colour(theme, back)); r < 4.5 {
				t.Errorf("the amber %s on --%s %s is %.2f:1, under 4.5:1", amber, back, colour(theme, back), r)
			}
		}
	}
}

// An open chain's diagram, wider than the cell that holds it, widens the
// cell rather than run over the next columns, and scrolls within the cell
// where the table has no room left. A table that scrolls sideways keeps the
// text kept out of sight for screen readers inside its box, so a column
// beyond the page's edge does not widen the page.
func TestTheStatusPagesChainStaysInItsCell(t *testing.T) {
	page := getPath(statusTargets(t), http.MethodGet, "/static-targets/status").Body.String()
	requireContains(t, page,
		"td.what:has(> details.chain[open]) { max-width: none; }",
		"td.what details.chain { overflow-x: auto; }",
		".scroll { overflow-x: auto; position: relative; }")
}
