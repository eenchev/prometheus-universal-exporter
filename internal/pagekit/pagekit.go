// Package pagekit is what the HTML pages of the exporter and of the target
// allocator share, so the two look and behave alike: the stylesheet and its
// light and dark colours, the theme picker and the script that applies the
// theme chosen before a page is first drawn, the project's logo as the
// browser-tab icon, the pages' Content-Security-Policy, the controls of a
// page that refreshes itself, and how a page writes a duration. Each page's bar and content stay its
// program's own.
package pagekit

import (
	_ "embed"
	"encoding/base64"
)

// Style is the pages' shared stylesheet. The colours follow the theme
// chosen in the bar (ThemeHead), or with Auto, the default, the browser's
// light or dark preference, and the layout works at phone width.
const Style = `
:root { color-scheme: light dark; ` + LightColours + ` }
:root[data-theme="light"] { color-scheme: light; }
@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { color-scheme: dark; ` + DarkColours + ` } }
:root[data-theme="dark"] { color-scheme: dark; ` + DarkColours + ` }
* { box-sizing: border-box; }
body { margin: 0; padding: 32px 16px; background: var(--bg); color: var(--fg); font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; overflow-wrap: anywhere; }
main { max-width: 860px; margin: 0 auto; }
h1 { font-size: 26px; margin: 0 0 4px; }
h2 { font-size: 17px; margin: 32px 0 8px; }
p.meta { color: var(--muted); margin: 0; }
a { color: var(--accent); }
code { background: var(--code); padding: 1px 5px; border-radius: 4px; font-size: 13px; }
ul.links { padding-left: 20px; }
` + selectStyle + ChromeStyle

// LightColours and DarkColours are the pages' colours in each theme. The
// dark ones apply when the browser prefers dark and no theme is chosen, and
// when Dark is; color-scheme follows, so the browser's own parts of the page,
// its scrollbars and form controls, match.
const (
	LightColours = "--fg: #1d1d1f; --muted: #5f6368; --bg: #ffffff; --panel: #f7f7f8; --line: #d9dce1; --accent: #c2410c; --on-accent: #ffffff; --code: #f3f4f6; --bad: #b91c1c; --good: #15803d; --danger-bg: #fef2f2; " + lightJSONColours + " " + lightYAMLColours
	DarkColours  = "--fg: #e8eaed; --muted: #9aa0a6; --bg: #16181b; --panel: #1d2024; --line: #34373c; --accent: #ff8a5c; --on-accent: #16181b; --code: #23262b; --bad: #f87171; --good: #4ade80; --danger-bg: #2c1517; " + darkJSONColours + " " + darkYAMLColours
)

// lightJSONColours and darkJSONColours are the colours of the debug page's
// JSON viewer (jsonView): keys, strings, numbers, booleans, null and the
// punctuation. Each reads at 5.5:1 or more on the pages' background, panels
// and code in its theme, and they differ in hue enough to tell a key from a
// string and a number from a string of digits.
const (
	lightJSONColours = "--json-key: #1e40af; --json-string: #166534; --json-number: #9a3412; --json-boolean: #7e22ce; --json-null: #be123c; --json-punct: #5f6368;"
	darkJSONColours  = "--json-key: #93c5fd; --json-string: #86efac; --json-number: #fdba74; --json-boolean: #d8b4fe; --json-null: #fda4af; --json-punct: #9aa0a6;"
)

// lightYAMLColours and darkYAMLColours are the colours of the debug page's
// collector editor (yamlLines) that the JSON viewer has none of: comments,
// a list's dashes, a block scalar's indicator and its text, anchors and
// aliases, tags, the exporter's {{param_…}} placeholders and ${NAME}
// references, each of the last two on a tint of its own, and a value the
// page withholds, muted on a grey; keys, strings, numbers, booleans, null
// and punctuation take the JSON viewer's colours, so a key is one colour on
// the page wherever it is shown. Each reads at 4.5:1 or more on
// the editor's backgrounds in its theme, the playground's and a configured
// collector's, on its tint, and on the band of a line an error names; the
// selection is a tint the text shows through.
const (
	lightYAMLColours = "--yaml-comment: #56657a; --yaml-dash: #92400e; --yaml-indicator: #a21caf; --yaml-block: #115e59; --yaml-anchor: #155e75; --yaml-tag: #854d0e; --yaml-param: #4338ca; --yaml-param-bg: #eef2ff; --yaml-env: #9d174d; --yaml-env-bg: #fdf2f8; --yaml-withheld: #4b5563; --yaml-withheld-bg: #e5e7eb; --editor-selection: rgba(59, 130, 246, 0.28);"
	darkYAMLColours  = "--yaml-comment: #8fa1b3; --yaml-dash: #fbbf24; --yaml-indicator: #f0abfc; --yaml-block: #5eead4; --yaml-anchor: #67e8f9; --yaml-tag: #fde047; --yaml-param: #a5b4fc; --yaml-param-bg: #272a4a; --yaml-env: #f9a8d4; --yaml-env-bg: #3a1a2c; --yaml-withheld: #b0b6bc; --yaml-withheld-bg: #34373c; --editor-selection: rgba(147, 197, 253, 0.3);"
)

// selectStyle draws the pages' dropdowns' arrow itself: the browser's own
// sits against the field's right edge, and its place differs between
// browsers. The arrow is two gradients in the muted text colour, so it
// follows the light or dark theme with no image, 14px in from the edge, and
// the text stops before it. The selector outweighs each page's own rule for
// its fields, whose background shorthand would otherwise take the arrow off.
const selectStyle = `
:root select:not([multiple]) { appearance: none; -webkit-appearance: none; padding-right: 34px; background-image: linear-gradient(45deg, transparent 50%, var(--muted) 50%), linear-gradient(135deg, var(--muted) 50%, transparent 50%); background-position: calc(100% - 19px) 50%, calc(100% - 14px) 50%; background-size: 5px 5px, 5px 5px; background-repeat: no-repeat; }
:root select:not([multiple]):disabled { opacity: 0.6; }
`

// Policy is the pages' Content-Security-Policy. Its img-src is for the
// icon (Icon), which is a data: URL.
const Policy = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// The theme is chosen in the bar of every page: Auto, the default, follows
// the browser's light or dark preference, and Light and Dark hold whatever
// it prefers. The choice is kept in the browser's local storage, so it holds
// on every page and every visit; where the browser keeps none, as in some
// private windows, it holds until the page is left. It is the data-theme
// attribute of the html element (Style), which ThemeHead sets from what
// is kept before the page is first drawn, so a page never shows in the other
// theme first; a page's refresh swaps in its content alone, under that
// element, and keeps it. Without JavaScript nothing could be kept or
// applied, so the picker stays hidden and the pages follow the browser.

// ThemeStorageKey is where the choice is kept: light or dark, and nothing
// for Auto. The exporter's pages and the allocator's share it, so a browser
// that opens both through one host keeps one choice.
const ThemeStorageKey = "theme"

// ThemeHead is the script every page's head runs first.
const ThemeHead = `<script id="theme-head">try { const theme = localStorage.getItem("` + ThemeStorageKey + `"); if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme; } catch (error) { /* Auto, then */ }</script>`

// ThemePicker is the picker in a page's bar, shown by its script, which
// runs in a function of its own so that nothing it declares meets a page's
// own script.
const ThemePicker = `<label class="theme" hidden>Theme <select id="theme">
<option value="auto">Auto</option><option value="light">Light</option><option value="dark">Dark</option>
</select></label><script id="theme-script">
(() => {
  const picker = document.getElementById("theme"), root = document.documentElement;
  picker.value = root.dataset.theme || "auto";
  picker.addEventListener("change", () => {
    if (picker.value === "auto") delete root.dataset.theme;
    else root.dataset.theme = picker.value;
    try {
      if (picker.value === "auto") localStorage.removeItem("` + ThemeStorageKey + `");
      else localStorage.setItem("` + ThemeStorageKey + `", picker.value);
    } catch (error) { /* chosen for this page alone */ }
  });
  picker.parentElement.hidden = false;
})();
</script>`

// ChromeStyle is the style of the bar at the top of every page, a
// header.site holding a nav of the home link and a .pages span of the
// other pages' links and the ThemePicker.
const ChromeStyle = `
header.site { margin: -16px 0 20px; padding-bottom: 10px; border-bottom: 1px solid var(--line); font-size: 14px; }
header.site nav { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: baseline; gap: 6px 20px; }
header.site .pages { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 16px; }
header.site a { text-decoration: none; }
header.site a:hover { text-decoration: underline; }
header.site [aria-current="page"] { color: var(--fg); font-weight: 600; }
header.site label.theme { color: var(--muted); }
header.site .danger-badge { padding: 1px 8px; border: 1px solid var(--bad); border-radius: 999px; background: var(--danger-bg); color: var(--bad); font-size: 12px; font-weight: 600; }
header.site label.theme[hidden] { display: none; }
header.site label.theme select { width: auto; height: 26px; margin: 0 0 0 4px; padding: 0 30px 0 8px; border: 1px solid var(--line); border-radius: 6px; background-color: var(--bg); color: var(--fg); font: inherit; font-size: 13px; background-position: calc(100% - 17px) 50%, calc(100% - 12px) 50%; }
`

// Logo is the project's logo, docs/logo.svg, which the go:embed pattern
// cannot reach from here: logo.svg is a copy of it, and
// TestThePagesIconIsTheProjectsLogo fails the moment the two differ.
//
//go:embed logo.svg
var Logo []byte

// Icon is the pages' browser-tab icon, the logo as a data: URL in the page
// itself, so it needs no request: none that a page's authentication would
// ask a credential for, nor one a reverse proxy's path prefix would have to
// be reckoned with. Policy's img-src allows data: for it. It is written into
// each page's template text rather than given to it as data, since
// html/template would refuse a data: URL put in an href.
var Icon = `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString(Logo) + `">`
