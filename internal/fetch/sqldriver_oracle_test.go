//go:build (!select_request_types || request_type_sql) && sql_driver_oracle

package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eenchev/prometheus-universal-exporter/internal/model"
	"github.com/eenchev/prometheus-universal-exporter/internal/testutil/alloctest"
	goora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/configurations"
)

// The oracle driver's URL names the service and the program, escapes the
// credentials, and switches TLS on, unverified when the spec says so; its
// target's port is 1521; it needs service_name and refuses database and
// tls.server_name; and it connects through the exporter's dialer.
func TestTheOracleDriver(t *testing.T) {
	spec := sqlSpec(t, "1521", nil, nil)
	spec.serviceName = "ORCLPDB1"
	if got := oracleURL(spec); !strings.HasPrefix(got, "oracle://metrics:p%40ss%3Aw%2Frd%3F@127.0.0.1:1521/ORCLPDB1?") || !strings.Contains(got, "PROGRAM=prometheus-universal-exporter") || strings.Contains(got, "SSL") {
		t.Errorf("%s", got)
	}
	spec.tls = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // G402: what the URL says of an unverified connection
	if got := oracleURL(spec); !strings.Contains(got, "SSL=TRUE") || !strings.Contains(got, "SSL+VERIFY=FALSE") && !strings.Contains(got, "SSL VERIFY=FALSE") {
		t.Errorf("%s", got)
	}
	f := newSQLFixture(t)
	c := f.collector(t, "db", SQLDriverOracle, "SELECT 1 AS one FROM dual", func(r *model.RequestConfig) { sqlOwn(r, func(o *SQLConfig) { o.ServiceName = "ORCL" }) })
	if address, err := parseSQLTarget(c, "db.example"); err != nil || address.hostPort != "db.example:1521" {
		t.Errorf("%+v %v", address, err)
	}
	for _, tc := range []struct {
		change func(*model.RequestConfig)
		want   string
	}{
		{func(r *model.RequestConfig) { sqlOwn(r, func(o *SQLConfig) { o.ServiceName = "" }) }, "request.service_name is required for the oracle driver"},
		{func(r *model.RequestConfig) { sqlOwn(r, func(o *SQLConfig) { o.Database = "app" }) }, "sets request.database, which the oracle driver does not take"},
		{func(r *model.RequestConfig) { r.TLS.ServerName = "db" }, "sets request.tls.server_name, which the oracle driver cannot honour"},
		{func(r *model.RequestConfig) {
			sqlOwn(r, func(o *SQLConfig) { o.Options = map[string]string{"ssl": "yes"} })
		}, `request.options.ssl is "yes"; the oracle driver takes one of false, true`},
	} {
		c := &model.Collector{Name: "db", Request: model.RequestConfig{Type: RequestTypeSQL, Username: "u", Own: SQLConfig{Driver: SQLDriverOracle, Statement: "q", ServiceName: "ORCL"}}}
		tc.change(&c.Request)
		if err := ValidateRequest(c); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v, want %q", err, tc.want)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	refusing := sqlSpec(t, listen(t), nil, nil)
	refusing.serviceName = "ORCL"
	var refused *TargetRefusedError
	if _, err := connectOracle(ctx, refusing); !errors.As(err, &refused) {
		t.Errorf("%v, want the policy's refusal", err)
	}
}

// oracleURLBefore is oracleURL as it was, by goora.BuildUrl, the oracle of
// the test below.
func oracleURLBefore(spec *sqlConnectSpec) string {
	port, _ := strconv.Atoi(spec.port)
	options := map[string]string{"PROGRAM": spec.appName}
	if spec.tls != nil {
		options["SSL"] = "TRUE"
		if spec.tls.InsecureSkipVerify {
			options["SSL VERIFY"] = "FALSE"
		}
	}
	return goora.BuildUrl(spec.host, port, spec.serviceName, spec.username, spec.password, options)
}

// Any account and password reach go-ora as written, as go-ora's own
// parsing of the URL reads them: one holding ':', which goora.BuildUrl
// left as it is and go-ora ended the account at, and '@', '/', '%', '?',
// '#', blanks and the rest. Every other account, password and service, a
// table and generated ones, gives go-ora the very configuration it was
// given before, with and without TLS.
func TestAnyOracleAccountAndPasswordReachGoOraAsWritten(t *testing.T) {
	parse := func(spec *sqlConnectSpec, build func(*sqlConnectSpec) string) *configurations.ConnectionConfig {
		t.Helper()
		config, err := goora.ParseConfig(build(spec))
		if err != nil {
			t.Fatalf("%q %q %q: %v", spec.username, spec.password, spec.serviceName, err)
		}
		return config
	}
	for _, tc := range []struct{ user, password string }{
		{"me:trics", "x"},
		{"a:b:c", "p:q"},
		{":", ":"},
		{"a@b", "c@d"},
		{"a/b", "c/d"},
		{"100%", "%41%"},
		{"a b", " c "},
		{"q?#&=+", "?#&=+"},
		{"c##metrics", "p@ss:w/rd?#%+ &="},
		{"métrics", "pässwört"},
		{"", "x"},
		{"x", ""},
	} {
		spec := sqlSpec(t, "1521", nil, nil)
		spec.username, spec.password, spec.serviceName = tc.user, tc.password, "orcl"
		if config := parse(spec, oracleURL); config.UserID != tc.user || config.Password != tc.password || config.ServiceName != "orcl" ||
			len(config.Servers) != 1 || config.Servers[0].Addr != "127.0.0.1" || config.Servers[0].Port != 1521 {
			t.Errorf("%q %q: go-ora reads account %q password %q service %q servers %+v", tc.user, tc.password, config.UserID, config.Password, config.ServiceName, config.Servers)
		}
	}
	alphabet := []rune("abcXYZ019 :@/%?#&=+;,.~!$'()*[]\\\"<>^`{|}\t\x00é€")
	random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a repeatable table, not a secret
	word := func() string {
		b := make([]rune, random.IntN(8))
		for i := range b {
			b[i] = alphabet[random.IntN(len(alphabet))]
		}
		return string(b)
	}
	compared := 0
	for i := range alloctest.UnlessRaced(2000, 500) {
		spec := sqlSpec(t, "1521", nil, nil)
		spec.username, spec.password, spec.serviceName = strings.ReplaceAll(word(), ":", ""), word(), "orcl"+strings.ReplaceAll(word(), "/", "")
		if i%2 == 1 {
			spec.tls = &tls.Config{InsecureSkipVerify: i%4 == 1, MinVersion: tls.VersionTLS12} //nolint:gosec // G402: what the URL says of an unverified connection
		}
		if _, err := goora.ParseConfig(oracleURLBefore(spec)); err != nil {
			continue
		}
		if got, want := parse(spec, oracleURL), parse(spec, oracleURLBefore); !reflect.DeepEqual(got, want) {
			t.Errorf("%q %q %q: go-ora reads\n%+v\nwhere it read\n%+v", spec.username, spec.password, spec.serviceName, got, want)
		}
		compared++
	}
	for _, service := range []string{"ORCLPDB1", "pdb/1", "pdb?SSL=false", "a#b", "a%2Fb", "a b"} {
		spec := sqlSpec(t, "1521", nil, nil)
		spec.serviceName = service
		if got, want := parse(spec, oracleURL), parse(spec, oracleURLBefore); !reflect.DeepEqual(got, want) || got.ServiceName != service {
			t.Errorf("service %q: go-ora reads\n%+v\nwhere it read\n%+v", service, got, want)
		}
	}
	if compared < alloctest.UnlessRaced(2000, 500)*9/10 {
		t.Errorf("compared %d configurations", compared)
	}
}

// go-ora's connection is guarded by one that runs statements, resets its
// session and says whether it is valid exactly as it does.
func TestTheGuardedOracleConnectionIsWhatGoOrasIs(t *testing.T) {
	assertGuardedLike(t, SQLDriverOracle, (*goora.Connection)(nil))
}
