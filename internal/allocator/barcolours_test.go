package allocator

import (
	"math"
	"regexp"
	"strconv"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/pagekit"
)

// barContrast is the WCAG contrast ratio of two #rrggbb colours.
func barContrast(a, b string) float64 {
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

// The bars' and swatches' colours stand out from the page and the panel at
// 3:1 or more in both themes, as a graphic that carries meaning must (WCAG
// 1.4.11); the two kinds are told apart by hue, and a draining segment by
// its stripes too.
func TestTheBarsColoursStandOutInBothThemes(t *testing.T) {
	bars := regexp.MustCompile(`--bar-static: (#[0-9a-f]{6}); --bar-probe: (#[0-9a-f]{6});`).FindAllStringSubmatch(statusStyle, -1)
	if len(bars) != 3 || bars[1][1] != bars[2][1] || bars[1][2] != bars[2][2] {
		t.Fatalf("the bars' colours are %v; want light, then dark twice", bars)
	}
	page := regexp.MustCompile(`--bg: (#[0-9a-f]{6}); --panel: (#[0-9a-f]{6});`)
	for i, theme := range []string{pagekit.LightColours, pagekit.DarkColours} {
		bg := page.FindStringSubmatch(theme)
		for _, bar := range bars[min(i, 1)][1:] {
			for _, back := range bg[1:] {
				if r := barContrast(bar, back); r < 3 {
					t.Errorf("the bar colour %s on %s is %.2f:1, under 3:1", bar, back, r)
				}
			}
		}
	}
}
