//go:build !select_request_types || request_type_http

package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// What a collector or a static target sets that loaded and then failed
// every scrape is refused when the configuration loads: an inline credential
// with a control character, more fixed labels than limits.max_labels_per_metric,
// a static target's port no connection can use, a response limit or a script
// timeout under its least, and a forward_headers entry or a query name that
// is never sent as written.

// checkedHTTPCollector is a configuration of one http collector, c, with
// request and collector settings put in at the two places named.
func checkedHTTPCollector(request, collector string) string {
	return "collectors:\n  - name: c\n    request:\n      type: http\n      path: /x\n" + request + collector +
		"    transform:\n      type: jq\n    metrics:\n      - name: m\n        expression: .a\n"
}

// An inline bearer_token, and a basic_auth username or password, holding a
// control character other than tab is refused when the configuration loads,
// naming the collector or the target, the key and the character and never
// the value: a collector's, one an environment variable with a trailing
// newline gave it, and a static target's. Each loaded, and the bearer token
// then failed every scrape, as Go refuses to send it. A tab, which a header
// value may hold, still loads, as does every credential without such a
// character.
func TestAnInlineCredentialWithAControlCharacterIsRefusedAtLoad(t *testing.T) {
	for name, test := range map[string]struct{ request, want string }{
		"a bearer token with a line feed":          {"      bearer_token: \"secret\\n\"\n", `collector "c" request.bearer_token has the control character '\n' in its value, which a header may not hold` + credentialAdvice},
		"a bearer token with a carriage return":    {"      bearer_token: \"se\\rcret\"\n", `collector "c" request.bearer_token has the control character '\r' in its value, which a header may not hold` + credentialAdvice},
		"a bearer token with a delete":             {"      bearer_token: \"secret\\x7f\"\n", `collector "c" request.bearer_token has the control character '\x7f' in its value, which a header may not hold` + credentialAdvice},
		"a basic password with a line feed":        {"      basic_auth: {username: u, password: \"secret\\n\"}\n", `collector "c" request.basic_auth.password has the control character '\n' in its value, which a Basic credential may not hold` + credentialAdvice},
		"a basic username with a nul":              {"      basic_auth: {username: \"u\\0\", password: secret}\n", `collector "c" request.basic_auth.username has the control character '\x00' in its value, which a Basic credential may not hold` + credentialAdvice},
		"a bearer token with a tab":                {"      bearer_token: \"se\\tcret\"\n", ""},
		"a bearer token":                           {"      bearer_token: secret\n", ""},
		"a basic credential with spaces and a tab": {"      basic_auth: {username: \"u v\", password: \"se\\tcret \"}\n", ""},
		"a basic credential of non-ASCII text":     {"      basic_auth: {username: ünïcode, password: пароль}\n", ""},
		"a bearer token file, read at the request": {"      bearer_token_file: /run/secrets/token\n", ""},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", checkedHTTPCollector(test.request, "")))
		checkRefusal(t, name, err, test.want)
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the message shows the credential: %v", name, err)
		}
	}
	// --config.expand-env takes a variable's value whole, and a Secret
	// made from a file that ends with a line break ends with one.
	t.Setenv("PUE_TEST_TOKEN", "secret\n")
	_, err := Load(testutil.WriteFile(t, "config.yaml", checkedHTTPCollector("      bearer_token: ${PUE_TEST_TOKEN}\n", "")), WithEnvExpansion())
	checkRefusal(t, "a bearer token from a variable with a trailing newline", err, `collector "c" request.bearer_token has the control character '\n' in its value`)

	configuration := checkedHTTPCollector("", "")
	for name, test := range map[string]struct{ request, want string }{
		"a target's bearer token with a line feed": {"bearer_token: \"a\\nb\"", `target "t" request.bearer_token has the control character '\n' in its value, which a header may not hold` + credentialAdvice},
		"a target's basic password with a return":  {"basic_auth: {username: u, password: \"secret\\r\"}", `target "t" request.basic_auth.password has the control character '\r' in its value, which a Basic credential may not hold` + credentialAdvice},
		"a target's bearer token":                  {"bearer_token: secret", ""},
		"a target's basic credential":              {"basic_auth: {username: u, password: secret}", ""},
	} {
		err := loadCheckedTargets(t, configuration, "interval: 1m\ntargets:\n  - name: t\n    collector: c\n    target: http://127.0.0.1:1\n    request: {"+test.request+"}\n")
		checkRefusal(t, name, err, test.want)
	}
}

// An inline basic_auth username with a colon is refused when the
// configuration loads, a collector's and a static target's, naming the key
// and never the value: a Basic credential ends the username at the first
// colon (RFC 7617), so the target read a shorter username and a password
// that started with the rest, and refused every scrape. A password with a
// colon, and a username without one, still load.
func TestAnInlineBasicUsernameWithAColonIsRefusedAtLoad(t *testing.T) {
	const colon = ` holds a colon (":"), which ends the username of a Basic credential (RFC 7617)`
	for name, test := range map[string]struct{ request, want string }{
		"a username with a colon":       {"      basic_auth: {username: \"metrics:reader\", password: secret}\n", `collector "c" request.basic_auth.username` + colon},
		"a username of a colon alone":   {"      basic_auth: {username: \":\", password: secret}\n", `collector "c" request.basic_auth.username` + colon},
		"a password with a colon":       {"      basic_auth: {username: metrics, password: \"se:cret\"}\n", ""},
		"a username with other symbols": {"      basic_auth: {username: \"metrics@example.com;x=1\", password: secret}\n", ""},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", checkedHTTPCollector(test.request, "")))
		checkRefusal(t, name, err, test.want)
		if err != nil && strings.Contains(err.Error(), "reader") {
			t.Errorf("%s: the message shows the credential: %v", name, err)
		}
	}
	configuration := checkedHTTPCollector("", "")
	for name, test := range map[string]struct{ request, want string }{
		"a target's username with a colon": {"basic_auth: {username: \"metrics:reader\", password: secret}", `target "t" request.basic_auth.username` + colon},
		"a target's password with a colon": {"basic_auth: {username: metrics, password: \"se:cret\"}", ""},
	} {
		err := loadCheckedTargets(t, configuration, "interval: 1m\ntargets:\n  - name: t\n    collector: c\n    target: http://127.0.0.1:1\n    request: {"+test.request+"}\n")
		checkRefusal(t, name, err, test.want)
	}
}

// validateCredentialsAsItWas is fetch's validateCredentials before it
// checked the inline credentials' characters, as its messages read.
func validateCredentialsAsItWas(x *model.Collector) error {
	if x.Request.BearerToken != "" && x.Request.BearerTokenFile != "" {
		return fmt.Errorf("collector %q cannot set both request.bearer_token and request.bearer_token_file", x.Name)
	}
	if x.Request.BasicAuth != nil && x.Request.BasicAuthFile != nil {
		return fmt.Errorf("collector %q cannot set both request.basic_auth and request.basic_auth_file", x.Name)
	}
	if x.Request.BasicAuthFile != nil && (strings.TrimSpace(x.Request.BasicAuthFile.Username) == "" || strings.TrimSpace(x.Request.BasicAuthFile.Password) == "") {
		return fmt.Errorf("collector %q basic_auth_file requires username and password paths", x.Name)
	}
	if (x.Request.BasicAuth != nil || x.Request.BasicAuthFile != nil) && (x.Request.BearerToken != "" || x.Request.BearerTokenFile != "") {
		return fmt.Errorf("collector %q cannot configure basic and bearer authentication together", x.Name)
	}
	return nil
}

// The check of an http collector's request says of every set of credential
// keys what it said, but for an inline credential with a control character
// other than tab, and an inline basic username with a colon: over bearer
// tokens, token files, basic credentials and their files, alone and
// together, with every character from 0 to 0x7f and beyond in the inline
// values.
func TestTheCheckOfTheCredentialsIsWhatItWasButForAControlCharacter(t *testing.T) {
	values := []string{"", "secret", "a b", "ünï"}
	for r := rune(0); r <= 0x80; r++ {
		values = append(values, "se"+string(r)+"cret")
	}
	controlled := func(s string) bool {
		return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f })
	}
	refused, colons, same := 0, 0, 0
	for _, value := range values {
		for _, request := range []model.RequestConfig{
			{BearerToken: value},
			{BearerToken: value, BearerTokenFile: "/token"},
			{BasicAuth: &model.BasicAuth{Username: value, Password: "pw"}},
			{BasicAuth: &model.BasicAuth{Username: "u", Password: value}},
			{BasicAuth: &model.BasicAuth{Username: value, Password: value}, BearerToken: value},
			{BasicAuth: &model.BasicAuth{Username: "u", Password: value}, BasicAuthFile: &model.BasicAuthFile{Username: "/u", Password: "/p"}},
			{BasicAuthFile: &model.BasicAuthFile{Username: value, Password: "/p"}},
			{BearerTokenFile: value},
		} {
			request.Type = fetch.RequestTypeHTTP
			x := &model.Collector{Name: "c", Request: request}
			was := validateCredentialsAsItWas(x)
			now := fetch.ValidateRequest(x)
			inline := request.BearerToken
			if request.BasicAuth != nil {
				inline += request.BasicAuth.Username + request.BasicAuth.Password
			}
			if was == nil && controlled(inline) {
				refused++
				if now == nil || !strings.Contains(now.Error(), "has the control character") {
					t.Errorf("%+v: %v, want a refusal for the control character", request, now)
				}
				continue
			}
			if was == nil && request.BasicAuth != nil && strings.Contains(request.BasicAuth.Username, ":") {
				colons++
				if now == nil || !strings.Contains(now.Error(), `request.basic_auth.username holds a colon (":")`) {
					t.Errorf("%+v: %v, want a refusal for the colon", request, now)
				}
				continue
			}
			same++
			if (was == nil) != (now == nil) || was != nil && was.Error() != now.Error() {
				t.Errorf("%+v: the check says %v, and said %v", request, now, was)
			}
		}
	}
	if refused == 0 || colons == 0 || same == 0 {
		t.Errorf("%d sets of credentials were refused for a control character, %d for a colon and %d checked as they were", refused, colons, same)
	}
}

// A collector whose every series would carry more fixed labels than
// limits.max_labels_per_metric is refused when it loads, naming the
// collector or the rule, the count and the limit, as the sibling checks of
// the value and help lengths are: the labels of transform.labels and a
// rule's static labels, each name once, as remove_labels and rename_labels
// leave them. A label that is not certain, one with a placeholder a probe
// may fill with nothing, or one a value_map may map to "", is not counted,
// nor is any label an expression gives; a python rule's labels make no
// labels at all. Each case refused loaded, and every probe failed.
func TestFixedLabelsOverMaxLabelsPerMetricAreRefusedAtLoad(t *testing.T) {
	collector := func(limit int, labels, rule string) string {
		return "collectors:\n  - name: c\n    request:\n      type: http\n      path: /x\n" +
			fmt.Sprintf("    limits:\n      max_labels_per_metric: %d\n", limit) +
			"    transform:\n      type: jq\n" + labels +
			"    metrics:\n      - name: m\n        expression: .a\n" + rule
	}
	twoStatic := "        labels:\n          - name: l1\n            value: x\n          - name: l2\n            value: y\n"
	for name, test := range map[string]struct {
		document, want string
	}{
		"a collector label and a rule's static label over 1": {collector(1, "      labels:\n        team: a\n", "        labels:\n          - name: l1\n            value: x\n"),
			`collector "c" metric "m" gives each of its series 2 labels, its static labels and transform.labels, more than limits.max_labels_per_metric 1, so every series would fail validation; drop labels it does not need or raise the limit`},
		"three collector labels over 2": {collector(2, "      labels: {a: x, b: y, c: z}\n", ""),
			`collector "c" gives every series 3 labels with transform.labels, more than limits.max_labels_per_metric 2, so every series would fail validation; drop labels it does not need or raise the limit`},
		"two static labels over 1":                   {collector(1, "", twoStatic), `metric "m" gives each of its series 2 labels`},
		"a label of both counted once, at the limit": {collector(2, "      labels: {l1: z}\n", twoStatic), ""},
		"at the limit":                               {collector(3, "      labels: {a: x}\n", twoStatic), ""},
		"one removed":                                {collector(1, "      labels: {a: x}\n      remove_labels: [l1]\n", "        labels:\n          - name: l1\n            value: x\n"), ""},
		"one renamed onto another":                   {collector(1, "      labels: {a: x, b: y}\n      rename_labels: {a: b}\n", ""), ""},
		"one renamed, still two":                     {collector(1, "      labels: {a: x, b: y}\n      rename_labels: {a: c}\n", ""), `collector "c" gives every series 2 labels with transform.labels`},
		"a collector label written empty":            {collector(1, "      labels: {a: x, b: \"\"}\n", ""), ""},
		"a label with a placeholder":                 {collector(1, "      labels: {a: x, b: \"{{param_b}}\"}\n", ""), ""},
		"a rule's label with a placeholder":          {collector(1, "      labels: {a: x}\n", "        labels:\n          - name: l1\n            value: \"{{param_l}}\"\n"), ""},
		"a static label a value_map may leave off":   {collector(1, "      labels: {a: x}\n", "        labels:\n          - name: l1\n            value: x\n      - name: m\n        expression: .b\n        labels:\n          - name: l1\n            expression: .l\n            value_map: {x: \"\"}\n"), ""},
		"an expression label":                        {collector(1, "      labels: {a: x}\n", "        labels:\n          - name: l1\n            expression: .l\n"), ""},
		"the default limit, 20":                      {strings.Replace(collector(1, "      labels: {a: x}\n", ""), "    limits:\n      max_labels_per_metric: 1\n", "", 1), ""},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", test.document))
		checkRefusal(t, name, err, test.want)
	}
	python := "collectors:\n  - name: p\n    request: {type: http}\n    limits: {max_labels_per_metric: 1}\n    transform:\n      type: python\n      script: metric('up', 1)\n      labels: LABELS\n    metrics:\n      - name: up\n        labels:\n          - {name: note, expression: note, truncate: true}\n"
	_, err := Load(testutil.WriteFile(t, "config.yaml", strings.Replace(python, "LABELS", "{a: x, b: y}", 1)))
	checkRefusal(t, "a python collector's two labels over 1", err, `collector "p" gives every series 2 labels with transform.labels, more than limits.max_labels_per_metric 1`)
	_, err = Load(testutil.WriteFile(t, "config.yaml", strings.Replace(python, "LABELS", "{a: x}", 1)))
	checkRefusal(t, "a python collector's label and its rule's label to cut", err, "")
}

// The check of the metric families says of a collector what it said before
// fixed labels were counted, wherever they are within the limit: the
// function as it was never read limits.max_labels_per_metric, so it is the
// check of the same collector with the limit switched off, over every
// collector the configurations of the package's tests and examples give and
// a table of labels around the limit.
func TestTheCheckOfTheMetricFamiliesIsWhatItWasWithinTheLabelLimit(t *testing.T) {
	compared, refused := 0, 0
	for labels := 0; labels <= 4; labels++ {
		for static := 0; static <= 3; static++ {
			for limit := 0; limit <= 6; limit++ {
				x := testutil.Collector("c", "json")
				x.Transform.Type = "jq"
				x.Transform.Labels = map[string]string{}
				for i := range labels {
					x.Transform.Labels[fmt.Sprintf("t%d", i)] = "v"
				}
				x.Metrics = []model.MetricRule{{Name: "m", Type: model.GaugeMetricType, Expression: ".a"}}
				for i := range static {
					x.Metrics[0].Labels = append(x.Metrics[0].Labels, model.LabelRule{Name: fmt.Sprintf("s%d", i), Value: "v"})
				}
				// Lengths past their limits too, which the families
				// refused before as they do now.
				for _, help := range []string{"", strings.Repeat("h", 2001)} {
					x.Metrics[0].Description = help
					x.Limits = model.Limits{MaxLabelsPerMetric: limit, MaxLabelValueLength: 500, MaxHelpLength: 2000}
					now := checkMetricFamilies(&x)
					off := x
					off.Limits.MaxLabelsPerMetric = 0
					was := checkMetricFamilies(&off)
					if limit > 0 && labels+static > limit && was == nil {
						refused++
						counted := labels + static
						if labels > limit {
							counted = labels
						}
						if now == nil || !strings.Contains(now.Error(), fmt.Sprintf("%d labels", counted)) {
							t.Errorf("%d collector labels and %d static ones under a limit of %d: %v", labels, static, limit, now)
						}
						continue
					}
					compared++
					if (now == nil) != (was == nil) || now != nil && now.Error() != was.Error() {
						t.Errorf("%d collector labels and %d static ones under a limit of %d: the check says %v, and said %v", labels, static, limit, now, was)
					}
				}
			}
		}
	}
	if refused == 0 || compared == 0 {
		t.Errorf("%d collectors refused for their labels, %d compared", refused, compared)
	}
}

// A static http target whose URL has a port no connection can use, 0 or one
// past 65535, is refused when the target file loads, naming the target and
// the port and not the URL, which may carry credentials; a non-numeric port
// already was, as no URL. Each loaded, and every scrape failed with an
// invalid port. A port from 1 to 65535, in any number of digits, and none
// at all still load.
func TestAStaticTargetsPortOutOfRangeIsRefusedAtLoad(t *testing.T) {
	configuration := checkedHTTPCollector("", "")
	for target, want := range map[string]string{
		"http://127.0.0.1:99999":             `target "t": has the port "99999" in its target URL; want a number from 1 to 65535`,
		"http://127.0.0.1:0":                 `target "t": has the port "0" in its target URL; want a number from 1 to 65535`,
		"https://user:pw@host.example:65536": `target "t": has the port "65536" in its target URL`,
		"http://[::1]:100000/status":         `has the port "100000"`,
		"http://h:99999999999999999999":      `has the port "99999999999999999999"`,
		"http://127.0.0.1:x":                 `target "t": must have an absolute target URL`,
		"http://127.0.0.1:1":                 "",
		"http://127.0.0.1:65535":             "",
		"http://127.0.0.1:080":               "",
		"http://[::1]:8080/status":           "",
		"https://host.example":               "",
		"http://host.example:/x":             "",
	} {
		err := loadCheckedTargets(t, configuration, "interval: 1m\ntargets:\n  - name: t\n    collector: c\n    target: '"+target+"'\n")
		checkRefusal(t, target, err, want)
		if err != nil && strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: the message shows the URL's credentials: %v", target, err)
		}
	}
}

// checkHTTPTargetAsItWas is fetch's checkHTTPTarget before it checked the
// port, for a static target and the collector's schemes left out.
func checkHTTPTargetAsItWas(target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return errors.New("must have an absolute target URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("target scheme %q is not allowed; request.allowed_schemes allows http, https", u.Scheme)
	}
	return nil
}

// The check of a static http target says of every URL what it said, but
// for one whose port is 0 or past 65535: over schemes, hosts and ports
// around the range.
func TestTheCheckOfAStaticTargetIsWhatItWasButForItsPort(t *testing.T) {
	x := testutil.Collector("c", "text")
	if err := fetch.ValidateRequest(&x); err != nil {
		t.Fatal(err)
	}
	refused, same := 0, 0
	for _, scheme := range []string{"http", "https", "ftp", ""} {
		for _, host := range []string{"127.0.0.1", "[::1]", "host.example", "u:p@host.example", ""} {
			for _, port := range []string{"", ":", ":0", ":00", ":1", ":80", ":0080", ":65535", ":65536", ":99999", ":-1", ":x", ":1e3", ":99999999999999999999"} {
				target := host + port + "/status"
				if scheme != "" {
					target = scheme + "://" + target
				}
				was, now := checkHTTPTargetAsItWas(target), fetch.CheckTarget(&x, target, true)
				if u, err := url.Parse(target); was == nil && err == nil && u.Port() != "" && (u.Port() == "0" || u.Port() == "00" || len(u.Port()) >= 5 && u.Port() > "65535") {
					refused++
					if now == nil || !strings.Contains(now.Error(), "in its target URL; want a number from 1 to 65535") {
						t.Errorf("%s: %v, want it refused for its port", target, now)
					}
					continue
				}
				same++
				if (was == nil) != (now == nil) || was != nil && was.Error() != now.Error() {
					t.Errorf("%s: the check says %v, and said %v", target, now, was)
				}
			}
		}
	}
	if refused == 0 || same == 0 {
		t.Errorf("%d targets refused for their port, %d checked as they were", refused, same)
	}
}

// A response limit, request.max_response_bytes or limits.max_response_bytes,
// under 1KiB, and a limits.script_timeout under 1ms, are refused when they
// load, naming the collector, the key, the value and the least; 0, like the
// key left out, is the default, and the least itself loads. A limit under a
// kibibyte is a unit left out, max_response_bytes: 10 for 10MiB, and a
// script given less than a millisecond times out by chance.
func TestAResponseLimitOrAScriptTimeoutUnderItsLeastIsRefusedAtLoad(t *testing.T) {
	for name, test := range map[string]struct{ request, collector, want string }{
		"a request limit of 1":       {"      max_response_bytes: 1\n", "", `collector "c" request.max_response_bytes is 1 bytes, under the least, 1KiB; a size without a unit is a number of bytes, so write the unit, such as 10MiB, or leave it out, or 0, for the default, 10MiB`},
		"a request limit of 1023B":   {"      max_response_bytes: 1023B\n", "", `request.max_response_bytes is 1023 bytes, under the least, 1KiB`},
		"a request limit of 0.5KiB":  {"      max_response_bytes: 0.5KiB\n", "", `request.max_response_bytes is 512 bytes, under the least, 1KiB`},
		"a collector limit of 10":    {"", "    limits:\n      max_response_bytes: 10\n", `collector "c" limits.max_response_bytes is 10 bytes, under the least, 1KiB`},
		"a request limit of 1KiB":    {"      max_response_bytes: 1KiB\n", "", ""},
		"a request limit of 1024":    {"      max_response_bytes: 1024\n", "", ""},
		"a collector limit of 1kB":   {"", "    limits:\n      max_response_bytes: 1kB\n", `limits.max_response_bytes is 1000 bytes, under the least, 1KiB`},
		"a request limit of 0":       {"      max_response_bytes: 0\n", "", ""},
		"a collector limit of 0":     {"", "    limits:\n      max_response_bytes: 0\n", ""},
		"a script timeout of 1ns":    {"", "    limits:\n      script_timeout: 1ns\n", `collector "c" limits.script_timeout is 1ns, under the least, 1ms, in which even a script that does nothing may not answer; leave it out, or 0, for the default, 100ms`},
		"a script timeout of 999µs":  {"", "    limits:\n      script_timeout: 999us\n", `limits.script_timeout is 999µs, under the least, 1ms`},
		"a script timeout of 1ms":    {"", "    limits:\n      script_timeout: 1ms\n", ""},
		"a script timeout of 0":      {"", "    limits:\n      script_timeout: 0\n", ""},
		"a negative script timeout":  {"", "    limits:\n      script_timeout: -1ns\n", "limits.script_timeout is -1ns, and a timeout must not be negative"},
		"a negative response limit":  {"      max_response_bytes: -1\n", "", "size -1 is negative"},
		"a script timeout of 0.5ms":  {"", "    limits:\n      script_timeout: 0.5ms\n", `limits.script_timeout is 500µs, under the least, 1ms`},
		"a script timeout of 1000µs": {"", "    limits:\n      script_timeout: 1000us\n", ""},
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", checkedHTTPCollector(test.request, test.collector)))
		checkRefusal(t, name, err, test.want)
	}
}

// The check of the least limits refuses a response limit from 1 to 1023
// bytes and a script timeout from 1ns to just under 1ms, and nothing else:
// 0, the least and everything past it, and the negative values, which the
// check of the limits before it refuses in its own words.
func TestTheLeastLimitsRefuseOnlyWhatIsUnderThem(t *testing.T) {
	for _, size := range []model.ByteSize{-1, 0, 1, 512, 1023, 1024, 1025, 1 << 20, 1<<63 - 1} {
		for _, key := range []string{"request", "limits"} {
			x := &model.Collector{Name: "c"}
			if key == "request" {
				x.Request.MaxResponseBytes = size
			} else {
				x.Limits.MaxResponseBytes = size
			}
			if err := checkLeastLimits(x); (err != nil) != (size > 0 && size < MinResponseBytes) {
				t.Errorf("%s.max_response_bytes %d: %v", key, size, err)
			}
		}
	}
	for _, timeout := range []time.Duration{-time.Second, 0, 1, time.Microsecond, time.Millisecond - 1, time.Millisecond, time.Second, 1<<63 - 1} {
		x := &model.Collector{Name: "c", Limits: model.Limits{ScriptTimeout: model.Duration(timeout)}}
		if err := checkLeastLimits(x); (err != nil) != (timeout > 0 && timeout < MinScriptTimeout) {
			t.Errorf("script_timeout %s: %v", timeout, err)
		}
	}
}

// An entry of request.forward_headers that would never be forwarded is
// refused when the configuration loads, saying why: one that is no header
// name, empty or with a space or a colon in it, and one of the headers a
// probe never forwards, in any case, Authorization, which
// forward_authorization forwards, Accept-Encoding, which the exporter sets
// itself, and the ones of the connection, any Proxy- header among them.
// Each loaded, and the header never reached the target. Blanks around a
// name are trimmed, as a probe trims them, and a header name like any other
// loads.
func TestAForwardHeadersEntryThatIsNeverForwardedIsRefusedAtLoad(t *testing.T) {
	for entry, want := range map[string]string{
		`""`:                  `collector "c" request.forward_headers has "", which is not a header name; a name is letters, digits and !#$%&'*+-.^_` + "`" + `|~, without spaces`,
		`"  "`:                `request.forward_headers has "  ", which is not a header name`,
		`"X Tenant"`:          `request.forward_headers has "X Tenant", which is not a header name`,
		`"X:Tenant"`:          `request.forward_headers has "X:Tenant", which is not a header name`,
		"Authorization":       `collector "c" request.forward_headers has "Authorization", which is never forwarded: the probe's Authorization is forwarded by request.forward_authorization: true alone`,
		"accept-encoding":     `request.forward_headers has "accept-encoding", which is never forwarded: the exporter asks the target for gzip itself`,
		"Host":                `request.forward_headers has "Host", which is never forwarded: it describes the connection to the exporter, not the request to the target`,
		`" connection "`:      `request.forward_headers has " connection ", which is never forwarded`,
		"TE":                  `has "TE", which is never forwarded`,
		"Content-Length":      `has "Content-Length", which is never forwarded`,
		"Transfer-Encoding":   `has "Transfer-Encoding", which is never forwarded`,
		"Trailer":             `has "Trailer", which is never forwarded`,
		"Upgrade":             `has "Upgrade", which is never forwarded`,
		"Proxy-Authorization": `has "Proxy-Authorization", which is never forwarded`,
		"proxy-tenant":        `has "proxy-tenant", which is never forwarded: it describes the connection`,
		"X-Tenant":            "",
		`" X-Tenant "`:        "",
		"x-proxy-tenant":      "",
		"Accept-Language":     "",
		"Hosts":               "",
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", checkedHTTPCollector("      forward_headers: ["+entry+"]\n", "")))
		checkRefusal(t, "forward_headers: ["+entry+"]", err, want)
	}
}

// A request.query name of nothing, or with a control character, is refused
// when the configuration loads; any other name, spaces and & and = in it
// too, loads, and is sent escaped.
func TestAQueryNameOfNothingOrWithAControlCharacterIsRefusedAtLoad(t *testing.T) {
	for name, want := range map[string]string{
		`""`:          `collector "c" request.query has a parameter without a name; write its name, or leave the parameter out`,
		`"region\n"`:  `collector "c" request.query name "region\n" has the control character '\n' in it; a parameter's name is sent as written, escaped`,
		`"a\tb"`:      `request.query name "a\tb" has the control character '\t' in it`,
		`"a\x7f"`:     `has the control character '\x7f' in it`,
		"region":      "",
		`"a b"`:       "",
		`"a&b=c"`:     "",
		`" "`:         "",
		`"ünïcode"`:   "",
		`"{{param}}"`: `request.query name "{{param}}" has a placeholder`,
	} {
		_, err := Load(testutil.WriteFile(t, "config.yaml", checkedHTTPCollector("      query: {"+name+": v}\n", "")))
		checkRefusal(t, "query name "+name, err, want)
	}
}
