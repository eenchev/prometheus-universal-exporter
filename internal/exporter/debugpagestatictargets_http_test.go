//go:build !select_request_types || request_type_http

package exporter

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// staticTargetsPageServer has two collectors, rooms with two static
// targets, one of them with credentials in its address, and halls with
// one; lonely has none. It is at full.
func staticTargetsPageServer(t *testing.T) *Server {
	t.Helper()
	cfg := &model.Config{Collectors: []model.Collector{roomsCollector("rooms"), roomsCollector("halls"), roomsCollector("lonely")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "sofia", Collector: "rooms", Target: "https://reader:s3cret-pass@sofia.example/rooms?token=s3cret-token"},
		{Name: "varna", Collector: "rooms", Target: "http://varna.example/rooms"},
		{Name: "plovdiv", Collector: "halls", Target: "http://plovdiv.example/halls"},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	server.SetDebug(DebugFull)
	return server
}

// debugCollectorStaticTargets is what /debug/collector?name=<name> answers
// as static_targets, and whether it answers the key at all.
func debugCollectorStaticTargets(t *testing.T, server *Server, name string) ([]debugStaticTarget, bool) {
	t.Helper()
	response := getPath(server, http.MethodGet, "/debug/collector?name="+name)
	if response.Code != http.StatusOK {
		t.Fatalf("/debug/collector?name=%s: %d %s", name, response.Code, response.Body.String())
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	raw, ok := answer["static_targets"]
	if !ok {
		return nil, false
	}
	var targets []debugStaticTarget
	if err := json.Unmarshal(raw, &targets); err != nil {
		t.Fatal(err)
	}
	return targets, true
}

// Scrape a static target is a list of the static targets scraped through
// the collector chosen, each with its address as the status page shows it,
// not a name to type; the page reads the list again from /debug/collector
// whenever the collector chosen, or the one the playground was copied from,
// changes, which answers it at full alone.
func TestTheDebugPageOffersTheStaticTargetsOfTheCollectorChosen(t *testing.T) {
	server := staticTargetsPageServer(t)
	page := getPage(t, server, "/debug?collector=rooms")
	requireContains(t, page,
		`<input type="radio" name="mode" value="static" id="mode-static"> Scrape a static target`,
		`<label for="static-target">Static target</label>`,
		`<select id="static-target" data-for="rooms">`,
		`<option value="sofia">sofia — https://redacted:redacted@sofia.example/rooms?token=&lt;redacted&gt;</option>`,
		`<option value="varna">varna — http://varna.example/rooms</option>`,
		`<p class="note" id="static-unavailable" hidden></p>`)
	for _, gone := range []string{"plovdiv", "s3cret", `<input id="static-target"`, "Static target name"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page of rooms has %q", gone)
		}
	}
	requireContains(t, getPage(t, server, "/debug?collector=halls"), `<option value="plovdiv">plovdiv — http://plovdiv.example/halls</option>`)
	targets, ok := debugCollectorStaticTargets(t, server, "rooms")
	want := []debugStaticTarget{{Name: "sofia", Target: "https://redacted:redacted@sofia.example/rooms?token=<redacted>"}, {Name: "varna", Target: "http://varna.example/rooms"}}
	if !ok || len(targets) != 2 || targets[0] != want[0] || targets[1] != want[1] {
		t.Errorf("/debug/collector?name=rooms answers static_targets %+v (%v), want %+v", targets, ok, want)
	}
	requireContains(t, debugPageScript(page),
		`const select = $("static-target"), through = fetcher();`,
		`url.searchParams.set("name", through);`,
		`else { targets = answer.static_targets || []; learnForm(through, answer); }`,
		`new Option(target.target ? target.name + " — " + target.target : target.name, target.name)`,
		"  showStaticTargets();\n")
	// At edit nothing is scraped, so neither the page nor /debug/collector
	// names a static target.
	server.SetDebug(DebugEdit)
	if targets, ok := debugCollectorStaticTargets(t, server, "rooms"); ok {
		t.Errorf("at edit /debug/collector answers static_targets %+v", targets)
	}
	if page := getPage(t, server, "/debug?collector=rooms&static_target=sofia"); strings.Contains(page, "sofia") || strings.Contains(page, `<select id="static-target"`) {
		t.Error("at edit the page offers a static target")
	}
}

// A collector no static target is scraped through offers none: the radio
// disabled and a note saying so; with no static target at all, the note
// says that; and the example playground, copied from no collector, scrapes
// none, as it fetches none.
func TestTheDebugPageSaysWhenNoStaticTargetUsesTheCollector(t *testing.T) {
	server := staticTargetsPageServer(t)
	lonely := getPage(t, server, "/debug?collector=lonely")
	requireContains(t, lonely,
		`<input type="radio" name="mode" value="static" id="mode-static" disabled> Scrape a static target`,
		`<p class="note" id="static-unavailable">No static target uses lonely.</p>`,
		`<select id="static-target" data-for="lonely" disabled>
</select>`)
	if targets, ok := debugCollectorStaticTargets(t, server, "lonely"); !ok || targets == nil || len(targets) != 0 {
		t.Errorf("/debug/collector?name=lonely answers static_targets %+v (%v), want []", targets, ok)
	}
	requireContains(t, debugPageScript(lonely), `if (note === "" && targets.length === 0) note = "No static target uses " + through + ".";`)
	playground := getPage(t, server, "/debug")
	requireContains(t, playground,
		`<input type="radio" name="mode" value="static" id="mode-static" disabled> Scrape a static target`,
		`<p class="note" id="fetch-unavailable">Choose a configured collector, or <em>Edit a copy</em> of one, to fetch a target or scrape a static target.</p>`,
		`<p class="note" id="static-unavailable" hidden></p>`)

	none := debugPageServer(t, &model.Config{Collectors: []model.Collector{roomsCollector("rooms")}})
	none.SetDebug(DebugFull)
	for _, path := range []string{"/debug", "/debug?collector=rooms"} {
		requireContains(t, getPage(t, none, path),
			`<input type="radio" name="mode" value="static" id="mode-static" disabled> Scrape a static target`,
			`<p class="note" id="static-unavailable">No static target is configured.</p>`)
	}
}

// ?static_target=<name> opens the page on that target, chosen in the list,
// with the collector it is scraped through chosen on the left, whichever
// collector the address names besides; a name no target has leaves the
// collector the address names.
func TestTheDebugPagePreselectsTheStaticTargetItsAddressNames(t *testing.T) {
	server := staticTargetsPageServer(t)
	for _, path := range []string{"/debug?static_target=varna", "/debug?collector=halls&static_target=varna"} {
		page := getPage(t, server, path)
		requireContains(t, page,
			`<select id="configured" data-chosen="rooms">`,
			`<option value="rooms" selected>rooms (http)</option>`,
			`<option value="varna" selected>varna — http://varna.example/rooms</option>`,
			`<option value="sofia">sofia`)
		if strings.Contains(page, `<option value="halls" selected>`) {
			t.Errorf("%s chose halls", path)
		}
	}
	unknown := getPage(t, server, "/debug?collector=halls&static_target=nobody")
	requireContains(t, unknown, `<select id="configured" data-chosen="halls">`, `<option value="plovdiv">plovdiv`)
	requireContains(t, debugPageScript(unknown),
		`pick($("configured"), $("configured").dataset.chosen);`,
		`if (asked.get("static_target") !== null && $("static-target") && pick($("static-target"), asked.get("static_target"))) choose("static");`)
}

// The list is the static targets in force: a reload of the static target
// file that adds one, or moves one to another collector, shows on the page
// and in /debug/collector.
func TestTheDebugPagesStaticTargetsFollowAReload(t *testing.T) {
	targets := "interval: 1m\ntargets:\n  - name: one\n    collector: first\n    target: http://one.example\n"
	r := newReloadable(t, testutil.CollectorsDocument("first", "second"), targets)
	r.server.SetDebug(DebugFull)
	requireContains(t, getPage(t, r.server, "/debug?collector=first"), `<option value="one">one — http://one.example</option>`)
	requireContains(t, getPage(t, r.server, "/debug?collector=second"), `No static target uses second.`)
	r.write(r.targets, "interval: 1m\ntargets:\n  - name: one\n    collector: second\n    target: http://one.example\n  - name: two\n    collector: first\n    target: http://two.example\n")
	if err := r.manager.Reload(config.ReloadTriggerHTTP); err != nil {
		t.Fatal(err)
	}
	first := getPage(t, r.server, "/debug?collector=first")
	requireContains(t, first, `<option value="two">two — http://two.example</option>`)
	if strings.Contains(first, `<option value="one">`) {
		t.Error("first still offers one after the reload moved it to second")
	}
	requireContains(t, getPage(t, r.server, "/debug?collector=second"), `<option value="one">one — http://one.example</option>`)
	if got, _ := debugCollectorStaticTargets(t, r.server, "first"); len(got) != 1 || got[0].Name != "two" {
		t.Errorf("/debug/collector?name=first after the reload: %+v", got)
	}
	if got, _ := debugCollectorStaticTargets(t, r.server, "second"); len(got) != 1 || got[0].Name != "one" {
		t.Errorf("/debug/collector?name=second after the reload: %+v", got)
	}
}
