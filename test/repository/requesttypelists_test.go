package repository

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The documentation named the request types in a dozen lists, each written
// when the list was complete, and each fell behind as dns, tlscert,
// kubernetes and s3file arrived: a page said label placeholders work with
// five types out of ten, the static targets page left four out of what a
// target takes, the build_info example showed a build from before dns. A
// list that stands for every type is marked in the markdown, and these
// tests keep each marked list naming every internal/fetch/requesttype_<name>.go
// of the tree, so a new type fails here until the lists name it (spec § 35).

// treeRequestTypes are the request types of the tree, opt-in ones included:
// the names of internal/fetch/requesttype_<name>.go but the guard, none.
func treeRequestTypes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("internal/fetch/requesttype_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "requesttype_"), ".go"); name != "none" {
			types = append(types, name)
		}
	}
	if len(types) < 6 {
		t.Fatalf("found %d request types under internal/fetch; the files have moved", len(types))
	}
	return types
}

// requestTypesMarker opens a marked list: all, every type of the tree, each
// written as the optional format says (`%s` by default); or default, every
// type of a default build and no opt-in one, each written as a word of a
// comma-separated list or in backquotes. requestTypesEnd closes it. Each
// is a line of its own, so a page that quotes a marker, as the spec does,
// marks nothing.
var (
	requestTypesMarker = regexp.MustCompile(`(?m)^<!-- request-types: (all|default)(?: "([^"]*)")? -->$`)
	requestTypesEnd    = regexp.MustCompile(`(?m)^<!-- /request-types -->$`)
)

// requestTypeList is one marked list of a page.
type requestTypeList struct {
	page, kind, format, text string
	line                     int
}

func markedRequestTypeLists(t *testing.T, page string) []requestTypeList {
	t.Helper()
	text := read(t, page)
	var lists []requestTypeList
	for _, m := range requestTypesMarker.FindAllStringSubmatchIndex(text, -1) {
		rest := text[m[1]:]
		loc := requestTypesEnd.FindStringIndex(rest)
		if loc == nil {
			t.Errorf("%s:%d: a request-types marker has no <!-- /request-types --> after it", page, strings.Count(text[:m[0]], "\n")+1)
			continue
		}
		end := loc[0]
		if next := requestTypesMarker.FindStringIndex(rest[:end]); next != nil {
			t.Errorf("%s:%d: a request-types marker opens inside another", page, strings.Count(text[:m[1]+next[0]], "\n")+1)
		}
		list := requestTypeList{page: page, kind: text[m[2]:m[3]], text: rest[:end], line: strings.Count(text[:m[0]], "\n") + 1, format: "`%s`"}
		if m[4] >= 0 {
			list.format = text[m[4]:m[5]]
		}
		lists = append(lists, list)
	}
	if n := len(requestTypesEnd.FindAllStringIndex(text, -1)); n != len(lists) {
		t.Errorf("%s has %d request-types markers and %d <!-- /request-types -->", page, len(lists), n)
	}
	return lists
}

// listNames reports whether a marked list names a type: as its format
// writes it, or, in a default list, as a word of a comma-separated list
// such as request_types="dns,graphite".
func listNames(list requestTypeList, typ string) bool {
	if strings.Contains(list.text, fmt.Sprintf(list.format, typ)) {
		return true
	}
	return list.kind == "default" && regexp.MustCompile(`[",]`+regexp.QuoteMeta(typ)+`[",]`).MatchString(list.text)
}

// requestTypeListProblems is what is wrong with the marked lists of the
// pages, given the types of the tree and the opt-in ones among them.
func requestTypeListProblems(lists []requestTypeList, types, optIn []string) []string {
	var problems []string
	for _, list := range lists {
		for _, typ := range types {
			named := listNames(list, typ)
			switch {
			case list.kind == "all" && !named:
				problems = append(problems, fmt.Sprintf("%s:%d: the list marked request-types: all does not name the %s request type as %s", list.page, list.line, typ, fmt.Sprintf(list.format, typ)))
			case list.kind == "default" && !named && !slices.Contains(optIn, typ):
				problems = append(problems, fmt.Sprintf("%s:%d: the list marked request-types: default does not name the %s request type, which a default build carries", list.page, list.line, typ))
			case list.kind == "default" && named && slices.Contains(optIn, typ):
				problems = append(problems, fmt.Sprintf("%s:%d: the list marked request-types: default names the opt-in %s request type, which a default build leaves out", list.page, list.line, typ))
			}
		}
	}
	return problems
}

// requestTypeListPages are the pages that must hold a marked list, each
// standing for every request type; a marker is honoured on any page.
var requestTypeListPages = map[string]int{
	"docs/CONFIGURATION.md":  2, // each type's keys, and each type's probe parameters
	"docs/STATIC-TARGETS.md": 1, // what a static target of each type takes
	"docs/REQUESTS.md":       1, // the types label placeholders work with
	"docs/SELF-METRICS.md":   1, // the build_info example, a default build
}

func TestMarkedListsNameEveryRequestType(t *testing.T) {
	types := treeRequestTypes(t)
	optIn := optInRequestTypes(t)
	var lists []requestTypeList
	for _, page := range markdownFiles(t) {
		found := markedRequestTypeLists(t, page)
		if want, ok := requestTypeListPages[page]; ok && len(found) != want {
			t.Errorf("%s has %d lists marked request-types, want %d: a marker has been dropped", page, len(found), want)
		}
		lists = append(lists, found...)
	}
	for _, problem := range requestTypeListProblems(lists, types, optIn) {
		t.Error(problem)
	}
}

// The check itself: a list missing a type, a default list naming an opt-in
// one or missing a default one, and a format the list does not follow are
// each found, and complete lists pass.
func TestTheRequestTypeListCheckFindsWhatIsMissing(t *testing.T) {
	types, optIn := []string{"dns", "http", "s3file"}, []string{"s3file"}
	for _, c := range []struct {
		list requestTypeList
		want int
	}{
		{requestTypeList{kind: "all", format: "`%s`", text: "`dns`, `http` and the opt-in `s3file`"}, 0},
		{requestTypeList{kind: "all", format: "`%s`", text: "`dns` and `http`"}, 1},
		{requestTypeList{kind: "all", format: "For `%s`,", text: "For `http`, this. For `dns`, that. `s3file` too."}, 1},
		{requestTypeList{kind: "default", format: "`%s`", text: `request_types="dns,http"`}, 0},
		{requestTypeList{kind: "default", format: "`%s`", text: `request_types="dns,http,s3file"`}, 1},
		{requestTypeList{kind: "default", format: "`%s`", text: `request_types="http"`}, 1},
		{requestTypeList{kind: "default", format: "`%s`", text: `request_types="dnsx,http"`}, 1},
	} {
		if got := requestTypeListProblems([]requestTypeList{c.list}, types, optIn); len(got) != c.want {
			t.Errorf("%+v: %d problems, want %d: %q", c.list, len(got), c.want, got)
		}
	}
}

// Every request type but http, which CONFIGURATION.md and REQUESTS.md
// cover, has a page of its own, docs/<NAME>.md, which spec § 35 requires
// by name and the README's index and CONFIGURATION.md's related pages link.
func TestEveryRequestTypeHasItsPage(t *testing.T) {
	spec := read(t, "docs/SPECIFICATION-EXPORTER.md")
	start := strings.Index(spec, "\n# 35. Documentation requirements")
	end := strings.Index(spec, "\n# 36. ")
	if start < 0 || end < start {
		t.Fatal("docs/SPECIFICATION-EXPORTER.md has no § 35 before § 36; the section has moved")
	}
	required := spec[start:end]
	readme := read(t, "README.md")
	_, related, ok := strings.Cut(read(t, "docs/CONFIGURATION.md"), "\n## Related pages\n")
	if !ok {
		t.Fatal("docs/CONFIGURATION.md has no \"## Related pages\" section; it has moved")
	}
	for _, typ := range treeRequestTypes(t) {
		if typ == "http" {
			continue
		}
		page := strings.ToUpper(typ) + ".md"
		if _, err := os.Stat("docs/" + page); err != nil {
			t.Errorf("the %s request type has no page docs/%s", typ, page)
			continue
		}
		if !strings.Contains(required, "`docs/"+page+"`") {
			t.Errorf("spec § 35 does not require docs/%s, the %s request type's page", page, typ)
		}
		if !strings.Contains(readme, "](docs/"+page+")") {
			t.Errorf("the README's index does not link docs/%s, the %s request type's page", page, typ)
		}
		if !strings.Contains(related, "]("+page+")") {
			t.Errorf("the related pages of docs/CONFIGURATION.md do not link %s, the %s request type's page", page, typ)
		}
	}
}

// The code layout of docs/DEVELOPMENT.md is where a contributor looks for
// where a thing lives, and it went without internal/allocclient, the
// root's allocator.go and tools/depupdate. Every package go list finds,
// and every file of the root package that is not a test, is named there in
// backquotes.
func TestTheCodeLayoutNamesEveryPackage(t *testing.T) {
	_, layout, ok := strings.Cut(read(t, "docs/DEVELOPMENT.md"), "\n## Code layout\n")
	if !ok {
		t.Fatal("docs/DEVELOPMENT.md has no \"## Code layout\" section; it has moved")
	}
	if next := strings.Index(layout, "\n## "); next >= 0 {
		layout = layout[:next]
	}
	module, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "list", "./...").Output()
	if err != nil {
		t.Fatal(err)
	}
	prefix := strings.TrimSpace(string(module)) + "/"
	var named int
	for _, pkg := range strings.Fields(string(out)) {
		rel, ok := strings.CutPrefix(pkg, prefix)
		if !ok {
			continue // the root package, whose files are checked below
		}
		named++
		if !strings.Contains(layout, "`"+rel+"`") {
			t.Errorf("the code layout of docs/DEVELOPMENT.md does not name the package %s", rel)
		}
	}
	if named < 10 {
		t.Fatalf("go list found %d packages below the root; the module has moved", named)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") && !strings.Contains(layout, "`"+f+"`") {
			t.Errorf("the code layout of docs/DEVELOPMENT.md does not name %s, a file of the root package", f)
		}
	}
}
