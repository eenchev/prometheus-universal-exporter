package decode

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// renderedHTML is a parsed document written out as markup again.
func renderedHTML(t *testing.T, root *html.Node) string {
	t.Helper()
	var out bytes.Buffer
	if err := html.Render(&out, root); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The content of a template is taken out of an HTML document when it is
// parsed: the row with placeholders a script copies, a template inside it,
// a template the parser moves out of a table's rows. The template element
// stays, with its attributes, and so does everything around it; an element
// named template inside an svg is no HTML template and keeps its content.
func TestParseHTMLEmptiesTemplates(t *testing.T) {
	const page = `<html><head><template id="head"><meta name="x"></template></head><body>` +
		`<div id="list">1<template id="item" class="t"><p>{{n}}</p><template><b>inner</b></template>text<!-- c --></template>2</div>` +
		`<table><template><tr><td>{{n}}</td></tr></template><tr><td>3</td></tr></table>` +
		`<svg><template><text>4</text></template></svg><TEMPLATE>upper</TEMPLATE>` +
		`</body></html>`
	const want = `<html><head><template id="head"></template></head><body>` +
		`<div id="list">1<template id="item" class="t"></template>2</div>` +
		`<table><template></template><tbody><tr><td>3</td></tr></tbody></table>` +
		`<svg><template><text>4</text></template></svg><template></template>` +
		`</body></html>`
	document, err := ParseHTML([]byte(page))
	if err != nil {
		t.Fatal(err)
	}
	if got := renderedHTML(t, document.Nodes[0]); got != want {
		t.Errorf("the document is\n%s\nwant\n%s", got, want)
	}
	// What was taken out is reached from nowhere in the document.
	if found := document.Find("p, b, meta").Length(); found != 0 {
		t.Errorf("%d elements of the templates are selected", found)
	}
	// The html decoder parses that way.
	decoded, err := decodeAs(t, "html", "text/html", page)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderedHTML(t, decoded.Data.(*HTMLDecoded).Document.Nodes[0]); got != want {
		t.Errorf("the decoder's document is\n%s\nwant\n%s", got, want)
	}
}

// A page without a template is the document the parser makes of it, node
// for node, and one with templates differs from it by their content alone:
// over every fixture page, ParseHTML's document is the parser's with the
// children of its HTML template elements removed.
func TestParseHTMLIsTheParsersDocumentButForTemplateContent(t *testing.T) {
	files, err := filepath.Glob("../../testdata/html/*.*html")
	if err != nil || len(files) < 10 {
		t.Fatalf("%d fixtures, %v", len(files), err)
	}
	withTemplates := 0
	for _, file := range files {
		page, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		document, err := ParseHTML(page)
		if err != nil {
			t.Fatal(err)
		}
		parsers, err := goquery.NewDocumentFromReader(bytes.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		if templates := parsers.Find("template").FilterFunction(func(_ int, template *goquery.Selection) bool { return template.Nodes[0].Namespace == "" }); templates.Length() > 0 {
			if !bytes.Contains(bytes.ToLower(page), []byte("<template")) {
				t.Fatalf("%s has a template its markup does not write", file)
			}
			withTemplates++
			templates.Contents().Remove()
		}
		if got, want := renderedHTML(t, document.Nodes[0]), renderedHTML(t, parsers.Nodes[0]); got != want {
			t.Errorf("%s is parsed into another document than the parser's without its templates' content", file)
		}
		if strings.Contains(renderedHTML(t, document.Nodes[0]), "{{name}}") {
			t.Errorf("%s still has a template's placeholder", file)
		}
	}
	if withTemplates == 0 {
		t.Fatal("no fixture has a template")
	}
}

// A template with a shadowrootmode attribute, of any value, or with the
// shadowroot attribute browsers read before it, is a declarative shadow
// root: a browser attaches its content to the element around it and renders
// it, so it is kept, where it was taken out with every template's. A
// template inside it is emptied like any other, and an attribute that only
// resembles those two keeps nothing.
func TestParseHTMLKeepsDeclarativeShadowRoots(t *testing.T) {
	const page = `<html><head></head><body>` +
		`<div id="host"><template shadowrootmode="open"><p class="shadow">7</p><template><b>{{n}}</b></template><slot></slot></template><p>8</p></div>` +
		`<div><template shadowrootmode="closed"><p>9</p></template></div>` +
		`<div><template SHADOWROOTMODE><p>10</p></template></div>` +
		`<div><template shadowroot="open"><p>11</p></template></div>` +
		`<div><template data-shadowrootmode="open"><p>12</p></template><template class="shadowroot"><p>13</p></template><template shadowrootmodes="open"><p>14</p></template></div>` +
		`</body></html>`
	const want = `<html><head></head><body>` +
		`<div id="host"><template shadowrootmode="open"><p class="shadow">7</p><template></template><slot></slot></template><p>8</p></div>` +
		`<div><template shadowrootmode="closed"><p>9</p></template></div>` +
		`<div><template shadowrootmode=""><p>10</p></template></div>` +
		`<div><template shadowroot="open"><p>11</p></template></div>` +
		`<div><template data-shadowrootmode="open"></template><template class="shadowroot"></template><template shadowrootmodes="open"></template></div>` +
		`</body></html>`
	document, err := ParseHTML([]byte(page))
	if err != nil {
		t.Fatal(err)
	}
	if got := renderedHTML(t, document.Nodes[0]); got != want {
		t.Errorf("the document is\n%s\nwant\n%s", got, want)
	}
	if got := document.Find("p.shadow").Text(); got != "7" {
		t.Errorf("p.shadow reads %q, want 7", got)
	}
	decoded, err := decodeAs(t, "html", "text/html", page)
	if err != nil {
		t.Fatal(err)
	}
	if got := renderedHTML(t, decoded.Data.(*HTMLDecoded).Document.Nodes[0]); got != want {
		t.Errorf("the decoder's document is\n%s\nwant\n%s", got, want)
	}
}

// headEmptyTemplates is emptyTemplates as it was: every HTML template is
// emptied.
func headEmptyTemplates(root *html.Node) {
	for node := root; ; {
		switch {
		case node.DataAtom == atom.Template && node.Type == html.ElementNode && node.Namespace == "":
			for node.FirstChild != nil {
				node.RemoveChild(node.FirstChild)
			}
		case node.FirstChild != nil:
			node = node.FirstChild
			continue
		}
		for node != root && node.NextSibling == nil {
			node = node.Parent
		}
		if node == root {
			return
		}
		node = node.NextSibling
	}
}

// Only a page with a declarative shadow root is parsed into another
// document than it was: every fixture page, none of which has one, and
// pages with templates of every other kind are the document they were,
// node for node.
func TestOnlyADeclarativeShadowRootChangesTheParsedDocument(t *testing.T) {
	pages := map[string]string{
		"templates":           `<div>1<template id="item" class="t"><p>{{n}}</p><template><b>inner</b></template>text<!-- c --></template>2</div>`,
		"in a table":          `<table><template><tr><td>{{n}}</td></tr></template><tr><td>3</td></tr></table>`,
		"in svg and math":     `<svg><template><text>4</text></template></svg><math><template shadowrootmode="open"><mi>5</mi></template></math>`,
		"in the head":         `<html><head><template><meta name="x"></template></head><body></body></html>`,
		"in a select":         `<select><option>a</option><template><option>b</option></template></select>`,
		"resembling":          `<template data-shadowrootmode="open"><p>6</p></template><template class="shadowrootmode"><p>7</p></template><template shadowrootclonable><p>8</p></template>`,
		"no template":         `<table><tr><td shadowrootmode="open">9</td></tr></table><div shadowroot="open"><p>10</p></div>`,
		"empty":               ``,
		"a shadow root":       `<div><template shadowrootmode="open"><p>11</p></template></div>`,
		"a legacy one":        `<div><template shadowroot="closed"><p>12</p></template></div>`,
		"one inside another":  `<template><div><template shadowrootmode="open"><p>13</p></template></div></template>`,
		"another inside one":  `<div><template shadowrootmode="open"><template><p>14</p></template><p>15</p></template></div>`,
		"a shadow root's svg": `<div><template shadowrootmode="open"><svg><template><text>16</text></template></svg></template></div>`,
	}
	shadowed := map[string]bool{"a shadow root": true, "a legacy one": true, "another inside one": true, "a shadow root's svg": true}
	files, err := filepath.Glob("../../testdata/html/*.*html")
	if err != nil || len(files) < 10 {
		t.Fatalf("%d fixtures, %v", len(files), err)
	}
	for _, file := range files {
		page, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		pages[file] = string(page)
	}
	for name, page := range pages {
		document, err := ParseHTML([]byte(page))
		if err != nil {
			t.Fatal(err)
		}
		parsers, err := goquery.NewDocumentFromReader(strings.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		headEmptyTemplates(parsers.Nodes[0])
		if got, was := renderedHTML(t, document.Nodes[0]), renderedHTML(t, parsers.Nodes[0]); (got != was) != shadowed[name] {
			t.Errorf("%s is parsed into\n%s\nand was parsed into\n%s", name, got, was)
		}
	}
}
