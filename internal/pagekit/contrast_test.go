package pagekit

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// colours reads the --name: #rrggbb tokens of a theme's colours.
func colours(t *testing.T, theme string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`--([a-z-]+): (#[0-9a-f]{6})`).FindAllStringSubmatch(theme, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// contrast is the WCAG contrast ratio of two #rrggbb colours.
func contrast(a, b string) float64 {
	luminance := func(hex string) float64 {
		var c [3]float64
		for i := range c {
			v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
			x := float64(v) / 255
			if x <= 0.03928 {
				c[i] = x / 12.92
			} else {
				c[i] = math.Pow((x+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	}
	x, y := luminance(a), luminance(b)
	return (max(x, y) + 0.05) / (min(x, y) + 0.05)
}

// In both themes the text colours read against every background a page
// puts text on at 4.5:1 or more, muted text and links among them, and a
// field's border at 3:1 or more, as a control's boundary needs to be seen;
// and every element the keyboard focuses has the accent's outline.
func TestThePagesColoursReadInBothThemes(t *testing.T) {
	for name, theme := range map[string]string{"light": LightColours, "dark": DarkColours} {
		c := colours(t, theme)
		for _, fg := range []string{"fg", "muted", "accent", "bad", "good"} {
			for _, bg := range []string{"bg", "panel", "code"} {
				if r := contrast(c[fg], c[bg]); r < 4.5 {
					t.Errorf("%s: --%s on --%s is %.2f:1, under 4.5:1", name, fg, bg, r)
				}
			}
		}
		if r := contrast(c["on-accent"], c["accent"]); r < 4.5 {
			t.Errorf("%s: --on-accent on --accent is %.2f:1", name, r)
		}
		for _, bg := range []string{"bg", "panel"} {
			if r := contrast(c["field"], c[bg]); r < 3 {
				t.Errorf("%s: --field on --%s is %.2f:1, under 3:1", name, bg, r)
			}
		}
	}
	for _, want := range []string{":focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }", ".sr-only { position: absolute;"} {
		if !strings.Contains(Style, want) {
			t.Errorf("the pages' style lacks %q", want)
		}
	}
	if !strings.Contains(RefreshStyle, "form.refresh select { border-color: var(--field); }") {
		t.Error("the refresh's select has no --field border")
	}
	for _, style := range []string{Style, SortableStyle} {
		if regexp.MustCompile(`(input|select|textarea)[^{]*\{[^}]*border: 1px solid var\(--line\)`).MatchString(style) {
			t.Errorf("a field's border is --line, under 3:1:\n%s", style)
		}
	}
}
