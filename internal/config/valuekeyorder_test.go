//go:build !select_request_types

package config

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/testutil"
)

// Of two keys refused by the same check, the error names the one that comes
// first in the file, in either order and on every load: the checks ranged
// over a map of the keys, so which they named changed from one load to the
// next. The negative keepalive_interval and idle_timeout of an ssh
// collector, its blank or templated credential files, a kubernetes
// collector's blank selectors, a json login's blank token and expires_in,
// and the keys of the other kind of login.
func TestTwoKeysOneCheckRefusesAreNamedInTheFilesOrder(t *testing.T) {
	ssh := func(keys string) string {
		return "collectors:\n  - name: c\n    request:\n      type: sshfile\n      root: /srv\n      path: a.prom\n      username: m\n      known_hosts_file: /kh\n" + keys +
			"    decoder: {type: prometheus}\n    transform: {type: prometheus}\n"
	}
	kube := func(keys string) string {
		return "collectors:\n  - name: c\n    request:\n      type: kubernetes\n      resource: pods\n" + keys + "    transform: {type: jq}\n    metrics: [{name: v, expression: .count}]\n"
	}
	login := func(keys string) string {
		return checkedHTTPCollector("      login:\n        type: json\n        url: /login\n        field_files: {password: /p}\n"+keys, "")
	}
	for name, test := range map[string]struct {
		document    func(string) string
		first, then string
		want        []string
	}{
		"negative durations": {ssh, "      keepalive_interval: -1s\n", "      idle_timeout: -1s\n      private_key_file: /k\n",
			[]string{"request.keepalive_interval must not be negative", "request.idle_timeout must not be negative"}},
		"blank credential files": {ssh, "      private_key_file: ' '\n", "      password_file: ' '\n",
			[]string{"request.private_key_file is written with blanks alone", "request.password_file is written with blanks alone"}},
		"templated credential files": {ssh, "      private_key_file: '/k/{{param_x}}'\n", "      password_file: '/p/{{param_x}}'\n",
			[]string{`request.private_key_file "/k/{{param_x}}" has a placeholder`, `request.password_file "/p/{{param_x}}" has a placeholder`}},
		"blank selectors": {kube, "      label_selector: ' '\n", "      field_selector: ' '\n",
			[]string{"request.label_selector is written with blanks alone", "request.field_selector is written with blanks alone"}},
		"blank login expressions": {login, "        token: ' '\n", "        expires_in: ' '\n",
			[]string{"request.login.token is blank", "request.login.expires_in is blank"}},
		"keys of the other login": {login, "        token: .t\n        scopes: [a]\n", "        client_id: x\n",
			[]string{"sets scopes, which a login of type json does not take", "sets client_id, which a login of type json does not take"}},
	} {
		for i, keys := range []string{test.first + test.then, test.then + test.first} {
			// Often enough that an order of a map's would show.
			for range 20 {
				_, err := Load(testutil.WriteFile(t, "config.yaml", test.document(keys)))
				if err == nil || !strings.Contains(err.Error(), test.want[i]) {
					t.Errorf("%s, %d: error %v, want %q", name, i, err, test.want[i])
					break
				}
			}
		}
	}
}
