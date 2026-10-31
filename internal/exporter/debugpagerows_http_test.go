//go:build !select_request_types || request_type_http

package exporter

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// The debug page's headers and probe parameters as rows of a name and a
// value, and the repeated names a run takes of them.

// A run's headers and parameters take a list of values for a name given
// more than once: a pasted response has each value of a repeated header,
// as a response with the header twice would, a header_<name> parameter
// forwards each, and any other parameter given twice is refused as /probe
// refuses it.
func TestTheDebugRunTakesRepeatedHeadersAndParameters(t *testing.T) {
	var got http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(debugExampleBody))
	}))
	defer target.Close()
	open := roomsCollector("open")
	open.Request.ForwardHeaders = []string{"X-Tenant"}
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms"), open}})

	run := pastedTo("rooms", debugExampleBody)
	run["headers"] = map[string]any{"X-Trace": []string{"one", "two"}, "X-Single": "three"}
	answer := postRun(t, server, run, http.StatusOK)
	if !answer.OK || answer.Response == nil {
		t.Fatalf("a pasted response with a repeated header: %+v", answer)
	}
	for _, want := range []string{"X-Trace: one", "X-Trace: two", "X-Single: three"} {
		if !slices.Contains(answer.Response.Headers, want) {
			t.Errorf("the response's headers %q lack %q", answer.Response.Headers, want)
		}
	}
	if at := slices.Index(answer.Response.Headers, "X-Trace: one"); at < 0 || at+1 >= len(answer.Response.Headers) || answer.Response.Headers[at+1] != "X-Trace: two" {
		t.Errorf("the repeated header's values are not in the order sent: %q", answer.Response.Headers)
	}

	run = pastedTo("rooms", debugExampleBody)
	run["params"] = map[string]any{"param_room": []string{"a", "b"}}
	if answer := postRun(t, server, run, http.StatusBadRequest); answer.Error != "probe parameter param_room is given 2 times; give it once" {
		t.Errorf("a parameter given twice answered %q", answer.Error)
	}
	for name, values := range map[string]any{"a number": 1, "a list of numbers": []int{1}, "an object": map[string]string{"a": "b"}} {
		run = pastedTo("rooms", debugExampleBody)
		run["headers"] = map[string]any{"X-Trace": values}
		if answer := postRun(t, server, run, http.StatusBadRequest); !strings.Contains(answer.Error, `"X-Trace" has neither a string nor a list of strings`) {
			t.Errorf("a header's value as %s answered %q", name, answer.Error)
		}
	}
	run = pastedTo("rooms", debugExampleBody)
	run["headers"] = map[string]any{"X-Trace": make([]string, debugMaxHeaders+1)}
	if answer := postRun(t, server, run, http.StatusBadRequest); !strings.Contains(answer.Error, "101 headers; a pasted response may have at most 100") {
		t.Errorf("101 values of one header answered %q", answer.Error)
	}

	server.SetDebug(DebugFull)
	answer = postRun(t, server, map[string]any{"configured": "open", "target": target.URL, "params": map[string]any{"header_X-Tenant": []string{"a", "b"}}}, http.StatusOK)
	if !answer.OK || !slices.Equal(got.Values("X-Tenant"), []string{"a", "b"}) {
		t.Errorf("header_X-Tenant given twice sent %q: %+v", got.Values("X-Tenant"), answer)
	}
}

// The page asks for the headers and the probe parameters as rows, a name
// and a value, which its script makes, labels, checks as the server does
// and sends as a run's headers and params; no field takes them as lines.
func TestTheDebugPageEditsHeadersAndParametersAsRows(t *testing.T) {
	server := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	server.SetDebug(DebugFull)
	page := getPage(t, server, "/debug?collector=rooms")
	for _, gone := range []string{`<textarea id="headers"`, `<textarea id="params"`, "one <code>Name: value</code> a line", "one <code>name=value</code> a line", `"also: "`, "lines($("} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %q", gone)
		}
	}
	requireContains(t, page,
		// The markup the script fills.
		`<div id="headers-part" role="group" aria-labelledby="headers-title">`, `<div class="kv-title" id="headers-title">Other headers</div>`,
		`<div class="kv" id="headers-rows"></div>`, `<button type="button" class="secondary" id="headers-add">+ Add header</button>`,
		`<div id="params-part" role="group" aria-labelledby="params-title">`, `<div class="kv-title" id="params-title">Probe parameters</div>`,
		`<datalist id="params-names"></datalist>`, `<div class="kv" id="params-rows"></div>`,
		`<button type="button" class="secondary" id="params-add">+ Add parameter</button>`,
		`<p class="note">A parameter left empty takes its default, shown greyed in its box; a name box suggests the parameters the collector takes.</p>`,
		`<p class="note" id="no-params" hidden>This collector takes no probe parameters.</p>`,
		// A row, labelled by its number, made with value and textContent.
		`row.name.setAttribute("aria-label", title + " name " + (i + 1));`, `row.value.setAttribute("aria-label", title + " value " + (i + 1));`,
		`row.remove.setAttribute("aria-label", "Remove " + noun + " " + (i + 1));`, `row.remove.textContent = "\u00d7";`,
		`if (names) row.name.setAttribute("list", names);`, `option.value = name;`, `row.value.placeholder = hint(name.trim());`,
		`const next = rows[at] || rows[at - 1];`, `(next ? next.name : addButton).focus();`, `if (event.key !== "Enter" || rows[rows.length - 1] !== row) return;`,
		`addButton.addEventListener("click", () => add("", "", true));`, `headers.add();`,
		// The checks, worded as the server's.
		`message = "A " + noun + " needs a name: give one, or clear the value.";`,
		`"Header name " + JSON.stringify(name) + " is not one HTTP allows."`,
		`"Probe parameter " + name + " is given " + all.filter((other) => other === name).length + " times; give it once."`,
		`if (name === "target" || name === "collector" || name === "debug") return name + " is one the debug page sets itself.";`,
		`if (headerParam(name) || !before.includes(name)) return "";`,
		`row.name.setAttribute("aria-invalid", "true");`, `row.name.setAttribute("aria-describedby", row.error.id);`,
		`throw new Error("A " + what + " row is wrong; it says why below it.");`,
		// What is sent: a blank name skipped, one value a string, more a
		// list; a header's name in any case, a parameter's as written.
		`if (name === "") continue;`, `(out[key] ||= []).push(row.value.value);`,
		`for (const name of Object.keys(out)) if (out[name].length === 1) out[name] = out[name][0];`,
		`request.headers = reads.http_answer ? filled(headers, "header", (name) => name.toLowerCase()) : {};`,
		`request.params = $("params-part").hidden ? {} : filled(params, "probe parameter", (name) => name);`,
		// The rows of a collector's parameters, and what is kept of them.
		`paramDefaults = new Map(own.map((param) => [param.name, param.default]));`,
		`const typed = params.rows.map((row) => ({ name: row.name.value.trim(), value: row.value.value })).filter((row) => takes.has(row.name));`,
		`params.add(param.name, at < 0 ? "" : typed.splice(at, 1)[0].value);`,
		// Name, value and × one line high, side by side while they fit.
		`.kv-row { display: flex; align-items: flex-start; gap: 6px; }`,
		`.kv-fields { display: flex; flex-wrap: wrap; gap: 6px; flex: 1 1 auto; min-width: 0; }`,
		`.kv-fields input { flex: 1 1 8em; width: auto; min-width: 0; height: 34px; }`,
		`.kv-row button.kv-remove { flex: 0 0 34px; width: 34px; height: 34px; padding: 0; margin: 0;`,
	)
	// The script's test of a header's name is the server's.
	if want := "const headerName = /" + strings.ReplaceAll(debugHeaderName.String(), "`", `\x60`) + "/;"; !strings.Contains(page, want) {
		t.Errorf("the page lacks %q", want)
	}
}
