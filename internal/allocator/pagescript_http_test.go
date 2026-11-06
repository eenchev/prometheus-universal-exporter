//go:build !select_request_types || request_type_http

package allocator

import (
	"regexp"
	"strings"
	"testing"
)

// A refresh asked while another waits for the URL as it is now is left to
// that one, but one asked after the URL changed, as the Show source labels
// box changes it, aborts the one waiting and asks again, so the latest
// wins: the box is never dropped for a refresh in flight, and an answer to
// the URL before is never swapped in, nor its abort said to have failed.
func TestTheLatestRefreshWins(t *testing.T) {
	page := ask(jobAllocator(t).Handler(""), "/?job=pods", "").Body.String()
	requireIn(t, page, "if (inFlight && inFlight.url === url.href) return;\n    if (inFlight) inFlight.abort();",
		`fetch(url, {credentials: "same-origin", cache: "no-store", signal: mine.signal})`,
		"const text = await response.text();\n      if (inFlight !== mine) return;",
		"if (inFlight === mine) inFlight = null;",
		`url.searchParams.set("meta", "1");`+"\n      else url.searchParams.delete(\"meta\");\n      history.replaceState(null, \"\", url);\n      refresh();")
	// html/template takes the script's comments out, so the catch is
	// matched over the blanks they leave.
	if !regexp.MustCompile(`\} catch \(error\) \{\s*if \(inFlight !== mine\) return;\s*state\.className = "bad";`).MatchString(page) {
		t.Error("a refresh aborted for a later one says it failed")
	}
	if strings.Contains(page, "if (inFlight) return;") {
		t.Error("a refresh asked while one is in flight is still dropped")
	}
}

// A sort mark is apart from its column's name by a no-break space, \00a0:
// a space after an escape such as \2195 ends the escape, so "\2195 " is
// the mark alone, flush with the name.
func TestTheSortMarksStandApartFromTheirNames(t *testing.T) {
	page := ask(jobAllocator(t).Handler(""), "/", "").Body.String()
	requireIn(t, page, `table.sortable th:not(.num) button::after { content: "\00a0\2195";`,
		`table.sortable th.num button::before { content: "\2195\00a0";`,
		`table.sortable th[aria-sort="ascending"]:not(.num) button::after { content: "\00a0\2191";`,
		`table.sortable th[aria-sort="descending"]:not(.num) button::after { content: "\00a0\2193";`,
		`table.sortable th.num[aria-sort="ascending"] button::before { content: "\2191\00a0";`,
		`table.sortable th.num[aria-sort="descending"] button::before { content: "\2193\00a0";`)
	if eaten := regexp.MustCompile(`content: "[^"]*\\[0-9a-fA-F]{1,6} [^"]*"`).FindAllString(page, -1); len(eaten) > 0 {
		t.Errorf("a content's space ends an escape and is lost: %q", eaten)
	}
}

// The probe jobs table sorts text with the numbers in it by their value,
// job2 before job10, and a letter's case no matter; its headings are
// plain without JavaScript, the script making them buttons and the table
// sortable, so a page without it shows no mark that does nothing.
func TestTheProbeJobsTableSortsNaturallyAndOnlyWithItsScript(t *testing.T) {
	page := ask(jobAllocator(t).Handler(""), "/", "").Body.String()
	requireIn(t, page, `const collator = new Intl.Collator(undefined, {numeric: true, sensitivity: "base"});`,
		"collator.compare(key(x), key(y))",
		`<table class="spread" id="probe-jobs">`+"\n"+`<thead><tr><th data-sort="text">Job</th><th data-sort="text">Collector</th><th class="num" data-sort="num">Targets</th>`,
		`button.append(...head.childNodes);`, `table.classList.add("sortable");`)
	if strings.Contains(page, "localeCompare(") {
		t.Error("the table still sorts text with a plain localeCompare")
	}
	_, table, _ := strings.Cut(page, `id="probe-jobs"`)
	table, _, _ = strings.Cut(table, "</table>")
	if strings.Contains(table, "<button") || strings.Contains(table, "aria-sort") {
		t.Errorf("the table's headings are buttons without the script:\n%s", table)
	}
}

// A job's view writes a failing source's error on a line of its own,
// muted, under the source's line, rather than running on after its
// refresh interval; and its targets' cells keep a target whole where it
// fits, the labels wrapping first.
func TestTheJobViewSetsASourcesErrorApartAndKeepsATargetWhole(t *testing.T) {
	page := ask(jobAllocator(t).Handler(""), "/?job=legacy", "").Body.String()
	if !regexp.MustCompile(`<span class="source"><strong>inventory</strong> <span class="muted">file_sd</span> <span class="bad">failing</span>, refreshed every 1m</span><br><span class="muted">[^<]+</span><br>`).MatchString(page) {
		_, row, _ := strings.Cut(page, "<th>Discovery</th>")
		row, _, _ = strings.Cut(row, "</tr>")
		t.Errorf("the failing source's error is not on a line of its own:\n%s", row)
	}
	requireIn(t, page, "table#job-targets td.target { overflow-wrap: break-word; }")
}
