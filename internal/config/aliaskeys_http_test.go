//go:build !select_request_types || request_type_http

package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
	"gopkg.in/yaml.v3"
)

// A key written as an alias of an anchored scalar, *k : value with &k ttl
// elsewhere, is the key the anchor holds, as YAML and the decoder read it.
// The blocks whose keys the exporter checks itself — otlp, web.basic_auth, a
// collector's cache, a static target's request and the top level of a
// collector file — matched the anchor's name instead: a valid key so written
// was refused as unknown, and an anchor named path that held body set the
// request's path. Now each reads it as the decoder does.

// aliasNames are anchors of the keys the documents below write as aliases,
// in a top-level x- list, which the exporter ignores.
const aliasNames = "x-names: [&on enabled, &ep endpoint, &int interval, &u username, &pw password, &t ttl, &s stale_if_error, &bad endpiont, &user user, &ttll ttll, &cs collectors, &m <<]\n"

// A key that is an alias loads in otlp, web.basic_auth and a collector's
// cache, alone and in a mapping merged in, with the values it gives, as
// yaml.v3 alone decodes the same document; one that names an unknown key is
// refused naming that key, at the alias's line, and so is an alias of <<,
// which is the text << to YAML and no merge.
func TestAKeyThatIsAnAliasLoadsInTheBlocksThatCheckTheirOwnKeys(t *testing.T) {
	document := aliasNames + `x-otlp: &otlp {*int : 30s}
otlp: {<<: *otlp, *on : true, *ep : 'http://collector.invalid:4318/v1/metrics'}
web:
  basic_auth: {*on : true, *u : admin, *pw : s3cret}
collectors:
` + mergeCollector + "    cache: {*t : 1m, *s : 1h}\n"
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", document))
	if err != nil {
		t.Fatalf("%v\n%s", err, document)
	}
	if !cfg.OTLP.Enabled || cfg.OTLP.Endpoint != "http://collector.invalid:4318/v1/metrics" || time.Duration(cfg.OTLP.Interval) != 30*time.Second {
		t.Errorf("otlp read as %+v", cfg.OTLP)
	}
	if auth := cfg.Web.BasicAuth; !auth.Enabled || auth.Username != "admin" || auth.Password != "s3cret" {
		t.Errorf("basic_auth read as %+v", auth)
	}
	if c := cfg.Collectors[0].Cache; time.Duration(c.TTL) != time.Minute || time.Duration(c.StaleIfError) != time.Hour {
		t.Errorf("cache read as %+v", c)
	}
	// yaml.v3 alone, without the blocks' own code, reads the same values.
	var plain struct {
		OTLP struct {
			Enabled  bool   `yaml:"enabled"`
			Endpoint string `yaml:"endpoint"`
			Interval string `yaml:"interval"`
		} `yaml:"otlp"`
		Web struct {
			BasicAuth map[string]any `yaml:"basic_auth"`
		} `yaml:"web"`
		Collectors []struct {
			Cache map[string]string `yaml:"cache"`
		} `yaml:"collectors"`
	}
	if err := yaml.Unmarshal([]byte(document), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.OTLP.Enabled != cfg.OTLP.Enabled || plain.OTLP.Endpoint != cfg.OTLP.Endpoint || plain.OTLP.Interval != "30s" || plain.Web.BasicAuth["username"] != "admin" || plain.Web.BasicAuth["password"] != "s3cret" || plain.Collectors[0].Cache["ttl"] != "1m" || plain.Collectors[0].Cache["stale_if_error"] != "1h" {
		t.Errorf("yaml.v3 alone reads %+v", plain)
	}

	for name, test := range map[string]struct{ document, want string }{
		"otlp's unknown key":        {aliasNames + "otlp: {*on : true, *bad : x}\n" + testutil.MinimalConfig, `line 2: unknown key "endpiont" in otlp`},
		"otlp's alias of <<":        {aliasNames + "x-o: &o {endpoint: e}\notlp: {*on : true, *m : *o}\n" + testutil.MinimalConfig, `line 3: unknown key "<<" in otlp`},
		"otlp without enabled":      {aliasNames + "otlp: {*ep : 'http://collector.invalid:4318/v1/metrics'}\n" + testutil.MinimalConfig, "line 2: otlp sets endpoint but not enabled"},
		"basic_auth's unknown key":  {aliasNames + "web: {basic_auth: {*on : true, *user : admin}}\n" + testutil.MinimalConfig, `line 2: unknown key "user" in web.basic_auth`},
		"basic_auth without switch": {aliasNames + "web: {basic_auth: {*u : admin, *pw : s3cret}}\n" + testutil.MinimalConfig, "line 2: web.basic_auth sets username and password but not enabled"},
		"cache's unknown key":       {aliasNames + "collectors:\n" + mergeCollector + "    cache: {*ttll : 1m}\n", `line 9: cache has the unknown key "ttll"`},
		"cache's alias of <<":       {aliasNames + "x-c: &c {ttl: 1m}\ncollectors:\n" + mergeCollector + "    cache: {*m : *c}\n", `line 10: cache has the unknown key "<<"`},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", test.document))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error %v, want %q\n%s", name, err, test.want, test.document)
		}
		// yaml.v3 alone refuses the unknown keys too, by the same name.
		if strings.Contains(test.want, "unknown key") {
			var plain struct {
				OTLP map[string]any `yaml:"otlp"`
				Web  struct {
					BasicAuth map[string]any `yaml:"basic_auth"`
				} `yaml:"web"`
				Collectors []struct {
					Cache map[string]any `yaml:"cache"`
				} `yaml:"collectors"`
			}
			if err := yaml.Unmarshal([]byte(test.document), &plain); err != nil {
				t.Fatal(err)
			}
			key := test.want[strings.Index(test.want, `"`)+1 : strings.LastIndex(test.want, `"`)]
			_, inOTLP := plain.OTLP[key]
			_, inAuth := plain.Web.BasicAuth[key]
			inCache := false
			if len(plain.Collectors) > 0 {
				_, inCache = plain.Collectors[0].Cache[key]
			}
			if !inOTLP && !inAuth && !inCache {
				t.Errorf("%s: yaml.v3 alone has no key %q", name, key)
			}
		}
	}
}

// A static target's request reads a key that is an alias as the key its
// anchor holds: an anchor named path that holds body sets the body, and one
// named body that holds path sets the path, the way round yaml.v3 reads it;
// they were swapped. An unknown key behind an alias is refused naming it.
func TestAKeyThatIsAnAliasInAStaticTargetsRequestIsTheKeyItNames(t *testing.T) {
	document := `x-names: [&path body, &body path, &a attempts]
interval: 1m
targets:
  - name: swapped
    collector: a
    target: http://one.example
    request: {*path : from the target, *body : /target, retry: {*a : 2}}
  - name: body only
    collector: a
    target: http://two.example
    request: {*path : ''}
`
	file, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", document))
	if err != nil {
		t.Fatal(err)
	}
	swapped, bodyOnly := file.Targets[0].Request, file.Targets[1].Request
	if swapped.Body != "from the target" || swapped.Path != "/target" || !swapped.PathSet || !swapped.BodySet || swapped.Retry == nil || *swapped.Retry.Attempts != 2 {
		t.Errorf("the swapped request read as %+v", swapped)
	}
	if bodyOnly.PathSet || !bodyOnly.BodySet || bodyOnly.Body != "" {
		t.Errorf("the request of an aliased body alone read as %+v", bodyOnly)
	}
	var plain struct {
		Targets []struct {
			Request map[string]any `yaml:"request"`
		} `yaml:"targets"`
	}
	if err := yaml.Unmarshal([]byte(document), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.Targets[0].Request["body"] != swapped.Body || plain.Targets[0].Request["path"] != swapped.Path {
		t.Errorf("yaml.v3 alone reads %v", plain.Targets[0].Request)
	}
	if _, path := plain.Targets[1].Request["path"]; path {
		t.Errorf("yaml.v3 alone reads a path in %v", plain.Targets[1].Request)
	}
	bad := "x-names: [&k pth]\ninterval: 1m\ntargets:\n  - name: t\n    collector: a\n    target: http://x\n    request: {*k : /x}\n"
	if _, err := LoadStaticTargets(testutil.WriteFile(t, "targets.yaml", bad)); err == nil || err.Error() != `line 7: unknown key "pth" in a static target's request` {
		t.Errorf("an unknown key behind an alias: %v", err)
	}
}

// A collector file's top level takes collectors written as an alias, alone
// or merged in, and refuses another key behind an alias naming it.
func TestAKeyThatIsAnAliasWorksAtTheTopOfACollectorFile(t *testing.T) {
	dir := t.TempDir()
	collectors := "\n    - {name: a, request: {type: http}, transform: {type: regex}, metrics: [{name: value, expression: 'v=(\\d+)'}]}\n"
	for name, file := range map[string]string{
		"alone":     "x-names: [&cs collectors]\n*cs :" + collectors,
		"merged in": "x-names: [&cs collectors]\nx-all: &all\n  *cs :" + strings.ReplaceAll(collectors, "\n    -", "\n      -") + "<<: *all\n",
	} {
		testutil.WriteIn(t, dir, "more.yaml", file)
		cfg, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [more.yaml]\n"))
		if err != nil {
			t.Errorf("%s: %v\n%s", name, err, file)
			continue
		}
		if len(cfg.Collectors) != 1 || cfg.Collectors[0].Name != "a" {
			t.Errorf("%s: collectors %v", name, testutil.CollectorNames(cfg))
		}
	}
	testutil.WriteIn(t, dir, "more.yaml", "x-names: [&w web]\n*w : {}\ncollectors:"+collectors)
	if _, err := Load(testutil.WriteIn(t, dir, "config.yaml", "collector_files: [more.yaml]\n")); err == nil || !strings.Contains(err.Error(), `line 2: "web" is not allowed`) {
		t.Fatalf("an alias of a key a collector file does not take: %v", err)
	}
}

// With environment expansion on, a top-level key that is an alias of an x-
// key is an x- block like one written out: the decoder ignores it, so a
// reference in it that nothing uses is left alone and its variable need not
// be set. It was expanded, and its unset variable refused the file.
func TestATopLevelAliasOfAnXKeyIsIgnoredByEnvironmentExpansion(t *testing.T) {
	document := "x-names: [&extra x-extra]\n*extra : {url: '${PUE_ALIAS_KEY_UNSET_VARIABLE}'}\n" + testutil.MinimalConfig
	cfg, err := Load(testutil.WriteFile(t, "config.yaml", document), WithEnvExpansion())
	if err != nil {
		t.Fatalf("%v\n%s", err, document)
	}
	if len(cfg.Collectors) != 1 {
		t.Fatalf("collectors %v", testutil.CollectorNames(cfg))
	}
	// A key that is an alias of anything else is not such a block.
	used := "x-names: [&web web]\n*web : {listen_address: '${PUE_ALIAS_KEY_UNSET_VARIABLE}'}\n" + testutil.MinimalConfig
	if _, err := Load(testutil.WriteFile(t, "config.yaml", used), WithEnvExpansion()); err == nil || !strings.Contains(err.Error(), "PUE_ALIAS_KEY_UNSET_VARIABLE") {
		t.Fatalf("a reference in an aliased web: %v", err)
	}
}
