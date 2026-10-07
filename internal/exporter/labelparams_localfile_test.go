//go:build !select_request_types || request_type_localfile

package exporter

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/config"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// A localfile collector's label values take the probe's parameters as its
// path does. A value a label takes is written as given: only one that also
// names the file is held to what a file's name may be.
func TestALocalFileCollectorsLabelsTakeAProbeParameter(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "billing.prom", promFile)
	testutil.WriteIn(t, root, "default.prom", strings.Replace(promFile, "7", "1", 1))
	server := labelServer(t, `collectors:
  - name: apps
    request: {type: localfile, root: `+root+`, path: "{{param_app:default}}.prom"}
    transform:
      type: prometheus
      labels:
        app: "{{param_app:default}}"
        team: "{{param_team:none}}"
    metrics:
      - name: app_jobs_total
        labels:
          - {name: source, value: "file-{{param_app:default}}"}
`)
	for name, tc := range map[string]struct{ query, want string }{
		"the defaults":              {"collector=apps", `app_jobs_total{app="default",queue="default",source="file-default",team="none"} 1`},
		"the path and the labels":   {"collector=apps&param_app=billing", `app_jobs_total{app="billing",queue="default",source="file-billing",team="none"} 7`},
		"a parameter of a label":    {"collector=apps&param_team=a%2Fb", `app_jobs_total{app="default",queue="default",source="file-default",team="a/b"} 1`},
		"a path beside the label's": {"collector=apps&path=billing.prom&param_app=x%2Fy", `app_jobs_total{app="x/y",queue="default",source="file-x/y",team="none"} 7`},
	} {
		got := probeFile(t, server, tc.query)
		if got.code != http.StatusOK || !slices.Equal(seriesLines(got.body), []string{tc.want}) {
			t.Errorf("%s: %d\n%s", name, got.code, got.body)
		}
	}
	probeFile(t, server, "collector=apps&param_ap=billing").must(t, http.StatusBadRequest, `probe parameters param_ap are not used by collector "apps": no placeholder in its request.path ("{{param_app:default}}.prom") or its label values names them`)
	probeFile(t, server, "collector=apps&param_app=..").must(t, http.StatusBadRequest, `must not be ".."`)
}

// A collector that reads a directory has no file for a probe to name, and
// refuses the path and param_<name> parameters for it. Where its label
// values hold placeholders, a param_ parameter has a label to fill: the
// ones a label names are taken, for every file of the directory, and one
// that no label names is refused as unused; the path stays refused. A
// static target's params fill them as a probe's parameters do.
func TestADirectoryCollectorsLabelsTakeAProbeParameter(t *testing.T) {
	root := t.TempDir()
	testutil.WriteIn(t, root, "a.prom", "# TYPE jobs_total counter\njobs_total{queue=\"x\"} 1\n")
	testutil.WriteIn(t, root, "b.prom", "# TYPE jobs_total counter\njobs_total{queue=\"x\"} 2\n")
	labelled := dirCollector("labelled", root, "*.prom")
	labelled.Transform.Labels = map[string]string{"site": "{{param_site}}", "rack": "{{param_rack:r1}}"}
	labelled.Metrics = []model.MetricRule{{Name: "jobs_total", Labels: []model.LabelRule{{Name: "source", Value: "dir-{{param_site}}"}}}}
	cfg := &model.Config{Collectors: []model.Collector{labelled, dirCollector("plain", root, "*.prom")}}
	file := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{
		{Name: "fra", Collector: "labelled", Params: map[string]string{"param_site": "fra", "param_rack": "r7"}},
	}}
	server := newStaticServer(t, cfg, file)
	server.logger = testutil.QuietLogger(t)
	probeFile(t, server, "collector=labelled&param_site=ams").must(t, http.StatusOK,
		`jobs_total{file="a.prom",queue="x",rack="r1",site="ams",source="dir-ams"} 1`,
		`jobs_total{file="b.prom",queue="x",rack="r1",site="ams",source="dir-ams"} 2`,
		`localfile_scrape_error{file="a.prom"} 0`,
	)
	probeFile(t, server, "collector=labelled").must(t, http.StatusBadRequest, "transform.labels.site needs param_site, which the probe did not supply and which has no default")
	probeFile(t, server, "collector=labelled&param_site=ams&param_sit=x").must(t, http.StatusBadRequest, `probe parameters param_sit are not used by collector "labelled": no placeholder in its request.path ("") or its label values names them`)
	probeFile(t, server, "collector=labelled&param_site=ams&path=a.prom").must(t, http.StatusBadRequest, "probe parameter path: collector \"labelled\" reads every file of a directory that request.files matches, so there is no file for it to name")
	// A directory collector whose labels hold no placeholder refuses every
	// param_ parameter in the words it did.
	probeFile(t, server, "collector=plain&param_site=ams").must(t, http.StatusBadRequest, "probe parameter param_site: collector \"plain\" reads every file of a directory that request.files matches, so there is no file for it to name; name a directory with the target instead")

	server.scrapeStaticTargets(context.Background(), 0)
	recorder := probeOnce(t, server, "/static-targets", nil)
	for _, want := range []string{
		`jobs_total{file="a.prom",queue="x",rack="r7",site="fra",source="dir-fra",static_target="fra"} 1`,
		`jobs_total{file="b.prom",queue="x",rack="r7",site="fra",source="dir-fra",static_target="fra"} 2`,
	} {
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), want+"\n") {
			t.Errorf("the static target's series lack %s: %d\n%s", want, recorder.Code, recorder.Body.String())
		}
	}
	missing := &model.StaticTargetFile{Interval: model.Duration(time.Minute), Targets: []model.StaticTarget{{Name: "t", Collector: "labelled"}}}
	if err := config.ValidateStaticTargets(missing); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateStaticTargetsAgainst(missing, cfg); err == nil || !strings.Contains(err.Error(), `target "t" uses collector "labelled", whose transform.labels.site needs param_site`) {
		t.Errorf("a static target without the parameter: %v", err)
	}
}
